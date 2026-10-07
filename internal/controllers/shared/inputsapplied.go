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
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// inputsFacts is what a pass learned about the inputs and the state they
// are applied to, for InputsApplied: decideInput records the state view
// the decision was made from, build records the gate that kept the inputs
// from being built.
type inputsFacts struct {
	// read is true once the pass decided from a state view.
	read bool
	// view is the state as the decision read it, CurrentHash included.
	view StateView
	// gate is the dependency or variables gate that stopped the inputs from
	// being built this pass; nil when they were built or not asked for.
	gate *Gate
}

// reconciling returns the Reconciling condition that goes with ia, the
// pass's InputsApplied: True while the inputs wait to be applied (False
// with ApplyPending, AwaitingApproval or ApplyRunning), naming why, and
// False otherwise, a failed apply included (kstatus would read that as
// in progress forever).
func reconciling(ia *metav1.Condition) metav1.Condition {
	if ia.Status == metav1.ConditionFalse && slices.Contains([]string{infrav1.ApplyPendingReason, infrav1.AwaitingApprovalReason, infrav1.ApplyRunningReason}, ia.Reason) {
		msg := ia.Reason
		if ia.Message != "" {
			msg += ": " + ia.Message
		}
		return metav1.Condition{Type: infrav1.ReconcilingCondition, Status: metav1.ConditionTrue, Reason: infrav1.InputsNotAppliedReason, Message: msg}
	}
	return metav1.Condition{Type: infrav1.ReconcilingCondition, Status: metav1.ConditionFalse, Reason: infrav1.ReconciledReason}
}

// inputsApplied returns the InputsApplied condition of the pass, with bk
// the pass's bookkeeping (nil when it did not run), or nil when the pass
// learned nothing about the inputs (it is deleting, paused, or stopped
// before reading the state) and the condition stays as it was. In order:
//
//   - an apply Job is active (status.activeJob, which the start of a Job
//     sets too): False/ApplyRunning;
//   - a TerraformPlan waits for its approval: False/AwaitingApproval;
//   - the current inputs could not be built, so they cannot be compared
//     with the applied ones: Unknown/InputsUnavailable;
//   - the last apply failed and none succeeded since: False/InputsApplyFailed;
//   - nothing was applied, or the current inputs hash differs from the
//     state's: False/ApplyPending (no Job yet, gated, or backing off);
//   - otherwise True/InputsApplied.
//
// An immutable kind builds no current hash after provisioning: what its
// state recorded is the inputs it was applied with.
func (r *reconciler) inputsApplied(bk *Bookkeeping) *metav1.Condition {
	if r.deleting || r.isPaused {
		return nil
	}
	cond := func(status metav1.ConditionStatus, reason, msg string) *metav1.Condition {
		return &metav1.Condition{Type: infrav1.InputsAppliedCondition, Status: status, Reason: reason, Message: msg}
	}
	if a := r.st.ActiveJob; a.Name != "" && a.Operation == infrav1.OperationApply {
		return cond(metav1.ConditionFalse, infrav1.ApplyRunningReason, "Job "+a.Name)
	}
	if !r.facts.read {
		// No apply runs any more, but the state was not read (held: lost,
		// unreadable, unconfirmed or retained): ApplyRunning would name a
		// finished Job for as long as the hold lasts.
		if prev, ok := r.before[infrav1.InputsAppliedCondition]; ok && prev.Reason == infrav1.ApplyRunningReason {
			return cond(metav1.ConditionUnknown, infrav1.InputsUnavailableReason,
				"The state was not read ("+stateReadableReason(r.obj)+"), so whether the inputs are applied is unknown")
		}
		return nil
	}
	v := r.facts.view
	if r.planWait != nil {
		return cond(metav1.ConditionFalse, infrav1.AwaitingApprovalReason, r.planWait.Message)
	}
	applied := v.Exists && v.InputsHash != "" && (!r.k.Mutable() || (v.CurrentHash != "" && v.CurrentHash == v.InputsHash))
	if !applied && r.facts.gate != nil {
		return cond(metav1.ConditionUnknown, infrav1.InputsUnavailableReason, r.facts.gate.Reason+": "+r.facts.gate.Message)
	}
	if bk != nil && r.lastApplyFailed(bk) {
		return cond(metav1.ConditionFalse, infrav1.InputsApplyFailedReason, "The last apply failed; it is retried with backoff")
	}
	if !applied {
		return cond(metav1.ConditionFalse, infrav1.ApplyPendingReason, "The current inputs are not applied yet")
	}
	return cond(metav1.ConditionTrue, infrav1.InputsAppliedReason, "")
}
