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

package main_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
)

func TestOperator(t *testing.T) {
	t.Log("Creating example resources")

	rootDir := os.Getenv("ROOT_DIR")

	err := createExampleResources(filepath.Join(rootDir, "examples"))
	require.NoError(t, err)

	t.Cleanup(func() {
		t.Log("Deleting example resources")

		_ = deleteExampleResources()
	})

	kubeconfig := filepath.Join(clientcmd.RecommendedConfigDir, "config")
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	require.NoError(t, err)

	clientset, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)

	dynClient, err := dynamic.NewForConfig(config)
	require.NoError(t, err)

	diskGVR := schema.GroupVersionResource{
		Group:    "virt-disk.gpu-ninja.com",
		Version:  "v1alpha1",
		Resource: "virtualdisks",
	}

	t.Log("Waiting for virtual disk to become ready")

	ctx := context.Background()
	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		vdisk, err := dynClient.Resource(diskGVR).Namespace("default").Get(ctx, "demo", metav1.GetOptions{})
		if err != nil {
			return true, err
		}

		phase, ok, err := unstructured.NestedString(vdisk.Object, "status", "phase")
		if err != nil {
			return false, err
		}

		if !ok || phase != "Ready" {
			t.Log("Virtual disk not ready")

			return false, nil
		}

		return true, nil
	})
	if err != nil {
		virtDiskPods, err := clientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=virt-disk,app.kubernetes.io/instance=demo",
		})
		require.NoError(t, err)
		require.Len(t, virtDiskPods.Items, 1)

		virtDiskPodLogs, err := clientset.CoreV1().Pods("default").GetLogs(virtDiskPods.Items[0].Name, &corev1.PodLogOptions{}).Stream(ctx)
		require.NoError(t, err)
		defer virtDiskPodLogs.Close()

		_, err = io.Copy(os.Stderr, virtDiskPodLogs)
		require.NoError(t, err)

		t.Fatal(fmt.Errorf("failed to wait for virtual disk to become ready: %w", err))
	}

	t.Log("Checking that the virtual disk is available as a block device")

	err = checkForVirtualDisk(ctx, clientset)
	require.NoError(t, err)

	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		job, err := clientset.BatchV1().Jobs("default").Get(ctx, "check-virtual-disk", metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		if job.Status.Failed > 0 {
			return false, fmt.Errorf("block device not found")
		}

		if job.Status.Succeeded > 0 {
			_ = clientset.BatchV1().Jobs("default").Delete(ctx, "check-virtual-disk", metav1.DeleteOptions{
				PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
			})

			return true, nil
		}

		return false, nil
	})
	require.NoError(t, err, "checking for virtual disk block device failed")

	t.Log("Deleting virtual disk")

	err = dynClient.Resource(diskGVR).Namespace("default").Delete(ctx, "demo", metav1.DeleteOptions{})
	require.NoError(t, err)

	t.Log("Waiting for virtual disk to be deleted")

	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := dynClient.Resource(diskGVR).Namespace("default").Get(ctx, "demo", metav1.GetOptions{})
		if err != nil && apierrors.IsNotFound(err) {
			return true, nil
		} else if err != nil {
			return false, err
		}

		return false, nil
	})
	require.NoError(t, err, "failed to wait for virtual disk to be deleted")

	t.Log("Recreating virtual disk")

	// Since the underlying qcow2 file is still present, we can recreate the virtual disk
	// with the same data
	err = createExampleResources(filepath.Join(rootDir, "examples"))
	require.NoError(t, err)

	t.Log("Waiting for virtual disk to become ready")

	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		vdisk, err := dynClient.Resource(diskGVR).Namespace("default").Get(ctx, "demo", metav1.GetOptions{})
		if err != nil {
			return true, err
		}

		phase, ok, err := unstructured.NestedString(vdisk.Object, "status", "phase")
		if err != nil {
			return false, err
		}

		if !ok || phase != "Ready" {
			t.Log("Virtual disk not ready")

			return false, nil
		}

		return true, nil
	})
	require.NoError(t, err, "failed to wait for virtual disk to become ready")

	t.Log("Checking that the virtual disk is available as a block device")

	err = checkForVirtualDisk(ctx, clientset)
	require.NoError(t, err)

	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		job, err := clientset.BatchV1().Jobs("default").Get(ctx, "check-virtual-disk", metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		if job.Status.Failed > 0 {
			return false, fmt.Errorf("block device not found")
		}

		if job.Status.Succeeded > 0 {
			_ = clientset.BatchV1().Jobs("default").Delete(ctx, "check-virtual-disk", metav1.DeleteOptions{
				PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
			})

			return true, nil
		}

		return false, nil
	})
	require.NoError(t, err, "checking for virtual disk block device failed")
}

func createExampleResources(examplesDir string) error {
	cmd := exec.Command("kapp", "deploy", "-y", "-a", "virt-disk-operator-examples", "-f", examplesDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func deleteExampleResources() error {
	cmd := exec.Command("kapp", "delete", "-y", "-a", "virt-disk-operator-examples")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func checkForVirtualDisk(ctx context.Context, clientset *kubernetes.Clientset) error {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "check-virtual-disk",
			Namespace: "default",
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "check",
							Image:   "busybox",
							Command: []string{"sh", "-c"},
							Args:    []string{"test -b /dev/mapper/demo--vg-demo--lv"},
							VolumeMounts: []corev1.VolumeMount{{
								Name:      "dev",
								MountPath: "/dev",
								ReadOnly:  true,
							}},
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(true),
							},
							TerminationMessagePath: "/tmp/termination-log",
						},
					},
					Volumes: []corev1.Volume{{
						Name: "dev",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: "/dev",
							},
						},
					}},
				},
			},
		},
	}

	_, err := clientset.BatchV1().Jobs("default").Create(ctx, job, metav1.CreateOptions{})
	return err
}
