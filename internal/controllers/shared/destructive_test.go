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
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// blockedSummaryText is the drift/plan summary a blocked guarded apply
// reports.
const blockedSummaryText = "the plan deletes or replaces 1 resource(s): module.role.lb (replace)"

// blockedPlan returns the plan a blocked guarded apply reports.
func blockedPlan() *runner.Plan {
	return &runner.Plan{Hash: runner.PlanHash([]string{"module.role.lb|delete,create"}), Replace: 1, Resources: []string{"module.role.lb (replace)"}}
}

// blockedResult returns the termination message of a blocked guarded
// apply, which reports its plan.
func blockedResult() string {
	return blockedResultOf(blockedPlan())
}

// blockedResultOf returns the termination message of a blocked guarded
// apply that reports p (nil: no plan).
func blockedResultOf(p *runner.Plan) string {
	return string(runner.Encode(runner.Result{
		Version: runner.ResultVersion, Op: runner.OpApply, Steps: []runner.Step{{Name: "init"}, {Name: "validate"}, {Name: "plan", Exit: 2}, {Name: "show-json"}},
		Error: &runner.Error{Kind: runner.ErrorKindBlocked, Tail: blockedSummaryText}, Plan: p,
	}))
}

// currentHash returns the inputs hash the fake kind renders, failing t on
// error.
func currentHash(t *testing.T) string {
	t.Helper()
	h, err := hash.Inputs(contract.RoleMachine, "registry.example/mod:1.0", machineIn())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// blockedEnv is a provisioned cluster-kind object whose newest apply, of
// the current inputs hash, the runner blocked; the state still records
// stateHash. Drift checks are off unless drift says otherwise.
type blockedEnv struct {
	*env
	hash string
}

// newBlockedEnv returns, failing t on error, a blockedEnv built as
// described on the blockedEnv type: state at stateHash (or the current hash
// if empty), a successful older apply, and a failed newer apply that the
// runner blocked, marked with RemediationAnnotation when remediation is
// true and further customized by mut.
func newBlockedEnv(t *testing.T, stateHash string, remediation bool, mut ...func(*infrav1.TerraformMachine)) blockedEnv {
	t.Helper()
	h := currentHash(t)
	if stateHash == "" {
		stateHash = h
	}
	e := newEnv(t, world(machine(append([]func(*infrav1.TerraformMachine){withFinalizer, notPaused, provisioned}, mut...)...))...)
	e.state.st = &state.State{InputsHash: stateHash}
	ok := job("a", jobs.OpApply, jobs.Succeeded, t0.Add(-2*time.Hour))
	ok.Annotations = map[string]string{state.InputsHashAnnotation: stateHash, BookkeptAnnotation: "true"}
	b := job("b", jobs.OpApply, jobs.Failed, t0.Add(-5*time.Minute))
	b.Annotations = map[string]string{state.InputsHashAnnotation: h}
	if remediation {
		b.Annotations[RemediationAnnotation] = "true"
	}
	if err := e.c.Create(t.Context(), b.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	e.runner.jobs = append(e.runner.jobs, ok, b)
	e.runner.pods["b"] = []corev1.Pod{*podWith(blockedResult(), "")}
	return blockedEnv{env: e, hash: h}
}

// syncMarks copies the annotations MarkBookkept patched onto Job b (in the
// client) into the fake runner's copy, which the next List returns.
// syncMarks copies the annotations MarkBookkept patched onto Job "b" (in
// the client) into the fake runner's copy, which the next List returns, and
// returns those annotations. It fails t on error.
func (e blockedEnv) syncMarks(t *testing.T) map[string]string {
	t.Helper()
	stored := &batchv1.Job{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "b"}, stored); err != nil {
		t.Fatal(err)
	}
	e.jobNamed(t, "b").Annotations = stored.Annotations
	return stored.Annotations
}

// kind returns, failing t on error, the fakeKind adapter for e's object,
// set up as a mutable cluster with a healthy reading, drift as its
// DriftPolicy (or a disabled policy when drift is nil) and e's
// status.pendingPlanRef.
func (e blockedEnv) kind(t *testing.T, drift *infrav1.DriftPolicy) *fakeKind {
	t.Helper()
	k := e.kindFor(t, readyOwner)
	k.asCluster, k.mutable, k.in = true, true, machineIn()
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	if drift == nil {
		drift = &infrav1.DriftPolicy{IntervalSeconds: new(int32(0))}
	}
	k.clusterDrift, k.planRef = drift, &e.planRef
	return k
}

// approveLive approves e's live TerraformPlan as alice, failing t when
// there is none, and returns its name.
func (e blockedEnv) approveLive(t *testing.T) string {
	t.Helper()
	for _, p := range e.plans(t) {
		if !phaseOf(&p).Terminal() {
			e.approve(t, p.Name, "alice")
			return p.Name
		}
	}
	t.Fatal("no live TerraformPlan")
	return ""
}

// sourceArgs returns the source container's Args from job, or nil if job
// has no source container.
func sourceArgs(job *batchv1.Job) []string {
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.Name == jobs.SourceContainer {
			return c.Args
		}
	}
	return nil
}

// TestDestructivePlanBlocked: a blocked cluster apply makes a Destructive
// TerraformPlan of its plan, sets ApplyJobSucceeded
// False/DestructivePlanBlocked with the addresses and the exact approve
// command, emits one Warning naming the plan, is not retried for the same
// inputs hash (neither right away nor once the Job is bookkept), runs
// expecting exactly that plan once it is approved, and makes it Applied.
func TestDestructivePlanBlocked(t *testing.T) {
	t.Parallel()
	e := newBlockedEnv(t, "h1:old", false)
	requeue, err := reconcileOnce(t, e.env, e.kind(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 0 {
		t.Fatalf("created %v; a blocked apply must not start again", e.runner.created)
	}
	if requeue != RetryMax {
		t.Errorf("requeue = %s, want %s", requeue, RetryMax)
	}
	bp := blockedPlan()
	name := planName(testName, "b", bp.Hash)
	tp := e.plan(t, name)
	if tp.Spec.Reason != infrav1.PlanReasonDestructive || tp.Spec.InputsHash != e.hash || tp.Spec.PlanHash != bp.Hash ||
		tp.Labels[infrav1.PlanDestructiveLabel] != "true" || tp.Labels[infrav1.PlanReasonLabel] != string(infrav1.PlanReasonDestructive) ||
		*tp.Spec.Summary.Replace != 1 {
		t.Errorf("TerraformPlan = %+v", tp)
	}
	checkPlan(t, tp, infrav1.PlanPhasePending, infrav1.PlanPendingReason, infrav1.PlanPendingReason)
	c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition)
	cmd := planApproveCommand(name, testNS)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.DestructivePlanBlockedReason ||
		!strings.HasPrefix(c.Message, "Job b: "+blockedSummaryText) || !strings.HasSuffix(c.Message, cmd) {
		t.Fatalf("ApplyJobSucceeded = %+v, want DestructivePlanBlocked naming the addresses and %q", c, cmd)
	}
	if got := e.rec.only(EventDestructivePlanBlocked); len(got) != 1 || !strings.Contains(got[0].note, name) || e.rec.count(EventJobFailed) != 0 {
		t.Errorf("events: DestructivePlanBlocked %+v, %d JobFailed; want one naming %s and none", got, e.rec.count(EventJobFailed), name)
	}
	if e.rec.count(EventPlanReady) != 0 {
		t.Errorf("%d PlanReady for a destructive plan", e.rec.count(EventPlanReady))
	}
	if lr := e.get(t).Status.LastRun; lr.Error.Kind != infrav1.RunErrorKindBlocked || lr.Error.Summary != blockedSummaryText {
		t.Errorf("lastRun.error = %+v", lr.Error)
	}
	if got := e.syncMarks(t); got[BlockedAnnotation] != "true" || got[BookkeptAnnotation] != "true" || got[PlanHashAnnotation] != bp.Hash {
		t.Fatalf("blocked Job annotations = %v", got)
	}

	// Bookkept: its pod is not read again, and still nothing starts.
	e.runner.pods["b"] = nil
	for range 2 {
		if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.runner.created) != 0 || e.rec.count(EventDestructivePlanBlocked) != 1 || len(e.plans(t)) != 1 {
		t.Fatalf("after bookkeeping: created %v, %d events, %d plans", e.runner.created, e.rec.count(EventDestructivePlanBlocked), len(e.plans(t)))
	}
	if c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition); !strings.Contains(c.Message, "module.role.lb") || !strings.HasSuffix(c.Message, cmd) {
		t.Errorf("the bookkept condition lost the addresses or the command: %q", c.Message)
	}

	// The approval starts the apply of exactly that plan.
	e.approveLive(t)
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 1 {
		t.Fatalf("created %v, want one approved apply", e.runner.created)
	}
	a := e.jobNamed(t, e.runner.created[0])
	args := sourceArgs(a)
	if !slices.Contains(args, "--guard-deletes") || !slices.Contains(args, "--inputs-hash="+e.hash) || !slices.Contains(args, "--expect-plan="+bp.Hash) ||
		slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--allow-deletes-hash") }) {
		t.Errorf("approved apply args = %v", args)
	}
	if a.Annotations[PlanAnnotation] != name {
		t.Errorf("approved apply annotations = %v", a.Annotations)
	}
	if got := e.rec.only(EventPlanApproved); len(got) != 1 || !strings.Contains(got[0].note, "alice") {
		t.Errorf("PlanApproved = %+v", got)
	}
	e.finishRunner(t, a.Name, jobs.Succeeded, t0, planResult(runner.OpApply, nil, ""))
	e.state.st.InputsHash = e.hash
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	checkPlan(t, e.plan(t, name), infrav1.PlanPhaseApplied, infrav1.PlanAppliedReason, infrav1.PlanApprovedReason)
	if e.rec.count(EventPlanApplied) != 1 || len(e.runner.created) != 1 {
		t.Errorf("%d PlanApplied, created %v", e.rec.count(EventPlanApplied), e.runner.created)
	}
}

// TestDestructivePlanNewInputs: once the inputs change, the apply runs
// again, guarded, and expects no plan: the blocked one was made for other
// inputs.
func TestDestructivePlanNewInputs(t *testing.T) {
	t.Parallel()
	e := newBlockedEnv(t, "h1:old", false)
	// The blocked Job rendered other inputs than the current ones.
	e.jobNamed(t, "b").Annotations[state.InputsHashAnnotation] = "h1:blocked"
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 1 {
		t.Fatalf("created %v, want the apply of the new inputs", e.runner.created)
	}
	args := sourceArgs(e.jobNamed(t, e.runner.created[0]))
	if !slices.Contains(args, "--guard-deletes") || slices.ContainsFunc(args, func(a string) bool {
		return strings.HasPrefix(a, "--allow-deletes-hash") || strings.HasPrefix(a, "--expect-plan")
	}) {
		t.Errorf("new-inputs apply args = %v, want guarded and unapproved", args)
	}
}

// TestDestructiveWaitsAfterMove: clusterctl move carries the plans, not
// the Jobs. A pending Destructive plan of the current inputs keeps the
// apply waiting with no blocked Job left, the condition naming the plan
// and its command; its approval starts the apply of exactly that plan.
func TestDestructiveWaitsAfterMove(t *testing.T) {
	t.Parallel()
	e := newBlockedEnv(t, "h1:old", false)
	e.runner.jobs = e.runner.jobs[:1] // the successful apply only
	tp := &infrav1.TerraformPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "m1-moved", Labels: map[string]string{infrav1.PlanPhaseLabel: string(infrav1.PlanPhasePending)},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: infrav1.GroupVersion.String(), Kind: state.KindTerraformCluster, Name: testName, UID: "m1-uid", Controller: new(true)}}},
		Spec: infrav1.TerraformPlanSpec{TargetRef: infrav1.PlanTargetRef{Kind: infrav1.PlanTargetCluster, Name: testName},
			PlanHash: "p2:moved", InputsHash: e.hash, Reason: infrav1.PlanReasonDestructive, Summary: infrav1.PlanSummary{Delete: new(int32(2))}},
	}
	if err := e.c.Create(t.Context(), tp); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		requeue, err := reconcileOnce(t, e.env, e.kind(t, nil))
		if err != nil {
			t.Fatal(err)
		}
		if len(e.runner.created) != 0 || requeue != RetryMax {
			t.Fatalf("created %v, requeue %s; want the apply to wait for the moved plan", e.runner.created, requeue)
		}
	}
	c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition)
	if c == nil || c.Reason != infrav1.DestructivePlanBlockedReason || !strings.HasPrefix(c.Message, "TerraformPlan m1-moved ") ||
		!strings.Contains(c.Message, "2 to delete") || !strings.HasSuffix(c.Message, planApproveCommand("m1-moved", testNS)) {
		t.Fatalf("ApplyJobSucceeded = %+v", c)
	}
	if e.rec.count(EventJobFailed) != 0 || e.rec.count(EventDestructivePlanBlocked) != 0 {
		t.Errorf("%d JobFailed, %d DestructivePlanBlocked for a moved plan's wait", e.rec.count(EventJobFailed), e.rec.count(EventDestructivePlanBlocked))
	}
	if e.planRef.Name != "m1-moved" {
		t.Errorf("status.pendingPlanRef = %+v", e.planRef)
	}
	e.approve(t, "m1-moved", "alice")
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 1 || !slices.Contains(sourceArgs(e.jobNamed(t, e.runner.created[0])), "--expect-plan=p2:moved") {
		t.Fatalf("created %v, want the approved apply", e.runner.created)
	}
}

// TestDestructiveBlockedWithoutPlan: a blocked apply that reported no plan
// leaves nothing to approve: no TerraformPlan, and the apply runs again,
// guarded, RetryMax after the block.
func TestDestructiveBlockedWithoutPlan(t *testing.T) {
	t.Parallel()
	e := newBlockedEnv(t, "h1:old", false)
	e.runner.pods["b"] = []corev1.Pod{*podWith(blockedResultOf(nil), "")}
	requeue, err := reconcileOnce(t, e.env, e.kind(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 0 || len(e.plans(t)) != 0 || requeue != RetryMax-5*time.Minute {
		t.Fatalf("created %v, %d plans, requeue %s; want a wait of what is left of %s", e.runner.created, len(e.plans(t)), requeue, RetryMax)
	}
	if c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition); c == nil || c.Reason != infrav1.DestructivePlanBlockedReason ||
		!strings.Contains(c.Message, "no plan to approve") {
		t.Errorf("ApplyJobSucceeded = %+v", c)
	}
	clk, ok := e.d.Clock.(*testingclock.FakePassiveClock)
	if !ok {
		t.Fatalf("clock is %T", e.d.Clock)
	}
	clk.SetTime(t0.Add(RetryMax))
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 1 || slices.ContainsFunc(sourceArgs(e.jobNamed(t, e.runner.created[0])), func(a string) bool { return strings.HasPrefix(a, "--expect-plan") }) {
		t.Fatalf("created %v, want the guarded apply again", e.runner.created)
	}
}

// TestDestructiveApprovedApplyOutcomes: an approved apply that fails keeps
// its plan Approved, and the retry, after the backoff, expects the same
// plan; one that finds the plan changed fails the plan, and the next
// guarded apply runs at once, expecting nothing.
func TestDestructiveApprovedApplyOutcomes(t *testing.T) {
	t.Parallel()
	e := newBlockedEnv(t, "h1:old", false)
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	e.syncMarks(t)
	name := e.approveLive(t)
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	first := e.runner.created[0]
	step := "apply"
	e.finishRunner(t, first, jobs.Failed, t0, string(runner.Encode(runner.Result{Version: runner.ResultVersion, Op: runner.OpApply,
		Error: &runner.Error{Kind: runner.ErrorKindStep, Step: &step, Tail: "Error: quota"}})))
	requeue, err := reconcileOnce(t, e.env, e.kind(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 1 || requeue != RetryBase {
		t.Fatalf("created %v, requeue %s; want the backoff", e.runner.created, requeue)
	}
	checkPlan(t, e.plan(t, name), infrav1.PlanPhaseApproved, infrav1.PlanApprovedReason, infrav1.PlanApprovedReason)
	clk, ok := e.d.Clock.(*testingclock.FakePassiveClock)
	if !ok {
		t.Fatalf("clock is %T", e.d.Clock)
	}
	clk.SetTime(t0.Add(RetryBase))
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 2 {
		t.Fatalf("created %v, want the retry", e.runner.created)
	}
	retry := e.jobNamed(t, e.runner.created[1])
	if !slices.Contains(sourceArgs(retry), "--expect-plan="+blockedPlan().Hash) || retry.Annotations[PlanAnnotation] != name {
		t.Errorf("retry args %v, annotations %v", sourceArgs(retry), retry.Annotations)
	}

	other := &runner.Plan{Hash: runner.PlanHash([]string{"module.role.lb|delete,create", "module.role.sg|delete"}), Replace: 1, Delete: 1}
	e.finishRunner(t, retry.Name, jobs.Failed, t0.Add(RetryBase+time.Minute), planResult(runner.OpApply, other, runner.ErrorKindPlanChanged))
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	checkPlan(t, e.plan(t, name), infrav1.PlanPhaseFailed, infrav1.PlanFailedReason, infrav1.PlanApprovedReason)
	if got := e.rec.only(EventPlanChanged); len(got) != 1 || !strings.Contains(got[0].note, "the next apply plans again") {
		t.Errorf("PlanChanged = %+v", got)
	}
	if len(e.plans(t)) != 1 {
		t.Errorf("%d plans: a changed destructive plan makes none until the guard blocks again", len(e.plans(t)))
	}
	if len(e.runner.created) != 3 || slices.ContainsFunc(sourceArgs(e.jobNamed(t, e.runner.created[2])), func(a string) bool { return strings.HasPrefix(a, "--expect-plan") }) {
		t.Fatalf("created %v, want the guarded apply at once", e.runner.created)
	}
}

// TestDestructivePlanBlocksCostNoBackoff: a blocked Job counts toward no
// retry backoff, and the mark survives bookkeeping.
func TestDestructivePlanBlocksCostNoBackoff(t *testing.T) {
	t.Parallel()
	bookkept := job("a3", jobs.OpApply, jobs.Failed, t0.Add(-time.Minute))
	bookkept.Annotations = map[string]string{BookkeptAnnotation: "true", BlockedAnnotation: "true"}
	done := []finished{
		{job: ptr(job("a4", jobs.OpApply, jobs.Failed, t0)), blocked: true},
		{job: &bookkept, bookkept: true, blocked: true},
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
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	got, err := collectFinished(t.Context(), e.d, []batchv1.Job{bookkept})
	if err != nil || len(got) != 1 || !got[0].blocked {
		t.Errorf("collectFinished = %+v, %v; want the bookkept Job marked blocked", got, err)
	}
}

// TestDestructiveRemediationBlocked: a drift remediation whose plan
// deletes or replaces resources is blocked the same way: no remediation
// loop, DriftDetected says why, and the approval of the (unchanged) inputs
// hash lets it run.
func TestDestructiveRemediationBlocked(t *testing.T) {
	t.Parallel()
	summary := "Job d: 0 to create, 0 to update, 0 to replace, 1 to delete"
	e := newBlockedEnv(t, "", true, func(m *infrav1.TerraformMachine) {
		m.Status.LastDriftCheck = &metav1.Time{Time: t0.Add(-time.Hour)}
		m.Status.Conditions = append(m.Status.Conditions, metav1.Condition{
			Type: infrav1.DriftDetectedCondition, Status: metav1.ConditionTrue, Reason: infrav1.DriftRemediatingReason,
			Message: summary + remediationMarker + "b applies the current inputs", LastTransitionTime: metav1.NewTime(t0.Add(-time.Hour)),
		})
	})
	remediate := &infrav1.DriftPolicy{IntervalSeconds: new(int32(0)), Action: infrav1.DriftActionRemediate}
	for range 2 {
		if _, err := reconcileOnce(t, e.env, e.kind(t, remediate)); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.runner.created) != 0 {
		t.Fatalf("created %v; a blocked remediation must not start again", e.runner.created)
	}
	obj := e.get(t)
	if c := conditions.Get(obj, infrav1.ApplyJobSucceededCondition); c == nil || c.Reason != infrav1.DestructivePlanBlockedReason {
		t.Errorf("ApplyJobSucceeded = %+v", c)
	}
	if c := conditions.Get(obj, infrav1.DriftDetectedCondition); c == nil || c.Reason != infrav1.DriftPendingReason ||
		!strings.HasPrefix(c.Message, summary) || !strings.Contains(c.Message, "b was blocked") {
		t.Errorf("DriftDetected = %+v", c)
	}
	e.approveLive(t)
	if _, err := reconcileOnce(t, e.env, e.kind(t, remediate)); err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 1 {
		t.Fatalf("created %v, want the approved remediation", e.runner.created)
	}
	if got := e.rec.only(EventDriftRemediationStarted); len(got) != 1 || !strings.Contains(got[0].note, e.runner.created[0]) {
		t.Errorf("DriftRemediationStarted = %+v", got)
	}
	j := e.jobNamed(t, e.runner.created[0])
	if j.Annotations[RemediationAnnotation] != "true" || !slices.Contains(sourceArgs(j), "--expect-plan="+blockedPlan().Hash) {
		t.Errorf("remediation Job = %v %v", j.Annotations, sourceArgs(j))
	}
}

// TestBlockedCondition proves applyDestroyCondition, for a blocked Job,
// sets ApplyJobSucceeded False/DestructivePlanBlocked with the blocked
// summary and the command that approves the TerraformPlan of its plan,
// keeps the command when the Job's result is gone (bookkept, its plan
// hash recorded), and says so when the Job reported no plan.
func TestBlockedCondition(t *testing.T) {
	t.Parallel()
	b := job("b", jobs.OpApply, jobs.Failed, t0)
	b.Annotations = map[string]string{state.InputsHashAnnotation: "h1:x"}
	m := machine()
	res := &jobs.Result{Plan: &runner.Plan{Hash: "p2:x"}, Error: &runner.Error{Kind: runner.ErrorKindBlocked, Tail: blockedSummaryText}}
	c := applyDestroyCondition(finished{job: &b, blocked: true, result: res}, m)
	name := planName(testName, "b", "p2:x")
	want := "Job b: " + blockedSummaryText + ". Nothing was applied, and no apply of these inputs runs until its plan is approved. " +
		"To apply it, approve TerraformPlan " + name + ": " + planApproveCommand(name, testNS)
	if c.Status != metav1.ConditionFalse || c.Reason != infrav1.DestructivePlanBlockedReason || c.Message != want || !namesJob(c.Message, "b") {
		t.Errorf("condition = %+v", c)
	}
	// Without its result (bookkept, condition lost) the command remains.
	b.Annotations[PlanHashAnnotation] = "p2:x"
	c = applyDestroyCondition(finished{job: &b, blocked: true, bookkept: true}, m)
	if c.Reason != infrav1.DestructivePlanBlockedReason || !strings.HasSuffix(c.Message, planApproveCommand(name, testNS)) {
		t.Errorf("bookkept condition = %+v", c)
	}
	delete(b.Annotations, PlanHashAnnotation)
	c = applyDestroyCondition(finished{job: &b, blocked: true, result: &jobs.Result{Error: res.Error}}, m)
	if !strings.Contains(c.Message, "no plan to approve") || strings.Contains(c.Message, "kubectl") {
		t.Errorf("condition without a plan = %+v", c)
	}
}

// newFailedApplyEnv returns, failing t on error, a provisioned cluster-kind
// object whose state records the current inputs hash (a successful apply
// of it, bookkept) and whose newest apply, of other inputs, failed part
// way, leaving the state's inputs hash in place. mut customizes the
// object.
func newFailedApplyEnv(t *testing.T, mut ...func(*infrav1.TerraformMachine)) blockedEnv {
	t.Helper()
	h := currentHash(t)
	e := newEnv(t, world(machine(append([]func(*infrav1.TerraformMachine){withFinalizer, notPaused, provisioned}, mut...)...))...)
	e.state.st = &state.State{InputsHash: h}
	ok := job("a", jobs.OpApply, jobs.Succeeded, t0.Add(-2*time.Hour))
	ok.Annotations = map[string]string{state.InputsHashAnnotation: h, BookkeptAnnotation: "true"}
	f := job("f", jobs.OpApply, jobs.Failed, t0.Add(-30*time.Minute))
	f.Annotations = map[string]string{state.InputsHashAnnotation: "h2:failed"}
	e.runner.jobs = append(e.runner.jobs, ok, f)
	return blockedEnv{env: e, hash: h}
}

// blockJob finishes name, an apply Job e's runner started, at finishedAt
// as the runner's guard stops it before a destructive plan, failing t
// when there is no such Job.
func (e blockedEnv) blockJob(t *testing.T, name string, finishedAt time.Time) {
	t.Helper()
	j := e.jobNamed(t, name)
	j.CreationTimestamp = metav1.NewTime(finishedAt.Add(-time.Minute))
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(finishedAt)}}
	e.runner.pods[name] = []corev1.Pod{*podWith(blockedResult(), "")}
}

// startsApply reconciles once with an adapter rendering in, failing t
// unless exactly one apply Job started, and returns it.
func (e blockedEnv) startsApply(t *testing.T, in contract.MachineInputs) *batchv1.Job {
	t.Helper()
	before := len(e.runner.created)
	k := e.kind(t, nil)
	k.in = in
	if _, err := reconcileOnce(t, e.env, k); err != nil {
		t.Fatal(err)
	}
	if got := e.runner.created[before:]; len(got) != 1 || !strings.Contains(got[0], "-apply-") {
		t.Fatalf("started %v, want one apply", got)
	}
	// A copy: pruning shifts the runner's Jobs.
	return e.jobNamed(t, e.runner.created[before]).DeepCopy()
}

// TestBlockedRetryAfterFailedApply: inputs that return to the state's
// after an apply of others failed part way are applied again; when that
// apply is blocked before a destructive plan (undoing the failed apply's
// creates), it is not forgotten for a blocked Job "changing nothing": the
// cluster waits for its approval, which then applies it. A failed-Jobs
// history limit of 1, which prunes the failed Job once the blocked one is
// the newest failure, changes nothing.
func TestBlockedRetryAfterFailedApply(t *testing.T) {
	t.Parallel()
	for name, limit := range map[string]*int32{"default history": nil, "failedJobsHistoryLimit 1": new(int32(1))} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newFailedApplyEnv(t, func(m *infrav1.TerraformMachine) {
				m.Spec.Jobs = &infrav1.JobPolicy{FailedJobsHistoryLimit: limit}
			})
			retry := e.startsApply(t, machineIn())
			if retry.Annotations[state.InputsHashAnnotation] != e.hash || retry.Annotations[AfterFailedApplyAnnotation] != "true" {
				t.Fatalf("retry annotations = %v, want inputs hash %s, marked after a failed apply", retry.Annotations, e.hash)
			}
			e.blockJob(t, retry.Name, t0.Add(-time.Minute))
			for range 2 {
				requeue, err := reconcileOnce(t, e.env, e.kind(t, nil))
				if err != nil {
					t.Fatal(err)
				}
				if len(e.runner.created) != 1 || requeue != RetryMax {
					t.Fatalf("blocked retry: created %v, requeue %s; want it to wait for the approval", e.runner.created, requeue)
				}
			}
			if limit != nil && !slices.Contains(e.runner.deleted, "f") {
				t.Fatalf("the failed Job was not pruned: deleted %v", e.runner.deleted)
			}
			name := planName(testName, retry.Name, blockedPlan().Hash)
			c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition)
			if c == nil || c.Reason != infrav1.DestructivePlanBlockedReason || !namesJob(c.Message, retry.Name) ||
				!strings.HasSuffix(c.Message, planApproveCommand(name, testNS)) {
				t.Fatalf("ApplyJobSucceeded = %+v, want the blocked retry's plan %s", c, name)
			}
			e.approve(t, name, "alice")
			approved := e.startsApply(t, machineIn())
			if args := sourceArgs(approved); !slices.Contains(args, "--expect-plan="+blockedPlan().Hash) || approved.Annotations[PlanAnnotation] != name {
				t.Errorf("approved retry args = %v, annotations %v", args, approved.Annotations)
			}
		})
	}
}

// TestBlockedEditAfterFailedApply: an edit blocked after an apply failed
// part way does not clear the failure: once the inputs return to the
// state's, they are applied again, and the condition then names the
// returned inputs, not the blocked edit's.
func TestBlockedEditAfterFailedApply(t *testing.T) {
	t.Parallel()
	e := newFailedApplyEnv(t)
	edit := machineIn()
	edit.BootstrapData = "ZWRpdA=="
	blocked := e.startsApply(t, edit)
	if blocked.Annotations[AfterFailedApplyAnnotation] != "true" {
		t.Fatalf("edit annotations = %v, want it marked after a failed apply", blocked.Annotations)
	}
	e.blockJob(t, blocked.Name, t0.Add(-2*time.Minute))
	retry := e.startsApply(t, machineIn())
	if retry.Annotations[state.InputsHashAnnotation] != e.hash || retry.Annotations[AfterFailedApplyAnnotation] != "true" {
		t.Fatalf("retry annotations = %v, want inputs hash %s, marked after a failed apply", retry.Annotations, e.hash)
	}
	e.blockJob(t, retry.Name, t0.Add(-time.Minute))
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	name := planName(testName, retry.Name, blockedPlan().Hash)
	if c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition); c == nil || !namesJob(c.Message, retry.Name) ||
		!strings.HasSuffix(c.Message, planApproveCommand(name, testNS)) {
		t.Errorf("ApplyJobSucceeded = %+v, want the blocked retry's plan %s", c, name)
	}
	if p := e.plan(t, name); p.Spec.InputsHash != e.hash || phaseOf(p) != infrav1.PlanPhasePending {
		t.Errorf("the retry's plan = %+v", p)
	}
}
