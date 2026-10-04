/*
Copyright 2026 The cluster-api-provider-terraform Authors.

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

// TerraformClusterTemplateSpec is the desired state of a
// TerraformClusterTemplate.
type TerraformClusterTemplateSpec struct {
	// template is the TerraformCluster created from this template.
	// +required
	Template TerraformClusterTemplateResource `json:"template,omitempty,omitzero"`
}

// TerraformClusterTemplateResource describes the TerraformCluster created from
// a template.
// +kubebuilder:validation:MinProperties=1
type TerraformClusterTemplateResource struct {
	TemplateMeta `json:",inline"`

	// spec of the TerraformCluster created from this template.
	// +required
	Spec TerraformClusterSpec `json:"spec,omitempty,omitzero"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=terraformclustertemplates,scope=Namespaced,categories=cluster-api
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.template.spec.source.image",description="Module image"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"

// TerraformClusterTemplate is the Schema for the terraformclustertemplates
// API: a template for TerraformClusters, used by ClusterClass.
type TerraformClusterTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state of the TerraformClusterTemplate.
	// +required
	Spec TerraformClusterTemplateSpec `json:"spec,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// TerraformClusterTemplateList contains a list of TerraformClusterTemplate.
type TerraformClusterTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TerraformClusterTemplate `json:"items"`
}
