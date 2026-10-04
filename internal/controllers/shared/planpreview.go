/*
Copyright 2026 The cluster-api-provider-terraform Authors.

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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
	"github.com/captf-io/cluster-api-provider-terraform/internal/strutil"
)

// maxPlanEntry is the API's limit on one status.plan.resources entry.
const maxPlanEntry = 600

// Plan preview (applyPolicy Manual): a plan Job's plan, or the new plan
// of an approved apply that found it changed, becomes status.plan; the
// apply waits until the approve-plan annotation names its hash, and the
// annotation is consumed once the apply of that plan succeeded.

// planApproveCommand returns the command that approves planHash on obj, a
// kind.
func planApproveCommand(kind string, obj client.Object, planHash string) string {
	return fmt.Sprintf("kubectl annotate %s %s -n %s %s=%s --overwrite",
		strings.ToLower(kind), obj.GetName(), obj.GetNamespace(), infrav1.ApprovePlanAnnotation, planHash)
}

// planCounts returns p formatted as "N to add, M to change, K to
// destroy", followed by ", J output(s) to change" when the plan changes
// outputs, so an output-only plan does not read as no changes.
func planCounts(p infrav1.PlanPreview) string {
	n := func(v *int32) int32 {
		if v == nil {
			return 0
		}
		return *v
	}
	s := fmt.Sprintf("%d to add, %d to change, %d to destroy", n(p.Add), n(p.Change), n(p.Destroy))
	if o := n(p.OutputChanges); o > 0 {
		s += fmt.Sprintf(", %d output(s) to change", o)
	}
	return s
}

// planCondition is ApplyJobSucceeded while the plan p waits for approval:
// Unknown/PlanAwaitingApproval for a plan Job's plan, Unknown/PlanChanged
// when an approved apply found the plan changed (changed). Bookkeeping (a
// plan-changed Job) and the reconciler (the wait) both build it here, so
// they agree; the message starts "Job <name>:" like every Job condition.
// kind and obj name the object the approve command targets. It returns
// the condition to set.
func planCondition(p infrav1.PlanPreview, changed bool, kind string, obj client.Object) metav1.Condition {
	c := metav1.Condition{
		Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionUnknown, Reason: infrav1.PlanAwaitingApprovalReason,
	}
	lead := "plan"
	if changed {
		c.Reason = infrav1.PlanChangedReason
		lead = "the plan changed since it was approved, so nothing was applied; new plan"
	}
	c.Message = fmt.Sprintf("Job %s: %s %s: %s (status.plan lists the resources). Nothing is applied until it is approved: %s",
		p.Job, lead, p.PlanHash, planCounts(p), planApproveCommand(kind, obj, p.PlanHash))
	return c
}

// previewOf returns the status.plan of f, a finished Job's plan; ok is
// false when its result carries none (a bookkept Job, or no result).
func previewOf(f *finished) (infrav1.PlanPreview, bool) {
	if f.result == nil || f.result.Plan == nil || f.result.Plan.Hash == "" {
		return infrav1.PlanPreview{}, false
	}
	rp := f.result.Plan
	at := metav1.NewTime(jobs.FinishedAt(f.job))
	p := infrav1.PlanPreview{
		InputsHash: f.job.Annotations[state.InputsHashAnnotation], Job: f.job.Name, PlanHash: rp.Hash,
		Add: new(int32(rp.Add)), Change: new(int32(rp.Change)), Destroy: new(int32(rp.Destroy)), // #nosec G115 -- plan resource counts, far below MaxInt32
		OutputChanges: new(int32(rp.Outputs)), CreatedAt: &at, // #nosec G115 -- output-change count, far below MaxInt32
	}
	truncated := rp.Truncated
	for i, r := range rp.Resources {
		if i == infrav1.MaxPlanResources {
			truncated = true
			break
		}
		if r = strutil.Truncate(r, maxPlanEntry); r != "" {
			p.Resources = append(p.Resources, r)
		}
	}
	if truncated {
		p.Truncated = new(true)
	}
	return p, true
}

// plans finds, in done, the newest finished Job that made a plan (a plan
// Job, or an apply that found its approved plan changed) and finished
// after the newest successful apply, for status.plan, and emits, through
// d for k's object, PlanReady or PlanChanged (and counts a changed plan)
// once per such Job, when it is counted.
func (bk *Bookkeeping) plans(d Deps, k Kind, done []finished) {
	obj := k.Object()
	found := false
	for i := range done {
		f := &done[i]
		op := jobs.OpOf(f.job)
		if op == jobs.OpApply && f.ok {
			found = true
		}
		planned := (op == jobs.OpPlan && f.ok) || (op == jobs.OpApply && f.planChanged)
		if !planned {
			continue
		}
		if !found {
			found, bk.lastPlan = true, f
		}
		if !f.counted {
			continue
		}
		p, ok := previewOf(f)
		if !ok {
			continue
		}
		if f.planChanged {
			d.Metrics.PlanApproval(k.Kind(), metrics.PlanChanged)
			d.EmitRelated(obj, f.job, corev1.EventTypeWarning, EventPlanChanged, "Plan",
				"Job %s stopped before applying: the plan changed since plan %s was approved. New plan %s: %s. Approve it: %s",
				f.job.Name, f.job.Annotations[ApprovedPlanAnnotation], p.PlanHash, planCounts(p), planApproveCommand(k.Kind(), obj, p.PlanHash))
			continue
		}
		if p.PlanHash == runner.EmptyPlanHash {
			d.EmitRelated(obj, f.job, corev1.EventTypeNormal, EventPlanReady, "Plan",
				"Job %s planned inputs hash %s: no changes, so the apply runs without an approval", f.job.Name, p.InputsHash)
			continue
		}
		d.EmitRelated(obj, f.job, corev1.EventTypeNormal, EventPlanReady, "Plan",
			"Job %s planned inputs hash %s: %s (plan %s, status.plan lists the resources). Approve it: %s",
			f.job.Name, p.InputsHash, planCounts(p), p.PlanHash, planApproveCommand(k.Kind(), obj, p.PlanHash))
	}
}

// recordPlan writes status.plan from bk.lastPlan, the newest plan a Job
// made this pass. A bookkept Job's plan was written when it finished; the
// status keeps it.
func (r *reconciler) recordPlan(bk *Bookkeeping) {
	if r.st.Plan == nil || bk.lastPlan == nil || bk.lastPlan.bookkept {
		return
	}
	if p, ok := previewOf(bk.lastPlan); ok {
		*r.st.Plan = p
	}
}

// planView returns status.plan for DecideOp; nil when none is recorded.
func (r *reconciler) planView() *PlanView {
	if r.st.Plan == nil || r.st.Plan.PlanHash == "" {
		return nil
	}
	return &PlanView{InputsHash: r.st.Plan.InputsHash, PlanHash: r.st.Plan.PlanHash}
}

// clearPlan empties status.plan.
func (r *reconciler) clearPlan() {
	if r.st.Plan != nil {
		*r.st.Plan = infrav1.PlanPreview{}
	}
}

// waitCondition returns ApplyJobSucceeded while the apply waits for the
// plan in status.plan: PlanChanged when bk shows the newest apply stopped
// with it, PlanAwaitingApproval otherwise.
func (r *reconciler) waitCondition(bk *Bookkeeping) metav1.Condition {
	p := *r.st.Plan
	changed := bk.LastApply != nil && bk.LastApplyPlanChanged && bk.LastApply.Name == p.Job
	return planCondition(p, changed, r.k.Kind(), r.obj)
}

// consumePlanApproval removes the approve-plan annotation once bk's apply
// of the approved plan succeeded, clears status.plan, emits PlanApplied
// and counts the approval, logging with ctx. Only an apply not yet
// bookkept consumes, as a restore does: the same plan hash approved again
// later (a recurring drift planned alike) is a new request. It returns any
// error from removing the annotation (removeAnnotation).
func (r *reconciler) consumePlanApproval(ctx context.Context, bk *Bookkeeping) error {
	if bk.LastApply == nil || !bk.LastApplySucceeded {
		return nil
	}
	planned := bk.LastApply.Annotations[ApprovedPlanAnnotation]
	f, ok := bk.finishedJob(bk.LastApply.Name)
	if planned == "" || !ok || f.bookkept {
		return nil
	}
	if r.st.Plan != nil && r.st.Plan.PlanHash == planned {
		r.clearPlan()
	}
	if r.annotation(infrav1.ApprovePlanAnnotation) != planned {
		return nil
	}
	if err := r.removeAnnotation(ctx, infrav1.ApprovePlanAnnotation); err != nil {
		return err
	}
	r.d.Metrics.PlanApproval(r.k.Kind(), metrics.PlanApproved)
	klog.FromContext(ctx).Info("The approved plan was applied; removed its approval", "planHash", planned, "Job", bk.LastApply.Name)
	r.d.EmitRelated(r.obj, bk.LastApply, corev1.EventTypeNormal, EventPlanApplied, "Reconcile",
		"Job %s applied the approved plan %s; removed %s, which approves one plan only",
		bk.LastApply.Name, planned, infrav1.ApprovePlanAnnotation)
	return nil
}
