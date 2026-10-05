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
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
)

// heldPending is the pending exports change the held-condition tests use.
var heldPending = inputs.Pending{ExportsHash: "h2:e1", ApprovalHash: "h2:a", Job: "a2", Summary: blockedSummaryText}

// heldReconciler returns a reconciler over a fresh machine whose durable
// inputs carry p (nil for none), with prev, when non-nil, as the
// object's ApplyJobSucceeded condition before the pass; t fails the test
// on error.
func heldReconciler(t *testing.T, p *inputs.Pending, prev *metav1.Condition) *reconciler {
	t.Helper()
	e := newEnv(t, machine())
	k := e.kindFor(t, readyOwner)
	if prev != nil {
		conditions.Set(k.obj, *prev)
	}
	return &reconciler{d: e.d, k: k, obj: k.obj, durable: &inputs.Durable{Pending: p}}
}

// TestApplyJobConditionHeld: while a change of the cluster's exports waits
// for approval, ApplyJobSucceeded stays DestructivePlanBlocked over a held
// apply's success, naming the blocked Job, its resources, the held
// exports and the command with the approval hash of the current inputs
// when the pass built them; a pass that built none keeps the condition
// already reported, and holds nothing it did not report (the change may
// be withdrawn); another failure, or no pending change, shows through.
func TestApplyJobConditionHeld(t *testing.T) {
	t.Parallel()
	succeeded := metav1.Condition{Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionTrue, Reason: infrav1.ApplySucceededReason, Message: "Job a3"}

	r := heldReconciler(t, &heldPending, nil)
	if c := r.applyJobCondition(&Bookkeeping{ApplyJob: succeeded}); c != succeeded {
		t.Errorf("a pass without inputs held a change it did not report: %+v", c)
	}
	r.guard = &Guard{}
	c := r.applyJobCondition(&Bookkeeping{ApplyJob: succeeded})
	if c.Status != metav1.ConditionFalse || c.Reason != infrav1.DestructivePlanBlockedReason || !namesJob(c.Message, "a2") ||
		!strings.Contains(c.Message, blockedSummaryText) || !strings.Contains(c.Message, "keeps applying with the exports of its last successful apply") ||
		!strings.HasSuffix(c.Message, "kubectl annotate terraformmachine m1 -n team-a captf.io/approve-destructive-plan=h2:a --overwrite") {
		t.Errorf("held condition = %+v", c)
	}

	r.guard = &Guard{Held: true, ApprovalHash: "h2:b"}
	if c := r.applyJobCondition(&Bookkeeping{ApplyJob: succeeded}); !strings.HasSuffix(c.Message, "=h2:b --overwrite") {
		t.Errorf("with the current inputs' approval hash: %s", c.Message)
	}

	prev := heldCondition(&heldPending, "h2:b", r.k.Kind(), r.obj)
	r = heldReconciler(t, &heldPending, &prev)
	if c := r.applyJobCondition(&Bookkeeping{ApplyJob: succeeded}); c.Message != prev.Message {
		t.Errorf("a pass without inputs replaced the reported hash: %s", c.Message)
	}

	failed := metav1.Condition{Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionFalse, Reason: infrav1.ApplyFailedReason, Message: "Job a4"}
	if c := r.applyJobCondition(&Bookkeeping{ApplyJob: failed}); c.Reason != infrav1.ApplyFailedReason {
		t.Errorf("a held apply's failure is hidden: %+v", c)
	}
	if c := heldReconciler(t, nil, nil).applyJobCondition(&Bookkeeping{ApplyJob: succeeded}); c.Reason != infrav1.ApplySucceededReason {
		t.Errorf("no pending change: %+v", c)
	}
}

// exportsEnv returns an env whose machine's durable inputs render
// exports, and the hash of exports; t fails the test on error.
func exportsEnv(t *testing.T, exports string) (*env, *fakeKind, *inputs.Durable, string) {
	t.Helper()
	e := newEnv(t, machine())
	k := e.kindFor(t, readyOwner)
	in := machineIn()
	in.ClusterOutputs = json.RawMessage(exports)
	files, err := render.Root(contract.RoleMachine, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := inputs.Write(t.Context(), e.c, k.obj, files, inputs.Meta{Image: "registry.example/mod:1.0"}); err != nil {
		t.Fatal(err)
	}
	d, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil {
		t.Fatal(err)
	}
	h, err := hash.Exports(json.RawMessage(exports))
	if err != nil {
		t.Fatal(err)
	}
	return e, k, d, h
}

// TestRecordExports: a successful pool apply's exports, read back from
// the durable tfvars, are recorded as applied and drop a pending change;
// a held apply, an apply without the annotation, and one whose exports
// the durable tfvars no longer hold record nothing.
func TestRecordExports(t *testing.T) {
	t.Parallel()
	ok := func(annotations map[string]string) *finished {
		j := job("a1", jobs.OpApply, jobs.Succeeded, t0)
		j.Annotations = annotations
		return &finished{job: &j, ok: true}
	}
	t.Run("recorded", func(t *testing.T) {
		t.Parallel()
		e, k, d, h := exportsEnv(t, `{"net":"n-2"}`)
		if err := inputs.SetPending(t.Context(), e.c, k.obj, heldPending); err != nil {
			t.Fatal(err)
		}
		d.Pending = &heldPending
		bk := &Bookkeeping{}
		if err := bk.recordExports(t.Context(), e.d, k, ok(map[string]string{ClusterOutputsHashAnnotation: h}), d); err != nil {
			t.Fatal(err)
		}
		got, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
		if err != nil || !bk.ExportsRecorded || string(bk.AppliedExports) != `{"net":"n-2"}` ||
			string(got.AppliedClusterOutputs) != `{"net":"n-2"}` || got.Pending != nil {
			t.Errorf("recorded %v %s; durable applied %s, pending %+v, %v", bk.ExportsRecorded, bk.AppliedExports, got.AppliedClusterOutputs, got.Pending, err)
		}
	})
	for name, annotations := range map[string]map[string]string{
		"held":          {ClusterOutputsHashAnnotation: "", HeldClusterOutputsAnnotation: "true"},
		"not a pool":    {},
		"another apply": {ClusterOutputsHashAnnotation: "h2:other"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, k, d, h := exportsEnv(t, `{"net":"n-2"}`)
			if _, held := annotations[HeldClusterOutputsAnnotation]; held {
				annotations[ClusterOutputsHashAnnotation] = h
			}
			bk := &Bookkeeping{}
			if err := bk.recordExports(t.Context(), e.d, k, ok(annotations), d); err != nil {
				t.Fatal(err)
			}
			got, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
			if err != nil || bk.ExportsRecorded || got.AppliedClusterOutputs != nil {
				t.Errorf("recorded %v, durable applied %s, %v", bk.ExportsRecorded, got.AppliedClusterOutputs, err)
			}
		})
	}
}

// TestRecordPending: a newly blocked pool apply guarded for an exports
// change records the change, with the runner's summary; a blocked apply
// without the approval hash (a cluster's), or one guarded although it
// renders the applied exports (after a partial apply), records none.
func TestRecordPending(t *testing.T) {
	t.Parallel()
	e, k, d, h := exportsEnv(t, `{"net":"n-2"}`)
	j := job("a2", jobs.OpApply, jobs.Failed, t0.Add(-time.Minute))
	f := &finished{job: &j, blocked: true}
	bk := &Bookkeeping{}
	if err := bk.recordPending(t.Context(), e.d, k, f, nil); err != nil || bk.PendingSet != nil {
		t.Fatalf("an unguarded blocked apply recorded %+v, %v", bk.PendingSet, err)
	}
	j.Annotations = map[string]string{ClusterOutputsHashAnnotation: h, ApprovalHashAnnotation: "h2:a"}
	pods := podWith(blockedResult(), "")
	r, err := jobs.ParseResult(pods)
	if err != nil {
		t.Fatal(err)
	}
	f.result = r
	applied := *d
	applied.AppliedExportsHash = h
	if err := bk.recordPending(t.Context(), e.d, k, f, &applied); err != nil || bk.PendingSet != nil {
		t.Fatalf("a blocked apply of the applied exports recorded %+v, %v", bk.PendingSet, err)
	}
	if err := bk.recordPending(t.Context(), e.d, k, f, nil); err != nil {
		t.Fatal(err)
	}
	got, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	want := inputs.Pending{ExportsHash: h, ApprovalHash: "h2:a", Job: "a2", Summary: blockedSummaryText}
	if err != nil || got.Pending == nil || *got.Pending != want || bk.PendingSet == nil || *bk.PendingSet != want {
		t.Errorf("pending %+v (bookkeeping %+v), want %+v; %v", got.Pending, bk.PendingSet, want, err)
	}
}

// failedAt returns the termination message of an apply that failed in
// step, after the steps before it in a guarded apply ran.
func failedAt(step string) string {
	var steps []runner.Step
	for _, s := range []string{runner.StepInit, runner.StepPlan, runner.StepShowJSON, runner.StepApply} {
		if s == step {
			steps = append(steps, runner.Step{Name: s, Exit: 1})
			break
		}
		steps = append(steps, runner.Step{Name: s})
	}
	return string(runner.Encode(runner.Result{
		Version: runner.ResultVersion, Op: runner.OpApply, Steps: steps,
		Error: &runner.Error{Kind: runner.ErrorKindStep, Step: &step, Tail: "Error: quota exceeded"},
	}))
}

// TestRecordPartial: a guarded pool apply that failed in or after its
// apply step, or whose result is unknown (its runner started, or its pod
// is gone), records its change as partly applied; one that failed before
// the apply step, whose runner never started (unscheduled, or its image
// never pulled), a held or unguarded one, and one when a partial change
// is already recorded record nothing.
func TestRecordPartial(t *testing.T) {
	t.Parallel()
	// killedPod is a pod whose runner started and was killed before it
	// wrote a result.
	killedPod := func() *corev1.Pod {
		p := podWith("", "")
		p.Status.ContainerStatuses[0].State.Terminated = &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}
		return p
	}
	guarded := map[string]string{ClusterOutputsHashAnnotation: "h2:e2", ApprovalHashAnnotation: "h2:a"}
	held := map[string]string{ClusterOutputsHashAnnotation: "h2:e1", ApprovalHashAnnotation: "h2:a", HeldClusterOutputsAnnotation: "true"}
	tests := []struct {
		name        string
		annotations map[string]string
		pod         *corev1.Pod
		recorded    *inputs.Partial
		want        bool
	}{
		{"failed in the apply step", guarded, podWith(failedAt(runner.StepApply), ""), nil, true},
		{"no result", guarded, nil, nil, true},
		{"failed at init", guarded, podWith(failedAt(runner.StepInit), ""), nil, false},
		{"image never pulled", guarded, podWith("", "ImagePullBackOff"), nil, false},
		{"pod never scheduled", guarded, &corev1.Pod{}, nil, false},
		{"runner never started", guarded, podWith("", "ContainerCreating"), nil, false},
		{"runner killed without a result", guarded, killedPod(), nil, true},
		{"held", held, podWith(failedAt(runner.StepApply), ""), nil, false},
		{"unguarded", map[string]string{ClusterOutputsHashAnnotation: "h2:e1"}, podWith(failedAt(runner.StepApply), ""), nil, false},
		{"already recorded", guarded, podWith(failedAt(runner.StepApply), ""), &inputs.Partial{ExportsHash: "h2:e2", Job: "a1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e, k, d, _ := exportsEnv(t, `{"net":"n-2"}`)
			d.Partial = tt.recorded
			j := job("a2", jobs.OpApply, jobs.Failed, t0.Add(-time.Minute))
			j.Annotations = tt.annotations
			f := &finished{job: &j, pod: tt.pod}
			if tt.pod != nil {
				// A pod without a termination message has no result.
				f.result, _ = jobs.ParseResult(tt.pod)
			}
			bk := &Bookkeeping{}
			if err := bk.recordPartial(t.Context(), e.d, k, f, d); err != nil {
				t.Fatal(err)
			}
			got, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
			if err != nil {
				t.Fatal(err)
			}
			want := inputs.Partial{ExportsHash: "h2:e2", Job: "a2"}
			if tt.want != (got.Partial != nil) || tt.want != (bk.PartialSet != nil) || (tt.want && (*got.Partial != want || *bk.PartialSet != want)) {
				t.Errorf("partial %+v (bookkeeping %+v), want recorded %v", got.Partial, bk.PartialSet, tt.want)
			}
		})
	}
}

// TestApplyJobConditionUnheld: a blocked pool apply while a change may
// be partly applied reports that the pool waits for the approval of the
// current approval hash, naming the Job that may have left the change,
// and keeps that report for the same Job; a held pool keeps its held
// condition.
func TestApplyJobConditionUnheld(t *testing.T) {
	t.Parallel()
	blocked := job("a3", jobs.OpApply, jobs.Failed, t0)
	blocked.Annotations = map[string]string{ClusterOutputsHashAnnotation: "h2:e1", ApprovalHashAnnotation: "h2:b"}
	bk := &Bookkeeping{
		LastApply: &blocked, LastApplyBlocked: true,
		ApplyJob: metav1.Condition{Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionFalse, Reason: infrav1.DestructivePlanBlockedReason, Message: "Job a3: x"},
	}
	p := heldPending
	p.Job = "a3"
	r := heldReconciler(t, &p, nil)
	r.guard = &Guard{ExportsHash: "h2:e1", Guarded: true, Partial: true, PartialJob: "a1", ApprovalHash: "h2:b"}
	c := r.applyJobCondition(bk)
	if c.Status != metav1.ConditionFalse || c.Reason != infrav1.DestructivePlanBlockedReason || !namesJob(c.Message, "a3") ||
		!strings.Contains(c.Message, blockedSummaryText) || !strings.Contains(c.Message, "Job a1, an earlier apply of a change of them, failed and may have applied part of it") ||
		strings.Contains(c.Message, "keeps applying") || !strings.HasSuffix(c.Message, "=h2:b --overwrite") {
		t.Errorf("unheld condition = %+v", c)
	}

	r = heldReconciler(t, &p, &c)
	r.guard = &Guard{ExportsHash: "h2:e1", Guarded: true, Partial: true, ApprovalHash: "h2:b"}
	if got := r.applyJobCondition(bk); got.Message != c.Message {
		t.Errorf("the report of the same Job changed: %s", got.Message)
	}

	r = heldReconciler(t, &p, nil)
	r.guard = &Guard{ExportsHash: "h2:e1", Guarded: true, Unrecorded: true, ApprovalHash: "h2:b"}
	if got := r.applyJobCondition(bk); !strings.Contains(got.Message, "are not recorded") || strings.Contains(got.Message, "keeps applying") {
		t.Errorf("unrecorded condition = %s", got.Message)
	}

	r = heldReconciler(t, &p, nil)
	r.guard = &Guard{Held: true, ApprovalHash: "h2:c"}
	if got := r.applyJobCondition(bk); !strings.Contains(got.Message, "keeps applying") {
		t.Errorf("held condition = %s", got.Message)
	}
}

// TestHeldApproval: the approval hash a held condition shows is read back
// for its Job only.
func TestHeldApproval(t *testing.T) {
	t.Parallel()
	r := heldReconciler(t, &heldPending, nil)
	held := heldCondition(&heldPending, "h2:b", r.k.Kind(), r.obj)
	if got := heldApproval(&held, "a2"); got != "h2:b" {
		t.Errorf("heldApproval = %q, want h2:b", got)
	}
	failed := metav1.Condition{Reason: infrav1.ApplyFailedReason, Message: "Job a2"}
	for name, c := range map[string]*metav1.Condition{"nil": nil, "another Job": &held, "another reason": &failed} {
		job := "a2"
		if name == "another Job" {
			job = "a3"
		}
		if got := heldApproval(c, job); got != "" {
			t.Errorf("%s: heldApproval = %q, want none", name, got)
		}
	}
}

// TestApplyJobConditionWithdrawn: once the exports are those of the last
// successful apply again, a pool's newest blocked apply of a change of
// them reports the last successful apply standing, with no approve
// command; a pass that built no inputs keeps that report; a guarded pass
// (a partly applied change) or exports still at the change do not.
func TestApplyJobConditionWithdrawn(t *testing.T) {
	t.Parallel()
	blocked := job("a3", jobs.OpApply, jobs.Failed, t0)
	blocked.Annotations = map[string]string{ClusterOutputsHashAnnotation: "h2:e1", ApprovalHashAnnotation: "h2:b"}
	bk := &Bookkeeping{
		LastApply: &blocked, LastApplyBlocked: true,
		ApplyJob: metav1.Condition{Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionFalse, Reason: infrav1.DestructivePlanBlockedReason, Message: "Job a3: x"},
	}
	p := heldPending
	p.Job, p.ExportsHash = "a3", "h2:e1"

	r := heldReconciler(t, &p, nil)
	r.guard = &Guard{ExportsHash: "h2:e0", Settled: true}
	c := r.applyJobCondition(bk)
	if c.Status != metav1.ConditionTrue || c.Reason != infrav1.ApplySucceededReason || !strings.Contains(c.Message, "Job a3 stopped before was withdrawn") ||
		strings.Contains(c.Message, infrav1.ApproveDestructivePlanAnnotation) || jobNamed(c.Message) != "" {
		t.Errorf("withdrawn condition = %+v", c)
	}
	if got := heldReconciler(t, &p, &c).applyJobCondition(bk); got.Message != c.Message {
		t.Errorf("a pass without inputs replaced the withdrawn report: %s", got.Message)
	}

	for name, g := range map[string]*Guard{
		"partly applied": {ExportsHash: "h2:e0", Settled: true, Guarded: true, Partial: true, ApprovalHash: "h2:c"},
		"not withdrawn":  {ExportsHash: "h2:e1", Held: true, ApprovalHash: "h2:b"},
	} {
		r := heldReconciler(t, &p, nil)
		r.guard = g
		if got := r.applyJobCondition(bk); got.Reason != infrav1.DestructivePlanBlockedReason {
			t.Errorf("%s: %+v", name, got)
		}
	}
}
