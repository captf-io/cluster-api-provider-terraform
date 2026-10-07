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

package terraformmachinepool

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/outputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/ownership"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Finalizer is the TerraformMachinePool finalizer.
const Finalizer = "terraformmachinepool.infrastructure.cluster.x-k8s.io"

// adapter implements shared.Kind for one TerraformMachinePool.
type adapter struct {
	d   shared.Deps
	obj *infrav1.TerraformMachinePool
	// guard is how an apply of the inputs BuildInputs built last is
	// guarded (guardExports).
	guard shared.Guard
	// approvedExports is the approval hash of the change of the cluster's
	// exports an approved TerraformPlan approves (ApproveExports); "" when
	// none does.
	approvedExports string
}

var (
	_ shared.Kind               = &adapter{}
	_ shared.MembershipObserver = &adapter{}
	_ shared.ExportsGuard       = &adapter{}
)

// newAdapter returns an adapter wrapping obj, using d for every call that
// needs a client, recorder or the other shared dependencies.
func newAdapter(d shared.Deps, obj *infrav1.TerraformMachinePool) *adapter {
	return &adapter{d: d, obj: obj}
}

// Object returns the wrapped TerraformMachinePool as a shared.Object.
func (a *adapter) Object() shared.Object { return a.obj }

// Kind returns state.KindTerraformMachinePool.
func (a *adapter) Kind() string { return state.KindTerraformMachinePool }

// Role returns contract.RoleMachinePool.
func (a *adapter) Role() contract.Role { return contract.RoleMachinePool }

// Finalizer returns Finalizer.
func (a *adapter) Finalizer() string { return Finalizer }

// Mutable reports true: a change of the pool's inputs re-applies it.
func (a *adapter) Mutable() bool { return true }

// RefreshAfterApply reports true: the group's members join after the
// apply, and a Node stays unschedulable until its providerID reaches
// MachinePool.spec.providerIDList.
func (a *adapter) RefreshAfterApply() bool { return true }

// Spec returns the wrapped TerraformMachinePool's spec as a
// shared.SpecView. PoolDrift is never nil: it is what marks the view as a
// pool's (shared.SpecView.PoolDrift), so an unset spec.drift is a zero
// policy.
func (a *adapter) Spec() shared.SpecView {
	s := a.obj.Spec
	drift := &infrav1.MachinePoolDriftPolicy{}
	if s.Drift != nil {
		drift = s.Drift.DeepCopy()
	}
	return shared.SpecView{
		WorkspaceSpec: s.WorkspaceSpec, PoolDrift: drift, InheritsDefaults: true,
		MembershipRefreshInterval: time.Duration(s.MembershipRefreshIntervalSeconds) * time.Second,
	}
}

// Status returns the wrapped TerraformMachinePool's status as a
// shared.CommonStatus.
func (a *adapter) Status() shared.CommonStatus {
	return shared.CommonStatus{WorkspaceStatus: &a.obj.Status.WorkspaceStatus, PendingPlanRef: &a.obj.Status.PendingPlanRef}
}

// MembershipConverging reports whether spec.providerIDList and
// status.replicas disagree: members are still joining or leaving.
func (a *adapter) MembershipConverging() bool {
	return len(a.obj.Spec.ProviderIDList) != int(ptr.Deref(a.obj.Status.Replicas, 0))
}

// Owner looks up the MachinePool (util.GetOwnerMachinePool: a
// MachinePool-kind ownerRef only), then the Cluster through the
// cluster-name label, then the Cluster's TerraformCluster
// (shared.LookupCluster):
//
//   - no MachinePool ownerRef, but another ownerRef: not owned yet,
//     WaitingForOwnerMachinePool;
//   - a MachinePool ownerRef whose MachinePool is gone: owner gone,
//     deletion still destroys from the durable inputs;
//   - a MachinePool ownerRef whose MachinePool does not reference this
//     TerraformMachinePool back (ownership.MachinePoolOwnerMismatch: wrong or
//     missing spec.template.spec.infrastructureRef, a UID mismatch, or a
//     disagreeing cluster-name label) is a forged or stale ownerRef:
//     OwnerMismatch, treated as no valid owner so no Job runs and nothing
//     is written to the named MachinePool or its Cluster; deletion still
//     destroys from the durable inputs, exactly as for an owner that is
//     gone;
//   - a Cluster whose infrastructureRef is not a TerraformCluster:
//     ClusterNotTerraform, nothing is rendered or run.
//
// ctx is used for every lookup. It returns the resolved shared.OwnerInfo
// (possibly carrying a Gate), or an error from a lookup failure other than
// not-found.
func (a *adapter) Owner(ctx context.Context) (shared.OwnerInfo, error) {
	return shared.ResolveOwner(ctx, a.d.Client, a.obj.ObjectMeta, shared.OwnerLookup[*clusterv1.MachinePool]{
		Noun: "MachinePool",
		Get: func(ctx context.Context) (*clusterv1.MachinePool, bool, error) {
			mp, err := util.GetOwnerMachinePool(ctx, a.d.Client, a.obj.ObjectMeta)
			return mp, mp != nil, err
		},
		Mismatch: func(mp *clusterv1.MachinePool) string {
			return ownership.MachinePoolOwnerMismatch(mp, a.obj, a.obj.Labels[clusterv1.ClusterNameLabel])
		},
		WaitingReason:  infrav1.WaitingForOwnerMachinePoolReason,
		WaitingMessage: "Waiting for the MachinePool ownerRef",
		Assign:         func(info *shared.OwnerInfo, mp *clusterv1.MachinePool) { info.MachinePool = mp },
	})
}

// BuildInputs checks the DependenciesReady gates against owner and builds
// the pool inputs, using ctx for every read. The pool is mutable: it runs
// on every reconcile, so the current inputs hash can be compared with the
// state's. The cluster's exports they render, and how their apply is
// guarded (ExportsGuard), follow durable, the durable inputs Secret
// (guardExports): while a destructive change of the exports waits for
// approval, the inputs render those of the last successful apply. It
// returns the built inputs, or a non-nil Gate (with nil inputs) while a
// gate is not yet clear, or an error from a failed read or hash.
func (a *adapter) BuildInputs(ctx context.Context, owner shared.OwnerInfo, durable *inputs.Durable) (any, *shared.Gate, error) {
	in := GateInput{MachinePoolGone: owner.MachinePool == nil, ClusterFound: owner.Cluster != nil, InfraClusterFound: owner.InfraCluster != nil}
	if g := CheckGates(in); in.MachinePoolGone || !in.ClusterFound {
		return nil, g, nil
	}
	p := owner.Cluster.Status.Initialization.InfrastructureProvisioned
	in.InfrastructureProvisioned = p != nil && *p

	exports, fds, ready, err := a.clusterOutputs(ctx, owner.InfraCluster)
	if err != nil {
		return nil, nil, err
	}
	in.ClusterOutputsReady = ready

	mp := owner.MachinePool
	a.setAutoscalingActive(mp)

	bootstrap, found, err := shared.ReadBootstrapSecret(ctx, a.d.APIReader, mp.Namespace, mp.Spec.Template.Spec.Bootstrap.DataSecretName)
	if err != nil {
		return nil, nil, err
	}
	in.BootstrapReady = found && shared.BootstrapReady(bootstrap)

	if g := CheckGates(in); g != nil {
		return nil, g, nil
	}
	vars, gate, err := shared.ResolveVariables(ctx, a.d.APIReader, a.obj.Namespace, contract.RoleMachinePool,
		shared.VariablesSpec{Inline: a.obj.Spec.Variables, From: a.obj.Spec.VariablesFrom})
	if err != nil || gate != nil {
		return nil, gate, err
	}
	mi := MachinePoolInputs(owner.Cluster, mp, a.obj, exports, fds, bootstrap)
	mi.Variables = vars
	if mi, a.guard, err = a.guardExports(mi, durable); err != nil {
		return nil, nil, err
	}
	return mi, nil, nil
}

// setAutoscalingActive sets the AutoscalingActive condition on a.obj from
// mp's autoscaler annotations (ParseAutoscaling): True/ReplicasManagedByModule
// when both are present and valid, False otherwise
// (AutoscalingDisabled/AutoscalingAnnotationsInvalid), and False with
// ReplicasManagedExternally when valid annotations meet a foreign
// replicas-managed-by value. Informational only:
// never feeds Ready, not mirrored to the MachinePool.
func (a *adapter) setAutoscalingActive(mp *clusterv1.MachinePool) {
	autoscaling, reason, message := ParseAutoscaling(mp)
	status := metav1.ConditionFalse
	if autoscaling.Enabled {
		if foreign, isForeign := foreignReplicasOwner(mp); isForeign {
			reason = infrav1.ReplicasManagedExternallyReason
			message = "annotation " + clusterv1.ReplicasManagedByAnnotation + " is " + strconv.Quote(foreign) +
				": another controller owns spec.replicas, so observed replicas are not written back"
		} else {
			status = metav1.ConditionTrue
		}
	}
	// The warning fires on the transition into the foreign-owner state, not
	// on every reconcile that finds it.
	if prev := conditions.Get(a.obj, infrav1.AutoscalingActiveCondition); reason == infrav1.ReplicasManagedExternallyReason &&
		(prev == nil || prev.Reason != reason) {
		a.d.Emit(a.obj, corev1.EventTypeWarning, shared.EventReplicasManagedExternally, "WriteBackReplicas", "MachinePool %s", message)
	}
	conditions.Set(a.obj, metav1.Condition{
		Type: infrav1.AutoscalingActiveCondition, Status: status, Reason: reason, Message: message,
	})
}

// clusterOutputs reads, using ctx and in one read of tc's state, the
// cluster's exports output and its failure-domain names, sorted. An
// externally managed TerraformCluster has no state: exports {} and the
// names of its status.failureDomains. It returns both and true once they
// are readable, false while waiting (nil tc, a state not written or
// mid-write, or either output invalid), or an error from any other read
// failure.
func (a *adapter) clusterOutputs(ctx context.Context, tc *infrav1.TerraformCluster) (json.RawMessage, []string, bool, error) {
	exports, names, ok, err := shared.ReadClusterOutputs(ctx, a.d, tc, "exports", "failure_domains")
	if err != nil || !ok {
		return nil, nil, false, err
	}
	return exports, sortedNames(names), true, nil
}

// sortedNames returns names sorted, or nil when empty.
func sortedNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	slices.Sort(names)
	return names
}

// ApplyOutputs maps st's decoded pool outputs onto a.obj, each only while
// the decode Result does not concern it (a problem keeps the previous
// value): spec.providerID (a null one keeps the previous value; the pool
// is mutable, so a new group ID replaces it, with an event),
// spec.providerIDList (sorted, deduplicated), status.replicas (0 written
// explicitly) and status.instances. status.ready is latched true with
// provisioning, and never reset. It returns the decode Result, the decoded
// health (nil if the health output has a problem), and always a nil error.
func (a *adapter) ApplyOutputs(_ context.Context, _ shared.OwnerInfo, st *state.State, _ *inputs.Durable) (outputs.Result, *contract.Health, error) {
	out, res := outputs.DecodeMachinePool(st)
	if id := out.ProviderID; id != nil && *id != a.obj.Spec.ProviderID {
		note := "spec.providerID set to %s"
		if a.obj.Spec.ProviderID != "" {
			note = "spec.providerID changed to %s"
		}
		a.obj.Spec.ProviderID = *id
		a.d.Emit(a.obj, corev1.EventTypeNormal, shared.EventProviderIDSet, "Reconcile", note, *id)
	}
	if !res.Concerns("provider_id_list") {
		a.obj.Spec.ProviderIDList = nil
		if len(out.ProviderIDList) > 0 {
			a.obj.Spec.ProviderIDList = slices.Clone(out.ProviderIDList)
		}
	}
	if !res.Concerns("replicas") && out.Replicas != nil {
		a.obj.Status.Replicas = new(*out.Replicas)
	}
	if !res.Concerns("instances") {
		a.obj.Status.Instances = poolInstances(out.Instances)
	}

	var health *contract.Health
	if !res.Concerns("health") {
		health = &out.Health
	}
	prev := ptr.Deref(a.obj.Status.Initialization.Provisioned, false)
	if prev || shared.Provisioned(prev, st.InputsHash != "", res.Valid(), health) {
		a.obj.Status.Ready = new(true)
	}
	return res, health, nil
}

// poolInstances maps in, the decoded instances, onto status.instances,
// field by field, with addresses sorted and deduplicated as a machine's
// are. It returns nil for no instances (the field has minItems 1).
func poolInstances(in []contract.PoolInstance) []infrav1.MachinePoolInstance {
	if len(in) == 0 {
		return nil
	}
	out := make([]infrav1.MachinePoolInstance, 0, len(in))
	for _, i := range in {
		mi := infrav1.MachinePoolInstance{
			ProviderID:    i.ProviderID,
			InstanceID:    ptr.Deref(i.InstanceID, ""),
			Addresses:     outputs.MachineAddresses(outputs.Dedupe(outputs.SortAddresses(i.Addresses))),
			FailureDomain: ptr.Deref(i.FailureDomain, ""),
		}
		if i.State != nil {
			mi.State = infrav1.HealthState(*i.State)
		}
		out = append(out, mi)
	}
	return out
}

// DeletionBlocked always returns false, nil: nothing depends on a pool.
// Destroy runs from the durable inputs, ungated on the bootstrap Secret or
// the Cluster.
func (a *adapter) DeletionBlocked(context.Context, shared.OwnerInfo) (bool, error) { return false, nil }
