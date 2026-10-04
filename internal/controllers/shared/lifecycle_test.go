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
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// TestPlanWaitKeptThroughChecks: while a drift remediation waits for the
// approval of its plan, the drift check that runs meanwhile does not turn
// ApplyJobSucceeded back to the last apply's result, neither on the pass
// that starts it nor while it runs; the approval ends the wait.
func TestPlanWaitKeptThroughChecks(t *testing.T) {
	t.Parallel()
	e := newPlanEnv(t, "", func(m *infrav1.TerraformMachine) {
		m.Status.LastDriftCheck = &metav1.Time{Time: t0.Add(-2 * time.Hour)}
		m.Status.Conditions = append(m.Status.Conditions, metav1.Condition{
			Type: infrav1.DriftDetectedCondition, Status: metav1.ConditionTrue, Reason: infrav1.DriftPendingReason,
			Message: "Job d: 0 to add, 1 to change, 0 to destroy", LastTransitionTime: metav1.NewTime(t0.Add(-2 * time.Hour)),
		})
	})
	remediate := &infrav1.DriftPolicy{IntervalSeconds: new(int32(3600)), Action: infrav1.DriftActionRemediate}
	e.reconcile(t, remediate)
	plan := e.newest(t)
	p := &runner.Plan{Hash: runner.PlanHash([]string{"module.role.tags|update"}), Change: 1, Resources: []string{"module.role.tags (update)"}}
	e.finishRunner(t, plan, jobs.Succeeded, t0.Add(-time.Minute), planResult(runner.OpPlan, p, ""))

	waits := func(pass string) {
		t.Helper()
		if c := planApplyReason(t, e); c == nil || c.Reason != infrav1.PlanAwaitingApprovalReason || !namesJob(c.Message, plan) {
			t.Errorf("%s: ApplyJobSucceeded = %+v, want PlanAwaitingApproval for %s", pass, c, plan)
		}
	}
	e.reconcile(t, remediate)
	if len(e.runner.created) != 2 || jobs.OpOf(e.jobNamed(t, e.newest(t))) != jobs.OpDrift {
		t.Fatalf("created %v, want the due drift check", e.runner.created)
	}
	waits("drift started")
	e.reconcile(t, remediate)
	waits("drift running")

	// Approved: the wait is over even before the next decision.
	e.approvePlan(t, p.Hash)
	e.reconcile(t, remediate)
	if c := planApplyReason(t, e); c == nil || c.Reason != infrav1.ApplySucceededReason {
		t.Errorf("approved: ApplyJobSucceeded = %+v, want the last apply's result", c)
	}
}

// TestStuckJobDeleteFinishes: the pass that deletes a Job stuck without
// its per-run Secret still sets ApplyJobSucceeded and Ready, and leaves
// status.activeJob empty.
func TestStuckJobDeleteFinishes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	const name = "captf-m-m1-apply-a1-abcdef"
	e.runner.jobs = append(e.runner.jobs, job(name, jobs.OpApply, jobs.Running, t0))
	k := e.kindFor(t, readyOwner)
	k.in = machineIn()
	requeue, err := reconcileOnce(t, e, k)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.runner.deleted, []string{name}) || requeue != time.Second {
		t.Fatalf("deleted %v, requeue %s; want the stuck Job deleted", e.runner.deleted, requeue)
	}
	m := e.get(t)
	if c := conditions.Get(m, infrav1.ApplyJobSucceededCondition); c == nil || c.Reason != infrav1.NoApplyYetReason {
		t.Errorf("ApplyJobSucceeded = %+v, want NoApplyYet", c)
	}
	if c := conditions.Get(m, infrav1.ReadyCondition); c == nil || c.Status == metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want computed and not True", c)
	}
	if m.Status.ActiveJob.Name != "" {
		t.Errorf("status.activeJob = %+v, want empty", m.Status.ActiveJob)
	}
}

// TestChecksWithoutDurableInputs: a machine whose durable inputs Secret is
// gone cannot render its due refresh; DriftJobSucceeded says so, pointing
// at the runbook, instead of the checks stopping silently, and the
// condition stays put across passes.
func TestChecksWithoutDurableInputs(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	a := job("a", jobs.OpApply, jobs.Succeeded, t0.Add(-time.Minute))
	a.Annotations = map[string]string{state.InputsHashAnnotation: "h1:x"}
	e.runner.jobs = append(e.runner.jobs, a)
	e.state.st = &state.State{Serial: 1, InputsHash: "h1:x"}
	for pass := range 2 {
		k := e.kindFor(t, readyOwner)
		k.refresh, k.health = true, &contract.Health{State: contract.HealthPending}
		requeue, err := reconcileOnce(t, e, k)
		if err != nil {
			t.Fatal(err)
		}
		if len(e.runner.created) != 0 || requeue != RetryMax {
			t.Fatalf("pass %d: created %v, requeue %s; want nothing started", pass, e.runner.created, requeue)
		}
		c := conditions.Get(e.get(t), infrav1.DriftJobSucceededCondition)
		if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != infrav1.DurableInputsMissingReason ||
			!strings.Contains(c.Message, durableRunbook) || !strings.Contains(c.Message, "refresh") {
			t.Errorf("pass %d: DriftJobSucceeded = %+v", pass, c)
		}
	}
	changed := 0
	for _, ev := range e.rec.only(EventConditionChanged) {
		if strings.HasPrefix(ev.note, infrav1.DriftJobSucceededCondition) {
			changed++
		}
	}
	if changed > 1 {
		t.Errorf("%d DriftJobSucceeded transitions over two passes, want at most 1", changed)
	}
}

// TestMismatchedApprovalNoted: an approval annotation that names another
// hash than the one an apply waits for is named in the waiting condition,
// once however many passes keep it, and the note goes with it.
func TestMismatchedApprovalNoted(t *testing.T) {
	t.Parallel()
	t.Run("destructive plan", func(t *testing.T) {
		t.Parallel()
		e := newBlockedEnv(t, "h1:old", false)
		reconcile := func() *metav1.Condition {
			t.Helper()
			if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
				t.Fatal(err)
			}
			return conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition)
		}
		if c := reconcile(); strings.Contains(c.Message, "annotation names") {
			t.Errorf("no approval: ApplyJobSucceeded = %q", c.Message)
		}
		e.syncMarks(t)
		e.approve(t, "h1:wrong")
		note := ". The " + infrav1.ApproveDestructivePlanAnnotation + ` annotation names "h1:wrong", which does not match ` + e.hash + ". To apply it"
		for pass := range 2 {
			c := reconcile()
			if c.Reason != infrav1.DestructivePlanBlockedReason || strings.Count(c.Message, note) != 1 || !strings.HasSuffix(c.Message, "="+e.hash+" --overwrite") {
				t.Errorf("pass %d: ApplyJobSucceeded = %+v, want one note %q before the command", pass, c, note)
			}
		}
		if len(e.runner.created) != 0 {
			t.Fatalf("created %v under a mismatched approval", e.runner.created)
		}
		obj := e.get(t)
		delete(obj.Annotations, infrav1.ApproveDestructivePlanAnnotation)
		if err := e.c.Update(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
		if c := reconcile(); strings.Contains(c.Message, "annotation names") {
			t.Errorf("approval removed: ApplyJobSucceeded = %q", c.Message)
		}
	})
	t.Run("plan", func(t *testing.T) {
		t.Parallel()
		e := newPlanEnv(t, "h1:old")
		e.reconcile(t, nil)
		plan := e.newest(t)
		p := &runner.Plan{Hash: runner.PlanHash([]string{"module.role.lb|update"}), Change: 1, Resources: []string{"module.role.lb (update)"}}
		e.finishRunner(t, plan, jobs.Succeeded, t0.Add(-time.Minute), planResult(runner.OpPlan, p, ""))
		e.approvePlan(t, "p1:another")
		e.reconcile(t, nil)
		note := "resources). The " + infrav1.ApprovePlanAnnotation + ` annotation names "p1:another", which does not match ` + p.Hash + ". Nothing is applied"
		for pass := range 2 {
			if c := planApplyReason(t, e); c == nil || c.Reason != infrav1.PlanAwaitingApprovalReason || strings.Count(c.Message, note) != 1 ||
				!strings.HasSuffix(c.Message, "="+p.Hash+" --overwrite") {
				t.Errorf("pass %d: ApplyJobSucceeded = %+v, want one note %q before the command", pass, c, note)
			}
			e.reconcile(t, nil)
		}
	})
}

// TestDestructiveApprovalAtStart: the apply a destructive-plan approval
// allows reports the approval, its hash and the Job, as it starts, once.
func TestDestructiveApprovalAtStart(t *testing.T) {
	t.Parallel()
	e := newBlockedEnv(t, "h1:old", false)
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	if n := e.rec.count(EventPlanApproved); n != 0 {
		t.Fatalf("%d PlanApproved before the approval", n)
	}
	e.approve(t, e.hash)
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 1 {
		t.Fatalf("created %v, want the approved apply", e.runner.created)
	}
	a := e.runner.created[0]
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	got := e.rec.only(EventPlanApproved)
	if len(got) != 1 || got[0].eventType != corev1.EventTypeNormal || !strings.Contains(got[0].note, e.hash) ||
		!strings.Contains(got[0].note, a) || !strings.Contains(got[0].note, infrav1.ApproveDestructivePlanAnnotation) {
		t.Errorf("PlanApproved = %+v, want one naming %s and Job %s", got, e.hash, a)
	}
}

// TestUnreadableStateLogged: an unreadable state is logged at V0 with its
// error when StateReadable first reports it, not again on every pass.
func TestUnreadableStateLogged(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	e.state.err = state.ErrStateCorrupt
	logger := ktesting.NewLogger(t, ktesting.NewConfig(ktesting.Verbosity(0), ktesting.BufferLogs(true)))
	ctx := klog.NewContext(t.Context(), logger)
	for range 2 {
		k := e.kindFor(t, readyOwner)
		k.in = machineIn()
		if _, err := Reconcile(ctx, e.d, k); err != nil {
			t.Fatal(err)
		}
	}
	underlier, ok := logger.GetSink().(ktesting.Underlier)
	if !ok {
		t.Fatalf("sink is %T", logger.GetSink())
	}
	out := underlier.GetBuffer().String()
	if n := strings.Count(out, "The state cannot be read"); n != 1 || !strings.Contains(out, state.ErrStateCorrupt.Error()) {
		t.Errorf("%d V0 lines over two passes, want 1 with the error; log:\n%s", n, out)
	}
}

// TestMirrorConflictNamed: CredentialsMirrored names the Secret that
// holds the mirror's name, the identity, and what to do about it.
func TestMirrorConflictNamed(t *testing.T) {
	t.Parallel()
	name := identity.MirrorName(testIdentity)
	squatter := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name}, Data: map[string][]byte{"x": []byte("y")}}
	e := newEnv(t, world(machine(withFinalizer, notPaused), squatter)...)
	logger := ktesting.NewLogger(t, ktesting.NewConfig(ktesting.Verbosity(0), ktesting.BufferLogs(true)))
	ctx := klog.NewContext(t.Context(), logger)
	for pass := range 2 {
		k := e.kindFor(t, readyOwner)
		k.in = machineIn()
		if _, err := Reconcile(ctx, e.d, k); err != nil {
			t.Fatal(err)
		}
		c := conditions.Get(e.get(t), infrav1.CredentialsMirroredCondition)
		if c == nil || c.Reason != infrav1.MirrorFailedReason || !strings.HasPrefix(c.Message, "Secret "+testNS+"/"+name+" ") ||
			!strings.Contains(c.Message, "TerraformClusterIdentity "+testIdentity) || !strings.Contains(c.Message, "Rename or remove it") {
			t.Errorf("pass %d: CredentialsMirrored = %+v", pass, c)
		}
	}
	underlier, ok := logger.GetSink().(ktesting.Underlier)
	if !ok {
		t.Fatalf("sink is %T", logger.GetSink())
	}
	out := underlier.GetBuffer().String()
	if n := strings.Count(out, "not a mirror of this identity"); n != 1 || !strings.Contains(out, testNS+"/"+name) {
		t.Errorf("%d V0 lines over two passes, want 1 naming the Secret; log:\n%s", n, out)
	}
}
