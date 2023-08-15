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

package controller_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	fakeutils "github.com/gpu-ninja/operator-utils/fake"
	"github.com/gpu-ninja/operator-utils/zaplogr"
	virtdiskv1alpha1 "github.com/gpu-ninja/virt-disk-operator/api/v1alpha1"
	"github.com/gpu-ninja/virt-disk-operator/internal/constants"
	"github.com/gpu-ninja/virt-disk-operator/internal/controller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestVirtualDiskReconciler(t *testing.T) {
	ctrl.SetLogger(zaplogr.New(zaptest.NewLogger(t)))

	scheme := runtime.NewScheme()

	err := appsv1.AddToScheme(scheme)
	require.NoError(t, err)

	err = corev1.AddToScheme(scheme)
	require.NoError(t, err)

	err = virtdiskv1alpha1.AddToScheme(scheme)
	require.NoError(t, err)

	vdisk := virtdiskv1alpha1.VirtualDisk{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: "default",
		},
		Spec: virtdiskv1alpha1.VirtualDiskSpec{
			Size: resource.MustParse("100Gi"),
			LVM: &virtdiskv1alpha1.VirtualDiskLVMSpec{
				VolumeGroup:   "demo-vg",
				LogicalVolume: "demo-lv",
			},
		},
	}

	operatorPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "operator",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Image: "",
				},
			},
		},
	}

	os.Setenv("POD_NAME", "operator")
	os.Setenv("POD_NAMESPACE", "default")

	subResourceClient := fakeutils.NewSubResourceClient(scheme)

	interceptorFuncs := interceptor.Funcs{
		SubResource: func(client client.WithWatch, subResource string) client.SubResourceClient {
			return subResourceClient
		},
	}

	r := &controller.VirtualDiskReconciler{
		Scheme: scheme,
	}

	ctx := context.Background()

	t.Run("Create or Update", func(t *testing.T) {
		eventRecorder := record.NewFakeRecorder(2)
		r.EventRecorder = eventRecorder

		subResourceClient.Reset()

		r.Client = fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&vdisk, &operatorPod).
			WithStatusSubresource(&vdisk, &operatorPod).
			WithInterceptorFuncs(interceptorFuncs).
			Build()

		resp, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: client.ObjectKey{
				Name:      vdisk.Name,
				Namespace: vdisk.Namespace,
			},
		})
		require.NoError(t, err)
		assert.NotZero(t, resp)

		require.Len(t, eventRecorder.Events, 1)
		event := <-eventRecorder.Events
		assert.Equal(t, "Normal Pending Waiting for daemonset pods to be ready", event)

		updatedVDisk := vdisk.DeepCopy()
		err = subResourceClient.Get(ctx, &vdisk, updatedVDisk)
		require.NoError(t, err)

		assert.Equal(t, virtdiskv1alpha1.VirtualDiskPhasePending, updatedVDisk.Status.Phase)
		assert.Len(t, updatedVDisk.Status.Conditions, 1)

		ds := &appsv1.DaemonSet{}
		err = r.Client.Get(ctx, types.NamespacedName{
			Name:      vdisk.Name,
			Namespace: vdisk.Namespace,
		}, ds)
		require.NoError(t, err)

		ds.Status.DesiredNumberScheduled = 1
		ds.Status.NumberReady = 1

		subResourceClient.Reset()

		r.Client = fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(updatedVDisk, ds, &operatorPod).
			WithStatusSubresource(updatedVDisk, ds, &operatorPod).
			WithInterceptorFuncs(interceptorFuncs).
			Build()

		resp, err = r.Reconcile(ctx, ctrl.Request{
			NamespacedName: client.ObjectKey{
				Name:      vdisk.Name,
				Namespace: vdisk.Namespace,
			},
		})
		require.NoError(t, err)
		assert.Zero(t, resp)

		require.Len(t, eventRecorder.Events, 1)
		event = <-eventRecorder.Events
		assert.Equal(t, "Normal Created Successfully created", event)

		err = subResourceClient.Get(ctx, &vdisk, updatedVDisk)
		require.NoError(t, err)

		assert.Equal(t, virtdiskv1alpha1.VirtualDiskPhaseReady, updatedVDisk.Status.Phase)
		assert.Len(t, updatedVDisk.Status.Conditions, 2)
	})

	t.Run("Delete", func(t *testing.T) {
		deletingVDisk := vdisk.DeepCopy()
		deletingVDisk.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Add(-1 * time.Second)}
		deletingVDisk.Finalizers = []string{constants.FinalizerName}

		eventRecorder := record.NewFakeRecorder(2)
		r.EventRecorder = eventRecorder

		r.Client = fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(deletingVDisk).
			WithStatusSubresource(deletingVDisk).
			Build()

		resp, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      vdisk.Name,
				Namespace: vdisk.Namespace,
			},
		})
		require.NoError(t, err)
		assert.Zero(t, resp)

		assert.Len(t, eventRecorder.Events, 0)
	})

	t.Run("Failure", func(t *testing.T) {
		eventRecorder := record.NewFakeRecorder(2)
		r.EventRecorder = eventRecorder

		failOnSecrets := interceptorFuncs
		failOnSecrets.Get = func(ctx context.Context, client client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*appsv1.DaemonSet); ok {
				return fmt.Errorf("bang")
			}

			return client.Get(ctx, key, obj, opts...)
		}

		subResourceClient.Reset()

		r.Client = fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&vdisk, &operatorPod).
			WithStatusSubresource(&vdisk, &operatorPod).
			WithInterceptorFuncs(failOnSecrets).
			Build()

		resp, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      vdisk.Name,
				Namespace: vdisk.Namespace,
			},
		})
		require.NoError(t, err)
		assert.Zero(t, resp)

		require.Len(t, eventRecorder.Events, 1)
		event := <-eventRecorder.Events
		assert.Equal(t, "Warning Failed Failed to reconcile daemonset: bang", event)

		updatedVDisk := vdisk.DeepCopy()
		err = subResourceClient.Get(ctx, &vdisk, updatedVDisk)
		require.NoError(t, err)

		assert.Equal(t, virtdiskv1alpha1.VirtualDiskPhaseFailed, updatedVDisk.Status.Phase)
	})
}
