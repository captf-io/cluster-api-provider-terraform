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

// TerraformClusterIdentitySpec is the desired state of a
// TerraformClusterIdentity: cloud credentials and who may use them.
// +kubebuilder:validation:MinProperties=1
type TerraformClusterIdentitySpec struct {
	// secretRef names the Secret holding the credentials. It is mirrored into
	// each allowed namespace that uses this identity and delivered to Jobs as
	// environment variables and files.
	// +required
	SecretRef SecretReference `json:"secretRef,omitempty,omitzero"`

	// allowedNamespaces restricts which namespaces may reference this
	// identity. Unset allows no namespace; `selector: {}` allows every
	// namespace; list and selector are ORed. An empty object is rejected.
	// +optional
	AllowedNamespaces *AllowedNamespaces `json:"allowedNamespaces,omitempty"`
}

// AllowedNamespaces selects namespaces allowed to use an identity. At least
// one of list and selector must be set.
// +kubebuilder:validation:XValidation:rule="has(self.list) || has(self.selector)",message="allowedNamespaces {} is ambiguous and rejected: set list, or write selector: {} to allow every namespace"
type AllowedNamespaces struct {
	// list of namespace names.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	List []string `json:"list,omitempty"`

	// selector matches namespace labels. An empty selector ({}) matches every
	// namespace.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// TerraformClusterIdentityStatus is the observed state of a
// TerraformClusterIdentity. The manager fills it: whether the credentials
// Secret exists, and where it is mirrored.
// +kubebuilder:validation:MinProperties=1
type TerraformClusterIdentityStatus struct {
	// conditions of the TerraformClusterIdentity. Ready is True when the
	// credentials Secret exists (SecretFound), False when it does not
	// (SecretNotFound).
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// namespaces where a mirror of the credentials Secret currently exists.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=1000
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	Namespaces []string `json:"namespaces,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=terraformclusteridentities,scope=Cluster,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Secret",type="string",JSONPath=".spec.secretRef.name",description="Credentials Secret"
// +kubebuilder:printcolumn:name="Namespace",type="string",JSONPath=".spec.secretRef.namespace",description="Credentials Secret namespace"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Ready condition"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"

// TerraformClusterIdentity is the Schema for the terraformclusteridentities
// API: cluster-scoped cloud credentials for TerraformClusters,
// TerraformMachines and TerraformMachinePools in allowed namespaces.
type TerraformClusterIdentity struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state of the TerraformClusterIdentity.
	// +required
	Spec TerraformClusterIdentitySpec `json:"spec,omitempty,omitzero"`

	// status is the observed state of the TerraformClusterIdentity.
	// +optional
	Status TerraformClusterIdentityStatus `json:"status,omitempty,omitzero"`
}

// GetConditions returns the conditions of the TerraformClusterIdentity.
func (i *TerraformClusterIdentity) GetConditions() []metav1.Condition {
	return i.Status.Conditions
}

// SetConditions sets the conditions of the TerraformClusterIdentity.
func (i *TerraformClusterIdentity) SetConditions(conditions []metav1.Condition) {
	i.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// TerraformClusterIdentityList contains a list of TerraformClusterIdentity.
type TerraformClusterIdentityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TerraformClusterIdentity `json:"items"`
}
