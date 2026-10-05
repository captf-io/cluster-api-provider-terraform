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

package terraformmachine

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
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

// Finalizer is the TerraformMachine finalizer.
const Finalizer = "terraformmachine.infrastructure.cluster.x-k8s.io"

// adapter implements shared.Kind for one TerraformMachine.
type adapter struct {
	d   shared.Deps
	obj *infrav1.TerraformMachine
}

var _ shared.Kind = &adapter{}

// newAdapter returns an adapter wrapping obj, using d for every call that
// needs a client, recorder or the other shared dependencies.
func newAdapter(d shared.Deps, obj *infrav1.TerraformMachine) *adapter {
	return &adapter{d: d, obj: obj}
}

// Object returns the wrapped TerraformMachine as a shared.Object.
func (a *adapter) Object() shared.Object { return a.obj }

// Kind returns state.KindTerraformMachine.
func (a *adapter) Kind() string { return state.KindTerraformMachine }

// Role returns contract.RoleMachine.
func (a *adapter) Role() contract.Role { return contract.RoleMachine }

// Finalizer returns Finalizer.
func (a *adapter) Finalizer() string { return Finalizer }

// Mutable reports false: the machine spec is immutable and its first
// apply's execution context is pinned.
func (a *adapter) Mutable() bool { return false }

// RefreshAfterApply reports true: a control-plane machine's addresses and
// health gate RKE2 joins.
func (a *adapter) RefreshAfterApply() bool { return true }

// Spec returns the wrapped TerraformMachine's spec as a shared.SpecView.
func (a *adapter) Spec() shared.SpecView {
	s := a.obj.Spec
	return shared.SpecView{
		WorkspaceSpec: s.WorkspaceSpec, MachineDrift: s.Drift, Remediation: s.Remediation, InheritsDefaults: true,
	}
}

// Status returns the wrapped TerraformMachine's status as a
// shared.CommonStatus.
func (a *adapter) Status() shared.CommonStatus {
	return shared.CommonStatus{WorkspaceStatus: &a.obj.Status.WorkspaceStatus, UnhealthySamples: &a.obj.Status.UnhealthySamples}
}

// Owner looks up the Machine (util.GetOwnerMachine: a Machine-kind ownerRef
// only), then the Cluster through the cluster-name label, then the
// Cluster's TerraformCluster:
//
//   - no Machine ownerRef, but another ownerRef (a KCP or RCP clone): not
//     owned yet, WaitingForOwnerMachine;
//   - a Machine ownerRef whose Machine is gone: owner gone, deletion still
//     destroys from the durable inputs;
//   - a Machine ownerRef whose Machine does not reference this
//     TerraformMachine back (ownership.MachineOwnerMismatch: wrong or missing
//     spec.infrastructureRef, a UID mismatch, or a disagreeing cluster-name
//     label) is a forged or stale ownerRef: OwnerMismatch, treated as no
//     valid owner so no Job runs and nothing is written to the named
//     Machine or its Cluster; deletion still destroys from the durable
//     inputs, exactly as for an owner that is gone;
//   - a Cluster whose infrastructureRef is not a TerraformCluster:
//     ClusterNotTerraform, nothing is rendered or run.
//
// ctx is used for every lookup. It returns the resolved shared.OwnerInfo
// (possibly carrying a Gate for the caller to return directly), or an
// error from a lookup failure other than not-found.
func (a *adapter) Owner(ctx context.Context) (shared.OwnerInfo, error) {
	return shared.ResolveOwner(ctx, a.d.Client, a.obj.ObjectMeta, shared.OwnerLookup[*clusterv1.Machine]{
		Noun: "Machine",
		Get: func(ctx context.Context) (*clusterv1.Machine, bool, error) {
			m, err := util.GetOwnerMachine(ctx, a.d.Client, a.obj.ObjectMeta)
			return m, m != nil, err
		},
		Mismatch: func(m *clusterv1.Machine) string {
			return ownership.MachineOwnerMismatch(m, a.obj, a.obj.Labels[clusterv1.ClusterNameLabel])
		},
		WaitingReason:  infrav1.WaitingForOwnerMachineReason,
		WaitingMessage: "Waiting for the Machine ownerRef; only a control-plane ownerRef is set",
		Assign:         func(info *shared.OwnerInfo, m *clusterv1.Machine) { info.Machine = m },
	})
}

// BuildInputs checks the DependenciesReady gates against owner and builds
// the machine inputs, using ctx for every read. It runs only until the
// first apply: machines are immutable, and the durable Secret plays no
// part. It returns the built inputs, or a non-nil Gate (with nil inputs)
// while a gate is not yet clear, or an error from a failed read.
func (a *adapter) BuildInputs(ctx context.Context, owner shared.OwnerInfo, _ *inputs.Durable) (any, *shared.Gate, error) {
	in := GateInput{MachineGone: owner.Machine == nil, ClusterFound: owner.Cluster != nil, InfraClusterFound: owner.InfraCluster != nil}
	if g := CheckGates(in); in.MachineGone || !in.ClusterFound {
		return nil, g, nil
	}
	p := owner.Cluster.Status.Initialization.InfrastructureProvisioned
	in.InfrastructureProvisioned = p != nil && *p

	exports, err := a.exports(ctx, owner.InfraCluster)
	if err != nil {
		return nil, nil, err
	}
	in.ExportsReady = exports != nil

	bootstrap, found, err := shared.ReadBootstrapSecret(ctx, a.d.APIReader, owner.Machine.Namespace, owner.Machine.Spec.Bootstrap.DataSecretName)
	if err != nil {
		return nil, nil, err
	}
	in.BootstrapReady = found && shared.BootstrapReady(bootstrap)

	if g := CheckGates(in); g != nil {
		return nil, g, nil
	}
	// Like everything else, the variables are read only until the first
	// apply: the durable inputs keep what the machine was created with.
	vars, gate, err := shared.ResolveVariables(ctx, a.d.APIReader, a.obj.Namespace, contract.RoleMachine,
		shared.VariablesSpec{Inline: a.obj.Spec.Variables, From: a.obj.Spec.VariablesFrom})
	if err != nil || gate != nil {
		return nil, gate, err
	}
	mi := MachineInputs(owner.Cluster, owner.Machine, a.obj, exports, bootstrap)
	mi.Variables = vars
	return mi, nil, nil
}

// exports reads tc's exports output from its state, using ctx for the
// read: {} for an externally managed TerraformCluster, nil while it
// cannot be read (including a nil tc). An unreadable or inconsistent state
// (the cluster Job may be writing chunks) is a wait, not an error. It
// returns the exports value, or an error from a read failure other than a
// wait condition.
func (a *adapter) exports(ctx context.Context, tc *infrav1.TerraformCluster) (json.RawMessage, error) {
	exports, _, ok, err := shared.ReadClusterOutputs(ctx, a.d, tc, "exports")
	if err != nil || !ok {
		return nil, err
	}
	return exports, nil
}

// ApplyOutputs maps st's decoded machine outputs onto a.obj:
// spec.providerID written once and never blanked (the Machine recopies it
// every reconcile), a different later value is ProviderIDChanged, the
// placement must equal owner.Machine's failure domain, addresses sorted,
// interruptible defaults to false. A violation of addresses,
// failure_domain or interruptible keeps the previous status value. After
// provisioning a null provider_id reports contract.HealthProviderIDMissing, and a
// terminated instance only when the next sample is null too;
// spec.providerID is kept. It returns the decode Result, the decoded health (nil
// if it has a problem), and always a nil error.
func (a *adapter) ApplyOutputs(_ context.Context, owner shared.OwnerInfo, st *state.State, _ *inputs.Durable) (outputs.Result, *contract.Health, error) {
	out, res := outputs.DecodeMachine(st)
	// A state with no inputs hash comes from an apply that has not
	// succeeded: its instance may be tainted and replaced by the retry, so
	// its provider_id is not latched until an apply succeeds (or the
	// machine is already provisioned).
	applied := st.InputsHash != "" || ptr.Deref(a.obj.Status.Initialization.Provisioned, false)
	if id := out.ProviderID; id != nil && applied {
		switch {
		case a.obj.Spec.ProviderID == "":
			a.obj.Spec.ProviderID = *id
			a.d.Emit(a.obj, corev1.EventTypeNormal, shared.EventProviderIDSet, "Reconcile", "spec.providerID set to %s", *id)
		case outputs.ProviderIDChanged(a.obj.Spec.ProviderID, *id):
			res.Violations = append(res.Violations, outputs.ProviderIDChangedViolation())
		}
	}
	if m := owner.Machine; m != nil {
		var requested *string
		if fd := m.Spec.FailureDomain; fd != "" {
			requested = &fd
		}
		if v := outputs.CheckPlacement(requested, out.FailureDomain); v != nil {
			res.Violations = append(res.Violations, *v)
		}
	}
	if !res.Concerns("addresses") {
		a.obj.Status.Addresses = outputs.MachineAddresses(out.Addresses)
	}
	if !res.Concerns("failure_domain") {
		a.obj.Status.FailureDomain = ""
		if out.FailureDomain != nil {
			a.obj.Status.FailureDomain = *out.FailureDomain
		}
	}
	if !res.Concerns("interruptible") {
		a.obj.Status.Interruptible = new(false)
		if out.Interruptible != nil {
			a.obj.Status.Interruptible = new(*out.Interruptible)
		}
	}

	var health *contract.Health
	if !res.Concerns("health") {
		health = &out.Health
	}
	// Only a null (or "") provider_id means the instance is gone. A
	// malformed one is an output violation (OutputsValid False), not a
	// terminated instance: with remediation.annotateMachine a wrong
	// Terminated would irreversibly replace a healthy Machine.
	if p := a.obj.Status.Initialization.Provisioned; p != nil && *p && slices.Contains(res.Pending, "provider_id") {
		health = a.providerIDMissing()
	}
	return res, health, nil
}

// providerIDMissing returns the health for a sample whose provider_id is
// missing after provisioning. One bad refresh must not terminate a
// healthy instance, so the first such sample is only Unknown
// (ProviderIDMissing) and the instance is reported terminated when a
// later sample, a refresh that completed since, is missing too. The
// message carries a "(sample <lastRefresh>)" marker naming the refresh the
// sample belongs to; it is compared by value against the current
// status.lastRefresh, never against the surrounding wording, so
// reconciling the same outputs again is not a second sample. Once the
// condition is InstanceTerminated, a still-missing provider_id keeps
// reporting Terminated until a non-null sample arrives, so the condition
// does not flap back to Unknown and remediation sees a stable reason.
func (a *adapter) providerIDMissing() *contract.Health {
	terminated := &contract.Health{
		State:   contract.HealthTerminated,
		Message: new("provider_id turned null after provisioning (instance terminated); spec.providerID kept"),
	}
	c := conditions.Get(a.obj, infrav1.InfrastructureHealthyCondition)
	if c != nil && c.Reason == infrav1.InstanceTerminatedReason {
		return terminated
	}
	stamp := "none"
	if t := a.obj.Status.LastRefresh; t != nil {
		stamp = t.UTC().Format(time.RFC3339)
	}
	if c != nil && c.Reason == infrav1.ProviderIDMissingReason {
		if m := sampleMarkerRE.FindStringSubmatch(c.Message); len(m) == 2 && m[1] != stamp {
			return terminated
		}
	}
	msg := fmt.Sprintf("provider_id is null after provisioning (sample %s); "+
		"the instance will be treated as terminated if the next sample is also missing", stamp)
	return &contract.Health{State: contract.HealthProviderIDMissing, Message: &msg}
}

// sampleMarkerRE matches the sample marker providerIDMissing writes into
// the ProviderIDMissing message: "(sample <stamp>)", where stamp is the
// RFC 3339 status.lastRefresh the sample belongs to, or "none". The older
// "(refresh <stamp>)" form is accepted too, so a condition written before
// the marker was renamed is still read.
var sampleMarkerRE = regexp.MustCompile(`\((?:sample|refresh) ([^)]+)\)`)

// DeletionBlocked always returns false, nil: nothing depends on a machine.
// Destroy runs from the durable inputs, ungated on the bootstrap Secret or
// the Cluster.
func (a *adapter) DeletionBlocked(context.Context, shared.OwnerInfo) (bool, error) { return false, nil }
