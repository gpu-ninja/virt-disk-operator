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
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/docker/go-units"
	"github.com/gpu-ninja/operator-utils/updater"
	"github.com/gpu-ninja/operator-utils/zaplogr"
	virtdiskv1alpha1 "github.com/gpu-ninja/virt-disk-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Allow recording of events.
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;update;patch

// Need to be able to read/write secrets (for encryption keys).
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

// Need to be able to get pods (primarily for determining the image to use).
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Need to be able to manage daemonsets.
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete

// +kubebuilder:rbac:groups=virt-disk.gpu-ninja.com,resources=virtualdisks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=virt-disk.gpu-ninja.com,resources=virtualdisks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=virt-disk.gpu-ninja.com,resources=virtualdisks/finalizers,verbs=update

const (
	// FinalizerName is the name of the finalizer used by controllers.
	FinalizerName = "virt-disk.gpu-ninja.com/finalizer"
	// reconcileRetryInterval is the interval at which the controller will retry
	// to reconcile a resource
	reconcileRetryInterval = 5 * time.Second
	// defaultOperatorImage is the default image to use for the virt disk daemonset.
	defaultOperatorImage = "ghcr.io/gpu-ninja/virt-disk-operator:latest"
)

// VirtualDiskReconciler reconciles a VirtualDisk object
type VirtualDiskReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
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

	if !controllerutil.ContainsFinalizer(&vdisk, FinalizerName) {
		logger.Info("Adding Finalizer")

		_, err := controllerutil.CreateOrPatch(ctx, r.Client, &vdisk, func() error {
			controllerutil.AddFinalizer(&vdisk, FinalizerName)

			return nil
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
	}

	if !vdisk.ObjectMeta.DeletionTimestamp.IsZero() {
		logger.Info("Deleting")

		if controllerutil.ContainsFinalizer(&vdisk, FinalizerName) {
			logger.Info("Removing Finalizer")

			_, err := controllerutil.CreateOrPatch(ctx, r.Client, &vdisk, func() error {
				controllerutil.RemoveFinalizer(&vdisk, FinalizerName)

				return nil
			})
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
			}
		}

		return ctrl.Result{}, nil
	}

	logger.Info("Creating or updating")

	image, err := r.getOperatorImage(ctx)
	if err != nil {
		logger.Error("Failed to get operator image", zap.Error(err))

		return ctrl.Result{}, fmt.Errorf("failed to get operator image: %w", err)
	}

	logger.Info("Reconciling daemonset")

	ds, err := r.daemonSetTemplate(ctx, logger, &vdisk, image)
	if err != nil {
		r.Recorder.Eventf(&vdisk, corev1.EventTypeWarning,
			"Failed", "Failed to generate daemonset template: %s", err)

		r.markFailed(ctx, &vdisk,
			fmt.Errorf("failed to generate daemonset template: %w", err))

		return ctrl.Result{}, fmt.Errorf("failed to generate daemonset template: %w", err)
	}

	if _, err := updater.CreateOrUpdateFromTemplate(ctx, r.Client, ds); err != nil {
		r.Recorder.Eventf(&vdisk, corev1.EventTypeWarning,
			"Failed", "Failed to reconcile daemonset: %s", err)

		r.markFailed(ctx, &vdisk,
			fmt.Errorf("failed to reconcile daemonset: %w", err))

		return ctrl.Result{}, fmt.Errorf("failed to reconcile daemonset: %w", err)
	}

	logger.Info("Daemonset successfully reconciled")

	ready, err := r.isDaemonSetReady(ctx, &vdisk)
	if err != nil {
		r.Recorder.Eventf(&vdisk, corev1.EventTypeWarning,
			"Failed", "Failed to check if daemonset is ready: %s", err)

		r.markFailed(ctx, &vdisk,
			fmt.Errorf("failed to check if daemonset is ready: %w", err))

		return ctrl.Result{}, fmt.Errorf("failed to check if daemonset is ready: %w", err)
	}

	if !ready {
		logger.Info("Waiting for daemonset to become ready")

		r.Recorder.Event(&vdisk, corev1.EventTypeNormal,
			"Pending", "Waiting for daemonset to become ready")

		if err := r.markPending(ctx, &vdisk); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: reconcileRetryInterval}, nil
	}

	if vdisk.Status.Phase != virtdiskv1alpha1.VirtualDiskPhaseReady {
		r.Recorder.Event(&vdisk, corev1.EventTypeNormal,
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
	key := client.ObjectKeyFromObject(vdisk)
	err := updater.UpdateStatus(ctx, r.Client, key, vdisk, func() error {
		vdisk.Status.ObservedGeneration = vdisk.ObjectMeta.Generation
		vdisk.Status.Phase = virtdiskv1alpha1.VirtualDiskPhasePending

		meta.SetStatusCondition(&vdisk.Status.Conditions, metav1.Condition{
			Type:               string(virtdiskv1alpha1.VirtualDiskConditionTypePending),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: vdisk.ObjectMeta.Generation,
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
	key := client.ObjectKeyFromObject(vdisk)
	err := updater.UpdateStatus(ctx, r.Client, key, vdisk, func() error {
		vdisk.Status.ObservedGeneration = vdisk.ObjectMeta.Generation
		vdisk.Status.Phase = virtdiskv1alpha1.VirtualDiskPhaseReady

		meta.SetStatusCondition(&vdisk.Status.Conditions, metav1.Condition{
			Type:               string(virtdiskv1alpha1.VirtualDiskConditionTypeReady),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: vdisk.ObjectMeta.Generation,
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

	key := client.ObjectKeyFromObject(vdisk)
	updateErr := updater.UpdateStatus(ctx, r.Client, key, vdisk, func() error {
		vdisk.Status.ObservedGeneration = vdisk.ObjectMeta.Generation
		vdisk.Status.Phase = virtdiskv1alpha1.VirtualDiskPhaseFailed

		meta.SetStatusCondition(&vdisk.Status.Conditions, metav1.Condition{
			Type:               string(virtdiskv1alpha1.VirtualDiskConditionTypeFailed),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: vdisk.ObjectMeta.Generation,
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

func (r *VirtualDiskReconciler) daemonSetTemplate(ctx context.Context, logger *zap.Logger, vdisk *virtdiskv1alpha1.VirtualDisk, image string) (*appsv1.DaemonSet, error) {
	volumes := []corev1.Volume{{
		Name: "dev",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: "/dev",
				Type: ptr.To(corev1.HostPathDirectory),
			},
		},
	}, {
		Name: "modules",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: "/lib/modules",
				Type: ptr.To(corev1.HostPathDirectory),
			},
		},
	}, {
		Name: "udev",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: "/run/udev",
				Type: ptr.To(corev1.HostPathDirectory),
			},
		},
	}, {
		Name: "data",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: vdisk.Spec.HostPath,
				Type: ptr.To(corev1.HostPathDirectoryOrCreate),
			},
		},
	}, {
		Name: "lvmrundir",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: "/run/lvm",
				Type: ptr.To(corev1.HostPathDirectoryOrCreate),
			},
		},
	}, {
		Name: "lvmlockdir",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: "/run/lock/lvm",
				Type: ptr.To(corev1.HostPathDirectoryOrCreate),
			},
		},
	}, {
		Name: "cryptsetuplockdir",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: "/run/cryptsetup",
				Type: ptr.To(corev1.HostPathDirectoryOrCreate),
			},
		},
	}}

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "data",
			MountPath: vdisk.Spec.HostPath,
		},
		{
			Name:      "dev",
			MountPath: "/dev",
		}, {
			Name:      "udev",
			MountPath: "/run/udev",
		}, {
			Name:      "lvmrundir",
			MountPath: "/run/lvm",
		}, {
			Name:      "lvmlockdir",
			MountPath: "/run/lock/lvm",
		}, {
			Name:      "cryptsetuplockdir",
			MountPath: "/run/cryptsetup",
		}, {
			Name:      "modules",
			MountPath: "/lib/modules",
			ReadOnly:  true,
		},
	}

	initContainers := []corev1.Container{{
		Name:    "load-nbd-module",
		Image:   image,
		Command: []string{"/bin/sh"},
		Args: []string{
			"-c",
			`
/sbin/modprobe nbd

while [ ! -b /dev/nbd0 ]; do
  echo 'Waiting for nbd device nodes'
  sleep 1
done
`,
		},
		SecurityContext: &corev1.SecurityContext{
			Privileged: ptr.To(true),
		},
		VolumeMounts: volumeMounts,
	}}

	args := []string{
		"attach",
		"--image=" + filepath.Join(vdisk.Spec.HostPath, fmt.Sprintf("%s-%s.qcow2", vdisk.Namespace, vdisk.Name)),
		"--size=" + units.BytesSize(float64(vdisk.Spec.Size.Value())),
		"--vg-name=" + vdisk.Spec.VolumeGroup,
	}

	if vdisk.Spec.LogicalVolume != "" {
		args = append(args, "--lv-name="+vdisk.Spec.LogicalVolume)
	}

	if vdisk.Spec.EncryptionKeySecretName != "" {
		logger.Info("Checking for existing encryption key")

		encryptionKeySecret := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vdisk.Spec.EncryptionKeySecretName,
				Namespace: vdisk.Namespace,
			},
		}

		if err := r.Get(ctx, client.ObjectKeyFromObject(&encryptionKeySecret), &encryptionKeySecret); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("failed to check for existing encryption key: %w", err)
			}

			logger.Info("Generating new encryption key")

			keyData := make([]byte, 64)
			n, err := rand.Read(keyData)
			if err != nil || n != 64 {
				return nil, fmt.Errorf("failed to generate encryption key: %w", err)
			}

			encryptionKeySecret.Data = map[string][]byte{
				"key": keyData,
			}

			if err := controllerutil.SetControllerReference(vdisk, &encryptionKeySecret, r.Scheme); err != nil {
				return nil, fmt.Errorf("failed to set controller reference: %w", err)
			}

			logger.Info("Creating encryption key secret")

			if err := r.Create(ctx, &encryptionKeySecret); err != nil {
				return nil, fmt.Errorf("failed to create encryption key secret: %w", err)
			}
		} else {
			logger.Info("Using existing encryption key")
		}

		args = append(args, "--key-file=/run/secrets/virt-disk-encryption-key/key")

		volumes = append(volumes, corev1.Volume{
			Name: "encryption-key",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: encryptionKeySecret.Name,
				},
			},
		})

		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "encryption-key",
			MountPath: "/run/secrets/virt-disk-encryption-key",
			ReadOnly:  true,
		})
	}

	virtDiskContainer := corev1.Container{
		Name:  "virt-disk",
		Image: image,
		Args:  args,
		SecurityContext: &corev1.SecurityContext{
			Privileged: ptr.To(true),
		},
		VolumeMounts: volumeMounts,
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
		Resources: vdisk.Spec.Resources,
	}

	ds := appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "virt-disk-" + vdisk.Name,
			Namespace: vdisk.Namespace,
			Labels:    make(map[string]string),
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":     "virt-disk",
					"app.kubernetes.io/instance": vdisk.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":     "virt-disk",
						"app.kubernetes.io/instance": vdisk.Name,
					},
				},
				Spec: corev1.PodSpec{
					// udev communication involves semaphores so need to share ipc namespace.
					HostIPC:                       true,
					TerminationGracePeriodSeconds: ptr.To(int64(10)),
					NodeSelector:                  vdisk.Spec.NodeSelector,
					InitContainers:                initContainers,
					Containers:                    []corev1.Container{virtDiskContainer},
					Volumes:                       volumes,
				},
			},
		},
	}

	if err := controllerutil.SetOwnerReference(vdisk, &ds, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set owner reference: %w", err)
	}

	for k, v := range vdisk.ObjectMeta.Labels {
		ds.ObjectMeta.Labels[k] = v
	}

	ds.ObjectMeta.Labels["app.kubernetes.io/name"] = "virt-disk"
	ds.ObjectMeta.Labels["app.kubernetes.io/managed-by"] = "virt-disk-operator"
	ds.ObjectMeta.Labels["app.kubernetes.io/instance"] = vdisk.Name

	return &ds, nil
}

func (r *VirtualDiskReconciler) isDaemonSetReady(ctx context.Context, vdisk *virtdiskv1alpha1.VirtualDisk) (bool, error) {
	ds := appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "virt-disk-" + vdisk.Name,
			Namespace: vdisk.Namespace,
		},
	}

	if err := r.Get(ctx, client.ObjectKeyFromObject(&ds), &ds); err != nil {
		return false, fmt.Errorf("failed to get daemonset: %w", err)
	}

	return ds.Status.DesiredNumberScheduled != 0 &&
		ds.Status.NumberReady == ds.Status.DesiredNumberScheduled, nil
}
