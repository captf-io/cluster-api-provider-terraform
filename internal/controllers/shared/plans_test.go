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
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cbmetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/testutil"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runlease"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// TestDecideOpManual: under applyPolicy Manual an apply of new inputs, a
// retry and a drift remediation are gated behind a plan Job and the
// approval of its TerraformPlan; the first apply is not; Automatic is
// unchanged.
func TestDecideOpManual(t *testing.T) {
	t.Parallel()
	changed := StateView{Exists: true, InputsHash: "h1:old", CurrentHash: "h1:new"}
	same := StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:a"}
	planOf := func(h, p string, ok bool) *PlanView {
		return &PlanView{Name: "c-0123456789", Reason: infrav1.PlanReasonManual, InputsHash: h, PlanHash: p, Approved: ok}
	}
	due := 30 * time.Minute // DriftInterval with no check yet: due at once
	tests := []struct {
		name string
		in   DecideInput
		want Decision
	}{
		{"automatic applies at once", DecideInput{Mutable: true, State: changed},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged"}},
		{"no plan yet: a plan Job", DecideInput{Mutable: true, ManualApply: true, State: changed},
			Decision{Action: ActionJob, Op: jobs.OpPlan, Reason: "InputsChanged", PlanFlow: true}},
		{"an approved plan of other inputs: a new plan Job", DecideInput{Mutable: true, ManualApply: true, State: changed, Plan: planOf("h1:older", "p2:x", true)},
			Decision{Action: ActionJob, Op: jobs.OpPlan, Reason: "InputsChanged", PlanFlow: true}},
		{"a destructive plan is no Manual plan: a plan Job", DecideInput{Mutable: true, ManualApply: true, State: changed,
			Plan: &PlanView{Name: "d", Reason: infrav1.PlanReasonDestructive, InputsHash: "h1:new", PlanHash: "p2:x", Approved: true}},
			Decision{Action: ActionJob, Op: jobs.OpPlan, Reason: "InputsChanged", PlanFlow: true}},
		{"unapproved: wait, and the input change pauses the schedule", DecideInput{Mutable: true, ManualApply: true, State: changed, Plan: planOf("h1:new", "p2:x", false),
			DriftInterval: due, Now: t0},
			Decision{RequeueAfter: RetryMax, Reason: ReasonPlanAwaitingApproval, Plan: "c-0123456789", PlanFlow: true}},
		{"approved: the apply expects the plan", DecideInput{Mutable: true, ManualApply: true, State: changed, Plan: planOf("h1:new", "p2:x", true)},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged", ExpectPlan: "p2:x", Plan: "c-0123456789", PlanFlow: true}},
		{"an empty plan needs no approval", DecideInput{Mutable: true, ManualApply: true, State: changed, Jobs: JobsView{EmptyPlan: "h1:new"}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged", ExpectPlan: runner.EmptyPlanHash, PlanFlow: true}},
		{"an empty plan of other inputs: a plan Job", DecideInput{Mutable: true, ManualApply: true, State: changed, Jobs: JobsView{EmptyPlan: "h1:older"}},
			Decision{Action: ActionJob, Op: jobs.OpPlan, Reason: "InputsChanged", PlanFlow: true}},
		{"the first apply is not gated", DecideInput{Mutable: true, ManualApply: true, State: StateView{CurrentHash: "h1:new"}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "NoState"}},
		{"a destructive block does not apply on top", DecideInput{Mutable: true, ManualApply: true, State: changed, Plan: planOf("h1:new", "p2:x", true),
			Jobs: JobsView{BlockedHash: "h1:new"}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged", ExpectPlan: "p2:x", Plan: "c-0123456789", PlanFlow: true}},
		{"a retry of a failed apply is gated and backs off", DecideInput{Mutable: true, ManualApply: true, State: same, Plan: planOf("h1:a", "p2:x", true), Now: t0,
			Jobs: JobsView{LastApplyFailed: true, Failures: map[jobs.Op]int{jobs.OpApply: 1}, LastFailure: map[jobs.Op]time.Time{jobs.OpApply: t0.Add(-20 * time.Second)}}},
			Decision{RequeueAfter: 40 * time.Second, Reason: "LastApplyFailedBackoff", Plan: "c-0123456789", PlanFlow: true}},
		{"a failed plan Job backs off", DecideInput{Mutable: true, ManualApply: true, State: changed, Now: t0,
			Jobs: JobsView{Failures: map[jobs.Op]int{jobs.OpPlan: 1}, LastFailure: map[jobs.Op]time.Time{jobs.OpPlan: t0.Add(-20 * time.Second)}}},
			Decision{RequeueAfter: 40 * time.Second, Reason: "InputsChangedBackoff", PlanFlow: true}},
		{"a drift remediation is gated", DecideInput{Mutable: true, ManualApply: true, State: same, Remediate: true, Jobs: JobsView{FailedLimit: 3}},
			Decision{Action: ActionJob, Op: jobs.OpPlan, Reason: "DriftRemediation", PlanFlow: true}},
		{"a waiting remediation keeps the schedule, and its plan", DecideInput{Mutable: true, ManualApply: true, State: same, Remediate: true, Jobs: JobsView{FailedLimit: 3},
			Plan: planOf("h1:a", "p2:x", false), DriftInterval: due, Now: t0},
			Decision{Action: ActionJob, Op: jobs.OpDrift, Reason: "DriftDue", Plan: "c-0123456789", PlanFlow: true}},
		{"a waiting remediation requeues at the next deadline", DecideInput{Mutable: true, ManualApply: true, State: same, Remediate: true, Jobs: JobsView{FailedLimit: 3},
			Plan: planOf("h1:a", "p2:x", false), DriftInterval: due, LastDriftCheck: at(-26 * time.Minute), Now: t0},
			Decision{RequeueAfter: 4 * time.Minute, Reason: ReasonPlanAwaitingApproval, Plan: "c-0123456789", PlanFlow: true}},
		{"up to date: no plan flow", DecideInput{Mutable: true, ManualApply: true, State: same, Plan: planOf("h1:a", "p2:x", false)},
			Decision{Reason: "UpToDate"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := DecideOp(tt.in); got != tt.want {
				t.Errorf("DecideOp = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// planEnv is a provisioned cluster-kind object with applyPolicy Manual
// whose state records old inputs; its current inputs hash is hash.
type planEnv struct {
	*env
	hash string
	reg  cbmetrics.KubeRegistry
	// policy is the applyPolicy the adapter reports; Manual unless a test
	// changes it.
	policy *infrav1.ApplyPolicy
}

// newPlanEnv returns, failing t on error, a planEnv as described on the
// planEnv type: state at stateHash (or the current hash if empty), a
// successful older apply, and each mut applied to the machine.
func newPlanEnv(t *testing.T, stateHash string, mut ...func(*infrav1.TerraformMachine)) planEnv {
	t.Helper()
	return newPlanEnvWith(t, interceptor.Funcs{}, stateHash, mut...)
}

// newPlanEnvWith is newPlanEnv with funcs as the fake client's
// interceptors: it builds, using t, the planEnv with state at stateHash (or
// the current hash if empty) and each mut applied to the machine, and
// returns it.
func newPlanEnvWith(t *testing.T, funcs interceptor.Funcs, stateHash string, mut ...func(*infrav1.TerraformMachine)) planEnv {
	t.Helper()
	e := newEnvWith(t, funcs, world(machine(append([]func(*infrav1.TerraformMachine){withFinalizer, notPaused, provisioned}, mut...)...))...)
	e.d.ClusterOperationGate = true
	var reg cbmetrics.KubeRegistry
	e.d.Metrics, reg = recorder(t)
	h := currentHash(t)
	if stateHash == "" {
		stateHash = h
	}
	e.state.st = &state.State{InputsHash: stateHash}
	ok := job("a", jobs.OpApply, jobs.Succeeded, t0.Add(-2*time.Hour))
	ok.Annotations = map[string]string{state.InputsHashAnnotation: stateHash, BookkeptAnnotation: "true"}
	e.runner.jobs = append(e.runner.jobs, ok)
	return planEnv{env: e, hash: h, reg: reg, policy: new(infrav1.ApplyPolicyManual)}
}

// kind returns, failing t on error, the fakeKind adapter for e's object,
// set up as a mutable cluster with e's applyPolicy, a healthy reading and
// drift as its DriftPolicy (or a disabled policy when drift is nil).
func (e planEnv) kind(t *testing.T, drift *infrav1.DriftPolicy) *fakeKind {
	t.Helper()
	k := e.kindFor(t, readyOwner())
	k.asCluster, k.mutable, k.in = true, true, machineIn()
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	if drift == nil {
		drift = &infrav1.DriftPolicy{IntervalSeconds: new(int32(0))}
	}
	k.clusterDrift, k.applyPolicy, k.planRef = drift, *e.policy, &e.planRef
	return k
}

// reconcile runs one reconcile of e's object with drift as its DriftPolicy,
// failing t on error, and returns the requeue duration.
func (e planEnv) reconcile(t *testing.T, drift *infrav1.DriftPolicy) time.Duration {
	t.Helper()
	requeue, err := reconcileOnce(t, e.env, e.kind(t, drift))
	if err != nil {
		t.Fatal(err)
	}
	return requeue
}

// plans returns e's TerraformPlans, newest first, failing t on error.
func (e *env) plans(t *testing.T) []infrav1.TerraformPlan {
	t.Helper()
	list := &infrav1.TerraformPlanList{}
	if err := e.c.List(t.Context(), list, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(list.Items, newestPlanFirst)
	return list.Items
}

// plan returns e's TerraformPlan name, failing t when it does not exist.
func (e *env) plan(t *testing.T, name string) *infrav1.TerraformPlan {
	t.Helper()
	p := &infrav1.TerraformPlan{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, p); err != nil {
		t.Fatal(err)
	}
	return p
}

// approve approves e's TerraformPlan name as user, as the webhook allows
// it, failing t on error.
func (e *env) approve(t *testing.T, name, user string) {
	t.Helper()
	p := e.plan(t, name)
	p.Spec.Approved, p.Spec.ApprovedBy = new(true), user
	p.Generation++
	if err := e.c.Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
}

// finishRunner gives the runner's Job name an outcome at finishedAt and a
// pod whose termination message is result, failing t on error.
func (e *env) finishRunner(t *testing.T, name string, outcome jobs.Outcome, finishedAt time.Time, result string) {
	t.Helper()
	j := e.jobNamed(t, name)
	typ := batchv1.JobComplete
	if outcome == jobs.Failed {
		typ = batchv1.JobFailed
	}
	j.Status.Conditions = []batchv1.JobCondition{{Type: typ, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(finishedAt)}}
	e.runner.pods[name] = []corev1.Pod{*podWith(result, "")}
}

// newest returns the name of the Job the runner created last, failing t if
// none were created.
func (e *env) newest(t *testing.T) string {
	t.Helper()
	if len(e.runner.created) == 0 {
		t.Fatal("no Job created")
	}
	return e.runner.created[len(e.runner.created)-1]
}

// planResult returns the termination message of a Job of op with plan p,
// or, when errKind is set, of an apply that stopped with that error kind.
func planResult(op string, p *runner.Plan, errKind string) string {
	r := runner.Result{Version: runner.ResultVersion, Op: op, Steps: []runner.Step{{Name: "init"}, {Name: "plan", Exit: 2}, {Name: "show-json"}}, Plan: p}
	if errKind != "" {
		r.Error = &runner.Error{Kind: errKind, Tail: "the plan changed since it was approved"}
	}
	return string(runner.Encode(r))
}

// planApplyReason returns e's stored object's ApplyJobSucceeded condition,
// failing t on error.
func planApplyReason(t *testing.T, e planEnv) *metav1.Condition {
	t.Helper()
	return conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition)
}

// checkPlan fails t unless p is in phase, its label and status agreeing,
// with its Ready condition at reason ready and its Approved condition at
// reason approvedReason.
func checkPlan(t *testing.T, p *infrav1.TerraformPlan, phase infrav1.PlanPhase, ready, approvedReason string) {
	t.Helper()
	if p.Labels[infrav1.PlanPhaseLabel] != string(phase) || p.Status.Phase != phase {
		t.Errorf("plan %s: label %q, status.phase %q, want %s", p.Name, p.Labels[infrav1.PlanPhaseLabel], p.Status.Phase, phase)
	}
	if got := conditions.GetReason(p, infrav1.ReadyCondition); got != ready {
		t.Errorf("plan %s: Ready reason %q, want %q", p.Name, got, ready)
	}
	if got := conditions.GetReason(p, infrav1.PlanApprovedCondition); got != approvedReason {
		t.Errorf("plan %s: Approved reason %q, want %q", p.Name, got, approvedReason)
	}
	if p.Status.ObservedGeneration < 1 {
		t.Errorf("plan %s: observedGeneration %d", p.Name, p.Status.ObservedGeneration)
	}
}

// TestPlanFlow walks a change through applyPolicy Manual: a plan Job (run
// lease only, nothing applied), its TerraformPlan (labels, owner, summary,
// status, status.pendingPlanRef), the condition with the approve command
// and one PlanReady, no Job while it waits, the approval (PlanApproved
// naming the approver), the approved apply with --expect-plan and the
// plan's name, a changed plan that fails the plan and waits as a new one
// without backoff, and the success that applies the new plan.
func TestPlanFlow(t *testing.T) {
	t.Parallel()
	e := newPlanEnv(t, "h1:old")
	if requeue := e.reconcile(t, nil); requeue != ActiveJobRequeue {
		t.Fatalf("requeue = %s", requeue)
	}
	planJob := e.newest(t)
	args := sourceArgs(e.jobNamed(t, planJob))
	if jobs.OpOf(e.jobNamed(t, planJob)) != jobs.OpPlan || !slices.Contains(args, "--op=plan") ||
		slices.Contains(args, "--guard-deletes") || slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--expect-plan") }) {
		t.Fatalf("plan Job %s args = %v", planJob, args)
	}
	if h := e.jobNamed(t, planJob).Annotations[state.InputsHashAnnotation]; h != e.hash {
		t.Errorf("plan Job inputs hash = %q, want %s", h, e.hash)
	}
	if e.lease(t, runLeaseOf(t, state.KindTerraformCluster, testName)) == nil {
		t.Error("the plan Job holds no run lease")
	}
	if e.lease(t, runlease.ClusterName(testNS, "c1")) != nil {
		t.Error("the plan Job took the cluster write lease; it only reads")
	}
	if _, err := inputs.Read(t.Context(), e.c, testNS, "c", testName); !errors.Is(err, inputs.ErrNotFound) {
		t.Errorf("the plan Job wrote the durable inputs: %v", err)
	}
	if n := e.rec.count(EventInputsChanged); n != 1 {
		t.Errorf("%d InputsChanged events", n)
	}

	// The plan: a TerraformPlan, the condition and one PlanReady; no apply.
	p1 := &runner.Plan{Hash: runner.PlanHash([]string{"module.role.lb|update", "module.role.sg|delete"}), Update: 1, Delete: 1,
		Resources: []string{"module.role.lb (update)", "module.role.sg (delete)"}}
	e.finishRunner(t, planJob, jobs.Succeeded, t0.Add(-30*time.Minute), planResult(runner.OpPlan, p1, ""))
	for i := range 2 {
		if requeue := e.reconcile(t, nil); requeue != RetryMax {
			t.Errorf("pass %d: requeue = %s, want %s", i, requeue, RetryMax)
		}
	}
	if len(e.runner.created) != 1 {
		t.Fatalf("created %v while the plan waits", e.runner.created)
	}
	plans := e.plans(t)
	if len(plans) != 1 {
		t.Fatalf("%d TerraformPlans, want 1", len(plans))
	}
	tp := plans[0]
	if tp.Name != planName(testName, planJob, p1.Hash) || tp.Spec.PlanHash != p1.Hash || tp.Spec.InputsHash != e.hash ||
		tp.Spec.Reason != infrav1.PlanReasonManual || tp.Spec.TargetRef != (infrav1.PlanTargetRef{Kind: infrav1.PlanTargetCluster, Name: testName}) ||
		*tp.Spec.Summary.Delete != 1 || *tp.Spec.Summary.Update != 1 || !slices.Equal(tp.Spec.Summary.Resources, p1.Resources) || tp.Spec.Approved != nil {
		t.Errorf("TerraformPlan = %+v", tp)
	}
	if tp.Labels[infrav1.PlanDestructiveLabel] != "true" || tp.Labels[infrav1.PlanReasonLabel] != "Manual" || tp.Labels[clusterv1.ClusterNameLabel] != "c1" {
		t.Errorf("labels = %v", tp.Labels)
	}
	if ref := metav1.GetControllerOf(&tp); ref == nil || ref.UID != "m1-uid" || ref.Kind != state.KindTerraformCluster || ref.BlockOwnerDeletion != nil {
		t.Errorf("controller ref = %+v", ref)
	}
	checkPlan(t, &tp, infrav1.PlanPhasePending, infrav1.PlanPendingReason, infrav1.PlanPendingReason)
	if e.planRef.Name != tp.Name {
		t.Errorf("status.pendingPlanRef = %+v, want %s", e.planRef, tp.Name)
	}
	cmd := planApproveCommand(tp.Name, testNS)
	if c := planApplyReason(t, e); c == nil || c.Status != metav1.ConditionUnknown || c.Reason != infrav1.PlanAwaitingApprovalReason ||
		!strings.HasSuffix(c.Message, cmd) || !strings.HasPrefix(c.Message, "TerraformPlan "+tp.Name+" ") {
		t.Fatalf("ApplyJobSucceeded = %+v, want PlanAwaitingApproval ending in %q", c, cmd)
	}
	if got := e.rec.only(EventPlanReady); len(got) != 1 || got[0].eventType != corev1.EventTypeNormal ||
		!strings.Contains(got[0].note, "0 to create, 1 to update, 0 to replace, 1 to delete") || !strings.Contains(got[0].note, tp.Name) {
		t.Errorf("PlanReady = %+v", got)
	}
	for _, ev := range e.rec.only(EventConditionChanged) {
		if strings.HasPrefix(ev.note, infrav1.ApplyJobSucceededCondition) {
			t.Errorf("ConditionChanged for the wait: %s", ev.note)
		}
	}

	// Bookkept: its pod is not read again, and it still waits.
	j := e.jobNamed(t, planJob)
	j.Annotations[BookkeptAnnotation], j.Annotations[PlanHashAnnotation] = "true", p1.Hash
	e.runner.pods[planJob] = nil
	for range 2 {
		e.reconcile(t, nil)
	}
	if len(e.runner.created) != 1 || len(e.plans(t)) != 1 || e.rec.count(EventPlanReady) != 1 {
		t.Fatalf("bookkept: created %v, %d plans, %d PlanReady", e.runner.created, len(e.plans(t)), e.rec.count(EventPlanReady))
	}

	// Approved: PlanApproved names the approver; the apply plans again
	// and applies only this plan.
	e.approve(t, tp.Name, "alice")
	e.reconcile(t, nil)
	if got := e.rec.only(EventPlanApproved); len(got) != 1 || !strings.Contains(got[0].note, "alice") || !strings.Contains(got[0].note, tp.Name) {
		t.Errorf("PlanApproved = %+v", got)
	}
	checkPlan(t, e.plan(t, tp.Name), infrav1.PlanPhaseApproved, infrav1.PlanApprovedReason, infrav1.PlanApprovedReason)
	if len(e.runner.created) != 2 {
		t.Fatalf("created %v, want the approved apply", e.runner.created)
	}
	apply1 := e.newest(t)
	j = e.jobNamed(t, apply1)
	if args := sourceArgs(j); jobs.OpOf(j) != jobs.OpApply || !slices.Contains(args, "--expect-plan="+p1.Hash) || !slices.Contains(args, "--guard-deletes") {
		t.Errorf("approved apply args = %v", args)
	}
	if j.Annotations[ApprovedPlanAnnotation] != p1.Hash || j.Annotations[PlanAnnotation] != tp.Name {
		t.Errorf("approved apply annotations = %v", j.Annotations)
	}
	if e.rec.count(EventInputsChanged) != 1 {
		t.Errorf("%d InputsChanged; want 1", e.rec.count(EventInputsChanged))
	}
	if e.lease(t, runlease.ClusterName(testNS, "c1")) == nil {
		t.Error("the approved apply did not take the cluster write lease")
	}

	// The plan changed: the plan fails, and the new plan waits for its own
	// approval, without backoff.
	p2 := &runner.Plan{Hash: runner.PlanHash([]string{"module.role.lb|update"}), Update: 1, Resources: []string{"module.role.lb (update)"}}
	e.finishRunner(t, apply1, jobs.Failed, t0.Add(-20*time.Minute), planResult(runner.OpApply, p2, runner.ErrorKindPlanChanged))
	e.reconcile(t, nil)
	if len(e.runner.created) != 2 {
		t.Fatalf("created %v after a changed plan", e.runner.created)
	}
	checkPlan(t, e.plan(t, tp.Name), infrav1.PlanPhaseFailed, infrav1.PlanFailedReason, infrav1.PlanApprovedReason)
	tp2 := e.plan(t, planName(testName, apply1, p2.Hash))
	if tp2.Spec.PlanHash != p2.Hash || tp2.Spec.InputsHash != e.hash || tp2.Spec.Reason != infrav1.PlanReasonManual || tp2.Labels[infrav1.PlanDestructiveLabel] != "false" {
		t.Errorf("new TerraformPlan = %+v", tp2)
	}
	checkPlan(t, tp2, infrav1.PlanPhasePending, infrav1.PlanPendingReason, infrav1.PlanPendingReason)
	if e.planRef.Name != tp2.Name {
		t.Errorf("status.pendingPlanRef = %+v, want %s", e.planRef, tp2.Name)
	}
	cmd2 := planApproveCommand(tp2.Name, testNS)
	if c := planApplyReason(t, e); c == nil || c.Status != metav1.ConditionUnknown || c.Reason != infrav1.PlanChangedReason ||
		!strings.HasSuffix(c.Message, cmd2) || !strings.HasPrefix(c.Message, "Job "+apply1+": ") || !strings.Contains(c.Message, tp.Name) {
		t.Fatalf("ApplyJobSucceeded = %+v, want PlanChanged ending in %q", c, cmd2)
	}
	if got := e.rec.only(EventPlanChanged); len(got) != 1 || got[0].eventType != corev1.EventTypeWarning || !strings.Contains(got[0].note, tp2.Name) {
		t.Errorf("PlanChanged = %+v", got)
	}
	if n := e.rec.count(EventJobFailed); n != 0 {
		t.Errorf("%d JobFailed events for a changed plan", n)
	}
	if lr := e.get(t).Status.LastRun; lr.Error.Kind != infrav1.RunErrorKindPlanChanged {
		t.Errorf("lastRun.error = %+v", lr.Error)
	}
	e.approve(t, tp2.Name, "bob")
	e.reconcile(t, nil)
	if len(e.runner.created) != 3 {
		t.Fatalf("created %v; a changed plan must not back off", e.runner.created)
	}
	apply2 := e.newest(t)
	if a := e.jobNamed(t, apply2); !slices.Contains(sourceArgs(a), "--expect-plan="+p2.Hash) || a.Annotations[PlanAnnotation] != tp2.Name {
		t.Errorf("second apply args = %v, annotations %v", sourceArgs(a), a.Annotations)
	}

	// Success: the plan is Applied, once, and no plan is live.
	e.finishRunner(t, apply2, jobs.Succeeded, t0.Add(-10*time.Minute), planResult(runner.OpApply, nil, ""))
	e.state.st.InputsHash = e.hash
	for range 2 {
		e.reconcile(t, nil)
	}
	checkPlan(t, e.plan(t, tp2.Name), infrav1.PlanPhaseApplied, infrav1.PlanAppliedReason, infrav1.PlanApprovedReason)
	if e.planRef.Name != "" {
		t.Errorf("status.pendingPlanRef = %+v, want empty", e.planRef)
	}
	if n := e.rec.count(EventPlanApplied); n != 1 {
		t.Errorf("%d PlanApplied events", n)
	}
	if c := planApplyReason(t, e); c == nil || c.Reason != infrav1.ApplySucceededReason {
		t.Errorf("ApplyJobSucceeded = %+v", c)
	}
	if len(e.runner.created) != 3 {
		t.Errorf("created %v after the success", e.runner.created)
	}
	want := `
# HELP captf_plan_approvals_total [ALPHA] ` + planApprovalsHelp(t) + `
# TYPE captf_plan_approvals_total counter
captf_plan_approvals_total{kind="TerraformCluster",result="applied"} 1
captf_plan_approvals_total{kind="TerraformCluster",result="approved"} 2
captf_plan_approvals_total{kind="TerraformCluster",result="created"} 2
captf_plan_approvals_total{kind="TerraformCluster",result="failed"} 1
`
	if err := testutil.GatherAndCompare(e.reg, strings.NewReader(want), metrics.PlanApprovalsName); err != nil {
		t.Error(err)
	}
	if n, err := gatherAndCount(e.reg, metrics.JobsTotalName); err != nil || n != 3 {
		t.Errorf("captf_jobs_total series = %d, %v; want plan succeeded, apply plan_changed and apply succeeded", n, err)
	}
}

// planApprovalsHelp returns the HELP text registered for
// metrics.PlanApprovalsName, failing t when no such spec exists.
func planApprovalsHelp(t *testing.T) string {
	t.Helper()
	for _, s := range metrics.Specs() {
		if s.Name == metrics.PlanApprovalsName {
			return s.Help
		}
	}
	t.Fatal("no captf_plan_approvals_total spec")
	return ""
}

// TestPlanEmptyDoesNotWait: a plan without changes needs no approval and
// becomes no TerraformPlan; the apply runs at once, expecting the empty
// plan.
func TestPlanEmptyDoesNotWait(t *testing.T) {
	t.Parallel()
	e := newPlanEnv(t, "h1:old")
	e.reconcile(t, nil)
	plan := e.newest(t)
	e.finishRunner(t, plan, jobs.Succeeded, t0.Add(-time.Minute), planResult(runner.OpPlan, runner.EmptyPlan(), ""))
	if requeue := e.reconcile(t, nil); requeue != ActiveJobRequeue || len(e.runner.created) != 2 {
		t.Fatalf("requeue %s, created %v; want the apply at once", requeue, e.runner.created)
	}
	if a := e.jobNamed(t, e.newest(t)); !slices.Contains(sourceArgs(a), "--expect-plan="+runner.EmptyPlanHash) || a.Annotations[PlanAnnotation] != "" {
		t.Errorf("apply args = %v, annotations %v", sourceArgs(a), a.Annotations)
	}
	if len(e.plans(t)) != 0 {
		t.Errorf("an empty plan became a TerraformPlan")
	}
	if e.rec.count(EventPlanReady) != 1 || e.rec.count(EventPlanApproved) != 0 {
		t.Errorf("%d PlanReady, %d PlanApproved; want 1 and 0", e.rec.count(EventPlanReady), e.rec.count(EventPlanApproved))
	}
	if c := planApplyReason(t, e); c != nil && c.Reason == infrav1.PlanAwaitingApprovalReason {
		t.Errorf("ApplyJobSucceeded = %+v; an empty plan never waits", c)
	}
}

// TestPlanHashMarked: bookkeeping records the hash of a finished Job's
// plan on it, since its result is not read again; a Job without a plan
// gets none.
func TestPlanHashMarked(t *testing.T) {
	t.Parallel()
	p, a := job("p", jobs.OpPlan, jobs.Succeeded, t0), job("a", jobs.OpApply, jobs.Succeeded, t0)
	c := fake.NewClientBuilder().WithObjects(&p, &a).Build()
	bk := &Bookkeeping{unmarked: []finished{
		{job: p.DeepCopy(), ok: true, result: &jobs.Result{Plan: runner.EmptyPlan()}},
		{job: a.DeepCopy(), ok: true, result: &jobs.Result{}},
	}}
	if err := bk.MarkBookkept(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"p": runner.EmptyPlanHash, "a": ""} {
		got := &batchv1.Job{}
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, got); err != nil {
			t.Fatal(err)
		}
		if got.Annotations[PlanHashAnnotation] != want || got.Annotations[BookkeptAnnotation] != "true" {
			t.Errorf("Job %s annotations = %v, want plan hash %q", name, got.Annotations, want)
		}
	}
}

// TestPlanJobFailed: a failed plan Job sets ApplyFailed naming it, emits
// JobFailed once, makes no TerraformPlan, and the next plan Job backs off.
func TestPlanJobFailed(t *testing.T) {
	t.Parallel()
	e := newPlanEnv(t, "h1:old")
	e.reconcile(t, nil)
	plan := e.newest(t)
	step := "plan"
	res := string(runner.Encode(runner.Result{Version: runner.ResultVersion, Op: runner.OpPlan,
		Error: &runner.Error{Kind: runner.ErrorKindStep, Step: &step, Tail: "Error: bad"}}))
	e.finishRunner(t, plan, jobs.Failed, t0.Add(-10*time.Second), res)
	for range 2 {
		if requeue := e.reconcile(t, nil); requeue <= 0 || requeue > RetryBase {
			t.Errorf("requeue = %s, want the plan's backoff", requeue)
		}
	}
	if len(e.runner.created) != 1 {
		t.Fatalf("created %v during the backoff", e.runner.created)
	}
	if c := planApplyReason(t, e); c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.ApplyFailedReason || !strings.HasPrefix(c.Message, "Job "+plan) {
		t.Errorf("ApplyJobSucceeded = %+v", c)
	}
	if n := e.rec.count(EventJobFailed); n != 1 {
		t.Errorf("%d JobFailed events", n)
	}
	if len(e.plans(t)) != 0 || e.planRef.Name != "" {
		t.Errorf("a failed plan Job made a plan: %v, %+v", e.plans(t), e.planRef)
	}
}

// TestPlanNoStateNotGated: the first apply of a new cluster runs without a
// plan Job: there is nothing to break.
func TestPlanNoStateNotGated(t *testing.T) {
	t.Parallel()
	e := newPlanEnv(t, "h1:old")
	e.state.st = nil
	e.runner.jobs = nil
	obj := e.get(t)
	obj.Status.Initialization.Provisioned = nil
	if err := e.c.Status().Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t, nil)
	if len(e.runner.created) != 1 || jobs.OpOf(e.jobNamed(t, e.newest(t))) != jobs.OpApply {
		t.Fatalf("created %v, want the first apply", e.runner.created)
	}
	if slices.ContainsFunc(sourceArgs(e.jobNamed(t, e.newest(t))), func(a string) bool { return strings.HasPrefix(a, "--expect-plan") }) {
		t.Error("the first apply expects a plan")
	}
}

// TestPlanDriftRemediationGated: a drift remediation plans first and waits
// for the approval; drift checks keep running meanwhile and keep the plan.
func TestPlanDriftRemediationGated(t *testing.T) {
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
	if jobs.OpOf(e.jobNamed(t, plan)) != jobs.OpPlan || e.rec.count(EventDriftRemediationStarted) != 0 {
		t.Fatalf("created %v, %d DriftRemediationStarted; want a plan Job only", e.runner.created, e.rec.count(EventDriftRemediationStarted))
	}
	p := &runner.Plan{Hash: runner.PlanHash([]string{"module.role.tags|update"}), Update: 1, Resources: []string{"module.role.tags (update)"}}
	e.finishRunner(t, plan, jobs.Succeeded, t0.Add(-time.Minute), planResult(runner.OpPlan, p, ""))
	// The drift check is due: it runs, and the plan stays.
	e.reconcile(t, remediate)
	if len(e.runner.created) != 2 || jobs.OpOf(e.jobNamed(t, e.newest(t))) != jobs.OpDrift {
		t.Fatalf("created %v, want the due drift check", e.runner.created)
	}
	name := planName(testName, plan, p.Hash)
	checkPlan(t, e.plan(t, name), infrav1.PlanPhasePending, infrav1.PlanPendingReason, infrav1.PlanPendingReason)
	if e.planRef.Name != name {
		t.Errorf("status.pendingPlanRef = %+v, want the remediation's plan kept", e.planRef)
	}
	if c := planApplyReason(t, e); c == nil || c.Reason != infrav1.PlanAwaitingApprovalReason {
		t.Errorf("ApplyJobSucceeded while the drift check runs = %+v", c)
	}
}

// TestPlanApprovedWhileJobRuns: an approval that arrives while another
// Job of the object runs is recorded at once (the plan is Approved, and
// PlanApproved emitted), and the approved apply starts once that Job
// finished.
func TestPlanApprovedWhileJobRuns(t *testing.T) {
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
	planJob := e.newest(t)
	p := &runner.Plan{Hash: runner.PlanHash([]string{"module.role.tags|update"}), Update: 1}
	e.finishRunner(t, planJob, jobs.Succeeded, t0.Add(-time.Minute), planResult(runner.OpPlan, p, ""))
	e.reconcile(t, remediate) // the due drift check starts
	drift := e.newest(t)
	name := planName(testName, planJob, p.Hash)
	e.approve(t, name, "alice")
	if requeue := e.reconcile(t, remediate); requeue != ActiveJobRequeue {
		t.Fatalf("requeue = %s while the drift Job runs", requeue)
	}
	checkPlan(t, e.plan(t, name), infrav1.PlanPhaseApproved, infrav1.PlanApprovedReason, infrav1.PlanApprovedReason)
	if e.rec.count(EventPlanApproved) != 1 || len(e.runner.created) != 2 {
		t.Fatalf("%d PlanApproved, created %v", e.rec.count(EventPlanApproved), e.runner.created)
	}
	e.finishRunner(t, drift, jobs.Succeeded, t0, string(runner.Encode(runner.Result{Version: runner.ResultVersion, Op: runner.OpDrift,
		Drift: &runner.Drift{Detected: true, Update: 1, Resources: []string{"module.role.tags (update)"}}})))
	e.reconcile(t, remediate)
	if len(e.runner.created) != 3 {
		t.Fatalf("created %v, want the approved remediation", e.runner.created)
	}
	if a := e.jobNamed(t, e.newest(t)); jobs.OpOf(a) != jobs.OpApply || a.Annotations[PlanAnnotation] != name {
		t.Errorf("approved remediation = %s, annotations %v", e.newest(t), a.Annotations)
	}
}

// TestPlanOneLivePerTarget: a plan of newer inputs supersedes the live one,
// so a target has at most one live plan, and status.pendingPlanRef names
// the newer one.
func TestPlanOneLivePerTarget(t *testing.T) {
	t.Parallel()
	e := newPlanEnv(t, "h1:old")
	e.reconcile(t, nil)
	first := e.newest(t)
	pa := &runner.Plan{Hash: runner.PlanHash([]string{"module.role.lb|update"}), Update: 1}
	e.finishRunner(t, first, jobs.Succeeded, t0.Add(-30*time.Minute), planResult(runner.OpPlan, pa, ""))
	e.reconcile(t, nil)
	older := planName(testName, first, pa.Hash)

	// A second plan Job, as for another change of the inputs, reports
	// another plan.
	second := job("c-plan-2", jobs.OpPlan, jobs.Succeeded, t0.Add(-10*time.Minute))
	second.Labels[jobs.OpLabel] = string(jobs.OpPlan)
	second.Annotations = map[string]string{state.InputsHashAnnotation: e.hash}
	e.runner.jobs = append(e.runner.jobs, second)
	pb := &runner.Plan{Hash: runner.PlanHash([]string{"module.role.lb|update", "module.role.sg|create"}), Create: 1, Update: 1}
	e.runner.pods[second.Name] = []corev1.Pod{*podWith(planResult(runner.OpPlan, pb, ""), "")}
	e.reconcile(t, nil)
	newer := planName(testName, second.Name, pb.Hash)
	checkPlan(t, e.plan(t, older), infrav1.PlanPhaseSuperseded, infrav1.PlanSupersededReason, infrav1.PlanNotApprovedReason)
	checkPlan(t, e.plan(t, newer), infrav1.PlanPhasePending, infrav1.PlanPendingReason, infrav1.PlanPendingReason)
	if e.planRef.Name != newer {
		t.Errorf("status.pendingPlanRef = %+v, want %s", e.planRef, newer)
	}
	if got := e.rec.only(EventPlanSuperseded); len(got) != 1 || got[0].eventType != corev1.EventTypeNormal || !strings.Contains(got[0].note, newer) {
		t.Errorf("PlanSuperseded = %+v", got)
	}
}

// TestPlanMoved: after clusterctl move the plans arrive without status and
// the Jobs stay behind. A plan's phase is rebuilt from its label and spec:
// a finished one stays finished, and an approved one is applied.
func TestPlanMoved(t *testing.T) {
	t.Parallel()
	e := newPlanEnv(t, "h1:old")
	owner := []metav1.OwnerReference{{APIVersion: infrav1.GroupVersion.String(), Kind: state.KindTerraformCluster, Name: testName, UID: "m1-uid", Controller: new(true)}}
	mk := func(name string, phase infrav1.PlanPhase, ok bool, at time.Time) *infrav1.TerraformPlan {
		p := &infrav1.TerraformPlan{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name, OwnerReferences: owner, CreationTimestamp: metav1.NewTime(at),
				Labels: map[string]string{infrav1.PlanPhaseLabel: string(phase)}},
			Spec: infrav1.TerraformPlanSpec{TargetRef: infrav1.PlanTargetRef{Kind: infrav1.PlanTargetCluster, Name: testName},
				PlanHash: "p2:" + name, InputsHash: e.hash, Reason: infrav1.PlanReasonManual, Summary: infrav1.PlanSummary{Update: new(int32(1))}},
		}
		if ok {
			p.Spec.Approved, p.Spec.ApprovedBy = new(true), "alice"
		}
		if err := e.c.Create(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mk("m1-done", infrav1.PlanPhaseSuperseded, false, t0.Add(-2*time.Hour))
	mk("m1-live", infrav1.PlanPhaseApproved, true, t0.Add(-time.Hour))
	// A plan of an earlier object of the same name is not this one's.
	foreign := mk("m1-foreign", infrav1.PlanPhasePending, false, t0)
	foreign.OwnerReferences[0].UID = "old-uid"
	if err := e.c.Update(t.Context(), foreign); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t, nil)
	checkPlan(t, e.plan(t, "m1-done"), infrav1.PlanPhaseSuperseded, infrav1.PlanSupersededReason, infrav1.PlanNotApprovedReason)
	checkPlan(t, e.plan(t, "m1-live"), infrav1.PlanPhaseApproved, infrav1.PlanApprovedReason, infrav1.PlanApprovedReason)
	if f := e.plan(t, "m1-foreign"); f.Status.Phase != "" {
		t.Errorf("a foreign plan was synced: %+v", f.Status)
	}
	if len(e.runner.created) != 1 {
		t.Fatalf("created %v, want the approved apply", e.runner.created)
	}
	if a := e.jobNamed(t, e.newest(t)); !slices.Contains(sourceArgs(a), "--expect-plan=p2:m1-live") || a.Annotations[PlanAnnotation] != "m1-live" {
		t.Errorf("apply args %v, annotations %v", sourceArgs(a), a.Annotations)
	}
	if e.rec.count(EventPlanApproved) != 0 {
		t.Errorf("a moved approval was announced again")
	}
}

// TestPlanChangedCostsNoBackoff: an apply stopped for a changed plan is no
// failure for backoff, retries or the remediation cap, and the mark
// survives bookkeeping; a plan Job without a readable plan is a failure.
func TestPlanChangedCostsNoBackoff(t *testing.T) {
	t.Parallel()
	bookkept := job("a3", jobs.OpApply, jobs.Failed, t0.Add(-time.Minute))
	bookkept.Annotations = map[string]string{BookkeptAnnotation: "true", PlanChangedAnnotation: "true"}
	done := []finished{
		{job: ptr(job("a4", jobs.OpApply, jobs.Failed, t0)), planChanged: true},
		{job: &bookkept, bookkept: true, planChanged: true},
		{job: ptr(job("a2", jobs.OpApply, jobs.Failed, t0.Add(-2*time.Minute)))},
	}
	bk := &Bookkeeping{View: JobsView{Failures: map[jobs.Op]int{}, LastFailure: map[jobs.Op]time.Time{}}}
	bk.countFailures(done)
	if bk.View.Failures[jobs.OpApply] != 1 {
		t.Errorf("apply failures = %d, want 1 (only a2)", bk.View.Failures[jobs.OpApply])
	}
	if n := remediationFailures(done, nil); n != 1 {
		t.Errorf("remediation failures = %d, want 1 (only a2)", n)
	}
	ok := finished{job: ptr(job("a5", jobs.OpApply, jobs.Succeeded, t0.Add(time.Minute))), ok: true}
	if n := retryNumber(append([]finished{ok}, done...), 0); n != 2 {
		t.Errorf("retry number = %d, want 2 (only a2 failed)", n)
	}
	if jobResult(done[0]) != metrics.ResultPlanChanged {
		t.Errorf("result = %s", jobResult(done[0]))
	}

	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	unreadable := job("p1", jobs.OpPlan, jobs.Succeeded, t0)
	e.runner.jobs = append(e.runner.jobs, unreadable)
	got, err := collectFinished(t.Context(), e.d, []batchv1.Job{bookkept, unreadable}, testNS, "s")
	if err != nil || len(got) != 2 || !got[0].planChanged || got[1].ok || !got[1].planUnreadable {
		t.Fatalf("collectFinished = %+v, %v; want a3 plan-changed, p1 failed as unreadable", got, err)
	}
	marked := unreadable.DeepCopy()
	marked.Annotations = map[string]string{BookkeptAnnotation: "true", PlanUnreadableAnnotation: "true"}
	if got, err := collectFinished(t.Context(), e.d, []batchv1.Job{*marked}, testNS, "s"); err != nil || got[0].ok {
		t.Errorf("bookkept unreadable plan = %+v, %v; want still failed", got, err)
	}
}

// TestEmptyPlan: only the newest plan, with no apply after it, counts, from
// its result or its PlanHashAnnotation.
func TestEmptyPlan(t *testing.T) {
	t.Parallel()
	planned := func(name string, op jobs.Op, outcome jobs.Outcome, hash string, annotated bool) finished {
		j := job(name, op, outcome, t0)
		j.Annotations = map[string]string{state.InputsHashAnnotation: "h1:" + name}
		f := finished{job: &j, ok: outcome == jobs.Succeeded}
		if annotated {
			j.Annotations[PlanHashAnnotation] = hash
		} else {
			f.result = &jobs.Result{Plan: &runner.Plan{Hash: hash}}
		}
		return f
	}
	changed := planned("a", jobs.OpApply, jobs.Failed, runner.EmptyPlanHash, false)
	changed.planChanged = true
	tests := []struct {
		name string
		done []finished
		want string
	}{
		{"none", nil, ""},
		{"an empty plan Job", []finished{planned("p", jobs.OpPlan, jobs.Succeeded, runner.EmptyPlanHash, false)}, "h1:p"},
		{"bookkept", []finished{planned("p", jobs.OpPlan, jobs.Succeeded, runner.EmptyPlanHash, true)}, "h1:p"},
		{"a plan with changes", []finished{planned("p", jobs.OpPlan, jobs.Succeeded, "p2:x", false)}, ""},
		{"an apply since", []finished{planned("a", jobs.OpApply, jobs.Succeeded, "", true), planned("p", jobs.OpPlan, jobs.Succeeded, runner.EmptyPlanHash, false)}, ""},
		{"a changed plan that is empty", []finished{changed}, "h1:a"},
		{"a drift check since does not matter", []finished{planned("d", jobs.OpDrift, jobs.Succeeded, "", true), planned("p", jobs.OpPlan, jobs.Succeeded, runner.EmptyPlanHash, false)}, "h1:p"},
	}
	for _, tt := range tests {
		if got := emptyPlan(tt.done); got != tt.want {
			t.Errorf("%s: emptyPlan = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestPlanWaitEvents: leaving a plan wait emits no stale Job outcome; a Job
// that finished this pass still reports its own.
func TestPlanWaitEvents(t *testing.T) {
	t.Parallel()
	wait := metav1.Condition{Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionUnknown, Reason: infrav1.PlanAwaitingApprovalReason,
		Message: "TerraformPlan m1-0123456789 plans inputs hash h1:x"}
	back := metav1.Condition{Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionTrue, Reason: infrav1.ApplySucceededReason, Message: "Job a"}
	old := finished{job: ptr(job("a", jobs.OpApply, jobs.Succeeded, t0)), ok: true, bookkept: true}
	if tr, ok := transitionFor(&wait, back, &Bookkeeping{byName: map[string]finished{"a": old}}); ok {
		t.Errorf("leaving the wait for a bookkept Job emitted %+v", tr)
	}
	fresh := finished{job: ptr(job("a", jobs.OpApply, jobs.Succeeded, t0)), ok: true}
	if tr, ok := transitionFor(&wait, back, &Bookkeeping{byName: map[string]finished{"a": fresh}}); !ok || tr.reason != EventJobSucceeded {
		t.Errorf("leaving the wait for a finished Job = %+v, %v", tr, ok)
	}
	changed := wait
	changed.Reason = infrav1.PlanChangedReason
	for _, prev := range []*metav1.Condition{nil, &back, &wait} {
		for _, c := range []metav1.Condition{wait, changed} {
			if tr, ok := transitionFor(prev, c, nil); ok {
				t.Errorf("%v → %s emitted %+v; PlanReady and PlanChanged come from bookkeeping", prev, c.Reason, tr)
			}
		}
	}
}

// TestPlanSummaryOf: an output-only plan records its output count and the
// summary names it instead of reading as no change; the resources are cut
// to the entries the API accepts.
func TestPlanSummaryOf(t *testing.T) {
	t.Parallel()
	s := summaryOf(&runner.Plan{Hash: "p2:x", OutputChanges: 2})
	if s.OutputChanges == nil || *s.OutputChanges != 2 || s.Import != nil || *s.Create != 0 || destructive(s) {
		t.Fatalf("summary = %+v", s)
	}
	if got, want := planCounts(s), "0 to create, 0 to update, 0 to replace, 0 to delete, 2 output(s) to change"; got != want {
		t.Errorf("planCounts = %q, want %q", got, want)
	}
	if got := planCounts(infrav1.PlanSummary{Create: new(int32(1)), Import: new(int32(2))}); strings.Contains(got, "output") || !strings.HasSuffix(got, ", 2 to import") {
		t.Errorf("planCounts = %q", got)
	}
	res := &runner.Plan{Hash: "p2:x", Create: 60, Replace: 1}
	for range infrav1.MaxPlanSummaryResources + 3 {
		res.Resources = append(res.Resources, strings.Repeat("é", 400)+" (create)")
	}
	s = summaryOf(res)
	if len(s.Resources) != infrav1.MaxPlanSummaryResources || s.Truncated == nil || !*s.Truncated || *s.Create != 60 || !destructive(s) {
		t.Fatalf("summary = %d resources, %+v", len(s.Resources), s)
	}
	for _, r := range s.Resources {
		if len(r) > maxPlanEntry || !strings.HasPrefix(r, "é") || strings.ContainsRune(r, '�') {
			t.Fatalf("entry of %d bytes", len(r))
		}
	}
}

// TestPlanName: a plan's name is the target's, cut to fit, and a digest of
// the Job and the plan hash: the same Job names the same plan, another Job
// another.
func TestPlanName(t *testing.T) {
	t.Parallel()
	a, b := planName("demo", "j1", "p2:x"), planName("demo", "j2", "p2:x")
	if a != planName("demo", "j1", "p2:x") || a == b || !strings.HasPrefix(a, "demo-") || len(a) != len("demo-")+planNameHex {
		t.Errorf("planName = %q, %q", a, b)
	}
	if long := planName(strings.Repeat("x", 300), "j", "p2:x"); len(long) != 253 {
		t.Errorf("long name of %d bytes", len(long))
	}
	if got := PlanTargetIndexer(&infrav1.TerraformPlan{Spec: infrav1.TerraformPlanSpec{TargetRef: infrav1.PlanTargetRef{Kind: infrav1.PlanTargetMachinePool, Name: "p"}}}); !slices.Equal(got, []string{"TerraformMachinePool/p"}) {
		t.Errorf("index = %v", got)
	}
	if got := PlanTargetIndexer(&infrav1.TerraformCluster{}); got != nil {
		t.Errorf("index of another kind = %v", got)
	}
}

// TestPlanCreateFailure: a TerraformPlan that cannot be created keeps the
// pass's Jobs unmarked, so the next pass reads the plan again and creates
// it then.
func TestPlanCreateFailure(t *testing.T) {
	t.Parallel()
	e := newPlanEnv(t, "h1:old")
	e.reconcile(t, nil)
	planJob := e.newest(t)
	p := &runner.Plan{Hash: runner.PlanHash([]string{"module.role.lb|update"}), Update: 1}
	e.finishRunner(t, planJob, jobs.Succeeded, t0.Add(-time.Minute), planResult(runner.OpPlan, p, ""))
	fail := true
	e.d.Client = failingCreates{Client: e.c, fail: &fail}
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err == nil {
		t.Fatal("no error from a failed create")
	}
	if j := e.jobNamed(t, planJob); j.Annotations[BookkeptAnnotation] == "true" {
		t.Fatal("the plan Job was marked bookkept although its plan was not recorded")
	}
	fail = false
	e.reconcile(t, nil)
	if len(e.plans(t)) != 1 || e.rec.count(EventPlanReady) != 1 {
		t.Errorf("%d plans, %d PlanReady after the retry", len(e.plans(t)), e.rec.count(EventPlanReady))
	}
}

// failingCreates is a client whose TerraformPlan creates fail while *fail.
type failingCreates struct {
	client.Client
	fail *bool
}

// Create fails a TerraformPlan create while *f.fail, else creates obj
// using ctx and opts. It returns the injected error, or the embedded
// client's.
func (f failingCreates) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*infrav1.TerraformPlan); ok && *f.fail {
		return errors.New("etcd unavailable")
	}
	return f.Client.Create(ctx, obj, opts...)
}

// TestPlanSupersededWhenStale: a live plan the pass's decision is not
// about is superseded, with a Normal PlanSuperseded saying why: a Manual
// plan whose inputs changed (a plan Job of the new ones starts in the
// same pass), were reverted to the state's, or whose applyPolicy is no
// longer Manual, and a Destructive plan once applyPolicy is Manual or its
// change is reverted.
func TestPlanSupersededWhenStale(t *testing.T) {
	t.Parallel()
	waiting := func(t *testing.T) (planEnv, string) {
		t.Helper()
		e := newPlanEnv(t, "h1:old")
		e.reconcile(t, nil)
		planJob := e.newest(t)
		p := &runner.Plan{Hash: runner.PlanHash([]string{"module.role.lb|update"}), Update: 1}
		e.finishRunner(t, planJob, jobs.Succeeded, t0.Add(-time.Minute), planResult(runner.OpPlan, p, ""))
		e.reconcile(t, nil)
		name := planName(testName, planJob, p.Hash)
		checkPlan(t, e.plan(t, name), infrav1.PlanPhasePending, infrav1.PlanPendingReason, infrav1.PlanPendingReason)
		return e, name
	}
	superseded := func(t *testing.T, e planEnv, name, why string) {
		t.Helper()
		checkPlan(t, e.plan(t, name), infrav1.PlanPhaseSuperseded, infrav1.PlanSupersededReason, infrav1.PlanNotApprovedReason)
		got := e.rec.only(EventPlanSuperseded)
		if len(got) != 1 || got[0].eventType != corev1.EventTypeNormal || !strings.Contains(got[0].note, why) {
			t.Errorf("PlanSuperseded = %+v, want one saying %q", got, why)
		}
		if e.planRef.Name != "" {
			t.Errorf("status.pendingPlanRef = %+v", e.planRef)
		}
	}
	t.Run("inputs changed", func(t *testing.T) {
		t.Parallel()
		e, name := waiting(t)
		k := e.kind(t, nil)
		edited := machineIn()
		edited.MachineName = "m2"
		k.in = edited
		if _, err := reconcileOnce(t, e.env, k); err != nil {
			t.Fatal(err)
		}
		superseded(t, e, name, "the inputs changed since it was planned")
		if len(e.runner.created) != 2 || jobs.OpOf(e.jobNamed(t, e.newest(t))) != jobs.OpPlan {
			t.Errorf("created %v, want a plan Job of the new inputs", e.runner.created)
		}
	})
	t.Run("inputs reverted", func(t *testing.T) {
		t.Parallel()
		e, name := waiting(t)
		e.state.st.InputsHash = e.hash
		e.jobNamed(t, "a").Annotations[state.InputsHashAnnotation] = e.hash
		e.reconcile(t, nil)
		superseded(t, e, name, "no apply of its inputs is due any more")
	})
	t.Run("applyPolicy Automatic", func(t *testing.T) {
		t.Parallel()
		e, name := waiting(t)
		*e.policy = infrav1.ApplyPolicyAutomatic
		e.reconcile(t, nil)
		superseded(t, e, name, "applyPolicy is no longer Manual")
		if a := e.jobNamed(t, e.newest(t)); jobs.OpOf(a) != jobs.OpApply || slices.ContainsFunc(sourceArgs(a), func(s string) bool { return strings.HasPrefix(s, "--expect-plan") }) {
			t.Errorf("created %v, want the guarded apply", e.runner.created)
		}
	})
	t.Run("destructive, applyPolicy Manual", func(t *testing.T) {
		t.Parallel()
		e := newBlockedEnv(t, "h1:old", false)
		if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
			t.Fatal(err)
		}
		name := planName(testName, "b", blockedPlan().Hash)
		k := e.kind(t, nil)
		k.applyPolicy = infrav1.ApplyPolicyManual
		if _, err := reconcileOnce(t, e.env, k); err != nil {
			t.Fatal(err)
		}
		checkPlan(t, e.plan(t, name), infrav1.PlanPhaseSuperseded, infrav1.PlanSupersededReason, infrav1.PlanNotApprovedReason)
		if len(e.runner.created) != 1 || jobs.OpOf(e.jobNamed(t, e.newest(t))) != jobs.OpPlan {
			t.Errorf("created %v, want a plan Job", e.runner.created)
		}
	})
}

// TestPlanKeptWhilePausedOrRunning: a stale plan is not superseded while
// the object is paused (clusterctl move copies it) or a Job runs (its
// approved apply may be the one running).
func TestPlanKeptWhilePausedOrRunning(t *testing.T) {
	t.Parallel()
	e := newPlanEnv(t, "h1:old")
	e.reconcile(t, nil)
	planJob := e.newest(t)
	p := &runner.Plan{Hash: runner.PlanHash([]string{"module.role.lb|update"}), Update: 1}
	e.finishRunner(t, planJob, jobs.Succeeded, t0.Add(-time.Minute), planResult(runner.OpPlan, p, ""))
	e.reconcile(t, nil)
	name := planName(testName, planJob, p.Hash)
	e.approve(t, name, "alice")
	e.reconcile(t, nil) // the approved apply starts
	*e.policy = infrav1.ApplyPolicyAutomatic
	e.reconcile(t, nil) // runs
	checkPlan(t, e.plan(t, name), infrav1.PlanPhaseApproved, infrav1.PlanApprovedReason, infrav1.PlanApprovedReason)

	e.runner.jobs = e.runner.jobs[:1]
	obj := e.get(t)
	obj.Annotations = map[string]string{clusterv1.PausedAnnotation: "true"}
	if err := e.c.Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t, nil)
	checkPlan(t, e.plan(t, name), infrav1.PlanPhaseApproved, infrav1.PlanApprovedReason, infrav1.PlanApprovedReason)
	if e.rec.count(EventPlanSuperseded) != 0 {
		t.Errorf("%d PlanSuperseded while running or paused", e.rec.count(EventPlanSuperseded))
	}
}

// TestPlanApprovalRace: an approval that lands just before the plan is
// superseded (the webhook refuses one after) is ignored: Approved
// False/ApprovalIgnored and one Warning naming the approver and the plan
// live now.
func TestPlanApprovalRace(t *testing.T) {
	t.Parallel()
	e := newPlanEnv(t, "h1:old")
	owner := []metav1.OwnerReference{{APIVersion: infrav1.GroupVersion.String(), Kind: state.KindTerraformCluster, Name: testName, UID: "m1-uid", Controller: new(true)}}
	for _, p := range []*infrav1.TerraformPlan{
		{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "m1-raced", OwnerReferences: owner, CreationTimestamp: metav1.NewTime(t0.Add(-time.Hour)),
			Labels: map[string]string{infrav1.PlanPhaseLabel: string(infrav1.PlanPhaseSuperseded)}},
			Spec: infrav1.TerraformPlanSpec{TargetRef: infrav1.PlanTargetRef{Kind: infrav1.PlanTargetCluster, Name: testName}, PlanHash: "p2:a", InputsHash: "h1:a",
				Reason: infrav1.PlanReasonManual, Summary: infrav1.PlanSummary{Update: new(int32(1))}, Approved: new(true), ApprovedBy: "alice"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "m1-now", OwnerReferences: owner, CreationTimestamp: metav1.NewTime(t0),
			Labels: map[string]string{infrav1.PlanPhaseLabel: string(infrav1.PlanPhasePending)}},
			Spec: infrav1.TerraformPlanSpec{TargetRef: infrav1.PlanTargetRef{Kind: infrav1.PlanTargetCluster, Name: testName}, PlanHash: "p2:b", InputsHash: e.hash,
				Reason: infrav1.PlanReasonManual, Summary: infrav1.PlanSummary{Update: new(int32(1))}}},
	} {
		if err := e.c.Create(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		e.reconcile(t, nil)
	}
	checkPlan(t, e.plan(t, "m1-raced"), infrav1.PlanPhaseSuperseded, infrav1.PlanSupersededReason, infrav1.PlanApprovalIgnoredReason)
	got := e.rec.only(EventPlanSuperseded)
	if len(got) != 1 || got[0].eventType != corev1.EventTypeWarning || !strings.Contains(got[0].note, "alice") || !strings.Contains(got[0].note, "m1-now") {
		t.Errorf("PlanSuperseded = %+v, want one Warning naming alice and m1-now", got)
	}
}

// TestPrunePlans: the newest maxFinishedPlans finished plans of the
// target stay, older ones go, and a live plan stays however old; nothing
// is pruned while the object is paused.
func TestPrunePlans(t *testing.T) {
	t.Parallel()
	for _, paused := range []bool{false, true} {
		e := newPlanEnv(t, "h1:old")
		owner := []metav1.OwnerReference{{APIVersion: infrav1.GroupVersion.String(), Kind: state.KindTerraformCluster, Name: testName, UID: "m1-uid", Controller: new(true)}}
		mk := func(name string, phase infrav1.PlanPhase, age time.Duration) {
			p := &infrav1.TerraformPlan{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name, OwnerReferences: owner, CreationTimestamp: metav1.NewTime(t0.Add(-age)),
					Labels: map[string]string{infrav1.PlanPhaseLabel: string(phase)}},
				Spec: infrav1.TerraformPlanSpec{TargetRef: infrav1.PlanTargetRef{Kind: infrav1.PlanTargetCluster, Name: testName}, PlanHash: "p2:" + name, InputsHash: e.hash,
					Reason: infrav1.PlanReasonManual, Summary: infrav1.PlanSummary{Delete: new(int32(1))}},
			}
			if err := e.c.Create(t.Context(), p); err != nil {
				t.Fatal(err)
			}
		}
		mk("m1-live", infrav1.PlanPhasePending, 100*time.Hour)
		for i := range maxFinishedPlans + 3 {
			mk(fmt.Sprintf("m1-done-%02d", i), infrav1.PlanPhaseApplied, time.Duration(i+1)*time.Hour)
		}
		if paused {
			obj := e.get(t)
			obj.Annotations = map[string]string{clusterv1.PausedAnnotation: "true"}
			if err := e.c.Update(t.Context(), obj); err != nil {
				t.Fatal(err)
			}
		}
		e.reconcile(t, nil)
		var names []string
		for _, p := range e.plans(t) {
			names = append(names, p.Name)
		}
		want := maxFinishedPlans + 1
		if paused {
			want = maxFinishedPlans + 4
		}
		if len(names) != want || slices.Contains(names, "m1-done-10") != paused || !slices.Contains(names, "m1-done-09") {
			t.Errorf("paused %v: plans %v", paused, names)
		}
		if p := e.plan(t, "m1-live"); phaseOf(p) != infrav1.PlanPhasePending {
			t.Errorf("paused %v: the live plan is %s", paused, phaseOf(p))
		}
	}
}
