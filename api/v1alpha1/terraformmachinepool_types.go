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
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// TerraformMachinePoolSpec is the desired state of a TerraformMachinePool:
// the machinepool-role module image and how to run it. Unlike a
// TerraformMachine, every field here is mutable: the pool is re-applied on
// a spec change, a replica change or the bootstrap Secret's rotation
// (https://captf.io/docs/module-author/contract/v1alpha1/machinepool.html "Lifecycle").
// +kubebuilder:validation:MinProperties=1
type TerraformMachinePoolSpec struct {
	// providerID is the scaling group's provider ID, set by the controller
	// from the module's provider_id output. Optional in the InfraMachinePool
	// contract; may stay unset for group-less implementations.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	ProviderID string `json:"providerID,omitempty"`

	// providerIDList are the provider IDs of every non-terminated member of
	// the group, set by the controller from the module's provider_id_list
	// output. Each entry must equal the corresponding Node's spec.providerID.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=10000
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=512
	ProviderIDList []string `json:"providerIDList,omitempty"`

	WorkspaceSpec `json:",inline"`

	// drift is merged field by field over the cluster's defaults.drift.
	// Unlike a machine's, a pool's drift may be remediated.
	// +optional
	Drift *MachinePoolDriftPolicy `json:"drift,omitempty"`

	// membershipRefreshIntervalSeconds is how often the controller runs
	// `apply -refresh-only` to pick up group membership changes (new or
	// departed instances) between applies, in seconds
	// (https://captf.io/docs/module-author/contract/v1alpha1/machinepool.html "Membership refresh"). 0 (unset)
	// means 60, applied at reconcile; the CRD schema's minimum of 15 makes 0
	// itself an invalid setting, so it unambiguously means unset, the same
	// convention as activeDeadlineSeconds and unhealthyThreshold
	// (kube-api-linter optionalfields: WhenRequired).
	// +optional
	// +kubebuilder:validation:Minimum=15
	// +kubebuilder:validation:Maximum=86400
	MembershipRefreshIntervalSeconds int32 `json:"membershipRefreshIntervalSeconds,omitempty"`
}

// HealthState mirrors the health.state enum of the module contract
// (internal/contract.HealthState, https://captf.io/docs/module-author/contract/v1alpha1/common.html), for
// MachinePoolInstance.State.
// +kubebuilder:validation:Enum=pending;running;degraded;stopped;terminated;unknown
type HealthState string

const (
	// HealthStatePending is the contract's "pending" health state.
	HealthStatePending HealthState = "pending"
	// HealthStateRunning is the contract's "running" health state.
	HealthStateRunning HealthState = "running"
	// HealthStateDegraded is the contract's "degraded" health state.
	HealthStateDegraded HealthState = "degraded"
	// HealthStateStopped is the contract's "stopped" health state.
	HealthStateStopped HealthState = "stopped"
	// HealthStateTerminated is the contract's "terminated" health state.
	HealthStateTerminated HealthState = "terminated"
	// HealthStateUnknown is the contract's "unknown" health state.
	HealthStateUnknown HealthState = "unknown"
)

// MachinePoolInstance is one entry of a TerraformMachinePool's
// status.instances, mapped from the module's instances output
// (https://captf.io/docs/module-author/contract/v1alpha1/machinepool.html "instances").
// +kubebuilder:validation:MinProperties=1
type MachinePoolInstance struct {
	// providerID of the instance.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	ProviderID string `json:"providerID,omitempty"`

	// instanceID is a provider-defined identifier, distinct from providerID
	// when the module has one to give.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	InstanceID string `json:"instanceID,omitempty"`

	// addresses of the instance.
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

	// state of the instance, the health.state enum.
	// +optional
	State HealthState `json:"state,omitempty"`
}

// TerraformMachinePoolStatus is the observed state of a
// TerraformMachinePool. Nothing here is load-bearing: every value is
// rebuilt after clusterctl move.
// +kubebuilder:validation:MinProperties=1
type TerraformMachinePoolStatus struct {
	// conditions of the TerraformMachinePool. Ready is mirrored by Cluster
	// API into the MachinePool's InfrastructureReady condition.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	WorkspaceStatus `json:",inline,omitzero"`

	// ready is the v1beta1 compatibility field Cluster API v1.14 still reads
	// to decide the pool is provisioned (external.IsReady,
	// capi/core/reconcilers/machinepool/machinepool_controller_phases.go).
	// It is latched together with initialization.provisioned: once true, it
	// stays true for the object's life.
	// +optional
	Ready *bool `json:"ready,omitempty"`

	// replicas is the group's desired capacity as observed at the last
	// refresh, from the module's replicas output. Outside a scaling
	// transition it equals len(providerIDList).
	// +optional
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`

	// instances are the group's members, from the module's instances
	// output. Provider-defined shape; not used by core Cluster API.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1000
	Instances []MachinePoolInstance `json:"instances,omitempty"`

	// pendingPlanRef names the live TerraformPlan of the pool: the plan of
	// a change of the cluster's exports that waits for an approval, or
	// whose approved apply has not finished yet. It is omitted when no plan
	// is live.
	// +optional
	PendingPlanRef PlanReference `json:"pendingPlanRef,omitempty,omitzero"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=terraformmachinepools,scope=Namespaced,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".metadata.labels['cluster\\.x-k8s\\.io/cluster-name']",description="Cluster"
// +kubebuilder:printcolumn:name="MachinePool",type="string",JSONPath=`.metadata.ownerReferences[?(@.kind=="MachinePool")].name`,description="MachinePool owning this TerraformMachinePool"
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".status.replicas",description="Desired replicas"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Ready condition"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"

// TerraformMachinePool is the Schema for the terraformmachinepools API: the
// InfraMachinePool of Cluster API, provisioned by a Terraform/OpenTofu
// module.
type TerraformMachinePool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state of the TerraformMachinePool.
	// +required
	Spec TerraformMachinePoolSpec `json:"spec,omitempty,omitzero"`

	// status is the observed state of the TerraformMachinePool.
	// +optional
	Status TerraformMachinePoolStatus `json:"status,omitempty,omitzero"`
}

// GetConditions returns the conditions of the TerraformMachinePool.
func (p *TerraformMachinePool) GetConditions() []metav1.Condition {
	return p.Status.Conditions
}

// SetConditions sets the conditions of the TerraformMachinePool.
func (p *TerraformMachinePool) SetConditions(conditions []metav1.Condition) {
	p.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// TerraformMachinePoolList contains a list of TerraformMachinePool.
type TerraformMachinePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TerraformMachinePool `json:"items"`
}
