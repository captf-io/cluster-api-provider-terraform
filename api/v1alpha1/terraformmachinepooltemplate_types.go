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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TerraformMachinePoolTemplateSpec is the desired state of a
// TerraformMachinePoolTemplate.
type TerraformMachinePoolTemplateSpec struct {
	// template is the TerraformMachinePool created from this template.
	// +required
	Template TerraformMachinePoolTemplateResource `json:"template,omitempty,omitzero"`
}

// TerraformMachinePoolTemplateResource describes the TerraformMachinePool
// created from a template.
// +kubebuilder:validation:MinProperties=1
type TerraformMachinePoolTemplateResource struct {
	TemplateMeta `json:",inline"`

	// spec of the TerraformMachinePool created from this template.
	// +required
	Spec TerraformMachinePoolSpec `json:"spec,omitempty,omitzero"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=terraformmachinepooltemplates,scope=Namespaced,categories=cluster-api
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.template.spec.source.image",description="Module image"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"

// TerraformMachinePoolTemplate is the Schema for the
// terraformmachinepooltemplates API: a template for TerraformMachinePools,
// used by MachinePools. Unlike TerraformMachineTemplate it has no status:
// pools have no scale-from-zero, so there is no capacity or nodeInfo to
// resolve.
type TerraformMachinePoolTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state of the TerraformMachinePoolTemplate.
	// +required
	Spec TerraformMachinePoolTemplateSpec `json:"spec,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// TerraformMachinePoolTemplateList contains a list of
// TerraformMachinePoolTemplate.
type TerraformMachinePoolTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TerraformMachinePoolTemplate `json:"items"`
}
