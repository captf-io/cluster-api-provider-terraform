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

package shared

import (
	"context"
	"encoding/json"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/outputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Object is a Terraform* object with conditions.
type Object interface {
	client.Object
	conditions.Setter
}

// OwnerInfo is the result of the owner lookup.
type OwnerInfo struct {
	// Cluster is the owning Cluster; nil when unknown or gone.
	Cluster *clusterv1.Cluster
	// Machine is the owning Machine (machines only).
	Machine *clusterv1.Machine
	// MachinePool is the owning MachinePool (pools only).
	MachinePool *clusterv1.MachinePool
	// InfraCluster is the TerraformCluster: the object itself for a
	// TerraformCluster, the Cluster's infrastructureRef for a machine or
	// pool, whose spec.defaults and identity it inherits. nil when unknown.
	InfraCluster *infrav1.TerraformCluster
	// HasOwnerRef is false when the object has no ownerRef of the expected
	// kind at all.
	HasOwnerRef bool
	// OwnerGone is true when the ownerRef's target is NotFound: deletion
	// still destroys from the durable inputs.
	OwnerGone bool
	// Gate, when set, stops the owner lookup short of a usable owner (for
	// example WaitingForOwnerMachine, ClusterNotTerraform, or
	// OwnerMismatch when an ownerRef's target does not reference this
	// object back); it becomes DependenciesReady. While the object is
	// deleting, Preamble lets a gate fall through instead of stopping, so
	// destroy still runs from the durable inputs (see Preamble's doc
	// comment): a mismatched owner must never block its own deletion.
	Gate *Gate
}

// Gate is a DependenciesReady condition that is not True.
type Gate struct {
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

// SpecView is the part of a Terraform* spec the shared core reads.
type SpecView struct {
	infrav1.WorkspaceSpec
	// Drift is a TerraformCluster's drift policy; nil for machines.
	Drift *infrav1.DriftPolicy
	// ApplyPolicy is a TerraformCluster's applyPolicy; "" (Automatic) for
	// machines.
	ApplyPolicy infrav1.ApplyPolicy
	// MachineDrift is a TerraformMachine's drift policy.
	MachineDrift *infrav1.MachineDriftPolicy
	// Remediation is a TerraformMachine's remediation policy; nil for the
	// cluster.
	Remediation *infrav1.MachineRemediation
	// PoolDrift is a TerraformMachinePool's drift policy. It marks the pool:
	// the pool adapter always sets it, to a zero value when spec.drift is
	// unset, and every other kind leaves it nil.
	PoolDrift *infrav1.MachinePoolDriftPolicy
	// MembershipRefreshInterval is a TerraformMachinePool's
	// spec.membershipRefreshIntervalSeconds; 0 when unset (the pool then
	// gets DefaultMembershipRefreshInterval) and for every other kind.
	MembershipRefreshInterval time.Duration
	// MaxActiveJobs is a TerraformCluster's spec.maxActiveJobs; 0 when
	// unset and for every other kind.
	MaxActiveJobs int32
	// InheritsDefaults is true for kinds whose unset fields come from the
	// TerraformCluster's spec.defaults (machines and pools); false for the
	// cluster itself.
	InheritsDefaults bool
}

// CommonStatus points into the status fields every Job-running kind has.
type CommonStatus struct {
	*infrav1.WorkspaceStatus
	// UnhealthySamples is nil for kinds that do not count samples (the
	// cluster).
	UnhealthySamples *int32
	// PendingPlanRef is status.pendingPlanRef, the live TerraformPlan of a
	// kind that keeps plans; nil for kinds without it (machines).
	PendingPlanRef *infrav1.PlanReference
}

// Kind adapts one Job-running kind to the shared core. The cluster and
// machine reconcilers implement it.
type Kind interface {
	// Object returns the object being reconciled.
	Object() Object
	// Kind returns its GVK Kind, as internal/state takes it.
	Kind() string
	// Role returns the module role it renders.
	Role() contract.Role
	// Finalizer returns its finalizer.
	Finalizer() string
	// Mutable reports whether a spec change re-applies (clusters) or the
	// spec is immutable (machines).
	Mutable() bool
	// RefreshAfterApply asks for a Refresh right after a successful apply,
	// so a machine's addresses and health reach status without waiting for
	// the next tick (RKE2 join latency). The refresh is skipped when the
	// apply's own outputs are valid and give a definite health reading
	// (neither pending nor unknown): that reading is the post-apply
	// sample. It reports whether the refresh is wanted.
	RefreshAfterApply() bool
	// Spec returns the spec fields the core reads.
	Spec() SpecView
	// Status returns pointers into the common status fields.
	Status() CommonStatus

	// Owner looks the owners up using ctx. It has no side effect on
	// the adapter: the result is passed back into the calls below. It
	// returns the owner lookup's result, or any lookup error.
	Owner(ctx context.Context) (OwnerInfo, error)
	// BuildInputs builds, using ctx, the contract inputs from owner, the
	// owner lookup's result. durable is the object's inputs records as
	// read once this reconcile, nil before the first apply. A non-nil
	// Gate means the inputs cannot be built yet; no Apply runs. It returns
	// the built inputs, the gate (nil when none), and any build error.
	BuildInputs(ctx context.Context, owner OwnerInfo, durable *inputs.Durable) (any, *Gate, error)
	// ApplyOutputs maps, using ctx, st's outputs into spec and status of the
	// object owned as owner describes, comparing against durable, the
	// object's inputs records. It returns the decode result, the health
	// output (nil when null), and any error mapping the outputs.
	ApplyOutputs(ctx context.Context, owner OwnerInfo, st *state.State, durable *inputs.Durable) (outputs.Result, *contract.Health, error)
	// DeletionBlocked reports, using ctx, whether deletion must wait, for
	// example for a TerraformCluster's machines, given owner, the owner
	// lookup's result. Machines return false. It
	// also returns any error from checking.
	DeletionBlocked(ctx context.Context, owner OwnerInfo) (bool, error)
}

// MembershipObserver is implemented by kinds whose membership converges
// asynchronously (TerraformMachinePool): the group's members join and
// leave after the apply, so the object is refreshed every
// MembershipConvergingInterval until its outputs agree
// (https://captf.io/docs/module-author/contract/v1alpha1/machinepool.html "Membership refresh"). Kinds
// without it never converge.
type MembershipObserver interface {
	// MembershipConverging reports whether the object's recorded membership
	// is still converging, read from its own fields (for a pool,
	// len(spec.providerIDList) != status.replicas) as they stand after this
	// pass mapped the state's outputs.
	MembershipConverging() bool
}

// ExportsGuard is implemented by kinds whose applies are guarded against a
// destructive plan only when they render a change of the cluster's
// exports (TerraformMachinePool). BuildInputs decides, from the durable
// Secret's record of the exports of the last successful apply
// (inputs.Durable.AppliedClusterOutputs), of a change waiting for
// approval (inputs.Durable.Pending) and of one a failed apply may have
// partly applied (inputs.Durable.Partial), which exports the inputs render
// and how their apply is guarded.
type ExportsGuard interface {
	// ExportsGuard returns how an apply of the inputs the last BuildInputs
	// call built is guarded; the zero Guard before any.
	ExportsGuard() Guard
	// WaitExports tells the next BuildInputs call which change of the
	// exports waits for approval: approvalHash is the approval hash the
	// kind's live ExportsChange TerraformPlan was made for while it waits,
	// "" when no such plan waits. A change is held only while its plan
	// waits: an approved one applies, and one whose plan was superseded is
	// guarded again, so its block makes a plan to approve.
	WaitExports(approvalHash string)
}

// Guard is how an apply of an ExportsGuard kind's built inputs is guarded.
type Guard struct {
	// ExportsHash is hash.Exports of the exports the inputs render, and
	// Exports those exports.
	ExportsHash string
	Exports     json.RawMessage
	// Guarded is true when the inputs render exports other than those of
	// the last successful apply, or when the state may not match those
	// (Partial): the apply stops before a plan that deletes or replaces
	// resources unless an approved TerraformPlan names ApprovalHash.
	Guarded bool
	// Partial is true while a failed guarded apply may have left a change
	// of the exports partly applied (inputs.Durable.Partial), and
	// PartialJob names that Job ("" when the record does not). While
	// Partial, nothing is held: an apply of the exports of the last
	// successful apply could revert that part with no guard.
	Partial    bool
	PartialJob string
	// Unrecorded is true when an apply is Guarded but the exports of the
	// last successful apply are not recorded (they do not fit next to
	// the rendered files, inputs.Write; their hash may still be): there
	// is nothing to hold, so a change stays Guarded, and a destructive
	// one waits for approval.
	Unrecorded bool
	// Unknown is true when the pool applied (the durable Secret's applied
	// marker or pinned digest, or status.initialization.provisioned) but
	// records neither the exports of its last successful apply nor their
	// hash: it applied before such records existed, and nothing proved
	// which exports it applied (the reconciler seeds them when it can).
	// Every apply is then Guarded and Unrecorded, so a destructive plan
	// waits for approval as a cluster's does, until a successful apply
	// records them.
	Unknown bool
	// Held is true while a change of the exports waits for approval (its
	// guarded apply was blocked, and its TerraformPlan, made for its
	// ApprovalHash, waits): the inputs render the exports of the last
	// successful apply instead, and their apply is not guarded.
	Held bool
	// ApprovalHash is the inputs hash a TerraformPlan of the change is made
	// for: hash.Approval of the inputs with the cluster's current exports.
	// Set when Guarded or Held.
	ApprovalHash string
	// Settled is true when the cluster's exports are those of the last
	// successful apply (by their recorded hash), or no apply recorded any
	// and no change is pending: nothing waits for approval, and a
	// recorded pending change is stale. A Settled apply is still Guarded
	// while Partial is set.
	Settled bool
}
