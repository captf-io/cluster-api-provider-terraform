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
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	captfconds "github.com/captf-io/cluster-api-provider-terraform/internal/conditions"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
	"github.com/captf-io/cluster-api-provider-terraform/internal/strutil"
)

// A plan that waits for an approval is a TerraformPlan: the controller
// creates one from the plan a finished Job reported (recordPlans), reads
// the target's plans back every pass (loadPlans), moves them through their
// phases (syncPlans) and lets DecideOp see the live one (planView). The
// captf.io/plan-phase label holds a finished plan's phase: unlike status,
// it survives clusterctl move. A live plan's phase follows spec.approved.

// PlanTargetIndex indexes TerraformPlans by their target,
// "<kind>/<name>" (PlanTargetIndexer), so a reconcile lists only its own
// object's plans.
const PlanTargetIndex = "captf.planTarget"

// PlanTargetIndexer returns o, a TerraformPlan's, target key.
func PlanTargetIndexer(o client.Object) []string {
	p, ok := o.(*infrav1.TerraformPlan)
	if !ok || p.Spec.TargetRef.Name == "" {
		return nil
	}
	return []string{planTargetKey(string(p.Spec.TargetRef.Kind), p.Spec.TargetRef.Name)}
}

// planTargetKey returns the PlanTargetIndex key of the object kind/name.
func planTargetKey(kind, name string) string {
	return kind + "/" + name
}

// maxPlanEntry is the API's limit on one spec.summary.resources entry.
const maxPlanEntry = 600

// planNameHex is how many hex digits of the plan's identity end its name.
const planNameHex = 10

// planName returns the name of the TerraformPlan that job, a Job of the
// object target, made with the plan of hash planHash:
// "<target>-<planNameHex hex digits>". It is deterministic per Job, so a
// pass that reads the Job again finds the plan it created, and a plan the
// same changes produce again later (a recurring drift) gets an object of
// its own.
func planName(target, job, planHash string) string {
	sum := sha256.Sum256([]byte(job + "\n" + planHash))
	const maxTarget = 253 - 1 - planNameHex
	if len(target) > maxTarget {
		target = target[:maxTarget]
	}
	return target + "-" + hex.EncodeToString(sum[:])[:planNameHex]
}

// planApproveCommand returns the command that approves the TerraformPlan
// name in namespace ns, filling approvedBy with the caller's username as
// the admission webhook requires.
func planApproveCommand(name, ns string) string {
	return fmt.Sprintf(`kubectl patch terraformplan %s -n %s --type merge -p '{"spec":{"approved":true,"approvedBy":"'"$(kubectl auth whoami -o jsonpath='{.status.userInfo.username}')"'"}}'`,
		name, ns)
}

// approved reports whether p is approved (spec.approved).
func approved(p *infrav1.TerraformPlan) bool {
	return p.Spec.Approved != nil && *p.Spec.Approved
}

// phaseOf returns p's phase: the terminal phase its captf.io/plan-phase
// label records, else Approved or Pending from spec.approved.
func phaseOf(p *infrav1.TerraformPlan) infrav1.PlanPhase {
	if ph := infrav1.PlanPhase(p.Labels[infrav1.PlanPhaseLabel]); ph.Terminal() {
		return ph
	}
	if approved(p) {
		return infrav1.PlanPhaseApproved
	}
	return infrav1.PlanPhasePending
}

// summaryOf returns the spec.summary of rp, a plan a Job reported:
// the counts, and the changed resources cut to the API's limits.
func summaryOf(rp *runner.Plan) infrav1.PlanSummary {
	// #nosec G115 -- plan resource counts, far below MaxInt32
	s := infrav1.PlanSummary{
		Create: new(int32(rp.Create)), Update: new(int32(rp.Update)), Replace: new(int32(rp.Replace)), Delete: new(int32(rp.Delete)),
	}
	for _, c := range []struct {
		dst **int32
		n   int
	}{{&s.Import, rp.Import}, {&s.Move, rp.Move}, {&s.Forget, rp.Forget}, {&s.OutputChanges, rp.OutputChanges}} {
		if c.n > 0 {
			*c.dst = new(int32(c.n)) // #nosec G115 -- a plan count, far below MaxInt32
		}
	}
	truncated := rp.Truncated
	for i, r := range rp.Resources {
		if i == infrav1.MaxPlanSummaryResources {
			truncated = true
			break
		}
		if r = strutil.Truncate(r, maxPlanEntry); r != "" {
			s.Resources = append(s.Resources, r)
		}
	}
	if truncated {
		s.Truncated = new(true)
	}
	return s
}

// planCounts returns s formatted as "N to create, M to update, K to
// replace, L to delete", followed by the import, move and forget counts and
// ", J output(s) to change" for those the plan has, so an output-only plan
// does not read as no changes.
func planCounts(s infrav1.PlanSummary) string {
	n := func(v *int32) int32 {
		if v == nil {
			return 0
		}
		return *v
	}
	out := fmt.Sprintf("%d to create, %d to update, %d to replace, %d to delete", n(s.Create), n(s.Update), n(s.Replace), n(s.Delete))
	for _, c := range []struct {
		n    *int32
		verb string
	}{{s.Import, "import"}, {s.Move, "move"}, {s.Forget, "forget"}} {
		if v := n(c.n); v > 0 {
			out += fmt.Sprintf(", %d to %s", v, c.verb)
		}
	}
	if o := n(s.OutputChanges); o > 0 {
		out += fmt.Sprintf(", %d output(s) to change", o)
	}
	return out
}

// destructive reports whether s replaces or deletes a resource.
func destructive(s infrav1.PlanSummary) bool {
	return (s.Replace != nil && *s.Replace > 0) || (s.Delete != nil && *s.Delete > 0)
}

// madePlanHash returns the hash of the plan f, a finished Job, made: its
// result's, else the PlanHashAnnotation bookkeeping recorded on it; ""
// when it made none.
func madePlanHash(f *finished) string {
	if f.result != nil && f.result.Plan != nil {
		return f.result.Plan.Hash
	}
	return f.job.Annotations[PlanHashAnnotation]
}

// emptyPlan returns the inputs hash the newest plan of done (newest
// first) planned without any change, when no apply finished after it: a
// plan Job that succeeded, or an approved apply that found the plan
// changed. It returns "" otherwise. That plan needs no approval, so it
// never becomes a TerraformPlan.
func emptyPlan(done []finished) string {
	for i := range done {
		f := &done[i]
		op := jobs.OpOf(f.job)
		if op != jobs.OpPlan && op != jobs.OpApply {
			continue
		}
		if ((op == jobs.OpPlan && f.ok) || f.planChanged) && madePlanHash(f) == runner.EmptyPlanHash {
			return f.job.Annotations[state.InputsHashAnnotation]
		}
		return ""
	}
	return ""
}

// tracksPlans reports whether the reconciled kind keeps TerraformPlans: its
// status has a pendingPlanRef.
func (r *reconciler) tracksPlans() bool {
	return r.st.PendingPlanRef != nil
}

// manualApply reports whether every apply of the object but the first
// waits for the approval of its plan: a TerraformCluster with applyPolicy
// Manual.
func (r *reconciler) manualApply() bool {
	return r.k.Kind() == state.KindTerraformCluster && r.eff.ApplyPolicy == infrav1.ApplyPolicyManual
}

// loadPlans lists, using ctx, the object's TerraformPlans into r.plans,
// newest first: the plans the PlanTargetIndex names it as target of, whose
// controller owner is this object (not an earlier one of the same name). A
// kind that keeps no plans lists nothing. It returns any list error.
func (r *reconciler) loadPlans(ctx context.Context) error {
	if !r.tracksPlans() {
		return nil
	}
	list := &infrav1.TerraformPlanList{}
	if err := r.d.Client.List(ctx, list, client.InNamespace(r.obj.GetNamespace()),
		client.MatchingFields{PlanTargetIndex: planTargetKey(r.k.Kind(), r.obj.GetName())}); err != nil {
		return fmt.Errorf("list TerraformPlans: %w", err)
	}
	r.plans = r.plans[:0]
	for i := range list.Items {
		if ref := metav1.GetControllerOf(&list.Items[i]); ref != nil && ref.UID == r.obj.GetUID() {
			r.plans = append(r.plans, list.Items[i])
		}
	}
	slices.SortFunc(r.plans, newestPlanFirst)
	return nil
}

// newestPlanFirst compares a and b for a sort, newest first: it returns a
// negative number when a was created later than b (by name for the same
// second), a positive one when earlier, and 0 for the same plan.
func newestPlanFirst(a, b infrav1.TerraformPlan) int {
	return cmp.Or(b.CreationTimestamp.Compare(a.CreationTimestamp.Time), cmp.Compare(b.Name, a.Name))
}

// livePlan returns the object's live plan (Pending or Approved), the
// newest when, unexpectedly, there are several; nil when none.
func (r *reconciler) livePlan() *infrav1.TerraformPlan {
	for i := range r.plans {
		if !phaseOf(&r.plans[i]).Terminal() {
			return &r.plans[i]
		}
	}
	return nil
}

// planNamed returns the object's plan name; nil when it has none such.
func (r *reconciler) planNamed(name string) *infrav1.TerraformPlan {
	for i := range r.plans {
		if r.plans[i].Name == name {
			return &r.plans[i]
		}
	}
	return nil
}

// planView returns the live plan for DecideOp; nil when none.
func (r *reconciler) planView() *PlanView {
	p := r.livePlan()
	if p == nil {
		return nil
	}
	return &PlanView{Name: p.Name, Reason: p.Spec.Reason, InputsHash: p.Spec.InputsHash, PlanHash: p.Spec.PlanHash, Approved: approved(p)}
}

// waitingExports returns the approval hash the object's live
// ExportsChange plan was made for while it waits for approval, "" when no
// such plan waits.
func (r *reconciler) waitingExports() string {
	if p := r.livePlan(); p != nil && p.Spec.Reason == infrav1.PlanReasonExportsChange && !approved(p) {
		return p.Spec.InputsHash
	}
	return ""
}

// maxFinishedPlans is how many finished (terminal) TerraformPlans of a
// target prunePlans keeps.
const maxFinishedPlans = 10

// supersedeStale supersedes, using ctx, the object's live Manual or
// Destructive plan when dec, the pass's decision, is not about it: its
// apply is no longer due (the drift it remediated is gone, the change was
// reverted), the inputs changed since it was planned (a plan Job of the
// new ones is due instead), or applyPolicy changed (a Manual plan under
// Automatic, a Destructive one under Manual). gate is the pass's
// dependency gate and view the state as read this pass: a gated pass, or
// one that built no inputs, cannot tell, and neither can a restore. The
// caller runs it only when no Job runs and the object is neither paused
// nor deleting. A pool's ExportsChange plan is supersedeWithdrawn's. It
// returns any write error.
func (r *reconciler) supersedeStale(ctx context.Context, dec Decision, gate *Gate, view StateView) error {
	p := r.livePlan()
	if p == nil || gate != nil || view.CurrentHash == "" || dec.Op == jobs.OpRestore || p.Spec.Reason == infrav1.PlanReasonExportsChange || dec.Plan == p.Name {
		return nil
	}
	manual := p.Spec.Reason == infrav1.PlanReasonManual
	var why string
	switch {
	case manual && !r.manualApply():
		why = "applyPolicy is no longer Manual"
	case !manual && r.manualApply():
		why = "applyPolicy is Manual now: a plan Job plans the change for approval instead"
	case p.Spec.InputsHash != view.CurrentHash:
		why = fmt.Sprintf("the inputs changed since it was planned (inputs hash %s, the plan's %s)", view.CurrentHash, p.Spec.InputsHash)
	default:
		why = "no apply of its inputs is due any more"
	}
	return r.supersede(ctx, p, why, "")
}

// prunePlans deletes, using ctx, the object's finished (terminal) plans
// beyond the maxFinishedPlans newest; a live plan is never deleted. The
// delete is conditional on each plan's UID, so a plan created again under
// the same name meanwhile stays. The caller runs it only when the object
// is neither paused nor deleting: clusterctl move copies the plans then,
// and garbage collection removes them with a deleted object. It returns
// any delete error.
func (r *reconciler) prunePlans(ctx context.Context) error {
	kept := 0
	var gone []string
	for i := range r.plans {
		p := &r.plans[i]
		if !phaseOf(p).Terminal() {
			continue
		}
		if kept < maxFinishedPlans {
			kept++
			continue
		}
		uid := p.UID
		err := r.d.Client.Delete(ctx, p, client.Preconditions{UID: &uid})
		if err = client.IgnoreNotFound(err); err != nil {
			return fmt.Errorf("prune TerraformPlan %s: %w", p.Name, err)
		}
		gone = append(gone, p.Name)
	}
	if len(gone) == 0 {
		return nil
	}
	klog.FromContext(ctx).V(LogFlow).Info("Pruned finished TerraformPlans", "kept", maxFinishedPlans, "deleted", gone)
	r.plans = slices.DeleteFunc(r.plans, func(p infrav1.TerraformPlan) bool { return slices.Contains(gone, p.Name) })
	return nil
}

// supersedeWithdrawn supersedes, using ctx, the object's live
// ExportsChange plan once this pass's guard (r.guard, of the inputs just
// built) neither holds nor guards the change it was made for: the
// cluster's exports are those of the last successful apply again
// (withdrawn), or another change of them, or an edit, moved the approval
// hash off it (superseded). A plan left live would approve the change
// again, with no new look, should the exports return to it. The build
// that calls it runs only when no Job runs, the object is neither paused
// nor deleting, and nothing gates the inputs. It returns any write error.
func (r *reconciler) supersedeWithdrawn(ctx context.Context) error {
	p, g := r.livePlan(), r.guard
	if p == nil || g == nil || p.Spec.Reason != infrav1.PlanReasonExportsChange || ((g.Held || g.Guarded) && g.ApprovalHash == p.Spec.InputsHash) {
		return nil
	}
	why := "the change of the cluster's exports it was made for was withdrawn: the exports are those of the last successful apply again"
	if !g.Settled {
		why = "the pool's inputs moved on: another change of the cluster's exports, or an edit, changed the approval hash it was made for"
	}
	return r.supersede(ctx, p, why, "")
}

// approveHint returns how to approve the plan made for inputsHash: "approve
// TerraformPlan <name>: <command>" for the object's live plan of that hash,
// else where to find it once it exists.
func (r *reconciler) approveHint(inputsHash string) string {
	if p := r.livePlan(); p != nil && p.Spec.InputsHash == inputsHash {
		return fmt.Sprintf("approve TerraformPlan %s: %s", p.Name, planApproveCommand(p.Name, p.Namespace))
	}
	return fmt.Sprintf("approve its TerraformPlan (kubectl get terraformplans -n %s -l %s=%s)",
		r.obj.GetNamespace(), infrav1.PlanPhaseLabel, infrav1.PlanPhasePending)
}

// blockedWaitCondition is ApplyJobSucceeded while an apply waits for p, a
// live Destructive or ExportsChange TerraformPlan, and no blocked Job
// reports it (after clusterctl move, which moves plans and not Jobs): what
// the plan would change, and the command that approves it.
func blockedWaitCondition(p *infrav1.TerraformPlan) metav1.Condition {
	return metav1.Condition{
		Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionFalse, Reason: infrav1.DestructivePlanBlockedReason,
		Message: fmt.Sprintf("TerraformPlan %s plans inputs hash %s: %s. Nothing is applied until it is approved: %s",
			p.Name, p.Spec.InputsHash, planCounts(p.Spec.Summary), planApproveCommand(p.Name, p.Namespace)),
	}
}

// setPendingPlanRef points status.pendingPlanRef at the live plan, or
// clears it when none is live.
func (r *reconciler) setPendingPlanRef() {
	if !r.tracksPlans() {
		return
	}
	*r.st.PendingPlanRef = infrav1.PlanReference{}
	if p := r.livePlan(); p != nil {
		r.st.PendingPlanRef.Name = p.Name
	}
}

// madePlan is what a finished Job's plan becomes.
type madePlan struct {
	reason     infrav1.PlanReason
	inputsHash string
}

// planOf returns what the plan f, a finished Job read this pass, reported
// becomes, and whether it becomes a TerraformPlan at all: the plan of a
// plan Job that changes anything (Manual), or the new plan of an apply that
// found the approved plan changed, when it applied a Manual plan, or the
// empty plan, of a TerraformCluster with applyPolicy Manual (Manual); the
// plan of an apply the runner blocked before deleting or replacing
// resources, made for its inputs hash (Destructive), or, for a pool apply
// guarded for a change of the cluster's exports, for its approval hash
// (ExportsChange).
func (r *reconciler) planOf(f *finished) (madePlan, bool) {
	if f.result == nil || f.result.Plan == nil || f.result.Plan.Hash == "" || f.result.Plan.Hash == runner.EmptyPlanHash {
		return madePlan{}, false
	}
	inputsHash := f.job.Annotations[state.InputsHashAnnotation]
	switch op := jobs.OpOf(f.job); {
	case op == jobs.OpPlan && f.ok:
		return madePlan{reason: infrav1.PlanReasonManual, inputsHash: inputsHash}, true
	case op == jobs.OpApply && f.planChanged:
		src := r.planNamed(f.job.Annotations[PlanAnnotation])
		if (src != nil && src.Spec.Reason == infrav1.PlanReasonManual) || (src == nil && r.manualApply()) {
			return madePlan{reason: infrav1.PlanReasonManual, inputsHash: inputsHash}, true
		}
	case op == jobs.OpApply && f.blocked:
		if a := f.job.Annotations[ApprovalHashAnnotation]; a != "" {
			return madePlan{reason: infrav1.PlanReasonExportsChange, inputsHash: a}, true
		}
		return madePlan{reason: infrav1.PlanReasonDestructive, inputsHash: inputsHash}, true
	}
	return madePlan{}, false
}

// recordPlans creates, using ctx, a TerraformPlan for every plan a Job
// that bk read this pass, oldest first, reported and that waits for an
// approval (planOf), emitting PlanReady for each one created, and the
// events of the plans that create none. A new plan supersedes the live
// plan it replaces: a target has at most one. A plan that already exists
// (a Job read again after the bookkept mark was lost) counts as created. A
// failed create keeps the pass's Jobs from being marked bookkept, so the
// next pass reads them, and their plans, again. It returns any create or
// supersede error.
func (r *reconciler) recordPlans(ctx context.Context, bk *Bookkeeping) error {
	if !r.tracksPlans() {
		return nil
	}
	fresh := slices.Clone(bk.unmarked)
	slices.SortFunc(fresh, func(a, b finished) int {
		return cmp.Or(jobs.FinishedAt(a.job).Compare(jobs.FinishedAt(b.job)), cmp.Compare(a.job.Name, b.job.Name))
	})
	for i := range fresh {
		f := &fresh[i]
		mp, ok := r.planOf(f)
		if !ok {
			r.planEvents(f, nil)
			continue
		}
		p, created, err := r.createPlan(ctx, f, mp)
		if err != nil {
			r.skipMark = true
			return err
		}
		if bk.madePlans == nil {
			bk.madePlans = map[string]string{}
		}
		bk.madePlans[f.job.Name] = p.Name
		if !created {
			continue
		}
		if err := r.supersedeOthers(ctx, p); err != nil {
			r.skipMark = true
			return err
		}
		r.planEvents(f, p)
	}
	return nil
}

// createPlan creates, using ctx, the TerraformPlan of mp for f, the Job
// that made it, and adds it to r.plans. It returns the plan, whether this
// call created it (false when it existed already), and any create error.
func (r *reconciler) createPlan(ctx context.Context, f *finished, mp madePlan) (*infrav1.TerraformPlan, bool, error) {
	rp := f.result.Plan
	ns, target := r.obj.GetNamespace(), r.obj.GetName()
	name := planName(target, f.job.Name, rp.Hash)
	if existing := r.planNamed(name); existing != nil {
		return existing, false, nil
	}
	summary := summaryOf(rp)
	p := &infrav1.TerraformPlan{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name,
			Labels: map[string]string{
				infrav1.PlanDestructiveLabel: strconv.FormatBool(destructive(summary)),
				infrav1.PlanPhaseLabel:       string(infrav1.PlanPhasePending),
				infrav1.PlanReasonLabel:      string(mp.reason),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: infrav1.GroupVersion.String(), Kind: r.k.Kind(), Name: target, UID: r.obj.GetUID(), Controller: new(true),
			}},
		},
		Spec: infrav1.TerraformPlanSpec{
			TargetRef:  infrav1.PlanTargetRef{Kind: infrav1.PlanTargetKind(r.k.Kind()), Name: target},
			PlanHash:   rp.Hash,
			InputsHash: mp.inputsHash,
			Reason:     mp.reason,
			Summary:    summary,
		},
	}
	if c := ClusterName(r.obj, r.owner); c != "" {
		p.Labels[clusterv1.ClusterNameLabel] = c
	}
	err := r.d.Client.Create(ctx, p)
	switch {
	case apierrors.IsAlreadyExists(err):
		return r.existingPlan(ctx, p, f)
	case err != nil:
		return nil, false, fmt.Errorf("create TerraformPlan %s: %w", name, err)
	}
	klog.FromContext(ctx).Info("Created a TerraformPlan that waits for approval", "TerraformPlan", klog.KObj(p), "Job", klog.KObj(f.job),
		"reason", mp.reason, "planHash", rp.Hash, "inputsHash", mp.inputsHash)
	r.d.Metrics.PlanApproval(r.k.Kind(), metrics.PlanCreated)
	r.plans = slices.Insert(r.plans, 0, *p)
	// Create drops the status; it is written now, not on the next pass.
	return &r.plans[0], true, r.writePlan(ctx, &r.plans[0], infrav1.PlanPhasePending)
}

// existingPlan handles, using ctx, a create of p, the TerraformPlan of the
// plan f made, that found it existing: one this object controls was
// created by a pass whose bookkept mark was lost, and the cache has not
// shown it yet; it is read through the API reader and added to r.plans,
// so the rest of the pass sees it. One another object controls predates
// this one (an earlier object of the same name, its garbage collection
// pending): it is not this object's plan, so an error is returned and
// the pass is retried until it is gone. It returns the plan, false (it
// was not created now), and any read error or that conflict.
func (r *reconciler) existingPlan(ctx context.Context, p *infrav1.TerraformPlan, f *finished) (*infrav1.TerraformPlan, bool, error) {
	live := &infrav1.TerraformPlan{}
	if err := r.d.APIReader.Get(ctx, client.ObjectKeyFromObject(p), live); err != nil {
		return nil, false, fmt.Errorf("get existing TerraformPlan %s: %w", p.Name, err)
	}
	if !metav1.IsControlledBy(live, r.obj) {
		return nil, false, fmt.Errorf("TerraformPlan %s exists, controlled by another object of this name; retrying once it is gone", p.Name)
	}
	klog.FromContext(ctx).V(LogFlow).Info("The TerraformPlan of the Job's plan exists already", "TerraformPlan", klog.KObj(live), "Job", klog.KObj(f.job))
	r.plans = slices.Insert(r.plans, 0, *live)
	return &r.plans[0], false, nil
}

// planEvents emits the events of the plan f, a finished Job read this
// pass, made: PlanReady naming p, the TerraformPlan it became (nil when it
// became none), or saying that an empty plan needs no approval, and
// PlanChanged for an approved apply that found the plan changed. A Job
// whose plan became no TerraformPlan emits only when bookkeeping counts it,
// once.
func (r *reconciler) planEvents(f *finished, p *infrav1.TerraformPlan) {
	op := jobs.OpOf(f.job)
	if p == nil && !f.counted {
		return
	}
	if f.planChanged && op == jobs.OpApply {
		note := fmt.Sprintf("Job %s stopped before applying: the plan changed since it was approved", f.job.Name)
		if src := f.job.Annotations[PlanAnnotation]; src != "" {
			note = fmt.Sprintf("Job %s stopped before applying: the plan changed since TerraformPlan %s was approved", f.job.Name, src)
		}
		switch {
		case p != nil:
			note += fmt.Sprintf(". New TerraformPlan %s: %s. Approve it: %s", p.Name, planCounts(p.Spec.Summary), planApproveCommand(p.Name, p.Namespace))
		case madePlanHash(f) == runner.EmptyPlanHash:
			note += "; the new plan changes nothing, so the apply runs without an approval"
		default:
			note += "; the next apply plans again"
		}
		r.d.EmitRelated(r.obj, f.job, corev1.EventTypeWarning, EventPlanChanged, "Plan", "%s", note)
		return
	}
	if op != jobs.OpPlan || !f.ok {
		return
	}
	if p == nil {
		if madePlanHash(f) == runner.EmptyPlanHash {
			r.d.EmitRelated(r.obj, f.job, corev1.EventTypeNormal, EventPlanReady, "Plan",
				"Job %s planned inputs hash %s: no changes, so the apply runs without an approval", f.job.Name, f.job.Annotations[state.InputsHashAnnotation])
		}
		return
	}
	r.d.EmitRelated(r.obj, p, corev1.EventTypeNormal, EventPlanReady, "Plan",
		"Job %s planned inputs hash %s: %s. TerraformPlan %s waits for approval: %s",
		f.job.Name, p.Spec.InputsHash, planCounts(p.Spec.Summary), p.Name, planApproveCommand(p.Name, p.Namespace))
}

// supersedeOthers marks, using ctx, every live plan of the object but p
// Superseded: p, just created, replaces them. It returns any write error.
func (r *reconciler) supersedeOthers(ctx context.Context, p *infrav1.TerraformPlan) error {
	for i := range r.plans {
		q := &r.plans[i]
		if q.Name == p.Name || phaseOf(q).Terminal() {
			continue
		}
		if err := r.supersede(ctx, q, "TerraformPlan "+p.Name+" replaces it", p.Name); err != nil {
			return err
		}
	}
	return nil
}

// supersede moves p to Superseded, using ctx, for why, emitting
// PlanSuperseded (a Warning when p was approved: its approval applied
// nothing) that names current, the plan live now ("" when none). It
// returns any write error.
func (r *reconciler) supersede(ctx context.Context, p *infrav1.TerraformPlan, why, current string) error {
	if err := r.writePlan(ctx, p, infrav1.PlanPhaseSuperseded); err != nil {
		return err
	}
	r.d.Metrics.PlanApproval(r.k.Kind(), metrics.PlanSuperseded)
	klog.FromContext(ctx).V(LogFlow).Info("Superseded a TerraformPlan", "TerraformPlan", klog.KObj(p), "why", why, "approved", approved(p))
	r.emitSuperseded(p, why, current)
	return nil
}

// emitSuperseded emits PlanSuperseded for p, superseded because of why:
// a Warning naming who approved it when it was approved, since its
// approval is ignored, and current, the plan live now ("" when none).
func (r *reconciler) emitSuperseded(p *infrav1.TerraformPlan, why, current string) {
	note := fmt.Sprintf("TerraformPlan %s is superseded: %s", p.Name, why)
	eventType := corev1.EventTypeNormal
	if approved(p) {
		eventType = corev1.EventTypeWarning
		note += fmt.Sprintf(". Its approval by %s is ignored: nothing of it was applied", p.Spec.ApprovedBy)
	}
	if current != "" && current != p.Name {
		note += ". The current plan is TerraformPlan " + current
	}
	r.d.EmitRelated(r.obj, p, eventType, EventPlanSuperseded, "Plan", "%s", note)
}

// syncPlans moves, using ctx, each of the object's plans to the phase bk,
// this pass's bookkeeping, shows: Approved once spec.approved is set
// (PlanApproved, naming the approver), Applied once the apply Job started
// for it (PlanAnnotation) succeeded (PlanApplied), Failed once that apply
// found the plan changed. Any other failure of that apply keeps it
// Approved: the retry applies the same plan. It writes the phase label and
// the status of every plan whose phase or status differs (after
// clusterctl move, which drops status, that rebuilds it), and warns once
// about an approval of a plan already superseded. It returns any write
// error.
func (r *reconciler) syncPlans(ctx context.Context, bk *Bookkeeping) error {
	for i := range r.plans {
		p := &r.plans[i]
		phase := phaseOf(p)
		if !phase.Terminal() {
			if f, ok := planApply(bk, p.Name); ok {
				switch {
				case f.ok:
					phase = infrav1.PlanPhaseApplied
				case f.planChanged:
					phase = infrav1.PlanPhaseFailed
				}
			}
		}
		labeled := infrav1.PlanPhase(p.Labels[infrav1.PlanPhaseLabel])
		if phase == labeled && equality.Semantic.DeepEqual(p.Status, planStatus(p, phase)) {
			continue
		}
		ignored := phase == infrav1.PlanPhaseSuperseded && approved(p) && conditions.GetReason(p, infrav1.PlanApprovedCondition) != infrav1.PlanApprovalIgnoredReason
		if err := r.writePlan(ctx, p, phase); err != nil {
			return err
		}
		r.planTransition(p, labeled, phase, ignored)
	}
	return nil
}

// planApply returns the newest finished apply Job bk read that applied the
// plan name (PlanAnnotation); ok is false when none.
func planApply(bk *Bookkeeping, name string) (finished, bool) {
	var newest finished
	found := false
	for _, f := range bk.byName {
		if jobs.OpOf(f.job) != jobs.OpApply || f.job.Annotations[PlanAnnotation] != name {
			continue
		}
		if !found || jobs.FinishedAt(f.job).After(jobs.FinishedAt(newest.job)) {
			newest, found = f, true
		}
	}
	return newest, found
}

// planTransition emits the event and counts the metric of p moving from
// the phase its label recorded, was, to phase; ignored is true when p is a
// superseded plan whose approval arrived too late and no event said so
// yet.
func (r *reconciler) planTransition(p *infrav1.TerraformPlan, was, phase infrav1.PlanPhase, ignored bool) {
	switch {
	case ignored:
		current := ""
		if l := r.livePlan(); l != nil {
			current = l.Name
		}
		r.emitSuperseded(p, "it was superseded before its approval was seen", current)
	case was == phase:
	case phase == infrav1.PlanPhaseApproved:
		r.d.Metrics.PlanApproval(r.k.Kind(), metrics.PlanApproved)
		r.d.EmitRelated(r.obj, p, corev1.EventTypeNormal, EventPlanApproved, "Plan",
			"TerraformPlan %s is approved by %s; the apply runs only if it plans exactly these changes again", p.Name, p.Spec.ApprovedBy)
	case phase == infrav1.PlanPhaseApplied:
		r.d.Metrics.PlanApproval(r.k.Kind(), metrics.PlanApplied)
		r.d.EmitRelated(r.obj, p, corev1.EventTypeNormal, EventPlanApplied, "Plan",
			"TerraformPlan %s, approved by %s, is applied", p.Name, p.Spec.ApprovedBy)
	case phase == infrav1.PlanPhaseFailed:
		r.d.Metrics.PlanApproval(r.k.Kind(), metrics.PlanFailed)
	}
}

// writePlan writes p's phase, using ctx: the captf.io/plan-phase label,
// then the status planStatus builds, each only when it differs. p, an
// element of r.plans, is updated in place. It returns any patch error; a
// plan that is gone needs no write.
func (r *reconciler) writePlan(ctx context.Context, p *infrav1.TerraformPlan, phase infrav1.PlanPhase) error {
	if p.Labels[infrav1.PlanPhaseLabel] != string(phase) {
		before := p.DeepCopy()
		metav1.SetMetaDataLabel(&p.ObjectMeta, infrav1.PlanPhaseLabel, string(phase))
		if err := r.d.Client.Patch(ctx, p, client.MergeFrom(before)); err != nil {
			return client.IgnoreNotFound(fmt.Errorf("label TerraformPlan %s %s: %w", p.Name, phase, err))
		}
	}
	status := planStatus(p, phase)
	if equality.Semantic.DeepEqual(p.Status, status) {
		return nil
	}
	before := p.DeepCopy()
	p.Status = status
	captfconds.ClampMessages(p)
	if err := r.d.Client.Status().Patch(ctx, p, client.MergeFrom(before)); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("update the status of TerraformPlan %s: %w", p.Name, err))
	}
	return nil
}

// planStatus returns the status of p in phase: the phase, its generation,
// and the Ready and Approved conditions, each keeping its last transition
// time while it does not change.
func planStatus(p *infrav1.TerraformPlan, phase infrav1.PlanPhase) infrav1.TerraformPlanStatus {
	q := &infrav1.TerraformPlan{Status: *p.Status.DeepCopy()}
	q.Generation = p.Generation
	ready := metav1.Condition{Type: infrav1.ReadyCondition, Status: metav1.ConditionTrue, ObservedGeneration: p.Generation}
	switch phase {
	case infrav1.PlanPhasePending:
		ready.Reason, ready.Message = infrav1.PlanPendingReason, "The plan waits for an approval"
	case infrav1.PlanPhaseApproved:
		ready.Reason, ready.Message = infrav1.PlanApprovedReason, "The plan is approved; its apply has not finished yet"
	case infrav1.PlanPhaseApplied:
		ready.Reason, ready.Message = infrav1.PlanAppliedReason, "The approved plan was applied"
	case infrav1.PlanPhaseSuperseded:
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, infrav1.PlanSupersededReason,
			"A newer plan replaced the plan, or it no longer applies to the target, before it was applied"
	case infrav1.PlanPhaseFailed:
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, infrav1.PlanFailedReason,
			"The apply of the approved plan planned other changes and stopped before applying them"
	}
	appr := metav1.Condition{Type: infrav1.PlanApprovedCondition, Status: metav1.ConditionFalse, ObservedGeneration: p.Generation}
	switch {
	case approved(p) && phase == infrav1.PlanPhaseSuperseded:
		appr.Reason, appr.Message = infrav1.PlanApprovalIgnoredReason,
			"Approved by "+p.Spec.ApprovedBy+", but the plan was superseded before its apply ran; nothing of it was applied"
	case approved(p):
		appr.Status, appr.Reason, appr.Message = metav1.ConditionTrue, infrav1.PlanApprovedReason, "Approved by "+p.Spec.ApprovedBy
	case phase.Terminal():
		appr.Reason, appr.Message = infrav1.PlanNotApprovedReason, "The plan was never approved"
	default:
		appr.Reason, appr.Message = infrav1.PlanPendingReason, "Set spec.approved, and spec.approvedBy to your username, to approve the plan"
	}
	conditions.Set(q, ready)
	conditions.Set(q, appr)
	q.Status.Phase = phase
	q.Status.ObservedGeneration = max(p.Generation, 1)
	return q.Status
}

// planChangedLead returns what the ApplyJobSucceeded message of job, an
// approved apply that found the plan changed, says after "Job <name>: ".
func planChangedLead(job *batchv1.Job) string {
	if src := job.Annotations[PlanAnnotation]; src != "" {
		return "the plan changed since TerraformPlan " + src + " was approved, so nothing was applied"
	}
	return "the plan changed since it was approved, so nothing was applied"
}

// waitCondition returns ApplyJobSucceeded while the apply waits for p, a
// Manual TerraformPlan: Unknown/PlanChanged when bk shows the newest apply
// made p after finding its approved plan changed, PlanAwaitingApproval
// otherwise. The message names p, its changes and the command that
// approves it, which stays last.
func (r *reconciler) waitCondition(bk *Bookkeeping, p *infrav1.TerraformPlan) metav1.Condition {
	c := metav1.Condition{Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionUnknown, Reason: infrav1.PlanAwaitingApprovalReason}
	body := fmt.Sprintf("TerraformPlan %s plans inputs hash %s: %s. Nothing is applied until it is approved: %s",
		p.Name, p.Spec.InputsHash, planCounts(p.Spec.Summary), planApproveCommand(p.Name, p.Namespace))
	if last := bk.LastApply; last != nil && bk.LastApplyPlanChanged {
		if f, ok := bk.finishedJob(last.Name); ok && planName(r.obj.GetName(), last.Name, madePlanHash(&f)) == p.Name {
			c.Reason = infrav1.PlanChangedReason
			c.Message = "Job " + last.Name + ": " + planChangedLead(last) + ". " + body
			return c
		}
	}
	c.Message = body
	return c
}

// planWaitCondition returns ApplyJobSucceeded while an apply waits for the
// approval of its plan, for finish: on a pass that decided, the wait
// DecideOp reported (planWait), also when it started a refresh or drift
// Job meanwhile; on a pass that did not (a Job runs, the Job cache lags,
// the state is held, the object is paused), the wait the condition
// reported as the pass began, while the live Manual plan it names still
// waits, unless bk, the pass's bookkeeping, read an apply that finished
// since, applyPolicy is no longer Manual, or the object is deleting. It
// returns nil when no wait stands.
func (r *reconciler) planWaitCondition(bk *Bookkeeping) *metav1.Condition {
	if r.decided {
		return r.planWait
	}
	prev, ok := r.before[infrav1.ApplyJobSucceededCondition]
	p := r.livePlan()
	if bk == nil || !ok || r.deleting || !r.manualApply() || p == nil || p.Spec.Reason != infrav1.PlanReasonManual || approved(p) ||
		!planWaitReasons.Has(prev.Reason) || !strings.Contains(prev.Message, "TerraformPlan "+p.Name+" ") {
		return nil
	}
	if bk.LastApply != nil {
		if f, ok := bk.finishedJob(bk.LastApply.Name); ok && !f.bookkept {
			return nil
		}
	}
	c := r.waitCondition(bk, p)
	return &c
}
