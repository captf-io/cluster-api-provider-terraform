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
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// firstApplyEnv returns, failing t on error, an env whose first pass over
// an immutable machine (built with mut as well) started its first apply,
// the kind each pass reconciles, and that Job's name.
func firstApplyEnv(t *testing.T, mut ...func(*infrav1.TerraformMachine)) (*env, func() *fakeKind, string) {
	t.Helper()
	e := newEnv(t, world(machine(append([]func(*infrav1.TerraformMachine){withFinalizer, notPaused, recordsMachine}, mut...)...))...)
	kind := func() *fakeKind {
		k := e.kindFor(t, readyOwner())
		k.in, k.mutable = machineIn(), false
		return k
	}
	if _, err := reconcileOnce(t, e, kind()); err != nil {
		t.Fatal(err)
	}
	first := e.newest(t)
	if got := e.jobNamed(t, first).Annotations[jobs.StateLockVersionAnnotation]; got != jobs.StateLockAbsent {
		t.Fatalf("first apply recorded state lock %q, want %q", got, jobs.StateLockAbsent)
	}
	return e, kind, first
}

// failAtDeadline marks Job name of e failed at its deadline, its pending
// pod deleted with it, as the Job controller leaves it, failing t when
// there is no such Job.
func (e *env) failAtDeadline(t *testing.T, name string) {
	t.Helper()
	e.jobNamed(t, name).Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded,
		LastTransitionTime: metav1.NewTime(t0.Add(-time.Minute)),
	}}
	e.runner.pods[name] = nil
}

// TestNeverStartedApplyIsNotHeld proves a first apply whose pod never ran
// (deleted at the deadline, so neither a pod nor a result is left) and
// that left the state lock as it found it is no unconfirmed apply: it is
// not recorded as interrupted, the object is not held, and the next apply
// starts.
func TestNeverStartedApplyIsNotHeld(t *testing.T) {
	t.Parallel()
	e, kind, first := firstApplyEnv(t)
	e.failAtDeadline(t, first)
	for range 2 {
		if _, err := reconcileOnce(t, e, kind()); err != nil {
			t.Fatal(err)
		}
	}
	if c := conditions.Get(e.get(t), infrav1.StateReadableCondition); c != nil && c.Reason == infrav1.ApplyOutcomeUnknownReason {
		t.Errorf("StateReadable = %+v, want no hold", c)
	}
	if d, _ := inputs.Read(t.Context(), e.c, testNS, "m", testName); d != nil && d.InterruptedApply != "" {
		t.Errorf("InterruptedApply = %q, want none", d.InterruptedApply)
	}
	if len(e.runner.created) != 2 {
		t.Errorf("created %v, want a second apply", e.runner.created)
	}
}

// TestLockedApplyWithoutPodIsHeld proves the same Job is held when its
// runtime did touch the state lock (the Lease changed): it ran, so it may
// have created resources.
func TestLockedApplyWithoutPodIsHeld(t *testing.T) {
	t.Parallel()
	e, kind, first := firstApplyEnv(t)
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: state.LeaseName(suffixOf(t, state.KindTerraformMachine, testName))}}
	if err := e.c.Create(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	e.failAtDeadline(t, first)
	if _, err := reconcileOnce(t, e, kind()); err != nil {
		t.Fatal(err)
	}
	if c := conditions.Get(e.get(t), infrav1.StateReadableCondition); c == nil || c.Reason != infrav1.ApplyOutcomeUnknownReason {
		t.Errorf("StateReadable = %+v, want ApplyOutcomeUnknown", c)
	}
	if len(e.runner.created) != 1 {
		t.Errorf("created %v, want no second apply", e.runner.created)
	}
	// The Job is over: InputsApplied no longer says it runs.
	if c := conditions.Get(e.get(t), infrav1.InputsAppliedCondition); c == nil || c.Reason == infrav1.ApplyRunningReason || c.Status != metav1.ConditionUnknown {
		t.Errorf("InputsApplied = %+v, want Unknown once no apply runs", c)
	}
}

// TestFailedApplyStepWithoutStateIsHeld proves a first apply that failed
// after its apply step ran, with no state saved, is unconfirmed: no
// second apply runs, a deletion keeps the finalizer, and confirming the
// Job created nothing (ConfirmNoResourcesAnnotation) releases it.
func TestFailedApplyStepWithoutStateIsHeld(t *testing.T) {
	t.Parallel()
	e, kind, first := firstApplyEnv(t)
	step := runner.StepApply
	e.finishRunner(t, first, jobs.Failed, t0.Add(-time.Minute), string(runner.Encode(runner.Result{
		Version: runner.ResultVersion, Op: runner.OpApply,
		Steps: []runner.Step{{Name: runner.StepInit}, {Name: runner.StepValidate}, {Name: runner.StepApply, Exit: 1}},
		Error: &runner.Error{Kind: runner.ErrorKindStep, Step: &step, Tail: "Error: saving state: connection refused"},
	})))
	if _, err := reconcileOnce(t, e, kind()); err != nil {
		t.Fatal(err)
	}
	if c := conditions.Get(e.get(t), infrav1.StateReadableCondition); c == nil || c.Reason != infrav1.ApplyOutcomeUnknownReason {
		t.Fatalf("StateReadable = %+v, want ApplyOutcomeUnknown", c)
	}
	if len(e.runner.created) != 1 {
		t.Errorf("created %v, want no second apply", e.runner.created)
	}

	if err := e.c.Delete(t.Context(), e.get(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileOnce(t, e, kind()); err != nil {
		t.Fatal(err)
	}
	if m := e.get(t); m == nil || !slices.Contains(m.Finalizers, testFinal) {
		t.Fatal("the finalizer went over an apply that may have created resources")
	}

	m := e.get(t)
	m.Annotations = map[string]string{infrav1.ConfirmNoResourcesAnnotation: first}
	if err := e.c.Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileOnce(t, e, kind()); err != nil {
		t.Fatal(err)
	}
	if e.get(t) != nil {
		t.Error("the confirmed object kept its finalizer")
	}
}

// TestOwnerlessReleaseWaitsForBookkeeping proves a deleting object without
// an owner reference keeps its finalizer through the preamble while an
// apply bookkeeping has not read yet may be unconfirmed: one that crashed
// (finished, no result, not bookkept), or the one status.activeJob names
// that is gone. The full reconcile records it first.
func TestOwnerlessReleaseWaitsForBookkeeping(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		jobs []batchv1.Job
	}{
		{"crashed, not bookkept", []batchv1.Job{job("j1", jobs.OpApply, jobs.Failed, t0)}},
		{"vanished", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, machine(deleting, notPaused, activeJob("j1")))
			e.runner.jobs = tt.jobs
			if err := inputs.WriteAttempt(t.Context(), e.c, e.get(t), inputs.Record{
				Files: renderMachine(t), Image: "registry.example/mod:1.0", Identity: testIdentity, InputsHash: "h1:x", Job: "j1",
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := Preamble(t.Context(), e.d, e.kindFor(t, OwnerInfo{})); err != nil {
				t.Fatal(err)
			}
			if m := e.get(t); m == nil || !slices.Contains(m.Finalizers, testFinal) {
				t.Error("the finalizer went before bookkeeping read the apply")
			}
		})
	}
}
