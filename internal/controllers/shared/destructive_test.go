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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// blockedResult returns the termination message of a blocked guarded apply.
func blockedResult() string {
	return string(runner.Encode(runner.Result{
		Version: runner.ResultVersion, Op: runner.OpApply, Steps: []runner.Step{{Name: "init"}, {Name: "validate"}, {Name: "plan", Exit: 2}, {Name: "show-json"}},
		Error: &runner.Error{Kind: runner.ErrorKindBlocked, Tail: blockedSummaryText},
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
// set up as a mutable cluster with a healthy reading and drift as its
// DriftPolicy (or a disabled policy when drift is nil).
func (e blockedEnv) kind(t *testing.T, drift *infrav1.DriftPolicy) *fakeKind {
	t.Helper()
	k := e.kindFor(t, readyOwner)
	k.asCluster, k.mutable, k.in = true, true, machineIn()
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	if drift == nil {
		drift = &infrav1.DriftPolicy{IntervalSeconds: new(int32(0))}
	}
	k.clusterDrift = drift
	return k
}

// approve sets the ApproveDestructivePlanAnnotation on e's stored object to
// value, failing t on error.
func (e blockedEnv) approve(t *testing.T, value string) {
	t.Helper()
	obj := e.get(t)
	metav1.SetMetaDataAnnotation(&obj.ObjectMeta, infrav1.ApproveDestructivePlanAnnotation, value)
	if err := e.c.Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
}

// TestApprovalConsumed: an approval is for one change. Once an apply of the
// approved inputs hash has succeeded, the annotation is removed, so it cannot
// also approve a later destructive drift remediation of the same inputs. An
// approval for a hash that has not been applied yet stays.
func TestApprovalConsumed(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		approvedFor string
		applied     string
		want        bool // annotation still there
	}{
		{name: "approved apply succeeded: removed", approvedFor: "h2:approved", applied: "h2:approved", want: false},
		{name: "approved hash not applied yet: kept", approvedFor: "h2:next", applied: "h2:approved", want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			approved := func(m *infrav1.TerraformMachine) {
				metav1.SetMetaDataAnnotation(&m.ObjectMeta, infrav1.ApproveDestructivePlanAnnotation, tt.approvedFor)
			}
			e := newEnv(t, world(machine(withFinalizer, notPaused, provisioned, approved))...)
			e.state.st = &state.State{InputsHash: tt.applied}
			ok := job("a", jobs.OpApply, jobs.Succeeded, t0.Add(-time.Hour))
			ok.Annotations = map[string]string{state.InputsHashAnnotation: tt.applied, BookkeptAnnotation: "true"}
			e.runner.jobs = append(e.runner.jobs, ok)
			k := e.kindFor(t, readyOwner)
			k.asCluster, k.mutable, k.in = true, true, machineIn()
			k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
			k.clusterDrift = &infrav1.DriftPolicy{IntervalSeconds: new(int32(0))}
			if _, err := reconcileOnce(t, e, k); err != nil {
				t.Fatal(err)
			}
			_, kept := e.get(t).Annotations[infrav1.ApproveDestructivePlanAnnotation]
			if kept != tt.want {
				t.Errorf("approval annotation kept = %v, want %v", kept, tt.want)
			}
			// Once: the next reconcile finds no approval to consume.
			if _, err := reconcileOnce(t, e, e.kindFor(t, readyOwner)); err != nil {
				t.Fatal(err)
			}
			if n := e.rec.count(EventDestructivePlanApprovalConsumed); (n == 1) == tt.want || n > 1 {
				t.Errorf("DestructivePlanApprovalConsumed = %d, want %d", n, map[bool]int{true: 0, false: 1}[tt.want])
			}
		})
	}
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

// TestDestructivePlanBlocked: a blocked cluster apply sets
// ApplyJobSucceeded False/DestructivePlanBlocked with the addresses and the
// exact approve command, emits one Warning, is not retried for the same
// inputs hash (neither right away nor once the Job is bookkept), and runs
// with the approval once the annotation names that hash.
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
	c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition)
	cmd := "kubectl annotate terraformcluster " + testName + " -n " + testNS + " captf.io/approve-destructive-plan=" + e.hash + " --overwrite"
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.DestructivePlanBlockedReason ||
		!strings.HasPrefix(c.Message, "Job b: "+blockedSummaryText) || !strings.Contains(c.Message, cmd) {
		t.Fatalf("ApplyJobSucceeded = %+v, want DestructivePlanBlocked naming the addresses and %q", c, cmd)
	}
	if n, m := e.rec.count(EventDestructivePlanBlocked), e.rec.count(EventJobFailed); n != 1 || m != 0 {
		t.Errorf("events: %d DestructivePlanBlocked, %d JobFailed; want 1 and 0", n, m)
	}
	if lr := e.get(t).Status.LastRun; lr.Error.Kind != infrav1.RunErrorKindBlocked || lr.Error.Summary != blockedSummaryText {
		t.Errorf("lastRun.error = %+v", lr.Error)
	}
	if got := e.syncMarks(t); got[BlockedAnnotation] != "true" || got[BookkeptAnnotation] != "true" {
		t.Fatalf("blocked Job annotations = %v", got)
	}

	// Bookkept: its pod is not read again, and still nothing starts.
	e.runner.pods["b"] = nil
	for range 2 {
		if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.runner.created) != 0 || e.rec.count(EventDestructivePlanBlocked) != 1 {
		t.Fatalf("after bookkeeping: created %v, %d events", e.runner.created, e.rec.count(EventDestructivePlanBlocked))
	}
	if c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition); !strings.Contains(c.Message, "module.role.lb") {
		t.Errorf("the bookkept condition lost the addresses: %q", c.Message)
	}

	// An approval of another hash changes nothing.
	e.approve(t, "h1:old")
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 0 {
		t.Fatalf("created %v under an approval of another hash", e.runner.created)
	}

	// The approval of this hash starts the apply, with the approval.
	e.approve(t, e.hash)
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 1 {
		t.Fatalf("created %v, want one approved apply", e.runner.created)
	}
	args := sourceArgs(e.jobNamed(t, e.runner.created[0]))
	if !slices.Contains(args, "--guard-deletes") || !slices.Contains(args, "--inputs-hash="+e.hash) || !slices.Contains(args, "--allow-deletes-hash="+e.hash) {
		t.Errorf("approved apply args = %v", args)
	}
}

// TestDestructivePlanNewInputs: once the inputs change, the apply runs
// again (guarded, without an approval: the old one names another hash).
func TestDestructivePlanNewInputs(t *testing.T) {
	t.Parallel()
	e := newBlockedEnv(t, "h1:old", false)
	// The blocked Job rendered other inputs than the current ones.
	e.jobNamed(t, "b").Annotations[state.InputsHashAnnotation] = "h1:blocked"
	e.approve(t, "h1:blocked")
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 1 {
		t.Fatalf("created %v, want the apply of the new inputs", e.runner.created)
	}
	args := sourceArgs(e.jobNamed(t, e.runner.created[0]))
	if !slices.Contains(args, "--guard-deletes") || slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--allow-deletes-hash") }) {
		t.Errorf("new-inputs apply args = %v, want guarded and unapproved", args)
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
	summary := "Job d: 0 to add, 0 to change, 1 to destroy"
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
	e.approve(t, e.hash)
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
	if j.Annotations[RemediationAnnotation] != "true" || !slices.Contains(sourceArgs(j), "--allow-deletes-hash="+e.hash) {
		t.Errorf("remediation Job = %v %v", j.Annotations, sourceArgs(j))
	}
}

// TestBlockedCondition proves applyDestroyCondition, for a blocked Job,
// sets ApplyJobSucceeded False/DestructivePlanBlocked with the blocked
// summary and the exact approve command, and keeps the approve command even
// when the Job's result is gone (bookkept).
func TestBlockedCondition(t *testing.T) {
	t.Parallel()
	b := job("b", jobs.OpApply, jobs.Failed, t0)
	b.Annotations = map[string]string{state.InputsHashAnnotation: "h1:x"}
	m := machine()
	res := &jobs.Result{Error: &runner.Error{Kind: runner.ErrorKindBlocked, Tail: blockedSummaryText}}
	c := applyDestroyCondition(finished{job: &b, blocked: true, result: res}, state.KindTerraformCluster, m)
	want := "Job b: " + blockedSummaryText + ". Nothing was applied, and no apply of these inputs runs until they are approved. " +
		"To apply it, approve inputs hash h1:x: kubectl annotate terraformcluster m1 -n team-a captf.io/approve-destructive-plan=h1:x --overwrite"
	if c.Status != metav1.ConditionFalse || c.Reason != infrav1.DestructivePlanBlockedReason || c.Message != want || !namesJob(c.Message, "b") {
		t.Errorf("condition = %+v", c)
	}
	// Without its result (bookkept, condition lost) the command remains.
	c = applyDestroyCondition(finished{job: &b, blocked: true, bookkept: true}, state.KindTerraformCluster, m)
	if c.Reason != infrav1.DestructivePlanBlockedReason || !strings.Contains(c.Message, "approve-destructive-plan=h1:x --overwrite") {
		t.Errorf("bookkept condition = %+v", c)
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
			c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition)
			if c == nil || c.Reason != infrav1.DestructivePlanBlockedReason || !namesJob(c.Message, retry.Name) ||
				!strings.HasSuffix(c.Message, "="+e.hash+" --overwrite") {
				t.Fatalf("ApplyJobSucceeded = %+v, want the blocked retry approving %s", c, e.hash)
			}
			e.approve(t, e.hash)
			approved := e.startsApply(t, machineIn())
			if args := sourceArgs(approved); !slices.Contains(args, "--allow-deletes-hash="+e.hash) {
				t.Errorf("approved retry args = %v", args)
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
	if c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition); c == nil || !namesJob(c.Message, retry.Name) ||
		!strings.HasSuffix(c.Message, "="+e.hash+" --overwrite") {
		t.Errorf("ApplyJobSucceeded = %+v, want the blocked retry approving %s", c, e.hash)
	}
}
