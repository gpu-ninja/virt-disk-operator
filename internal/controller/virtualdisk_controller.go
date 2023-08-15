/* SPDX-License-Identifier: Apache-2.0
 *
 * Copyright 2023 Damian Peckett <damian@pecke.tt>.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dario.cat/mergo"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/docker/go-units"
	"github.com/gpu-ninja/operator-utils/zaplogr"
	virtdiskv1alpha1 "github.com/gpu-ninja/virt-disk-operator/api/v1alpha1"
	"github.com/gpu-ninja/virt-disk-operator/internal/constants"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Allow recording of events.
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;update;patch

// Need to be able to get pods (primarily for determining the image to use).
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Need to be able to manage daemonsets.
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete

// +kubebuilder:rbac:groups=virt-disk.gpu-ninja.com,resources=virtualdisks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=virt-disk.gpu-ninja.com,resources=virtualdisks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=virt-disk.gpu-ninja.com,resources=virtualdisks/finalizers,verbs=update

const (
	defaultOperatorImage = "ghcr.io/gpu-ninja/virt-disk-operator:latest"
)

// VirtualDiskReconciler reconciles a VirtualDisk object
type VirtualDiskReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	EventRecorder record.EventRecorder
}

func (r *VirtualDiskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := zaplogr.FromContext(ctx)

	logger.Info("Reconciling")

	var vdisk virtdiskv1alpha1.VirtualDisk
	if err := r.Get(ctx, req.NamespacedName, &vdisk); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	if !controllerutil.ContainsFinalizer(&vdisk, constants.FinalizerName) {
		logger.Info("Adding Finalizer")

		_, err := controllerutil.CreateOrPatch(ctx, r.Client, &vdisk, func() error {
			controllerutil.AddFinalizer(&vdisk, constants.FinalizerName)

			return nil
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
	}

	if !vdisk.ObjectMeta.DeletionTimestamp.IsZero() {
		logger.Info("Deleting")

		if controllerutil.ContainsFinalizer(&vdisk, constants.FinalizerName) {
			logger.Info("Removing Finalizer")

			_, err := controllerutil.CreateOrPatch(ctx, r.Client, &vdisk, func() error {
				controllerutil.RemoveFinalizer(&vdisk, constants.FinalizerName)

				return nil
			})
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
			}
		}

		return ctrl.Result{}, nil
	}

	if vdisk.Status.Phase == virtdiskv1alpha1.VirtualDiskPhaseFailed {
		logger.Info("Virtual disk device is in failed state, ignoring")

		return ctrl.Result{}, nil
	}

	logger.Info("Creating or updating")

	image, err := r.getOperatorImage(ctx)
	if err != nil {
		logger.Error("Failed to get operator image", zap.Error(err))

		return ctrl.Result{}, fmt.Errorf("failed to get operator image: %w", err)
	}

	dsNamespaceName := types.NamespacedName{Name: vdisk.Name, Namespace: vdisk.Namespace}
	ds := appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dsNamespaceName.Name,
			Namespace: dsNamespaceName.Namespace,
		},
	}

	var creating bool
	if err := r.Get(ctx, dsNamespaceName, &ds); err != nil && errors.IsNotFound(err) {
		creating = true
	}

	opResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &ds, func() error {
		if err := controllerutil.SetControllerReference(&vdisk, &ds, r.Scheme); err != nil {
			return fmt.Errorf("failed to set owner reference: %w", err)
		}

		initContainers := []corev1.Container{{
			Name:  "load-nbd-module",
			Image: image,
			Command: []string{
				"/sbin/modprobe",
			},
			Args: []string{
				"nbd",
			},
			SecurityContext: &corev1.SecurityContext{
				Privileged: ptr.To(true),
			},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      "modules",
				MountPath: "/lib/modules",
				ReadOnly:  true,
			}},
		}}

		args := []string{
			"mount",
			"--image=" + filepath.Join(vdisk.Spec.HostPath, fmt.Sprintf("%s-%s.qcow2", vdisk.Namespace, vdisk.Name)),
			"--size=" + units.BytesSize(float64(vdisk.Spec.Size.Value())),
		}

		if vdisk.Spec.LVM != nil {
			initContainers = append(initContainers, corev1.Container{
				Name:    "clean-up-orphaned-device",
				Image:   image,
				Command: []string{"/bin/sh"},
				Args: []string{
					"-c",
					"[ -d $DEV ] && /sbin/dmsetup remove $DEV || true",
				},
				Env: []corev1.EnvVar{{
					Name:  "DEV",
					Value: toDevMapperPath(vdisk.Spec.LVM),
				}},
				SecurityContext: &corev1.SecurityContext{
					Privileged: ptr.To(true),
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "dev",
					MountPath: "/dev",
				}},
			})

			args = append(args, "--lv="+vdisk.Spec.LVM.LogicalVolume, "--vg="+vdisk.Spec.LVM.VolumeGroup)
		}

		if vdisk.ObjectMeta.Labels == nil {
			vdisk.ObjectMeta.Labels = make(map[string]string)
		}

		for k, v := range vdisk.ObjectMeta.Labels {
			vdisk.ObjectMeta.Labels[k] = v
		}

		vdisk.ObjectMeta.Labels["app.kubernetes.io/name"] = "virt-disk"
		vdisk.ObjectMeta.Labels["app.kubernetes.io/component"] = "virt-disk"
		vdisk.ObjectMeta.Labels["app.kubernetes.io/instance"] = vdisk.Name

		spec := appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":      "virt-disk",
					"app.kubernetes.io/component": "virt-disk",
					"app.kubernetes.io/instance":  vdisk.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: vdisk.ObjectMeta.Labels,
				},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To(int64(10)),
					NodeSelector:                  vdisk.Spec.NodeSelector,
					InitContainers:                initContainers,
					Containers: []corev1.Container{{
						Name:  "virt-disk",
						Image: image,
						Args:  args,
						SecurityContext: &corev1.SecurityContext{
							Privileged: ptr.To(true),
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "dev",
							MountPath: "/dev",
						}, {
							Name:      "udev",
							MountPath: "/run/udev",
						}, {
							Name:      "data",
							MountPath: vdisk.Spec.HostPath,
						}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path:   "/readyz",
									Port:   intstr.FromInt(8081),
									Scheme: corev1.URISchemeHTTP,
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "dev",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: "/dev",
							},
						},
					}, {
						Name: "udev",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: "/run/udev",
							},
						},
					}, {
						Name: "modules",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: "/lib/modules",
							},
						},
					}, {
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: vdisk.Spec.HostPath,
							},
						},
					}},
				},
			},
		}

		if creating {
			ds.Spec = spec
		} else if err := mergo.Merge(&ds.Spec, spec, mergo.WithOverride, mergo.WithSliceDeepCopy); err != nil {
			return fmt.Errorf("failed to merge spec: %w", err)
		}

		return nil
	})
	if err != nil {
		logger.Error("Failed to reconcile daemonset", zap.Error(err))

		r.EventRecorder.Eventf(&vdisk, corev1.EventTypeWarning,
			"Failed", "Failed to reconcile daemonset: %s", err)

		r.markFailed(ctx, &vdisk,
			fmt.Errorf("failed to reconcile daemonset: %w", err))

		return ctrl.Result{}, nil
	}

	if opResult != controllerutil.OperationResultNone {
		logger.Info("DaemonSet successfully reconciled, marking as pending",
			zap.String("operation", string(opResult)))

		if err := r.markPending(ctx, &vdisk); err != nil {
			return ctrl.Result{}, err
		}
	}

	if ds.Status.DesiredNumberScheduled == 0 || ds.Status.NumberReady != ds.Status.DesiredNumberScheduled {
		r.EventRecorder.Event(&vdisk, corev1.EventTypeNormal,
			"Pending", "Waiting for daemonset pods to be ready")

		if err := r.markPending(ctx, &vdisk); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: constants.ReconcileRetryInterval}, nil
	}

	if vdisk.Status.Phase != virtdiskv1alpha1.VirtualDiskPhaseReady {
		r.EventRecorder.Event(&vdisk, corev1.EventTypeNormal,
			"Created", "Successfully created")

		if err := r.markReady(ctx, &vdisk); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *VirtualDiskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&virtdiskv1alpha1.VirtualDisk{}).
		Owns(&appsv1.DaemonSet{}).
		Complete(r)
}

func (r *VirtualDiskReconciler) markPending(ctx context.Context, vdisk *virtdiskv1alpha1.VirtualDisk) error {
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, vdisk, func() error {
		vdisk.Status.ObservedGeneration = vdisk.Generation
		vdisk.Status.Phase = virtdiskv1alpha1.VirtualDiskPhasePending

		meta.SetStatusCondition(&vdisk.Status.Conditions, metav1.Condition{
			Type:               string(virtdiskv1alpha1.VirtualDiskConditionTypePending),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: vdisk.Generation,
			Reason:             "Pending",
			Message:            "Virtual disk device is pending",
		})

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to mark as pending: %w", err)
	}

	return nil
}

func (r *VirtualDiskReconciler) markReady(ctx context.Context, vdisk *virtdiskv1alpha1.VirtualDisk) error {
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, vdisk, func() error {
		vdisk.Status.ObservedGeneration = vdisk.Generation
		vdisk.Status.Phase = virtdiskv1alpha1.VirtualDiskPhaseReady

		meta.SetStatusCondition(&vdisk.Status.Conditions, metav1.Condition{
			Type:               string(virtdiskv1alpha1.VirtualDiskConditionTypeReady),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: vdisk.Generation,
			Reason:             "Ready",
			Message:            "Virtual disk device is ready",
		})

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to mark as ready: %w", err)
	}

	return nil
}

func (r *VirtualDiskReconciler) markFailed(ctx context.Context, vdisk *virtdiskv1alpha1.VirtualDisk, err error) {
	logger := zaplogr.FromContext(ctx)

	_, updateErr := controllerutil.CreateOrPatch(ctx, r.Client, vdisk, func() error {
		vdisk.Status.ObservedGeneration = vdisk.Generation
		vdisk.Status.Phase = virtdiskv1alpha1.VirtualDiskPhaseFailed

		meta.SetStatusCondition(&vdisk.Status.Conditions, metav1.Condition{
			Type:               string(virtdiskv1alpha1.VirtualDiskConditionTypeFailed),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: vdisk.Generation,
			Reason:             "Failed",
			Message:            err.Error(),
		})

		return nil
	})
	if updateErr != nil {
		logger.Error("Failed to mark as failed", zap.Error(updateErr))
	}
}

func (r *VirtualDiskReconciler) getOperatorImage(ctx context.Context) (string, error) {
	logger := zaplogr.FromContext(ctx)

	podName, ok := os.LookupEnv("POD_NAME")
	if !ok {
		logger.Warn("Running outside of cluster, \"POD_NAME\" is not set, using default image")

		return defaultOperatorImage, nil
	}

	podNamespace, ok := os.LookupEnv("POD_NAMESPACE")
	if !ok {
		logger.Warn("Running outside of cluster, \"POD_NAMESPACE\" is not set, using default image")

		return defaultOperatorImage, nil
	}

	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{
		Name:      podName,
		Namespace: podNamespace,
	}, &pod); err != nil {
		return "", err
	}

	if len(pod.Spec.Containers) == 0 {
		return "", fmt.Errorf("no containers found in the operator pod")
	}

	return pod.Spec.Containers[0].Image, nil
}

func toDevMapperPath(lvm *virtdiskv1alpha1.VirtualDiskLVMSpec) string {
	lvmEscape := func(input string) string {
		return strings.ReplaceAll(input, "-", "--")
	}

	return fmt.Sprintf("/dev/mapper/%s-%s", lvmEscape(lvm.VolumeGroup), lvmEscape(lvm.LogicalVolume))
}
