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
	"fmt"
	"slices"

	"k8s.io/utils/ptr"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
)

// ExportsGuard returns how an apply of the inputs the last BuildInputs
// call built is guarded (guardExports); the zero Guard before any.
func (a *adapter) ExportsGuard() shared.Guard { return a.guard }

// WaitExports records, for the next BuildInputs call, approvalHash: the
// approval hash of the change of the cluster's exports whose live
// ExportsChange TerraformPlan waits for approval, "" when none does.
func (a *adapter) WaitExports(approvalHash string) { a.waitingExports = approvalHash }

// guardExports decides which cluster exports in, the pool inputs built
// with the cluster's current exports, renders, and how their apply is
// guarded, from durable's record of the last successful apply's exports,
// of a change waiting for approval, and of one a failed apply may have
// partly applied:
//
//   - no record and no pending change of a pool that never applied, or
//     the current exports are the recorded ones (by their hash, which
//     stays when the record itself no longer fits next to the rendered
//     files), and no change is partly applied: in as built, unguarded
//     (Settled). A first apply is never guarded, as a cluster's is not;
//     bootstrap rotations, version rolls, replicas and spec edits stay
//     unguarded while the exports are unchanged.
//   - no record and no hash of a pool that applied (the applied marker, a
//     pinned digest, or status.initialization.provisioned): it applied
//     before records existed, and the reconciler proved no baseline to
//     seed (shared seedExports), so the exports of its last successful
//     apply are unknown (Unknown): in as built, guarded by its approval
//     hash (Guarded, Unrecorded), with nothing to hold, until a
//     successful apply records its exports.
//   - the current exports are the pending change, whose guarded apply was
//     blocked for the approval hash the inputs have now, whose
//     TerraformPlan, made for that hash, waits for approval
//     (WaitExports), the record is there and no change is partly
//     applied: in with the recorded exports in their place (Held). An
//     edit of anything but bootstrap_data while held moves the approval
//     hash off the blocked one, so the change is guarded again: its plan,
//     made for the old hash, could never be approved for the new one.
//     Exports that return to a change whose plan was superseded meanwhile
//     are guarded again too, so their block makes a plan to approve. The pool keeps applying,
//     unguarded, everything but that change; refresh and drift render the
//     same, so the change does not read as drift.
//   - any other change of the exports (a new one, or the pending one
//     approved, or any change while only the hash of the applied
//     exports is recorded: Unrecorded, the rendered files left no room
//     for the record), or any exports while a change is partly applied:
//     in as built, guarded by its approval hash (Guarded). A partly
//     applied change means the state may not match the recorded exports,
//     and a missing record leaves nothing to hold, so an apply that would
//     delete or replace resources then waits for approval, as a
//     cluster's does.
//
// The approval hash is hash.Approval of in with the current exports: the
// inputs hash without bootstrap_data. It returns the inputs to render
// and their Guard, or an error from hashing.
func (a *adapter) guardExports(in contract.MachinePoolInputs, durable *inputs.Durable) (contract.MachinePoolInputs, shared.Guard, error) {
	current, err := hash.Exports(in.ClusterOutputs)
	if err != nil {
		return in, shared.Guard{}, fmt.Errorf("hash the cluster's exports: %w", err)
	}
	g := shared.Guard{ExportsHash: current, Exports: in.ClusterOutputs, Settled: true}
	if durable == nil {
		return in, g, nil
	}
	// The hash outlives a record dropped for size; an unreadable record
	// counts as none to hold.
	applied := durable.AppliedExportsHash
	if p := durable.Partial; p != nil {
		g.Partial, g.PartialJob = true, p.Job
	}
	// No hash with a change pending is a record that predates the hash
	// and was dropped, not a pool that never applied: the change stays
	// guarded.
	g.Settled = applied == current || (applied == "" && durable.Pending == nil)
	// No hash after an apply is a pool that applied before records
	// existed: whatever it applied, it is unknown.
	g.Unknown = applied == "" && (durable.Meta.Applied || durable.Meta.ImageDigest != "" || ptr.Deref(a.obj.Status.Initialization.Provisioned, false))
	if g.Settled && !g.Partial && !g.Unknown {
		return in, g, nil
	}
	g.Unrecorded = len(durable.AppliedClusterOutputs) == 0
	approval, err := hash.Approval(contract.RoleMachinePool, a.obj.Spec.Source.Image, in)
	if err != nil {
		return in, shared.Guard{}, fmt.Errorf("approval hash: %w", err)
	}
	g.ApprovalHash = approval
	if p := durable.Pending; p != nil && p.ExportsHash == current && p.ApprovalHash == approval && a.waitingExports == approval && !g.Partial && !g.Unrecorded {
		in.ClusterOutputs = slices.Clone(durable.AppliedClusterOutputs)
		g.ExportsHash, g.Exports, g.Held = applied, in.ClusterOutputs, true
		return in, g, nil
	}
	g.Guarded = true
	return in, g, nil
}
