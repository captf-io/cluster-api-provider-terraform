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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	captfconds "github.com/captf-io/cluster-api-provider-terraform/internal/conditions"
	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// buildInputs builds the kind's inputs using ctx (build), and, when an
// ExportsGuard kind applied before the exports of its last successful
// apply were recorded and this pass proves which those were
// (seedExports), records them and builds again, so this pass's guard
// reads them. bk is this pass's bookkeeping; view is the state as read
// this pass, whose CurrentHash it sets. It returns the built inputs, the
// gate (nil when none), and any error from building, hashing or seeding.
func (r *reconciler) buildInputs(ctx context.Context, bk *Bookkeeping, view *StateView) (any, *Gate, error) {
	in, gate, err := r.build(ctx, view)
	if err != nil || gate != nil {
		return in, gate, err
	}
	seeded, err := r.seedExports(ctx, bk, *view)
	if err != nil || !seeded {
		return in, nil, err
	}
	return r.build(ctx, view)
}

// build builds the kind's inputs using ctx from the owners and the
// durable Secret, an ExportsGuard kind's with the change of the cluster's
// exports its approved TerraformPlan approves (approvedExports), sets
// DependenciesReady from the gate, and, without a gate, reads how the kind
// guards them (observeGuard) and sets view's CurrentHash, and the pass's
// bookkeeping's, for a mutable kind. It returns the built inputs, the gate
// (nil when none), and any error from building or hashing.
func (r *reconciler) build(ctx context.Context, view *StateView) (any, *Gate, error) {
	if eg, ok := r.k.(ExportsGuard); ok {
		eg.ApproveExports(r.approvedExports())
	}
	in, gate, err := r.k.BuildInputs(ctx, r.owner, r.durable)
	if err != nil {
		return nil, nil, err
	}
	if gate != nil {
		captfconds.SetDependenciesReady(r.obj, gate.Status, gate.Reason, gate.Message)
		return in, gate, nil
	}
	captfconds.SetDependenciesReady(r.obj, metav1.ConditionTrue, infrav1.DependenciesReadyReason, "")
	r.observeGuard()
	if r.k.Mutable() {
		if view.CurrentHash, err = r.inputsHash(in); err != nil {
			return nil, nil, err
		}
		if r.bk != nil {
			r.bk.CurrentHash = view.CurrentHash
		}
	}
	return in, nil, nil
}

// seedExports records, using ctx, the exports of the last successful
// apply of an ExportsGuard kind that applied before such records existed
// (Guard.Unknown), once this pass can prove which they were, and reports
// whether it recorded them. bk is this pass's bookkeeping and view the
// state as read this pass. The proof is one of:
//
//   - the newest apply Job succeeded, the state records its inputs hash,
//     and the durable Secret's rendered inputs hash to it as well
//     (inputs.PoolInputsHash): the Secret still holds that apply's
//     inputs, so their exports are the applied ones. The rendered inputs
//     are checked, not assumed: an apply deleted while it ran, or whose
//     Job was never created, wrote the Secret after it.
//   - no apply Job is retained and the current inputs hash to the
//     state's: the current exports are the applied ones.
//
// An apply Job recorded as gone before it finished (InterruptedApply)
// may have changed the state after the last successful apply, so nothing
// is seeded then. Without a proof the exports stay unknown: every apply
// is guarded, and the next successful one records its exports. Exports
// that do not fit next to the rendered files are seeded by their hash
// alone (inputs.SeedClusterOutputs). It sets r.durable's record so a
// build reads it, and returns any error from recording.
func (r *reconciler) seedExports(ctx context.Context, bk *Bookkeeping, view StateView) (bool, error) {
	d := r.durable
	if r.guard == nil || !r.guard.Unknown || d == nil || d.InterruptedApply != "" || !view.Exists || view.InputsHash == "" {
		return false, nil
	}
	var raw json.RawMessage
	var from string
	last := bk.LastApply
	switch {
	case last != nil && bk.LastApplySucceeded && last.Annotations[state.InputsHashAnnotation] == view.InputsHash:
		if h, err := inputs.PoolInputsHash(d); err != nil || h != view.InputsHash {
			klog.FromContext(ctx).V(LogFlow).Info("Not recording the exports of the last successful apply: the durable inputs are not its own", "Job", klog.KObj(last))
			return false, nil
		}
		raw, from = inputs.LastClusterOutputs(d), "the durable inputs of Job "+last.Name
	case !slices.ContainsFunc(bk.Jobs, func(j batchv1.Job) bool { return jobs.OpOf(&j) == jobs.OpApply }) && view.CurrentHash == view.InputsHash:
		raw, from = r.guard.Exports, "the current inputs"
	default:
		return false, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return false, nil
	}
	exports := buf.Bytes()
	h, err := hash.Exports(exports)
	if err != nil {
		return false, nil
	}
	fits, err := inputs.SeedClusterOutputs(ctx, r.d.Client, r.obj, exports)
	switch {
	case errors.Is(err, inputs.ErrNotFound), errors.Is(err, inputs.ErrExportsRecorded):
		return false, nil
	case err != nil:
		return false, err
	}
	d.AppliedExportsHash, d.AppliedClusterOutputs = h, nil
	if fits {
		d.AppliedClusterOutputs = exports
	}
	klog.FromContext(ctx).Info("Recorded the cluster exports of the last successful apply, which applied before they were recorded",
		"from", from, "exportsHash", h, "recorded", fits)
	return true, nil
}
