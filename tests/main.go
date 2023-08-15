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

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/fatih/color"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
)

var (
	red   = color.New(color.FgRed).SprintFunc()
	green = color.New(color.FgGreen).SprintFunc()
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	pwd, err := os.Getwd()
	if err != nil {
		logger.Fatal(red("Failed to get current working directory"), zap.Error(err))
	}

	logger.Info("Building operator image")

	buildContextPath := filepath.Clean(filepath.Join(pwd, ".."))

	imageName := "ghcr.io/gpu-ninja/virt-disk-operator:latest-dev"
	if err := buildOperatorImage(buildContextPath, "Dockerfile", imageName); err != nil {
		logger.Fatal(red("Failed to build operator image"), zap.Error(err))
	}

	logger.Info("Creating k3d cluster")

	clusterName := "virt-disk-operator-test"
	configPath := filepath.Join(pwd, "k3d-config.yaml")
	if err := createK3dCluster(clusterName, configPath); err != nil {
		logger.Fatal(red("Failed to create k3d cluster"), zap.Error(err))
	}
	defer func() {
		logger.Info("Deleting k3d cluster")

		if err := deleteK3dCluster(clusterName); err != nil {
			logger.Fatal(red("Failed to delete k3d cluster"), zap.Error(err))
		}
	}()

	logger.Info("Loading operator image into k3d cluster")

	if err := loadOperatorImage(clusterName, imageName); err != nil {
		logger.Fatal(red("Failed to load operator image"), zap.Error(err))
	}

	logger.Info("Installing operator")

	if err := installOperator(filepath.Clean(filepath.Join(pwd, "../config"))); err != nil {
		logger.Fatal(red("Failed to install operator"), zap.Error(err))
	}

	logger.Info("Creating example resources")

	if err := createExampleResources(filepath.Join(pwd, "../examples")); err != nil {
		logger.Fatal(red("Failed to create example resources"), zap.Error(err))
	}

	kubeconfig := filepath.Join(clientcmd.RecommendedConfigDir, "config")
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		logger.Fatal(red("Failed to build kubeconfig"), zap.Error(err))
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		logger.Fatal(red("Failed to create kubernetes client"), zap.Error(err))
	}

	diskGVR := schema.GroupVersionResource{
		Group:    "virt-disk.gpu-ninja.com",
		Version:  "v1alpha1",
		Resource: "virtualdisks",
	}

	logger.Info("Waiting for virtual disk to become ready")

	ctx := context.Background()
	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		ldapUser, err := dynClient.Resource(diskGVR).Namespace("default").Get(ctx, "demo", metav1.GetOptions{})
		if err != nil {
			return true, err
		}

		phase, ok, err := unstructured.NestedString(ldapUser.Object, "status", "phase")
		if err != nil {
			return false, err
		}

		if !ok || phase != "Ready" {
			logger.Info("Virtual disk not ready")

			return false, nil
		}

		return true, nil
	})
	if err != nil {
		logger.Fatal(red("Failed to wait for virtual disk to become ready"), zap.Error(err))
	}

	logger.Info("Checking that the virtual disk is available as a block device")

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		logger.Fatal(red("Failed to create kubernetes client"), zap.Error(err))
	}

	if err := checkForVirtualDisk(ctx, clientset); err != nil {
		logger.Fatal(red("Failed to check for virtual disk"), zap.Error(err))
	}

	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		job, err := clientset.BatchV1().Jobs("default").Get(ctx, "check-virtual-disk", metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		if job.Status.Succeeded > 0 {
			return true, nil
		}

		if job.Status.Failed > 0 {
			return false, fmt.Errorf("block device not found")
		}

		return false, nil
	})
	if err != nil {
		logger.Fatal(red("Checking for virtual disk block device failed"), zap.Error(err))
	}

	logger.Info(green("Virtual disk is successfully created and available as a block device"))
}

func buildOperatorImage(buildContextPath, relDockerfilePath, image string) error {
	cmd := exec.Command("docker", "build", "-t", image, "-f",
		filepath.Join(buildContextPath, relDockerfilePath), buildContextPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func createK3dCluster(clusterName, configPath string) error {
	cmd := exec.Command("k3d", "cluster", "create", "-c", configPath, "--wait")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func deleteK3dCluster(clusterName string) error {
	cmd := exec.Command("k3d", "cluster", "delete", clusterName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func loadOperatorImage(clusterName, imageName string) error {
	cmd := exec.Command("k3d", "image", "import", "-c", clusterName, imageName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func installOperator(configDir string) error {
	cmd := exec.Command("ytt", "-f", "config", "-f", configDir)
	patchedYAML, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}

	cmd = exec.Command("kapp", "deploy", "-y", "-a", "virt-disk-operator", "-f", "-")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = bytes.NewReader(patchedYAML)

	return cmd.Run()
}

func createExampleResources(examplesDir string) error {
	cmd := exec.Command("kapp", "deploy", "-y", "-a", "virt-disk-operator-examples", "-f", examplesDir)
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
							}},
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(true),
							},
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
