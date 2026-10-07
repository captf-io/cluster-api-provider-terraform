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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
)

// killedPod returns a pod whose runner started and was killed without
// writing a result (OOMKilled, exit 137).
func killedPod() corev1.Pod {
	p := corev1.Pod{}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  jobs.SourceContainer,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}},
	}}
	return p
}

// TestCrashedFirstApplyHolds: a first apply killed mid-run (its runner
// started, no result) may have created resources before any state was
// written. For every kind, live or deleting, the object is held with
// ApplyOutcomeUnknown naming the Job: no second first apply (a second set
// of resources), no finalizer dropped over the first. Confirming that the
// Job created nothing (ConfirmNoResourcesAnnotation) releases it in the
// same pass: the live object applies again, the deleting one goes.
func TestCrashedFirstApplyHolds(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name              string
		mutable, deleting bool
	}{
		{"immutable, live", false, false},
		{"immutable, deleting", false, true},
		{"mutable, live", true, false},
		{"mutable, deleting", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, world(machine(withFinalizer, notPaused, recordsMachine))...)
			kind := func() *fakeKind {
				k := e.kindFor(t, readyOwner)
				k.in, k.mutable = machineIn(), tt.mutable
				return k
			}
			if _, err := reconcileOnce(t, e, kind()); err != nil {
				t.Fatal(err)
			}
			first := e.newest(t)
			e.jobNamed(t, first).Status.Conditions = []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(t0.Add(-time.Minute)),
			}}
			e.runner.pods[first] = []corev1.Pod{killedPod()}
			if tt.deleting {
				if err := e.c.Delete(t.Context(), e.get(t)); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				requeue, err := reconcileOnce(t, e, kind())
				if err != nil {
					t.Fatal(err)
				}
				if requeue != StateRequeue {
					t.Errorf("requeue = %s, want %s", requeue, StateRequeue)
				}
			}
			if len(e.runner.created) != 1 {
				t.Errorf("created %v after the crash, want nothing more", e.runner.created)
			}
			m := e.get(t)
			if m == nil {
				t.Fatal("the finalizer was dropped over what the crashed apply may have created")
			}
			c := conditions.Get(m, infrav1.StateReadableCondition)
			if c == nil || c.Reason != infrav1.ApplyOutcomeUnknownReason || !strings.Contains(c.Message, first) ||
				!strings.Contains(c.Message, infrav1.ConfirmNoResourcesAnnotation) || strings.Contains(c.Message, "Deletion is held") != tt.deleting {
				t.Errorf("StateReadable = %+v, want ApplyOutcomeUnknown naming %s", c, first)
			}
			if d, err := inputs.Read(t.Context(), e.c, testNS, "m", testName); err != nil || d.InterruptedApply != first || !d.Attempt.MayHaveApplied {
				t.Errorf("records = %+v, %v; want %s unconfirmed and may have applied", d, err, first)
			}

			e.annotate(t, infrav1.ConfirmNoResourcesAnnotation, "another-job")
			if _, err := reconcileOnce(t, e, kind()); err != nil {
				t.Fatal(err)
			}
			if len(e.runner.created) != 1 || e.get(t) == nil {
				t.Fatal("a confirmation naming another Job released the object")
			}
			e.annotate(t, infrav1.ConfirmNoResourcesAnnotation, first)
			if _, err := reconcileOnce(t, e, kind()); err != nil {
				t.Fatal(err)
			}
			if tt.deleting {
				if m := e.get(t); m != nil {
					t.Errorf("the confirmed deletion kept the object: %+v", m.Finalizers)
				}
				return
			}
			if len(e.runner.created) != 2 || !strings.Contains(e.newest(t), "-apply-") {
				t.Errorf("created %v after the confirmation, want a second first apply", e.runner.created)
			}
			if _, ok := e.get(t).Annotations[infrav1.ConfirmNoResourcesAnnotation]; ok {
				t.Error("the confirmation was not consumed")
			}
		})
	}
}

// TestRestoreClearsUnconfirmedApply: a successful restore newer than the
// newest apply replaces whatever that apply left: the record of an apply
// whose outcome is unconfirmed, and the attempt's may-have-applied mark,
// go. A restore older than the newest apply clears nothing.
func TestRestoreClearsUnconfirmedApply(t *testing.T) {
	t.Parallel()
	for name, newer := range map[string]bool{"newer than the apply": true, "older than the apply": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, world(machine(withFinalizer, notPaused))...)
			k := e.kindFor(t, readyOwner)
			e.writeUnapplied(t, k.obj, seedJob)
			if err := inputs.SetMayHaveApplied(t.Context(), e.c, k.obj); err != nil {
				t.Fatal(err)
			}
			durable, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
			if err != nil {
				t.Fatal(err)
			}
			restoredAt, appliedAt := t0, t0.Add(-time.Minute)
			if !newer {
				restoredAt, appliedAt = appliedAt, restoredAt
			}
			restore, apply := job("r", jobs.OpRestore, jobs.Succeeded, restoredAt), job(seedJob, jobs.OpApply, jobs.Failed, appliedAt)
			bk := &Bookkeeping{lastRestore: &finished{job: &restore, ok: true}, LastApply: &apply}
			if err := bk.restored(t.Context(), e.d, k, durable); err != nil {
				t.Fatal(err)
			}
			d, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
			if err != nil {
				t.Fatal(err)
			}
			if cleared := d.InterruptedApply == "" && !d.Attempt.MayHaveApplied; cleared != newer || bk.InterruptedCleared != newer || bk.MayHaveAppliedCleared != newer {
				t.Errorf("records = unconfirmed %q, may have applied %v (reported %v, %v); want cleared %v",
					d.InterruptedApply, d.Attempt.MayHaveApplied, bk.InterruptedCleared, bk.MayHaveAppliedCleared, newer)
			}
		})
	}
}
