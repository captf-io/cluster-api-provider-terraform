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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// clusterBlockedReconciler returns a reconciler over a fresh cluster-kind
// object with the drift action action and the conditions conds set
// before the pass; t fails the test on error.
func clusterBlockedReconciler(t *testing.T, action infrav1.DriftAction, conds ...metav1.Condition) *reconciler {
	t.Helper()
	e := newEnv(t, machine())
	k := e.kindFor(t, readyOwner)
	k.asCluster, k.mutable = true, true
	for _, c := range conds {
		conditions.Set(k.obj, c)
	}
	r := &reconciler{d: e.d, k: k, obj: k.obj}
	r.eff.DriftAction = action
	return r
}

// clusterBlockedBookkeeping returns the bookkeeping of a cluster whose
// newest apply, Job b of inputs hash h1:b (a drift remediation when
// remediation is true, already bookkept), was blocked before a
// destructive plan, after prior (nil for none); current is the pass's
// current inputs hash.
func clusterBlockedBookkeeping(current string, remediation bool, prior *finished) *Bookkeeping {
	b := job("b", jobs.OpApply, jobs.Failed, t0)
	b.Annotations = map[string]string{state.InputsHashAnnotation: "h1:b", BlockedAnnotation: "true", BookkeptAnnotation: "true"}
	if remediation {
		b.Annotations[RemediationAnnotation] = "true"
	}
	return &Bookkeeping{
		LastApply: &b, LastApplyBlocked: true, priorApply: prior, CurrentHash: current,
		byName: map[string]finished{"b": {job: &b, blocked: true, bookkept: true}},
		ApplyJob: metav1.Condition{
			Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionFalse, Reason: infrav1.DestructivePlanBlockedReason,
			Message: "Job b: x. Nothing was applied, and no apply of these inputs runs until they are approved.",
		},
	}
}

// TestClusterBlockWithdrawn: a TerraformCluster apply blocked before a
// destructive plan is reported withdrawn once its apply is no longer due
// (the current inputs moved off the blocked ones, or the drift the
// blocked remediation was for is gone): the last successful apply stands,
// or the failure before the block shows through, with no approve command.
// While the apply is still due, or a pass cannot tell, the block stays; a
// withdrawal already reported is kept by a pass that cannot tell.
func TestClusterBlockWithdrawn(t *testing.T) {
	t.Parallel()
	driftTrue := metav1.Condition{Type: infrav1.DriftDetectedCondition, Status: metav1.ConditionTrue, Reason: infrav1.DriftPendingReason, Message: "Job d"}
	driftFalse := metav1.Condition{Type: infrav1.DriftDetectedCondition, Status: metav1.ConditionFalse, Reason: infrav1.NoDriftReason}
	priorJob := job("p", jobs.OpApply, jobs.Failed, t0.Add(-time.Hour))
	priorFailed := &finished{job: &priorJob, bookkept: true}
	priorReported := metav1.Condition{Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionFalse, Reason: infrav1.ApplyFailedReason, Message: "Job p: step apply failed"}
	withdrawn := metav1.Condition{
		Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionTrue, Reason: infrav1.ApplySucceededReason,
		Message: liftedPrefix("b") + "the inputs it planned are no longer the current ones. The last successful apply stands",
	}
	for _, tt := range []struct {
		name        string
		bk          *Bookkeeping
		action      infrav1.DriftAction
		conds       []metav1.Condition
		deleting    bool
		wantReason  string
		wantMessage string // substring
	}{
		{
			name: "reverted inputs withdraw the block", bk: clusterBlockedBookkeeping("h1:s", false, nil),
			wantReason: infrav1.ApplySucceededReason, wantMessage: "the inputs it planned are no longer the current ones. The last successful apply stands",
		},
		{
			name: "the blocked inputs, still due, stay blocked", bk: clusterBlockedBookkeeping("h1:b", false, nil),
			wantReason: infrav1.DestructivePlanBlockedReason, wantMessage: "until they are approved",
		},
		{
			name: "a pass that built no inputs keeps the block", bk: clusterBlockedBookkeeping("", false, nil),
			wantReason: infrav1.DestructivePlanBlockedReason,
		},
		{
			name: "a pass that built no inputs keeps a reported withdrawal", bk: clusterBlockedBookkeeping("", false, nil),
			conds:      []metav1.Condition{withdrawn},
			wantReason: infrav1.ApplySucceededReason, wantMessage: withdrawn.Message,
		},
		{
			name: "the failure before the block shows through", bk: clusterBlockedBookkeeping("h1:c", false, priorFailed),
			wantReason: infrav1.ApplyFailedReason, wantMessage: "Job p",
		},
		{
			name: "a pass that built no inputs keeps the reported failure", bk: clusterBlockedBookkeeping("", false, priorFailed),
			conds:      []metav1.Condition{priorReported},
			wantReason: infrav1.ApplyFailedReason, wantMessage: "step apply failed",
		},
		{
			name: "a blocked remediation is withdrawn once the drift is gone", bk: clusterBlockedBookkeeping("h1:b", true, nil),
			action: infrav1.DriftActionRemediate, conds: []metav1.Condition{driftFalse},
			wantReason: infrav1.ApplySucceededReason, wantMessage: "it remediated drift, which is no longer detected",
		},
		{
			name: "a blocked remediation is withdrawn once remediation is off", bk: clusterBlockedBookkeeping("", true, nil),
			action: infrav1.DriftActionReport, conds: []metav1.Condition{driftTrue},
			wantReason: infrav1.ApplySucceededReason, wantMessage: "the drift action is no longer Remediate",
		},
		{
			name: "a blocked remediation of drift still detected stays blocked", bk: clusterBlockedBookkeeping("h1:b", true, nil),
			action: infrav1.DriftActionRemediate, conds: []metav1.Condition{driftTrue},
			wantReason: infrav1.DestructivePlanBlockedReason,
		},
		{
			name: "a deletion keeps the block", bk: clusterBlockedBookkeeping("", true, nil), deleting: true,
			action: infrav1.DriftActionRemediate, conds: []metav1.Condition{driftFalse},
			wantReason: infrav1.DestructivePlanBlockedReason,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := clusterBlockedReconciler(t, tt.action, tt.conds...)
			r.deleting = tt.deleting
			c := r.applyJobCondition(tt.bk)
			if c.Reason != tt.wantReason || !strings.Contains(c.Message, tt.wantMessage) {
				t.Fatalf("ApplyJobSucceeded = %+v, want %s with %q", c, tt.wantReason, tt.wantMessage)
			}
			if c.Reason != infrav1.DestructivePlanBlockedReason && strings.Contains(c.Message, infrav1.ApproveDestructivePlanAnnotation) {
				t.Errorf("a withdrawn block still names the approve command: %s", c.Message)
			}
		})
	}
}

// TestClusterBlockWithdrawnByRevert: a TerraformCluster apply blocked
// before a destructive plan of inputs that are no longer the current ones
// (the spec was reverted to the state's) is reported withdrawn by the
// reconcile, which sets the pass's current inputs hash on the
// bookkeeping, and no Job starts: the state already holds the current
// inputs. A second pass that reads the Job again, unmarked, keeps it so.
func TestClusterBlockWithdrawnByRevert(t *testing.T) {
	t.Parallel()
	e := newBlockedEnv(t, "", false)
	e.jobNamed(t, "b").Annotations[state.InputsHashAnnotation] = "h1:blocked"
	for pass := range 2 {
		if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
			t.Fatal(err)
		}
		c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition)
		if c == nil || c.Status != metav1.ConditionTrue || c.Reason != infrav1.ApplySucceededReason ||
			!strings.HasPrefix(c.Message, liftedPrefix("b")) || !strings.Contains(c.Message, "no longer the current ones") {
			t.Fatalf("pass %d: ApplyJobSucceeded = %+v, want the block withdrawn", pass, c)
		}
	}
	if len(e.runner.created) != 0 {
		t.Errorf("created %v; nothing is due", e.runner.created)
	}
}

// TestClusterBlockedRemediationWithdrawnOnce: a blocked drift remediation
// of a TerraformCluster is withdrawn by the reconcile once a drift check
// found no drift: ApplyJobSucceeded goes back to True with one Normal
// ConditionChanged, no Job starts, and later passes emit nothing more.
func TestClusterBlockedRemediationWithdrawnOnce(t *testing.T) {
	t.Parallel()
	e := newBlockedEnv(t, "", true, func(m *infrav1.TerraformMachine) {
		m.Status.LastDriftCheck = &metav1.Time{Time: t0.Add(-time.Hour)}
		m.Status.Conditions = append(m.Status.Conditions, metav1.Condition{
			Type: infrav1.DriftDetectedCondition, Status: metav1.ConditionTrue, Reason: infrav1.DriftRemediatingReason,
			Message: "Job d: 0 to add, 0 to change, 1 to destroy" + remediationMarker + "b applies the current inputs", LastTransitionTime: metav1.NewTime(t0.Add(-time.Hour)),
		})
	})
	remediate := &infrav1.DriftPolicy{IntervalSeconds: new(int32(0)), Action: infrav1.DriftActionRemediate}
	if _, err := reconcileOnce(t, e.env, e.kind(t, remediate)); err != nil {
		t.Fatal(err)
	}
	e.syncMarks(t)
	if c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition); c == nil || c.Reason != infrav1.DestructivePlanBlockedReason {
		t.Fatalf("ApplyJobSucceeded = %+v, want the block", c)
	}

	// A drift check found no drift since.
	obj := e.get(t)
	conditions.Set(obj, metav1.Condition{Type: infrav1.DriftDetectedCondition, Status: metav1.ConditionFalse, Reason: infrav1.NoDriftReason})
	if err := e.c.Status().Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := reconcileOnce(t, e.env, e.kind(t, remediate)); err != nil {
			t.Fatal(err)
		}
	}
	c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != infrav1.ApplySucceededReason || !strings.HasPrefix(c.Message, liftedPrefix("b")) {
		t.Fatalf("ApplyJobSucceeded = %+v, want the block withdrawn", c)
	}
	if len(e.runner.created) != 0 {
		t.Errorf("created %v; nothing is due", e.runner.created)
	}
	var withdrawals int
	for _, ev := range e.rec.only(EventConditionChanged) {
		if strings.HasPrefix(ev.note, infrav1.ApplyJobSucceededCondition+": True/") {
			withdrawals++
			if ev.eventType != normal {
				t.Errorf("withdrawal event type = %s, want Normal", ev.eventType)
			}
		}
	}
	if withdrawals != 1 {
		t.Errorf("%d ApplyJobSucceeded ConditionChanged events, want 1 (all: %v)", withdrawals, e.rec.reasons)
	}
}
