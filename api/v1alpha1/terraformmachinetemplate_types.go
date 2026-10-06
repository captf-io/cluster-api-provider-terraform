/*
Copyright 2026 The CAPTF Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TerraformMachineTemplateSpec is the desired state of a
// TerraformMachineTemplate.
type TerraformMachineTemplateSpec struct {
	// template is the TerraformMachine created from this template.
	// +required
	Template TerraformMachineTemplateResource `json:"template,omitempty,omitzero"`

	// capacity of the nodes the template creates, as resource quantities
	// (e.g. cpu: "4", memory: 16Gi), reported for Cluster Autoscaler scale
	// from zero. It overrides the image label io.captf.capacity entirely: no
	// per-resource merge. Unlike spec.template, it can change after
	// creation, and it is never copied to the TerraformMachines. The
	// capacity.cluster-autoscaler.kubernetes.io/* annotations on a
	// MachineDeployment or MachineSet still take precedence over it.
	// CAPTF never writes a node count from it.
	// +optional
	Capacity corev1.ResourceList `json:"capacity,omitempty"`
}

// TerraformMachineTemplateResource describes the TerraformMachine created from
// a template.
// +kubebuilder:validation:MinProperties=1
type TerraformMachineTemplateResource struct {
	TemplateMeta `json:",inline"`

	// spec of the TerraformMachine created from this template.
	// +required
	Spec TerraformMachineSpec `json:"spec,omitempty,omitzero"`
}

// Architecture is a node CPU architecture, as reported for scale from zero.
// +kubebuilder:validation:Enum=amd64;arm64;s390x;ppc64le
type Architecture string

const (
	// ArchitectureAmd64 is amd64.
	ArchitectureAmd64 Architecture = "amd64"
	// ArchitectureArm64 is arm64.
	ArchitectureArm64 Architecture = "arm64"
	// ArchitectureS390x is s390x.
	ArchitectureS390x Architecture = "s390x"
	// ArchitecturePpc64le is ppc64le.
	ArchitecturePpc64le Architecture = "ppc64le"
)

// NodeInfo describes the nodes the template creates, for Cluster Autoscaler
// scale from zero. It comes from the image label io.captf.node-info.
// +kubebuilder:validation:MinProperties=1
type NodeInfo struct {
	// architecture of the node's CPU.
	// +optional
	Architecture Architecture `json:"architecture,omitempty"`

	// operatingSystem of the node, e.g. linux.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	OperatingSystem string `json:"operatingSystem,omitempty"`
}

// CapacitySourceKind says where a template's status.capacity came from.
// +kubebuilder:validation:Enum=Spec;Image
type CapacitySourceKind string

const (
	// CapacitySourceSpec means status.capacity is spec.capacity.
	CapacitySourceSpec CapacitySourceKind = "Spec"
	// CapacitySourceImage means status.capacity is the image label
	// io.captf.capacity (or unset, when the image declares none).
	CapacitySourceImage CapacitySourceKind = "Image"
)

// CapacitySource records where the capacity was resolved from.
// +kubebuilder:validation:MinProperties=1
// +kubebuilder:validation:XValidation:rule="!has(self.source) || self.source != 'Image' || has(self.image)",message="image is required when source is Image"
type CapacitySource struct {
	// source is Spec when status.capacity is spec.capacity, and Image when
	// it comes from the image label io.captf.capacity.
	// +optional
	Source CapacitySourceKind `json:"source,omitempty"`

	// image is the spec image reference last resolved. The image always
	// supplies nodeInfo, even when spec.capacity supplies the capacity. It
	// is unset when source is Spec and the image could not be inspected.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image,omitempty"`
}

// TerraformMachineTemplateStatus is the observed state of a
// TerraformMachineTemplate: the node size declared by its image, for Cluster
// Autoscaler scale from zero.
// +kubebuilder:validation:MinProperties=1
type TerraformMachineTemplateStatus struct {
	// conditions of the TerraformMachineTemplate (CapacityResolved,
	// VariablesValid).
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// capacity of the nodes the template creates: spec.capacity when set,
	// else the image label io.captf.capacity (capacitySource.source says
	// which). Unset when neither declares any.
	// +optional
	Capacity corev1.ResourceList `json:"capacity,omitempty"`

	// nodeInfo of the nodes the template creates, from the image label
	// io.captf.node-info.
	// +optional
	NodeInfo NodeInfo `json:"nodeInfo,omitempty,omitzero"`

	// capacitySource is the spec image reference capacity and nodeInfo were
	// resolved from; they are re-resolved when the spec image changes.
	// +optional
	CapacitySource CapacitySource `json:"capacitySource,omitempty,omitzero"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=terraformmachinetemplates,scope=Namespaced,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.template.spec.source.image",description="Module image"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"

// TerraformMachineTemplate is the Schema for the terraformmachinetemplates API:
// a template for TerraformMachines, used by MachineDeployments, MachineSets,
// KubeadmControlPlane and ClusterClass.
type TerraformMachineTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state of the TerraformMachineTemplate.
	// +required
	Spec TerraformMachineTemplateSpec `json:"spec,omitempty,omitzero"`

	// status is the observed state of the TerraformMachineTemplate.
	// +optional
	Status TerraformMachineTemplateStatus `json:"status,omitempty,omitzero"`
}

// GetConditions returns the conditions of the TerraformMachineTemplate.
func (t *TerraformMachineTemplate) GetConditions() []metav1.Condition {
	return t.Status.Conditions
}

// SetConditions sets the conditions of the TerraformMachineTemplate.
func (t *TerraformMachineTemplate) SetConditions(conditions []metav1.Condition) {
	t.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// TerraformMachineTemplateList contains a list of TerraformMachineTemplate.
type TerraformMachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TerraformMachineTemplate `json:"items"`
}
