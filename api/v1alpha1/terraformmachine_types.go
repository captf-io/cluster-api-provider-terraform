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
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// TerraformMachineSpec is the desired state of a TerraformMachine: the
// machine-role module image and how to run it. source, identityRef,
// variables and variablesFrom define the machine and are immutable after
// creation, and providerID can only be
// set once, by the controller; the admission webhook enforces this. jobs,
// drift and remediation are operational policy and may change at any time.
// +kubebuilder:validation:MinProperties=1
type TerraformMachineSpec struct {
	// providerID is the instance's provider ID, set by the controller from the
	// module's provider_id output. It must equal the Node's spec.providerID.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	ProviderID string `json:"providerID,omitempty"`

	WorkspaceSpec `json:",inline"`

	// drift is merged field by field over the cluster's defaults.drift. Drift
	// on a machine is always reported, never remediated.
	// +optional
	Drift *MachineDriftPolicy `json:"drift,omitempty"`

	// remediation configures how an unhealthy instance is signalled to
	// Cluster API beyond the Ready condition.
	// +optional
	Remediation *MachineRemediation `json:"remediation,omitempty"`
}

// TerraformMachineStatus is the observed state of a TerraformMachine. Nothing
// here is load-bearing: every value is rebuilt after clusterctl move.
// +kubebuilder:validation:MinProperties=1
type TerraformMachineStatus struct {
	// conditions of the TerraformMachine. Ready is mirrored by Cluster API into
	// the Machine's InfrastructureReady condition.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	WorkspaceStatus `json:",inline,omitzero"`

	// addresses of the instance, from the module's addresses output, in the
	// controller's canonical order.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	Addresses []clusterv1.MachineAddress `json:"addresses,omitempty"`

	// failureDomain the instance actually runs in.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	FailureDomain string `json:"failureDomain,omitempty"`

	// interruptible is true for spot/preemptible instances. Cluster API then
	// labels the Node cluster.x-k8s.io/interruptible.
	// +optional
	Interruptible *bool `json:"interruptible,omitempty"`

	// unhealthySamples counts consecutive unhealthy health samples (one per
	// completed refresh or drift Job after provisioning, or per apply whose
	// own outputs stood in for the post-apply refresh); unset when the
	// instance is healthy. It lives in status only, so it restarts at 0
	// after clusterctl move.
	// +optional
	// +kubebuilder:validation:Minimum=1
	UnhealthySamples int32 `json:"unhealthySamples,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=terraformmachines,scope=Namespaced,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".metadata.labels['cluster\\.x-k8s\\.io/cluster-name']",description="Cluster"
// +kubebuilder:printcolumn:name="Machine",type="string",JSONPath=`.metadata.ownerReferences[?(@.kind=="Machine")].name`,description="Machine owning this TerraformMachine"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Ready condition"
// +kubebuilder:printcolumn:name="Provisioned",type="boolean",JSONPath=".status.initialization.provisioned",description="Infrastructure provisioned"
// +kubebuilder:printcolumn:name="ProviderID",type="string",JSONPath=".spec.providerID",description="Provider ID"
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.source.image",description="Module image",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"

// TerraformMachine is the Schema for the terraformmachines API: the
// InfraMachine of Cluster API, provisioned by a Terraform/OpenTofu module.
type TerraformMachine struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state of the TerraformMachine.
	// +required
	Spec TerraformMachineSpec `json:"spec,omitempty,omitzero"`

	// status is the observed state of the TerraformMachine.
	// +optional
	Status TerraformMachineStatus `json:"status,omitempty,omitzero"`
}

// GetConditions returns the conditions of the TerraformMachine.
func (m *TerraformMachine) GetConditions() []metav1.Condition {
	return m.Status.Conditions
}

// SetConditions sets the conditions of the TerraformMachine.
func (m *TerraformMachine) SetConditions(conditions []metav1.Condition) {
	m.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// TerraformMachineList contains a list of TerraformMachine.
type TerraformMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TerraformMachine `json:"items"`
}
