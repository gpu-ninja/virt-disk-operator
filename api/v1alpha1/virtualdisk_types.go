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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type VirtualDiskPhase string

const (
	VirtualDiskPhasePending VirtualDiskPhase = "Pending"
	VirtualDiskPhaseReady   VirtualDiskPhase = "Ready"
	VirtualDiskPhaseFailed  VirtualDiskPhase = "Failed"
)

type VirtualDiskConditionType string

const (
	VirtualDiskConditionTypePending VirtualDiskConditionType = "Pending"
	VirtualDiskConditionTypeReady   VirtualDiskConditionType = "Ready"
	VirtualDiskConditionTypeFailed  VirtualDiskConditionType = "Failed"
)

type VirtualDiskSpec struct {
	// HostPath is the optional path where the virtual disk device will be stored.
	// +kubebuilder:default:="/var/lib/virt-disk"
	HostPath string `json:"hostPath,omitempty"`
	// Size is the size of the virtual disk device.
	Size resource.Quantity `json:"size"`
	// VolumeGroup is the name of the LVM volume group to create.
	VolumeGroup string `json:"volumeGroup"`
	// LogicalVolume is the name of the optional LVM logical volume to create.
	// It will be allocated to use all available space in the volume group.
	LogicalVolume string `json:"logicalVolume,omitempty"`
	// Encryption is a flag indicating whether the virtual disk device should be encrypted using LUKS.
	Encryption bool `json:"encryption,omitempty"`
	// EncryptionKeySecretName is the name of the secret that will be created to store the LUKS encryption key.
	EncryptionKeySecretName string `json:"encryptionKeySecretName,omitempty"`
	// NodeSelector allows specifying which nodes a virtual disk device should be created on.
	// If not specified, the virtual disk device will be created on all nodes.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Resources allows specifying the resource requirements for the virtual disk container.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

type VirtualDiskStatus struct {
	// Phase is the current state of the virtual disk device.
	Phase VirtualDiskPhase `json:"phase,omitempty"`
	// ObservedGeneration is the most recent generation observed for this virtual disk device by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions represents the latest available observations of the virtual disk devices current state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VirtualDisk is the Schema for the VirtualDisks API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vdisk
type VirtualDisk struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VirtualDiskSpec   `json:"spec,omitempty"`
	Status VirtualDiskStatus `json:"status,omitempty"`
}

// VirtualDiskList contains a list of VirtualDisk.
// +kubebuilder:object:root=true
type VirtualDiskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualDisk `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VirtualDisk{}, &VirtualDiskList{})
}
