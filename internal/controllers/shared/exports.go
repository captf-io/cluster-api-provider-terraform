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
	"fmt"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// recordExports records, using ctx and d, the cluster exports that f, a
// newly finished successful apply of k, rendered as k's applied exports
// (inputs.RecordClusterOutputs, which also drops a pending or partly
// applied change). Only a pool apply carries
// ClusterOutputsHashAnnotation; one that rendered held exports
// (HeldClusterOutputsAnnotation) records nothing: it applied none of the
// pending change. The value is read back from durable, the
// durable Secret as read this pass, whose tfvars are those of the apply
// that started last, and is recorded only when it hashes as f says, so a
// newer apply's exports are never taken for f's. A record already in
// place costs no call. It returns any error from recording.
func (bk *Bookkeeping) recordExports(ctx context.Context, d Deps, k Kind, f *finished, durable *inputs.Durable) error {
	want := f.job.Annotations[ClusterOutputsHashAnnotation]
	if want == "" || f.job.Annotations[HeldClusterOutputsAnnotation] == "true" || durable == nil {
		return nil
	}
	raw := inputs.LastClusterOutputs(durable)
	var buf bytes.Buffer
	if got, err := hash.Exports(raw); err != nil || got != want || json.Compact(&buf, raw) != nil {
		klog.FromContext(ctx).V(LogFlow).Info("Not recording the exports of an apply whose inputs the durable Secret no longer holds", "Job", klog.KObj(f.job))
		return nil
	}
	exports := buf.Bytes()
	if durable.Pending == nil && durable.Partial == nil && bytes.Equal(durable.AppliedClusterOutputs, exports) &&
		durable.Secret.Annotations[inputs.AppliedClusterOutputsHashAnnotation] == want {
		return nil
	}
	recorded, err := inputs.RecordClusterOutputs(ctx, d.Client, k.Object(), exports)
	switch {
	case errors.Is(err, inputs.ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	bk.ExportsRecorded, bk.AppliedExports, bk.AppliedExportsHash = true, nil, want
	if recorded {
		bk.AppliedExports = exports
	}
	klog.FromContext(ctx).V(LogFlow).Info("Recorded the cluster exports of the successful apply", "Job", klog.KObj(f.job), "exportsHash", want, "recorded", recorded)
	return nil
}

// recordPending records, using ctx and d, the change of the cluster's
// exports that f, k's newest apply, newly finished and blocked before a
// destructive plan, rendered (inputs.SetPending): until it is approved, k
// renders the exports of its last successful apply. Only a pool apply
// guarded for that change carries both hashes. A guarded apply of the
// exports durable, the durable Secret as read this pass, records as
// applied is no change of them (it is guarded because a change may be
// partly applied): it waits for its approval, and records nothing. The
// summary is the runner's (addresses and actions only), cut like
// status.lastRun's. It returns any error from recording.
func (bk *Bookkeeping) recordPending(ctx context.Context, d Deps, k Kind, f *finished, durable *inputs.Durable) error {
	p := inputs.Pending{
		ExportsHash: f.job.Annotations[ClusterOutputsHashAnnotation], ApprovalHash: f.job.Annotations[ApprovalHashAnnotation], Job: f.job.Name,
	}
	if p.ExportsHash == "" || p.ApprovalHash == "" || p.ExportsHash == appliedExportsHash(durable) {
		return nil
	}
	if f.result != nil && f.result.Error != nil {
		p.Summary = runSummary(f.result.Error.Tail)
	}
	err := inputs.SetPending(ctx, d.Client, k.Object(), p)
	switch {
	case errors.Is(err, inputs.ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	bk.PendingSet = &p
	klog.FromContext(ctx).Info("A change of the cluster's exports waits for approval of its destructive plan; the pool keeps applying with the exports of its last successful apply",
		"Job", klog.KObj(f.job), "approvalHash", p.ApprovalHash)
	return nil
}

// recordPartial records, using ctx and d, the change of the cluster's
// exports that f, k's newest apply, newly finished and failed for a
// reason other than a block, may have partly applied (inputs.SetPartial):
// a pool apply guarded for a change (ApprovalHashAnnotation, not held)
// whose apply step may have run (mayHaveApplied): it reached it, its plan
// having passed the guard, approved or not, or its runner started and
// left no result to tell. The state may then not match the exports of
// the last successful apply, and k stops holding those in place of a
// change. durable is the durable Secret as read this pass; a change
// already recorded there is kept, so the record names the first Job that
// may have left one. It returns any error from recording.
func (bk *Bookkeeping) recordPartial(ctx context.Context, d Deps, k Kind, f *finished, durable *inputs.Durable) error {
	p := inputs.Partial{ExportsHash: f.job.Annotations[ClusterOutputsHashAnnotation], Job: f.job.Name}
	if p.ExportsHash == "" || f.job.Annotations[ApprovalHashAnnotation] == "" || f.job.Annotations[HeldClusterOutputsAnnotation] == "true" ||
		(durable != nil && durable.Partial != nil) || !mayHaveApplied(f) {
		return nil
	}
	err := inputs.SetPartial(ctx, d.Client, k.Object(), p)
	switch {
	case errors.Is(err, inputs.ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	bk.PartialSet = &p
	klog.FromContext(ctx).Info("A guarded apply of a change of the cluster's exports failed and may have applied part of it; "+
		"until an apply succeeds, every apply of the pool is guarded and none falls back to the exports of its last successful apply", "Job", klog.KObj(f.job))
	return nil
}

// appliedExportsHash returns hash.Exports of the exports durable records
// as those of the last successful apply (inputs.Durable.AppliedExportsHash,
// kept when the record itself was dropped for size), or "" when it
// records none (or durable is nil).
func appliedExportsHash(durable *inputs.Durable) string {
	if durable == nil {
		return ""
	}
	return durable.AppliedExportsHash
}

// mayHaveApplied reports whether f, a failed apply, may have changed
// resources: its result lists the apply step, or there is no result to
// tell and its runner started (an interrupted or killed run) or its pod
// is gone. A pod whose runner never started (unschedulable until the
// deadline, or its image never pulled) changed nothing.
func mayHaveApplied(f *finished) bool {
	switch {
	case f.result != nil:
		return slices.ContainsFunc(f.result.Steps, func(s runner.Step) bool { return s.Name == runner.StepApply })
	case f.pod != nil:
		return runnerStarted(f.pod)
	}
	return true
}

// runnerStarted reports whether pod's runner container
// (jobs.SourceContainer) ever started: it runs, or ran, now or before a
// restart.
func runnerStarted(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == jobs.SourceContainer {
			return cs.State.Running != nil || cs.State.Terminated != nil ||
				cs.LastTerminationState.Running != nil || cs.LastTerminationState.Terminated != nil
		}
	}
	return false
}

// observeGuard reads, after BuildInputs built the inputs, how an
// ExportsGuard kind guards their apply into r.guard. Other kinds leave
// r.guard nil.
func (r *reconciler) observeGuard() {
	if eg, ok := r.k.(ExportsGuard); ok {
		g := eg.ExportsGuard()
		r.guard = &g
	}
}

// pending returns the change of the cluster's exports that waits for
// approval (inputs.Durable.Pending), or nil, also while this pass's guard
// says the exports are those of the last successful apply again
// (Guard.Settled): that withdraws the change, but its record stays, so
// exports that return to it hold it again instead of waiting on its
// blocked Job. A successful apply of the cluster's own exports removes the
// record (recordExports).
func (r *reconciler) pending() *inputs.Pending {
	if r.durable == nil || (r.guard != nil && r.guard.Settled) {
		return nil
	}
	return r.durable.Pending
}

// guardApprovalHash returns the hash an approval of the apply about to be
// decided must name when the kind's guard says it is guarded
// (Guard.Guarded), else "": the caller then uses the inputs hash.
func (r *reconciler) guardApprovalHash() string {
	if r.guard == nil || !r.guard.Guarded {
		return ""
	}
	return r.guard.ApprovalHash
}

// guardRequest fills req, an apply of an ExportsGuard kind, from r.guard:
// the exports hash it renders, whether they are held, and the approval
// hash that guards it when it renders a change of them.
func (r *reconciler) guardRequest(req *JobRequest) {
	g := r.guard
	req.ExportsHash, req.HeldExports = g.ExportsHash, g.Held
	if g.Guarded {
		req.ApprovalHash = g.ApprovalHash
	}
}

// annotateExports records on job, a pool apply started for req, the
// exports hash it renders, whether they are held, and the approval hash
// that guards it, for bookkeeping to read back once it finished.
func annotateExports(job *batchv1.Job, req JobRequest) {
	if req.Op != jobs.OpApply || req.ExportsHash == "" {
		return
	}
	metav1.SetMetaDataAnnotation(&job.ObjectMeta, ClusterOutputsHashAnnotation, req.ExportsHash)
	if req.HeldExports {
		metav1.SetMetaDataAnnotation(&job.ObjectMeta, HeldClusterOutputsAnnotation, "true")
	}
	if req.ApprovalHash != "" {
		metav1.SetMetaDataAnnotation(&job.ObjectMeta, ApprovalHashAnnotation, req.ApprovalHash)
	}
}

// applyJobCondition returns the ApplyJobSucceeded condition bk implies,
// or, while an apply Job that disappeared keeps an apply due and bk's
// newest apply predates it (interruptedCondition), that the Job
// disappeared and an apply is due; or, for a pool whose newest apply was
// blocked while it cannot hold the
// exports of its last successful apply (unheldCondition), why it waits;
// or, once such a blocked change is withdrawn (withdrawnCondition), or a
// cluster's blocked apply is no longer due (liftedCondition), that
// the last successful apply stands; or, while a change of the cluster's
// exports waits for approval (pending), heldCondition in place of a
// success or of the blocked Job's own report: the held applies'
// successes must not hide the pending action. Another failure shows
// through. The condition carries the approval hash of the current inputs
// (r.guard) when this pass built them. A pass that built none (a gate,
// or a deletion) cannot tell whether the change still waits or was
// withdrawn: it keeps the held condition already reported for the same
// Job, so its hash does not flip, and otherwise reports bk's own
// condition, unless that is the blocked Job's report of the change or a
// held apply's success not reported yet (newHeldSuccess); a
// withdrawn change is not held again until a pass that built the inputs
// says so. A deleting pool applies nothing, so its held condition says
// that instead (deletingCondition).
func (r *reconciler) applyJobCondition(bk *Bookkeeping) metav1.Condition {
	if c, ok := r.interruptedCondition(bk); ok {
		return c
	}
	if c, ok := r.unheldCondition(bk); ok {
		return c
	}
	if c, ok := r.withdrawnCondition(bk); ok {
		return c
	}
	c, p := bk.ApplyJob, r.pending()
	if p == nil || (c.Reason != infrav1.ApplySucceededReason && c.Reason != infrav1.DestructivePlanBlockedReason) {
		return c
	}
	prev := conditions.Get(r.obj, infrav1.ApplyJobSucceededCondition)
	reported := prev != nil && prev.Reason == infrav1.DestructivePlanBlockedReason && namesJob(prev.Message, p.Job)
	own := c.Reason == infrav1.DestructivePlanBlockedReason && namesJob(c.Message, p.Job)
	approval := p.ApprovalHash
	switch {
	case r.deleting && (reported || own):
		return deletingCondition(p)
	case r.guard != nil && r.guard.Held:
		approval = r.guard.ApprovalHash
	case reported && (r.guard == nil || !strings.Contains(prev.Message, unheldWait)):
		// A wait reported while the pool could not hold is over once
		// this pass's guard says so (unheldCondition did not apply).
		return *prev
	case r.guard == nil && !own && !newHeldSuccess(bk, prev):
		return c
	}
	return heldCondition(p, r.approveHint(approval))
}

// newHeldSuccess reports whether bk's newest apply is a held apply
// (HeldClusterOutputsAnnotation) that succeeded, and prev, the
// ApplyJobSucceeded condition already set, does not report that success.
// A held apply rendered the exports of the last successful apply in place
// of a pending change, so that change still waited when it started, and
// its success must not hide it. A pass that built the inputs reports a
// held apply's success only once the change is withdrawn (Guard.Settled):
// a gated pass then keeps reporting it.
func newHeldSuccess(bk *Bookkeeping, prev *metav1.Condition) bool {
	last := bk.LastApply
	if last == nil || !bk.LastApplySucceeded || last.Annotations[HeldClusterOutputsAnnotation] != "true" {
		return false
	}
	return prev == nil || prev.Reason != infrav1.ApplySucceededReason || !namesJob(prev.Message, last.Name)
}

// deletingCondition is ApplyJobSucceeded while p, a change of the
// cluster's exports, waited for approval of its destructive plan when
// the pool's deletion began: what the plan would delete or replace, and
// that no apply runs any more, so no approve command applies. It returns
// the condition to set.
func deletingCondition(p *inputs.Pending) metav1.Condition {
	summary := p.Summary
	if summary == "" {
		summary = "the plan deletes or replaces resources (the Job's log lists them)"
	}
	return metav1.Condition{
		Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionFalse, Reason: infrav1.DestructivePlanBlockedReason,
		Message: fmt.Sprintf("Job %s: %s. The plan is for a change of the cluster's exports (captf_cluster_outputs), and nothing of it was applied. "+
			"The pool is being deleted, so no apply runs.", p.Job, summary),
	}
}

// unheldWait ends the condition of a pool's blocked apply whose exports
// it cannot hold: what it waits for instead.
const unheldWait = "no apply runs until this one is approved, or an input other than bootstrap_data changes."

// unheldCondition returns ApplyJobSucceeded, and true, when bk's newest
// apply is a pool apply guarded for its exports (ApprovalHashAnnotation)
// that was blocked, the exports are still the ones it rendered, and this
// pass's guard says the pool cannot hold the
// exports of its last successful apply in place of them (Guard.Partial,
// Guard.Unrecorded): the pool then waits for the approval like a
// cluster, and the condition must say so, not that it keeps applying. It
// says what the plan is for, by the recorded hash of the applied exports
// (unheldWhy). A condition already reported so for that Job is kept: it
// does not change while the Job is the newest. It returns false when the
// condition is not this one.
func (r *reconciler) unheldCondition(bk *Bookkeeping) (metav1.Condition, bool) {
	g, last := r.guard, bk.LastApply
	if g == nil || !g.Guarded || (!g.Partial && !g.Unrecorded) || !bk.LastApplyBlocked || last == nil ||
		last.Annotations[ApprovalHashAnnotation] == "" || last.Annotations[ClusterOutputsHashAnnotation] != g.ExportsHash ||
		bk.ApplyJob.Reason != infrav1.DestructivePlanBlockedReason {
		return metav1.Condition{}, false
	}
	if prev := conditions.Get(r.obj, infrav1.ApplyJobSucceededCondition); prev != nil && prev.Reason == infrav1.DestructivePlanBlockedReason &&
		namesJob(prev.Message, last.Name) && strings.Contains(prev.Message, unheldWait) {
		return *prev, true
	}
	why := unheldWhy(g, appliedExportsHash(r.durable))
	return poolBlockedCondition(last.Name, r.blockedSummary(bk, last.Name), why+unheldWait, r.approveHint(g.ApprovalHash)), true
}

// unheldWhy returns what the plan of a pool's blocked apply that cannot
// hold the exports of its last successful apply is for, and why it waits,
// from g, the guard of that apply, and applied, the recorded hash of
// those exports ("" when none is recorded): a change of the cluster's
// exports, or, after a change was partly applied (Guard.Partial), the
// exports of the last successful apply themselves (a rotation, a version
// roll, a spec edit or a revert), or, with nothing recorded, perhaps a
// change of them; then why it cannot hold: a change partly applied, the
// exports unknown (Guard.Unknown: the pool applied before this version
// recorded them), or not recorded for size. It returns the sentences that
// end before unheldWait.
func unheldWhy(g *Guard, applied string) string {
	const exports = "the cluster's exports (captf_cluster_outputs)"
	settled := applied != "" && applied == g.ExportsHash
	var why string
	switch {
	case settled:
		why = "The plan is not for a change of " + exports + ", which are those of the last successful apply, and nothing of it was applied. "
	case applied == "":
		why = "The plan may be for a change of " + exports + ", and nothing of it was applied. "
	default:
		why = "The plan is for a change of " + exports + ", and nothing of it was applied. "
	}
	if g.Partial {
		earlier := "An earlier apply of a change of them"
		if g.PartialJob != "" {
			earlier = "Job " + g.PartialJob + ", an earlier apply of a change of them,"
		}
		why += earlier + " failed and may have applied part of it, so every apply of the pool is guarded until one succeeds"
		if !settled {
			why += ", and the pool cannot fall back to the exports of its last successful apply"
		}
		return why + ": "
	}
	if g.Unknown {
		return why + "The exports of the pool's last successful apply are unknown (it applied before this version recorded them), " +
			"so the pool cannot keep applying with them: "
	}
	unrecorded := "The exports of the last successful apply are not recorded"
	if applied != "" {
		unrecorded += ", only their hash"
	}
	return why + unrecorded + " (with the rendered inputs they do not fit in the durable inputs Secret), so the pool cannot keep applying with them: "
}

// withdrawnCondition returns ApplyJobSucceeded, and true, when bk's
// newest apply is a pool apply blocked on a change of the cluster's
// exports (ApprovalHashAnnotation) that this pass's guard says is
// withdrawn: the exports are those of the last successful apply again
// (Guard.Settled), and the pool does not guard them (no change is partly
// applied). The block's approve command no longer applies, so the
// condition reports what stands instead: the last successful apply, True,
// unless the newest apply before the blocked Job that may have changed
// anything (Bookkeeping.priorApply) failed; then that failure, as it was
// reported: the current inputs did not apply. An apply of them is due,
// for inputs that differ from the state's, or for the blocked Job having
// started after that failure (AfterFailedApplyAnnotation keeps
// LastApplyFailed past it); a failed drift remediation is retried only
// as drift remediation is. A
// condition already reported so is kept, and a pass that built no inputs
// keeps it while the Job is the newest. It returns false when the
// condition is not this one. A TerraformCluster's blocked apply is
// liftedCondition's.
func (r *reconciler) withdrawnCondition(bk *Bookkeeping) (metav1.Condition, bool) {
	last := bk.LastApply
	if !bk.LastApplyBlocked || last == nil || bk.ApplyJob.Reason != infrav1.DestructivePlanBlockedReason {
		return metav1.Condition{}, false
	}
	if r.k.Kind() == state.KindTerraformCluster {
		return r.liftedCondition(bk, last)
	}
	if last.Annotations[ApprovalHashAnnotation] == "" {
		return metav1.Condition{}, false
	}
	c, reported := r.standingCondition(bk, fmt.Sprintf("The change of the cluster's exports (captf_cluster_outputs) that Job %s stopped before was withdrawn: "+
		"the exports are those of the last successful apply again, which stands, and nothing of the change was applied", last.Name))
	if !reported && r.guard == nil {
		return metav1.Condition{}, false
	}
	g := r.guard
	return c, g == nil || (g.Settled && !g.Guarded && last.Annotations[ClusterOutputsHashAnnotation] != g.ExportsHash)
}

// standingCondition returns what ApplyJobSucceeded reports once the block
// of bk's newest apply is withdrawn: the last successful apply, True with
// msg, unless the newest apply before the blocked Job that may have
// changed anything (Bookkeeping.priorApply) failed; then that failure, as
// it was reported. It also reports whether the object's condition already
// says so, in which case that condition is returned: a bookkept Job's
// result is gone, and the failure as first reported says more than it can
// be told again.
func (r *reconciler) standingCondition(bk *Bookkeeping, msg string) (metav1.Condition, bool) {
	c := metav1.Condition{Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionTrue, Reason: infrav1.ApplySucceededReason, Message: msg}
	if p := bk.priorApply; p != nil && !p.ok {
		c = applyDestroyCondition(*p, r.obj)
	}
	prev, name := conditions.Get(r.obj, infrav1.ApplyJobSucceededCondition), jobNamed(c.Message)
	if prev != nil && prev.Status == c.Status && (prev.Message == c.Message || (name != "" && namesJob(prev.Message, name))) {
		return *prev, true
	}
	return c, false
}

// liftedPrefix returns how the ApplyJobSucceeded message of a
// TerraformCluster starts once its apply job, blocked before a
// destructive plan, is no longer due (liftedCondition).
func liftedPrefix(job string) string {
	return "Job " + job + " stopped before a plan that deletes or replaces resources, and nothing of it was applied; that apply is no longer due: "
}

// liftedCondition returns ApplyJobSucceeded, and true, when last, bk's
// newest apply, is a TerraformCluster apply blocked before a destructive
// plan whose apply is no longer due (blockLifted): the inputs it planned
// are no longer the current ones, or the drift remediation it was is no
// longer wanted. The block's approve command then no longer applies, and
// DecideOp waits for nothing, so the condition reports what stands
// instead (standingCondition): the last successful apply, or the failure
// of the apply before the block. A TerraformMachine's applies are never
// guarded, so they are never blocked. A pass that cannot tell (no inputs
// built, and no drift remediation lifted) keeps a withdrawal already
// reported while the Job is the newest. It returns false when the
// condition is not this one.
func (r *reconciler) liftedCondition(bk *Bookkeeping, last *batchv1.Job) (metav1.Condition, bool) {
	why, known := r.blockLifted(bk, last)
	if known && why == "" {
		return metav1.Condition{}, false
	}
	if known {
		c, _ := r.standingCondition(bk, liftedPrefix(last.Name)+why+". The last successful apply stands")
		return c, true
	}
	prev := conditions.Get(r.obj, infrav1.ApplyJobSucceededCondition)
	if prev == nil || prev.Reason == infrav1.DestructivePlanBlockedReason {
		return metav1.Condition{}, false
	}
	if prev.Reason == infrav1.ApplySucceededReason && strings.HasPrefix(prev.Message, liftedPrefix(last.Name)) {
		return *prev, true
	}
	// The failure before the block, reported once the block was lifted: a
	// pass that first reads the blocked Job reports the block instead.
	if f, ok := bk.finishedJob(last.Name); ok && f.bookkept {
		if p := bk.priorApply; p != nil && !p.ok && prev.Status == metav1.ConditionFalse && namesJob(prev.Message, p.job.Name) {
			return *prev, true
		}
	}
	return metav1.Condition{}, false
}

// blockLifted reports why the apply of last, bk's newest apply, blocked
// before a destructive plan, is no longer due, "" when it still is, and
// whether this pass can tell. It is no longer due once the current inputs
// (Bookkeeping.CurrentHash) hash otherwise than the ones it planned (an
// apply of those is due instead, or none when they are the state's), or,
// for a drift remediation, once the drift is no longer detected or the
// drift action is no longer Remediate, while no failed apply keeps an
// apply of the same inputs due (lastApplyFailed). A deleting object
// applies nothing; whether the block still stands is left to the
// condition already reported.
func (r *reconciler) blockLifted(bk *Bookkeeping, last *batchv1.Job) (string, bool) {
	if r.deleting {
		return "", false
	}
	if bk.CurrentHash != "" && bk.CurrentHash != last.Annotations[state.InputsHashAnnotation] {
		return "the inputs it planned are no longer the current ones", true
	}
	if last.Annotations[RemediationAnnotation] == "true" && !r.lastApplyFailed(bk) {
		switch {
		case r.eff.DriftAction != infrav1.DriftActionRemediate:
			return "it remediated drift, and the drift action is no longer " + string(infrav1.DriftActionRemediate), true
		case !conditions.IsTrue(r.obj, infrav1.DriftDetectedCondition):
			return "it remediated drift, which is no longer detected", true
		}
	}
	return "", bk.CurrentHash != ""
}

// blockedSummary returns what the plan of name, a blocked apply, would
// delete or replace: the pending change's summary when it is that Job's,
// else the runner's when bk read its result this pass, else "".
func (r *reconciler) blockedSummary(bk *Bookkeeping, name string) string {
	if p := r.pending(); p != nil && p.Job == name && p.Summary != "" {
		return p.Summary
	}
	if f, ok := bk.finishedJob(name); ok && f.result != nil && f.result.Error != nil {
		return runSummary(f.result.Error.Tail)
	}
	return ""
}

// heldCondition is ApplyJobSucceeded while p, a change of the cluster's
// exports, waits for approval of its destructive plan: what the plan
// would delete or replace, that the pool keeps applying with the exports
// of its last successful apply, and hint, how to approve the change's
// TerraformPlan (approveHint). It returns the condition to set.
func heldCondition(p *inputs.Pending, hint string) metav1.Condition {
	return poolBlockedCondition(p.Job, p.Summary, "The plan is for a change of the cluster's exports (captf_cluster_outputs), and nothing of it was applied. "+
		"Until it is approved, the pool keeps applying with the exports of its last successful apply.", hint)
}

// poolBlockedCondition is ApplyJobSucceeded for job, a pool apply blocked
// before a destructive plan: summary, what the plan would delete or
// replace ("" when unknown), why, what the pool does meanwhile, and hint,
// how to approve its TerraformPlan. It returns the condition to set.
func poolBlockedCondition(job, summary, why, hint string) metav1.Condition {
	if summary == "" {
		summary = "the plan deletes or replaces resources (the Job's log lists them)"
	}
	return metav1.Condition{
		Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionFalse, Reason: infrav1.DestructivePlanBlockedReason,
		Message: fmt.Sprintf("Job %s: %s. %s To apply it, %s", job, summary, why, hint),
	}
}
