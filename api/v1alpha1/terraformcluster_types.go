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
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// ApproveDestructivePlanAnnotation approves one destructive apply of a
// TerraformCluster, or of a TerraformMachinePool's change of the cluster's
// exports. On a TerraformCluster its value is an inputs hash: an apply (or
// drift remediation) whose plan deletes or replaces resources runs only
// when this annotation names the hash of the inputs it renders. On a
// TerraformMachinePool only an apply that renders a change of the
// cluster's exports (captf_cluster_outputs) is guarded, and its value is
// the approval hash: the hash of those inputs without bootstrap_data, so
// it survives the bootstrap provider's rotations. Until it is approved the
// pool keeps applying with the exports of its last successful apply,
// unless an earlier guarded apply failed part way, or the pool applied
// before this version recorded those exports and they are unknown (every
// apply of the pool is then guarded, and a destructive one waits for
// approval, as a cluster's does, until an apply succeeds). TerraformMachines are never guarded. Any other change
// is guarded again, whatever the annotation says, and the controller
// removes the annotation once an apply of the approved hash succeeded, or,
// on a pool, once the exports it approved a change of return to the
// applied ones. Approving needs only patch
// on the object. ApplyJobSucceeded (DestructivePlanBlocked) gives the exact
// command.
const ApproveDestructivePlanAnnotation = "captf.io/approve-destructive-plan"

// ApprovePlanAnnotation approves one plan of a TerraformCluster with
// applyPolicy Manual. Its value is a plan hash (status.plan.planHash): the
// apply waiting for that plan runs, and applies only if it plans exactly
// the same changes again. Approving a plan also approves the deletes and
// replacements it lists; ApproveDestructivePlanAnnotation is not needed on
// top. The controller removes it once the apply succeeded.
// ApplyJobSucceeded (PlanAwaitingApproval) gives the exact command.
const ApprovePlanAnnotation = "captf.io/approve-plan"

// ApplyPolicy decides whether a TerraformCluster applies a change on its
// own or waits until its plan is approved.
// +kubebuilder:validation:Enum=Automatic;Manual
type ApplyPolicy string

const (
	// ApplyPolicyAutomatic applies every change as soon as it is seen, only
	// guarded against destructive plans.
	ApplyPolicyAutomatic ApplyPolicy = "Automatic"
	// ApplyPolicyManual plans every change first (a plan Job), shows the
	// plan in status.plan and applies it only once ApprovePlanAnnotation
	// names its hash. The first apply of a new cluster is not gated.
	ApplyPolicyManual ApplyPolicy = "Manual"
)

// RestoreStateAnnotation requests a state restore on a TerraformCluster,
// TerraformMachine or TerraformMachinePool. Its value is the serial of a
// state backup (status.stateBackups): a restore Job pushes that backup
// into the backend with `state push -force`, and the controller removes
// the annotation once it succeeded. A failed restore is not retried for
// the same serial.
// Deletion wins over a restore, except while the deletion is held because
// the state is missing or unreadable: then the restore runs first and the
// destroy follows.
const RestoreStateAnnotation = "captf.io/restore-state"

// AbandonInfrastructureAnnotation releases a deleting TerraformCluster,
// TerraformMachine or TerraformMachinePool whose deletion is held because
// its state is missing or unreadable (StateReadable False), whose last
// destroy Job failed, or whose destroy cannot start: the durable inputs
// it renders from are gone (ApplyJobSucceeded False/DestroyFailed), the
// identity does not allow the namespace or no longer exists
// (IdentityNotAllowed), or the runner credentials cannot be prepared
// (the Deleting condition says the destroy waits for them). Its value
// must be the object's metadata.uid; any other value is ignored. The
// controller then removes the finalizer without a destroy Job, records a
// Warning event, and leaves whatever the module created running and
// untracked. An object whose state reads and whose destroy can start is
// destroyed as usual.
const AbandonInfrastructureAnnotation = "captf.io/abandon-infrastructure"

// TerraformClusterSpec is the desired state of a TerraformCluster: the
// cluster-role module image and how to run it.
// +kubebuilder:validation:MinProperties=1
type TerraformClusterSpec struct {
	// controlPlaneEndpoint is the endpoint of the cluster's API server. A value
	// set by the user is passed to the module as its control_plane_endpoint
	// input; otherwise the controller writes the module's output here once.
	// host and port are set together. Once both are set it is immutable
	// (the webhook enforces this).
	// +optional
	// +kubebuilder:validation:XValidation:rule="has(self.host) == has(self.port)",message="host and port must be set together"
	ControlPlaneEndpoint *clusterv1.APIEndpoint `json:"controlPlaneEndpoint,omitempty"`

	WorkspaceSpec `json:",inline"`

	// drift configures drift detection for this cluster.
	// +optional
	Drift *DriftPolicy `json:"drift,omitempty"`

	// applyPolicy decides when a change is applied. Automatic (the default,
	// applied at reconcile) applies every change of the inputs, and a drift
	// remediation, as soon as it is seen; only a plan that deletes or
	// replaces resources waits for captf.io/approve-destructive-plan.
	// Manual runs a plan Job first, reports the plan in status.plan and
	// waits until the captf.io/approve-plan annotation names its hash; the
	// apply then runs only if it plans the same changes again. The first
	// apply of a new cluster (no state yet) is never gated. Mutable.
	// +optional
	ApplyPolicy ApplyPolicy `json:"applyPolicy,omitempty"`

	// defaults are inherited by the TerraformMachines and TerraformMachinePools
	// of this cluster, field by field: a field a machine or pool sets wins,
	// an unset one comes from here. They do not apply to the
	// TerraformCluster itself.
	// +optional
	Defaults *TerraformClusterDefaults `json:"defaults,omitempty"`
}

// TerraformClusterDefaults are values the TerraformMachines and
// TerraformMachinePools of a cluster inherit when they do not set them.
// There is no source: every role names its own image.
type TerraformClusterDefaults struct {
	// identityRef is used by machines and pools without their own
	// identityRef. When unset, such machines and pools use
	// spec.identityRef.
	// +optional
	IdentityRef IdentityReference `json:"identityRef,omitempty,omitzero"`

	// jobs is merged field by field under each machine's or pool's jobs
	// policy (see JobPolicy).
	// +optional
	Jobs *JobPolicy `json:"jobs,omitempty"`

	// drift is merged field by field under each machine's or pool's drift
	// policy. A pool's drift is never fully disabled: an inherited
	// intervalSeconds of 0 disables a machine's drift checks but not a
	// pool's, which then uses the controller's default interval.
	// +optional
	Drift *MachineDriftPolicy `json:"drift,omitempty"`
}

// TerraformClusterStatus is the observed state of a TerraformCluster. Nothing
// here is load-bearing: every value is rebuilt from spec, the state Secret, the
// durable inputs Secret or the Job list, because clusterctl move does not
// restore status.
// +kubebuilder:validation:MinProperties=1
type TerraformClusterStatus struct {
	// conditions of the TerraformCluster. Ready is mirrored by Cluster API into
	// the Cluster's InfrastructureReady condition.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	WorkspaceStatus `json:",inline,omitzero"`

	// failureDomains reported by the module's failure_domains output.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	FailureDomains []clusterv1.FailureDomain `json:"failureDomains,omitempty"`

	// exports is a copy of the cluster module's exports output, published
	// for consumers outside CAPTF, such as a Terraform root that installs
	// add-ons. It is any JSON value the module returns (an object is
	// recommended), in compact form; a null or absent output becomes {}.
	// It is omitted when its compact form is larger than
	// MaxPublishedExportsBytes, and for an externally managed cluster; a
	// module output that breaks the contract leaves the previous value. The
	// controller never reads it back: machines and pools read the exports
	// from the cluster's state. Anyone who can get the TerraformCluster can
	// read it, so exports must hold no secrets.
	// +optional
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	Exports runtime.RawExtension `json:"exports,omitempty,omitzero"`

	// plan is the plan of the change waiting for approval under
	// applyPolicy Manual; empty when none waits. Approve it by setting the
	// captf.io/approve-plan annotation to plan.planHash.
	// +optional
	Plan PlanPreview `json:"plan,omitempty,omitzero"`
}

// MaxPublishedExportsBytes caps the compact JSON size of status.exports; a
// larger exports output is not published.
const MaxPublishedExportsBytes = 64 << 10

// MaxPlanResources caps status.plan.resources.
const MaxPlanResources = 50

// PlanPreview summarizes a plan for review: counts and the address and
// action of each changed resource, never a value.
type PlanPreview struct {
	// inputsHash is the hash of the inputs the plan was made for.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	InputsHash string `json:"inputsHash,omitempty"`

	// job is the Job that made the plan: a plan Job, or an approved apply
	// that found the plan changed.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Job string `json:"job,omitempty"`

	// planHash fingerprints the plan's changes: the value of the
	// captf.io/approve-plan annotation that approves it.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	PlanHash string `json:"planHash,omitempty"`

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

	// createdAt is when the plan was made.
	// +optional
	CreatedAt *metav1.Time `json:"createdAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=terraformclusters,scope=Namespaced,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".metadata.labels['cluster\\.x-k8s\\.io/cluster-name']",description="Cluster"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Ready condition"
// +kubebuilder:printcolumn:name="Provisioned",type="boolean",JSONPath=".status.initialization.provisioned",description="Infrastructure provisioned"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".spec.controlPlaneEndpoint.host",description="Control-plane endpoint host"
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.source.image",description="Module image",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"

// TerraformCluster is the Schema for the terraformclusters API: the
// InfraCluster of Cluster API, provisioned by a Terraform/OpenTofu module.
type TerraformCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state of the TerraformCluster.
	// +required
	Spec TerraformClusterSpec `json:"spec,omitempty,omitzero"`

	// status is the observed state of the TerraformCluster.
	// +optional
	Status TerraformClusterStatus `json:"status,omitempty,omitzero"`
}

// GetConditions returns the conditions of the TerraformCluster.
func (c *TerraformCluster) GetConditions() []metav1.Condition {
	return c.Status.Conditions
}

// SetConditions sets the conditions of the TerraformCluster.
func (c *TerraformCluster) SetConditions(conditions []metav1.Condition) {
	c.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// TerraformClusterList contains a list of TerraformCluster.
type TerraformClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TerraformCluster `json:"items"`
}
