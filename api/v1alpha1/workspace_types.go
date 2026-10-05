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

// WorkspaceSpec is the part of a Job-running kind's spec every such kind
// shares: the role module image, its identity, Job policy and module
// variables. TerraformClusterSpec and TerraformMachineSpec embed it.
type WorkspaceSpec struct {
	// source is the role image: module code and runtime.
	// +required
	Source Source `json:"source,omitempty,omitzero"`

	// identityRef names the TerraformClusterIdentity whose credentials this
	// object's Jobs use. Whether it is required, and where it falls back to
	// when unset, depends on the kind.
	// +optional
	IdentityRef IdentityReference `json:"identityRef,omitempty,omitzero"`

	// jobs tunes the Jobs that run this object's module. On a kind that
	// inherits defaults it is merged field by field over them (see
	// JobPolicy).
	// +optional
	Jobs *JobPolicy `json:"jobs,omitempty"`

	// variables are module variables, a JSON object: each key becomes a
	// named argument of the role module, converted by the module's declared
	// type. Keys are Terraform identifiers; captf_ names and the role's
	// contract inputs are reserved. Inline variables win over variablesFrom.
	// Whether a change re-applies or is rejected as immutable depends on the
	// kind. A key the module does not declare fails the apply ("Unsupported
	// argument").
	// +optional
	// +kubebuilder:validation:Type=object
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=256
	// +kubebuilder:pruning:PreserveUnknownFields
	Variables runtime.RawExtension `json:"variables,omitempty,omitzero"`

	// variablesFrom reads module variables from ConfigMaps and Secrets in
	// this namespace labeled captf.io/variables=true, in list order: a later
	// source wins on the same key, and inline variables win over all of
	// them. Whether and when a change to a referenced source takes effect
	// depends on the kind.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	VariablesFrom []VariablesSource `json:"variablesFrom,omitempty"`
}

// WorkspaceStatus is the part of a Job-running kind's status every such kind
// shares. Nothing here is load-bearing: every value is rebuilt from spec,
// the state Secret, the durable inputs Secret or the Job list, because
// clusterctl move does not restore status. TerraformClusterStatus and
// TerraformMachineStatus embed it.
type WorkspaceStatus struct {
	// initialization is the v1beta2 contract's initialization status.
	// +optional
	Initialization Initialization `json:"initialization,omitempty,omitzero"`

	// observedGeneration is the generation this status was computed for.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// activeJob is the Job currently running for this object, if any.
	// +optional
	ActiveJob ActiveJob `json:"activeJob,omitempty,omitzero"`

	// lastRun is the result of the most recent completed Job.
	// +optional
	LastRun LastRun `json:"lastRun,omitempty,omitzero"`

	// lastDriftCheck is when the last drift check completed.
	// +optional
	LastDriftCheck *metav1.Time `json:"lastDriftCheck,omitempty"`

	// lastRefresh is when the last refresh or drift check completed, or,
	// for a kind whose apply itself can give a definite health reading,
	// when that apply finished instead (that reading stands in for the
	// refresh after the apply).
	// +optional
	LastRefresh *metav1.Time `json:"lastRefresh,omitempty"`

	// pendingRefreshes counts the consecutive health samples (completed
	// refresh or drift Jobs) that read pending since the last other reading
	// or the last apply; unset otherwise. It spaces the refreshes while
	// health is pending: 30s, then 1m, 2m, 4m and at most 5m. It lives in
	// status only, so it restarts at 0 (30s) after clusterctl move.
	// +optional
	// +kubebuilder:validation:Minimum=1
	PendingRefreshes int32 `json:"pendingRefreshes,omitempty"`

	// observedStateSerial is the Terraform state serial the outputs were read
	// from.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ObservedStateSerial int64 `json:"observedStateSerial,omitempty"`

	// lastRestoredSerial is the backup serial of the last restore Job the
	// controller consumed (succeeded or failed), so a restore-state
	// annotation naming it is not run again.
	// +optional
	// +kubebuilder:validation:Minimum=1
	LastRestoredSerial int64 `json:"lastRestoredSerial,omitempty"`

	// stateSecretSuffix is the kubernetes backend secret_suffix of this
	// object's state. Informational: the controller derives it
	// deterministically.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	StateSecretSuffix string `json:"stateSecretSuffix,omitempty"`

	// source records what the last Job actually ran.
	// +optional
	Source SourceStatus `json:"source,omitempty,omitzero"`

	// stateBackups are the state backups the controller keeps (newest
	// first), as of the last backup, prune or restore request.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	StateBackups []StateBackup `json:"stateBackups,omitempty"`
}

// TemplateMeta holds the metadata field every *TemplateResource copies onto
// the object it creates. TerraformClusterTemplateResource and
// TerraformMachineTemplateResource embed it.
type TemplateMeta struct {
	// metadata is copied to the object created from this template.
	// +optional
	ObjectMeta *clusterv1.ObjectMeta `json:"metadata,omitempty"`
}
