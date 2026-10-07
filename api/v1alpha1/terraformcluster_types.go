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

// ApplyPolicy decides whether a TerraformCluster applies a change on its
// own or waits until its plan is approved.
// +kubebuilder:validation:Enum=Automatic;Manual
type ApplyPolicy string

const (
	// ApplyPolicyAutomatic applies every change as soon as it is seen, only
	// guarded against destructive plans.
	ApplyPolicyAutomatic ApplyPolicy = "Automatic"
	// ApplyPolicyManual plans every change first (a plan Job), records the
	// plan as a TerraformPlan and applies it only once that plan is
	// approved (spec.approved). The first apply of a new cluster is not
	// gated.
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

// TerraformClusterSpec is the desired state of a TerraformCluster: the
// cluster-role module image and how to run it.
// +kubebuilder:validation:MinProperties=1
type TerraformClusterSpec struct {
	// controlPlaneEndpoint is the endpoint of the cluster's API server. A value
	// set by the user is passed to the module as its control_plane_endpoint
	// input; otherwise the controller writes the module's output here once.
	// host and port are set together. Once both are set it is immutable
	// (the webhook and a CEL rule of the CRD enforce this).
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
	// replaces resources becomes a TerraformPlan that waits for approval.
	// Manual runs a plan Job first, records a plan with changes as a
	// TerraformPlan and waits until that plan is approved; the apply then
	// runs only if it plans the same changes again. The first
	// apply of a new cluster (no state yet) is never gated. Mutable.
	// +optional
	ApplyPolicy ApplyPolicy `json:"applyPolicy,omitempty"`

	// maxActiveJobs caps the Jobs of this cluster that run at once, counting
	// the TerraformCluster's own and those of its machines and pools. The
	// manager starts no Job beyond it: an operation waits
	// (WaitingForJobSlot) until one finishes. Drift checks and refreshes
	// start only below 80% of it, so applies and destroys keep headroom. The
	// count comes from the Job cache, so the cap is soft by a few Jobs. 0
	// (unset) means the manager's --cluster-max-active-jobs; the CRD schema's
	// minimum of 1 makes 0 itself an invalid setting, so it unambiguously
	// means unset, the same convention as membershipRefreshIntervalSeconds
	// (kube-api-linter optionalfields: WhenRequired).
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxActiveJobs int32 `json:"maxActiveJobs,omitempty"`

	// defaults are inherited by the TerraformMachines and TerraformMachinePools
	// of this cluster, field by field: a field a machine or pool sets wins,
	// an unset one comes from here. They do not apply to the
	// TerraformCluster itself.
	// +optional
	Defaults *TerraformClusterDefaults `json:"defaults,omitempty"`
}

// TerraformClusterDefaults are values the TerraformMachines and
// TerraformMachinePools of a cluster inherit when they do not set them.
// Operational policy inherits in this order: the machine's or pool's own
// field, then spec.defaults, then the TerraformCluster's own field of the
// same name where it has one (identityRef, jobs, drift, deletionPolicy),
// then the built-in default. Module inputs (source, variables,
// variablesFrom) and adoptRetainedState are never inherited: every role
// names its own image.
type TerraformClusterDefaults struct {
	// identityRef is used by machines and pools without their own
	// identityRef. When unset, such machines and pools use
	// spec.identityRef.
	// +optional
	IdentityRef IdentityReference `json:"identityRef,omitempty,omitzero"`

	// jobs is merged field by field under each machine's or pool's jobs
	// policy, and over the TerraformCluster's own spec.jobs (see
	// JobPolicy).
	// +optional
	Jobs *JobPolicy `json:"jobs,omitempty"`

	// drift is merged field by field under each machine's or pool's drift
	// policy, and over the TerraformCluster's own spec.drift. A pool's drift
	// is never fully disabled: an inherited intervalSeconds of 0 disables a
	// machine's drift checks but not a pool's, which then uses the
	// controller's default interval. action is inherited by pools only: a
	// machine's drift is always reported, never remediated.
	// +optional
	Drift *DriftPolicy `json:"drift,omitempty"`

	// remediation is merged field by field under each machine's
	// remediation policy. Pools have none.
	// +optional
	Remediation *MachineRemediation `json:"remediation,omitempty"`

	// membershipRefreshIntervalSeconds is the membership refresh interval
	// of each pool that does not set its own, in seconds. 0 (unset) means
	// 60, applied at reconcile. Machines have none.
	// +optional
	// +kubebuilder:validation:Minimum=15
	// +kubebuilder:validation:Maximum=86400
	MembershipRefreshIntervalSeconds int32 `json:"membershipRefreshIntervalSeconds,omitempty"`

	// deletionPolicy is the deletionPolicy of each machine and pool that
	// does not set its own. When unset they take the TerraformCluster's own
	// spec.deletionPolicy, else Destroy.
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
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

	// pendingPlanRef names the live TerraformPlan of the cluster: the plan
	// that waits for an approval, or whose approved apply has not finished
	// yet. It is omitted when no plan is live.
	// +optional
	PendingPlanRef PlanReference `json:"pendingPlanRef,omitempty,omitzero"`
}

// MaxPublishedExportsBytes caps the compact JSON size of status.exports; a
// larger exports output is not published.
const MaxPublishedExportsBytes = 64 << 10

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=terraformclusters,scope=Namespaced,shortName=tfc,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".metadata.labels['cluster\\.x-k8s\\.io/cluster-name']",description="Cluster"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Ready condition"
// +kubebuilder:printcolumn:name="Provisioned",type="boolean",JSONPath=".status.initialization.provisioned",description="Infrastructure provisioned"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".spec.controlPlaneEndpoint.host",description="Control-plane endpoint host"
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.source.image",description="Module image",priority=1
// +kubebuilder:printcolumn:name="InputsApplied",type="string",JSONPath=`.status.conditions[?(@.type=="InputsApplied")].status`,description="InputsApplied condition",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"

// TerraformCluster is the Schema for the terraformclusters API: the
// InfraCluster of Cluster API, provisioned by a Terraform/OpenTofu module.
// It requires spec.identityRef, which the TerraformClusterTemplate it shares
// the spec type with may leave to a ClusterClass patch. The endpoint
// immutability is a rule of the kind as well, not of TerraformClusterSpec,
// which the template shares and whose immutability only the webhook enforces.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.controlPlaneEndpoint) || !has(oldSelf.spec.controlPlaneEndpoint.host) || !has(oldSelf.spec.controlPlaneEndpoint.port) || (has(self.spec.controlPlaneEndpoint) && self.spec.controlPlaneEndpoint == oldSelf.spec.controlPlaneEndpoint)",message="controlPlaneEndpoint is immutable once it has a host and a port: every Machine and kubeconfig of the cluster points at it",fieldPath=".spec.controlPlaneEndpoint"
// +kubebuilder:validation:XValidation:rule="has(self.spec.identityRef)",message="an identity is mandatory: set spec.identityRef (spec.defaults.identityRef only applies to machines)",fieldPath=".spec.identityRef"
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
