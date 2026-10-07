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

// Labels the controller puts on every TerraformPlan, so a policy engine,
// a dashboard or `kubectl get -l` can select plans without reading specs.
// Together with cluster.x-k8s.io/cluster-name (the Cluster of the target)
// they are part of the public integration API of TerraformPlan: their keys
// and values are frozen for v1alpha1.
const (
	// PlanDestructiveLabel is "true" when the plan replaces or deletes a
	// resource (summary.replace + summary.delete > 0), "false" otherwise.
	PlanDestructiveLabel = "captf.io/destructive"

	// PlanPhaseLabel mirrors status.phase. It also survives clusterctl move,
	// which drops status: only the manager may write it, and a plan in a
	// terminal phase (Applied, Superseded, Failed) can no longer be approved.
	PlanPhaseLabel = "captf.io/plan-phase"

	// PlanReasonLabel mirrors spec.reason.
	PlanReasonLabel = "captf.io/plan-reason"
)

// PlanApprovedCondition is the condition type that reports whether the plan
// was approved: True once spec.approved is set. Ready is the other type a
// TerraformPlan carries.
const PlanApprovedCondition = "Approved"

// MaxPlanSummaryResources caps spec.summary.resources.
const MaxPlanSummaryResources = 50

// PlanTargetKind is the kind of object a TerraformPlan is about.
// +kubebuilder:validation:Enum=TerraformCluster;TerraformMachinePool
type PlanTargetKind string

const (
	// PlanTargetCluster is a TerraformCluster.
	PlanTargetCluster PlanTargetKind = "TerraformCluster"
	// PlanTargetMachinePool is a TerraformMachinePool.
	PlanTargetMachinePool PlanTargetKind = "TerraformMachinePool"
)

// PlanReason is why a plan waits for an approval.
// +kubebuilder:validation:Enum=Manual;Destructive;ExportsChange
type PlanReason string

const (
	// PlanReasonManual is a plan of a TerraformCluster with applyPolicy
	// Manual: every change waits for an approval.
	PlanReasonManual PlanReason = "Manual"
	// PlanReasonDestructive is a plan of an applyPolicy Automatic object
	// that deletes or replaces resources.
	PlanReasonDestructive PlanReason = "Destructive"
	// PlanReasonExportsChange is a plan of a TerraformMachinePool that
	// applies a change of the cluster's exports.
	PlanReasonExportsChange PlanReason = "ExportsChange"
)

// PlanPhase is where a TerraformPlan is in its life. Pending and Approved
// are live: at most one live plan exists for a target. Applied, Superseded
// and Failed are terminal.
// +kubebuilder:validation:Enum=Pending;Approved;Applied;Superseded;Failed
type PlanPhase string

const (
	// PlanPhasePending waits for an approval.
	PlanPhasePending PlanPhase = "Pending"
	// PlanPhaseApproved was approved; its apply has not finished.
	PlanPhaseApproved PlanPhase = "Approved"
	// PlanPhaseApplied was approved and its apply succeeded.
	PlanPhaseApplied PlanPhase = "Applied"
	// PlanPhaseSuperseded was replaced by a newer plan, or became moot, before
	// it was applied.
	PlanPhaseSuperseded PlanPhase = "Superseded"
	// PlanPhaseFailed was approved but its apply failed or planned other
	// changes.
	PlanPhaseFailed PlanPhase = "Failed"
)

// Terminal reports whether p is a terminal phase: the plan is finished and
// can no longer be approved.
func (p PlanPhase) Terminal() bool {
	return p == PlanPhaseApplied || p == PlanPhaseSuperseded || p == PlanPhaseFailed
}

// PlanTargetRef names the object, in the plan's namespace, the plan is for.
type PlanTargetRef struct {
	// kind of the target.
	// +required
	Kind PlanTargetKind `json:"kind,omitempty"`

	// name of the target.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name,omitempty"`
}

// PlanSummary summarizes a plan for review: counts and the address and
// action of each changed resource, never a value.
// +kubebuilder:validation:MinProperties=1
type PlanSummary struct {
	// create is the number of resources the plan creates.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Create *int32 `json:"create,omitempty"`

	// update is the number of resources the plan updates in place.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Update *int32 `json:"update,omitempty"`

	// replace is the number of resources the plan replaces: deletes and
	// creates again. A replacement counts here only.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Replace *int32 `json:"replace,omitempty"`

	// delete is the number of resources the plan deletes, not counting
	// replacements.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Delete *int32 `json:"delete,omitempty"`

	// import is the number of resources the plan imports into the state.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Import *int32 `json:"import,omitempty"`

	// move is the number of resources a moved block moves to a new address.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Move *int32 `json:"move,omitempty"`

	// forget is the number of resources the plan removes from the state
	// without destroying them.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Forget *int32 `json:"forget,omitempty"`

	// outputChanges is the number of root module outputs the plan changes.
	// An output change alone still needs approval: cluster exports feed
	// every machine and pool module.
	// +optional
	// +kubebuilder:validation:Minimum=0
	OutputChanges *int32 `json:"outputChanges,omitempty"`

	// resources are "<address> (<labels>)" of the changed resources, sorted
	// by address, at most 50. The labels are the action (create, update,
	// delete, replace, read or forget) unless the resource is otherwise
	// unchanged, then "import" when the plan imports it and "move" when a
	// moved block moves it, comma-separated: "aws_instance.a (import)",
	// "aws_instance.b (update, move)".
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=600
	Resources []string `json:"resources,omitempty"`

	// truncated is true when resources lists fewer resources than the plan
	// changes.
	// +optional
	Truncated *bool `json:"truncated,omitempty"`
}

// TerraformPlanSpec is a plan waiting for, or past, an approval. The
// controller writes every field but approved and approvedBy when it creates
// the object, and they never change afterwards; the approver writes approved
// and approvedBy.
type TerraformPlanSpec struct {
	// targetRef is the object the plan is for. The TerraformPlan is owned by
	// it and moves with it.
	// +required
	TargetRef PlanTargetRef `json:"targetRef,omitempty,omitzero"`

	// planHash fingerprints the plan's changes: an approval covers exactly
	// the plan with this hash, and the apply runs only if it plans the same
	// changes again.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	PlanHash string `json:"planHash,omitempty"`

	// inputsHash is the hash of the inputs the plan was made for.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	InputsHash string `json:"inputsHash,omitempty"`

	// reason the plan waits for an approval.
	// +required
	Reason PlanReason `json:"reason,omitempty"`

	// summary of the plan.
	// +required
	Summary PlanSummary `json:"summary,omitempty,omitzero"`

	// approved approves the plan, once and for good: it can only change from
	// false to true, while the plan is live. Whoever may create or update a
	// TerraformPlan may approve plans.
	// +optional
	Approved *bool `json:"approved,omitempty"`

	// approvedBy is the user that approved the plan. It must equal the
	// username of the request that sets approved to true: the admission
	// webhook checks it, so the field names who approved, not who claims to
	// have.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	ApprovedBy string `json:"approvedBy,omitempty"`
}

// TerraformPlanStatus is the observed state of a TerraformPlan. The manager
// fills it.
// +kubebuilder:validation:MinProperties=1
type TerraformPlanStatus struct {
	// conditions of the TerraformPlan: Ready is True while the plan is
	// applied or waiting as it should, and False when it failed or was
	// superseded; Approved is True once the plan is approved.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// phase of the plan. It is mirrored to the captf.io/plan-phase label,
	// which survives clusterctl move.
	// +optional
	Phase PlanPhase `json:"phase,omitempty"`

	// observedGeneration is the generation this status was computed for.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=terraformplans,scope=Namespaced,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Target",type="string",JSONPath=".spec.targetRef.name",description="Target object"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".spec.reason",description="Why the plan waits"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="Plan phase"
// +kubebuilder:printcolumn:name="Create",type="integer",JSONPath=".spec.summary.create",description="Resources to create"
// +kubebuilder:printcolumn:name="Update",type="integer",JSONPath=".spec.summary.update",description="Resources to update"
// +kubebuilder:printcolumn:name="Replace",type="integer",JSONPath=".spec.summary.replace",description="Resources to replace"
// +kubebuilder:printcolumn:name="Delete",type="integer",JSONPath=".spec.summary.delete",description="Resources to delete"
// +kubebuilder:printcolumn:name="Approved",type="boolean",JSONPath=".spec.approved",description="Approved"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"

// TerraformPlan is the Schema for the terraformplans API: a plan the
// controller made for a TerraformCluster or TerraformMachinePool and that
// waits for an approval before it is applied. The controller creates it;
// approving it is setting spec.approved. Its labels, conditions and field
// names are a public integration API.
type TerraformPlan struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the plan and its approval.
	// +required
	Spec TerraformPlanSpec `json:"spec,omitempty,omitzero"`

	// status is the observed state of the TerraformPlan.
	// +optional
	Status TerraformPlanStatus `json:"status,omitempty,omitzero"`
}

// GetConditions returns the conditions of the TerraformPlan.
func (p *TerraformPlan) GetConditions() []metav1.Condition {
	return p.Status.Conditions
}

// SetConditions sets the conditions of the TerraformPlan.
func (p *TerraformPlan) SetConditions(conditions []metav1.Condition) {
	p.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// TerraformPlanList contains a list of TerraformPlan.
type TerraformPlanList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TerraformPlan `json:"items"`
}
