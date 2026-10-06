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
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cbmetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/testutil"
	"sigs.k8s.io/cluster-api/util/conditions"

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
// retry and a drift remediation are gated behind a plan Job and its
// approval; the first apply is not; Automatic is unchanged.
func TestDecideOpManual(t *testing.T) {
	t.Parallel()
	changed := StateView{Exists: true, InputsHash: "h1:old", CurrentHash: "h1:new"}
	same := StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:a"}
	planOf := func(h, p string) *PlanView { return &PlanView{InputsHash: h, PlanHash: p} }
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
		{"a plan of other inputs: a new plan Job", DecideInput{Mutable: true, ManualApply: true, State: changed, Plan: planOf("h1:older", "p2:x"), ApprovedPlan: "p2:x"},
			Decision{Action: ActionJob, Op: jobs.OpPlan, Reason: "InputsChanged", PlanFlow: true}},
		{"unapproved: wait, and the input change pauses the schedule", DecideInput{Mutable: true, ManualApply: true, State: changed, Plan: planOf("h1:new", "p2:x"),
			ApprovedPlan: "p2:other", DriftInterval: due, Now: t0},
			Decision{RequeueAfter: RetryMax, Reason: ReasonPlanAwaitingApproval, PlanFlow: true}},
		{"approved: the apply expects the plan", DecideInput{Mutable: true, ManualApply: true, State: changed, Plan: planOf("h1:new", "p2:x"), ApprovedPlan: "p2:x"},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged", ExpectPlan: "p2:x", PlanFlow: true}},
		{"a plan of an older fingerprint version: a new plan Job", DecideInput{Mutable: true, ManualApply: true, State: changed, Plan: planOf("h1:new", "p1:x"), ApprovedPlan: "p1:x"},
			Decision{Action: ActionJob, Op: jobs.OpPlan, Reason: "InputsChanged", PlanFlow: true}},
		{"an old empty plan is planned again, not waited on", DecideInput{Mutable: true, ManualApply: true, State: changed,
			Plan: planOf("h1:new", "p1:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"), DriftInterval: due, Now: t0},
			Decision{Action: ActionJob, Op: jobs.OpPlan, Reason: "InputsChanged", PlanFlow: true}},
		{"an empty plan needs no approval", DecideInput{Mutable: true, ManualApply: true, State: changed, Plan: planOf("h1:new", runner.EmptyPlanHash)},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged", ExpectPlan: runner.EmptyPlanHash, PlanFlow: true}},
		{"the first apply is not gated", DecideInput{Mutable: true, ManualApply: true, State: StateView{CurrentHash: "h1:new"}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "NoState"}},
		{"a destructive block does not apply on top", DecideInput{Mutable: true, ManualApply: true, State: changed, Plan: planOf("h1:new", "p2:x"), ApprovedPlan: "p2:x",
			Jobs: JobsView{BlockedHash: "h1:new"}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged", ExpectPlan: "p2:x", PlanFlow: true}},
		{"a retry of a failed apply is gated and backs off", DecideInput{Mutable: true, ManualApply: true, State: same, Plan: planOf("h1:a", "p2:x"), ApprovedPlan: "p2:x", Now: t0,
			Jobs: JobsView{LastApplyFailed: true, Failures: map[jobs.Op]int{jobs.OpApply: 1}, LastFailure: map[jobs.Op]time.Time{jobs.OpApply: t0.Add(-20 * time.Second)}}},
			Decision{RequeueAfter: 40 * time.Second, Reason: "LastApplyFailedBackoff", PlanFlow: true}},
		{"a failed plan Job backs off", DecideInput{Mutable: true, ManualApply: true, State: changed, Now: t0,
			Jobs: JobsView{Failures: map[jobs.Op]int{jobs.OpPlan: 1}, LastFailure: map[jobs.Op]time.Time{jobs.OpPlan: t0.Add(-20 * time.Second)}}},
			Decision{RequeueAfter: 40 * time.Second, Reason: "InputsChangedBackoff", PlanFlow: true}},
		{"a drift remediation is gated", DecideInput{Mutable: true, ManualApply: true, State: same, Remediate: true, Jobs: JobsView{FailedLimit: 3}},
			Decision{Action: ActionJob, Op: jobs.OpPlan, Reason: "DriftRemediation", PlanFlow: true}},
		{"a waiting remediation keeps the schedule, and its plan", DecideInput{Mutable: true, ManualApply: true, State: same, Remediate: true, Jobs: JobsView{FailedLimit: 3},
			Plan: planOf("h1:a", "p2:x"), DriftInterval: due, Now: t0},
			Decision{Action: ActionJob, Op: jobs.OpDrift, Reason: "DriftDue", PlanFlow: true}},
		{"a waiting remediation requeues at the next deadline", DecideInput{Mutable: true, ManualApply: true, State: same, Remediate: true, Jobs: JobsView{FailedLimit: 3},
			Plan: planOf("h1:a", "p2:x"), DriftInterval: due, LastDriftCheck: at(-26 * time.Minute), Now: t0},
			Decision{RequeueAfter: 4 * time.Minute, Reason: ReasonPlanAwaitingApproval, PlanFlow: true}},
		{"up to date: no plan flow", DecideInput{Mutable: true, ManualApply: true, State: same, Plan: planOf("h1:a", "p2:x")},
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
}

// newPlanEnv returns, failing t on error, a planEnv as described on the
// planEnv type: state at stateHash (or the current hash if empty), a
// successful older apply, and each mut applied to the machine.
func newPlanEnv(t *testing.T, stateHash string, mut ...func(*infrav1.TerraformMachine)) planEnv {
	t.Helper()
	e := newEnv(t, world(machine(append([]func(*infrav1.TerraformMachine){withFinalizer, notPaused, provisioned}, mut...)...))...)
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
	return planEnv{env: e, hash: h, reg: reg}
}

// kind returns, failing t on error, the fakeKind adapter for e's object,
// set up as a mutable cluster with applyPolicy Manual, a healthy reading
// and drift as its DriftPolicy (or a disabled policy when drift is nil).
func (e planEnv) kind(t *testing.T, drift *infrav1.DriftPolicy) *fakeKind {
	t.Helper()
	k := e.kindFor(t, readyOwner)
	k.asCluster, k.mutable, k.in = true, true, machineIn()
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	if drift == nil {
		drift = &infrav1.DriftPolicy{IntervalSeconds: new(int32(0))}
	}
	k.clusterDrift, k.applyPolicy, k.plan = drift, infrav1.ApplyPolicyManual, &e.plan
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

// approvePlan sets the ApprovePlanAnnotation on e's stored object to value,
// failing t on error.
func (e planEnv) approvePlan(t *testing.T, value string) {
	t.Helper()
	obj := e.get(t)
	metav1.SetMetaDataAnnotation(&obj.ObjectMeta, infrav1.ApprovePlanAnnotation, value)
	if err := e.c.Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
}

// finishRunner gives the runner's Job name an outcome at finishedAt and a
// pod whose termination message is result, failing t on error.
func (e planEnv) finishRunner(t *testing.T, name string, outcome jobs.Outcome, finishedAt time.Time, result string) {
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
func (e planEnv) newest(t *testing.T) string {
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

// TestPlanPreviewFlow walks a change through applyPolicy Manual: a plan Job
// (run lease only, nothing applied), status.plan, the condition with the
// approve command and one PlanReady, no Job while it waits, the approved
// apply with --expect-plan, a changed plan that waits for its own
// approval without backoff, and the success that consumes the approval and
// clears status.plan.
func TestPlanPreviewFlow(t *testing.T) {
	t.Parallel()
	e := newPlanEnv(t, "h1:old")
	if requeue := e.reconcile(t, nil); requeue != ActiveJobRequeue {
		t.Fatalf("requeue = %s", requeue)
	}
	plan := e.newest(t)
	args := sourceArgs(e.jobNamed(t, plan))
	if jobs.OpOf(e.jobNamed(t, plan)) != jobs.OpPlan || !slices.Contains(args, "--op=plan") ||
		slices.Contains(args, "--guard-deletes") || slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--expect-plan") }) {
		t.Fatalf("plan Job %s args = %v", plan, args)
	}
	if h := e.jobNamed(t, plan).Annotations[state.InputsHashAnnotation]; h != e.hash {
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

	// The plan: status.plan, the condition and one PlanReady; no apply.
	p1 := &runner.Plan{Hash: runner.PlanHash([]string{"module.role.lb|update", "module.role.sg|delete"}), Update: 1, Delete: 1,
		Resources: []string{"module.role.lb (update)", "module.role.sg (delete)"}}
	e.finishRunner(t, plan, jobs.Succeeded, t0.Add(-30*time.Minute), planResult(runner.OpPlan, p1, ""))
	for i := range 2 {
		if requeue := e.reconcile(t, nil); requeue != RetryMax {
			t.Errorf("pass %d: requeue = %s, want %s", i, requeue, RetryMax)
		}
	}
	if len(e.runner.created) != 1 {
		t.Fatalf("created %v while the plan waits", e.runner.created)
	}
	if e.plan.PlanHash != p1.Hash || e.plan.InputsHash != e.hash || e.plan.Job != plan || *e.plan.Delete != 1 ||
		!slices.Equal(e.plan.Resources, p1.Resources) || e.plan.CreatedAt == nil {
		t.Errorf("status.plan = %+v", e.plan)
	}
	cmd := "kubectl annotate terraformcluster " + testName + " -n " + testNS + " captf.io/approve-plan=" + p1.Hash + " --overwrite"
	if c := planApplyReason(t, e); c == nil || c.Status != metav1.ConditionUnknown || c.Reason != infrav1.PlanAwaitingApprovalReason ||
		!strings.Contains(c.Message, cmd) || !strings.HasPrefix(c.Message, "Job "+plan+": ") {
		t.Fatalf("ApplyJobSucceeded = %+v, want PlanAwaitingApproval with %q", c, cmd)
	}
	if got := e.rec.only(EventPlanReady); len(got) != 1 || got[0].eventType != corev1.EventTypeNormal ||
		!strings.Contains(got[0].note, "0 to create, 1 to update, 0 to replace, 1 to delete") || !strings.Contains(got[0].note, cmd) {
		t.Errorf("PlanReady = %+v", got)
	}
	for _, ev := range e.rec.only(EventConditionChanged) {
		if strings.HasPrefix(ev.note, infrav1.ApplyJobSucceededCondition) {
			t.Errorf("ConditionChanged for the wait: %s", ev.note)
		}
	}

	// Bookkept: its pod is not read again, and it still waits.
	metav1.SetMetaDataAnnotation(&e.jobNamed(t, plan).ObjectMeta, BookkeptAnnotation, "true")
	e.runner.pods[plan] = nil
	e.approvePlan(t, "p1:another")
	for range 2 {
		e.reconcile(t, nil)
	}
	if len(e.runner.created) != 1 || e.plan.PlanHash != p1.Hash || e.rec.count(EventPlanReady) != 1 {
		t.Fatalf("bookkept: created %v, plan %s, %d PlanReady", e.runner.created, e.plan.PlanHash, e.rec.count(EventPlanReady))
	}

	// Approved: the apply plans again and applies only this plan.
	e.approvePlan(t, p1.Hash)
	e.reconcile(t, nil)
	if len(e.runner.created) != 2 {
		t.Fatalf("created %v, want the approved apply", e.runner.created)
	}
	apply1 := e.newest(t)
	j := e.jobNamed(t, apply1)
	if args := sourceArgs(j); jobs.OpOf(j) != jobs.OpApply || !slices.Contains(args, "--expect-plan="+p1.Hash) || !slices.Contains(args, "--guard-deletes") {
		t.Errorf("approved apply args = %v", args)
	}
	if j.Annotations[ApprovedPlanAnnotation] != p1.Hash {
		t.Errorf("approved apply annotations = %v", j.Annotations)
	}
	if e.rec.count(EventPlanApproved) != 1 || e.rec.count(EventInputsChanged) != 1 {
		t.Errorf("%d PlanApproved, %d InputsChanged; want 1 and 1", e.rec.count(EventPlanApproved), e.rec.count(EventInputsChanged))
	}
	if e.lease(t, runlease.ClusterName(testNS, "c1")) == nil {
		t.Error("the approved apply did not take the cluster write lease")
	}

	// The plan changed: the new plan waits for its own approval, without
	// backoff.
	p2 := &runner.Plan{Hash: runner.PlanHash([]string{"module.role.lb|update"}), Update: 1, Resources: []string{"module.role.lb (update)"}}
	e.finishRunner(t, apply1, jobs.Failed, t0.Add(-20*time.Minute), planResult(runner.OpApply, p2, runner.ErrorKindPlanChanged))
	e.reconcile(t, nil)
	if len(e.runner.created) != 2 {
		t.Fatalf("created %v after a changed plan", e.runner.created)
	}
	if e.plan.PlanHash != p2.Hash || e.plan.Job != apply1 || e.plan.InputsHash != e.hash {
		t.Errorf("status.plan = %+v, want the new plan", e.plan)
	}
	cmd2 := "captf.io/approve-plan=" + p2.Hash + " --overwrite"
	if c := planApplyReason(t, e); c == nil || c.Status != metav1.ConditionUnknown || c.Reason != infrav1.PlanChangedReason || !strings.Contains(c.Message, cmd2) {
		t.Fatalf("ApplyJobSucceeded = %+v, want PlanChanged with %q", c, cmd2)
	}
	if got := e.rec.only(EventPlanChanged); len(got) != 1 || got[0].eventType != corev1.EventTypeWarning || !strings.Contains(got[0].note, cmd2) {
		t.Errorf("PlanChanged = %+v", got)
	}
	if n := e.rec.count(EventJobFailed); n != 0 {
		t.Errorf("%d JobFailed events for a changed plan", n)
	}
	if lr := e.get(t).Status.LastRun; lr.Error.Kind != infrav1.RunErrorKindPlanChanged {
		t.Errorf("lastRun.error = %+v", lr.Error)
	}
	if a := e.get(t).Annotations[infrav1.ApprovePlanAnnotation]; a != p1.Hash {
		t.Errorf("the approval of the old plan was consumed: %q", a)
	}
	e.approvePlan(t, p2.Hash)
	e.reconcile(t, nil)
	if len(e.runner.created) != 3 {
		t.Fatalf("created %v; a changed plan must not back off", e.runner.created)
	}
	apply2 := e.newest(t)
	if !slices.Contains(sourceArgs(e.jobNamed(t, apply2)), "--expect-plan="+p2.Hash) {
		t.Errorf("second apply args = %v", sourceArgs(e.jobNamed(t, apply2)))
	}

	// Success: the approval is consumed and status.plan cleared, once.
	e.finishRunner(t, apply2, jobs.Succeeded, t0.Add(-10*time.Minute), planResult(runner.OpApply, nil, ""))
	e.state.st.InputsHash = e.hash
	for range 2 {
		e.reconcile(t, nil)
	}
	obj := e.get(t)
	if _, ok := obj.Annotations[infrav1.ApprovePlanAnnotation]; ok {
		t.Errorf("the approval was not consumed: %v", obj.Annotations)
	}
	if e.plan.PlanHash != "" {
		t.Errorf("status.plan = %+v, want empty", e.plan)
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
captf_plan_approvals_total{kind="TerraformCluster",result="approved"} 1
captf_plan_approvals_total{kind="TerraformCluster",result="changed"} 1
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

// TestPlanEmptyDoesNotWait: a plan without changes needs no approval; the
// apply runs at once, expecting the empty plan.
func TestPlanEmptyDoesNotWait(t *testing.T) {
	t.Parallel()
	e := newPlanEnv(t, "h1:old")
	e.reconcile(t, nil)
	plan := e.newest(t)
	e.finishRunner(t, plan, jobs.Succeeded, t0.Add(-time.Minute), planResult(runner.OpPlan, runner.EmptyPlan(), ""))
	if requeue := e.reconcile(t, nil); requeue != ActiveJobRequeue || len(e.runner.created) != 2 {
		t.Fatalf("requeue %s, created %v; want the apply at once", requeue, e.runner.created)
	}
	if args := sourceArgs(e.jobNamed(t, e.newest(t))); !slices.Contains(args, "--expect-plan="+runner.EmptyPlanHash) {
		t.Errorf("apply args = %v", args)
	}
	if e.rec.count(EventPlanReady) != 1 || e.rec.count(EventPlanApproved) != 0 {
		t.Errorf("%d PlanReady, %d PlanApproved; want 1 and 0", e.rec.count(EventPlanReady), e.rec.count(EventPlanApproved))
	}
	if c := planApplyReason(t, e); c != nil && c.Reason == infrav1.PlanAwaitingApprovalReason {
		t.Errorf("ApplyJobSucceeded = %+v; an empty plan never waits", c)
	}
}

// TestPlanJobFailed: a failed plan Job sets ApplyFailed naming it, emits
// JobFailed once, and the next plan Job backs off.
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
	if e.plan.PlanHash != "" {
		t.Errorf("status.plan = %+v", e.plan)
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
	if e.plan.PlanHash != p.Hash || e.plan.InputsHash != e.hash {
		t.Errorf("status.plan = %+v, want the remediation's plan kept", e.plan)
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
	got, err := collectFinished(t.Context(), e.d, []batchv1.Job{bookkept, unreadable})
	if err != nil || len(got) != 2 || !got[0].planChanged || got[1].ok || !got[1].planUnreadable {
		t.Fatalf("collectFinished = %+v, %v; want a3 plan-changed, p1 failed as unreadable", got, err)
	}
	marked := unreadable.DeepCopy()
	marked.Annotations = map[string]string{BookkeptAnnotation: "true", PlanUnreadableAnnotation: "true"}
	if got, err := collectFinished(t.Context(), e.d, []batchv1.Job{*marked}); err != nil || got[0].ok {
		t.Errorf("bookkept unreadable plan = %+v, %v; want still failed", got, err)
	}
}

// TestPlanWaitEvents: leaving a plan wait emits no stale Job outcome; a Job
// that finished this pass still reports its own.
func TestPlanWaitEvents(t *testing.T) {
	t.Parallel()
	wait := metav1.Condition{Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionUnknown, Reason: infrav1.PlanAwaitingApprovalReason, Message: "Job p: plan p1:x"}
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

// TestPlanOutputChanges: an output-only plan records its output count in
// status.plan and the summary names it instead of reading as no change.
func TestPlanOutputChanges(t *testing.T) {
	t.Parallel()
	j := job("p", jobs.OpPlan, jobs.Succeeded, t0)
	p, ok := previewOf(&finished{job: &j, result: &jobs.Result{Plan: &runner.Plan{Hash: "p1:x", OutputChanges: 2}}})
	if !ok || p.OutputChanges == nil || *p.OutputChanges != 2 {
		t.Fatalf("preview = %+v", p)
	}
	if got, want := planCounts(p), "0 to create, 0 to update, 0 to replace, 0 to delete, 2 output(s) to change"; got != want {
		t.Errorf("planCounts = %q, want %q", got, want)
	}
	if got := planCounts(infrav1.PlanPreview{Create: new(int32(1))}); strings.Contains(got, "output") {
		t.Errorf("planCounts without outputs = %q", got)
	}
}

// TestPlanPreviewOf: status.plan caps the entries the API accepts.
func TestPlanPreviewOf(t *testing.T) {
	t.Parallel()
	j := job("p", jobs.OpPlan, jobs.Succeeded, t0)
	j.Annotations = map[string]string{state.InputsHashAnnotation: "h1:x"}
	res := &runner.Plan{Hash: "p1:x", Create: 60}
	for range infrav1.MaxPlanResources + 3 {
		res.Resources = append(res.Resources, strings.Repeat("é", 400)+" (create)")
	}
	p, ok := previewOf(&finished{job: &j, result: &jobs.Result{Plan: res}})
	if !ok || len(p.Resources) != infrav1.MaxPlanResources || p.Truncated == nil || !*p.Truncated || *p.Create != 60 || p.InputsHash != "h1:x" {
		t.Fatalf("preview = %d resources, %+v", len(p.Resources), p)
	}
	for _, r := range p.Resources {
		if len(r) > maxPlanEntry || !strings.HasPrefix(r, "é") || strings.ContainsRune(r, '�') {
			t.Fatalf("entry of %d bytes", len(r))
		}
	}
	if _, ok := previewOf(&finished{job: &j}); ok {
		t.Error("a Job without a result has a plan")
	}
}
