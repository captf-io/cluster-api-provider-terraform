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
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
)

// deadlineJob returns a failed Job called name, of op, finished at at, that
// the Job controller killed for exceeding activeDeadlineSeconds.
func deadlineJob(name string, op jobs.Op, at time.Time) batchv1.Job {
	j := job(name, op, jobs.Failed, at)
	j.Status.Conditions[0].Reason = batchv1.JobReasonDeadlineExceeded
	return j
}

// TestDeadlineKilledJobsCountAsFailures proves a Job killed by its deadline
// counts toward backoff even when the runner reported it interrupted or an
// annotation says so, while a plain interrupt still costs nothing.
func TestDeadlineKilledJobsCountAsFailures(t *testing.T) {
	t.Parallel()
	stale := deadlineJob("a3", jobs.OpApply, t0.Add(-time.Minute))
	stale.Annotations = map[string]string{BookkeptAnnotation: "true", InterruptedAnnotation: "true"}
	done := []finished{
		{job: ptr(deadlineJob("a4", jobs.OpApply, t0)), interrupted: true},
		{job: &stale, bookkept: true, interrupted: true},
		{job: ptr(job("a2", jobs.OpApply, jobs.Failed, t0.Add(-2*time.Minute))), interrupted: true},
	}
	bk := &Bookkeeping{View: JobsView{Failures: map[jobs.Op]int{}, LastFailure: map[jobs.Op]time.Time{}}}
	bk.countFailures(done)
	if bk.View.Failures[jobs.OpApply] != 2 || !bk.View.LastFailure[jobs.OpApply].Equal(t0) {
		t.Errorf("apply failures = %d at %s, want 2 at %s", bk.View.Failures[jobs.OpApply], bk.View.LastFailure[jobs.OpApply], t0)
	}
}

// TestDeadlineKilledJobsReachFailureLimit proves repeated deadline kills
// accumulate failures, so a step that always hangs reaches the retry limit.
func TestDeadlineKilledJobsReachFailureLimit(t *testing.T) {
	t.Parallel()
	const limit = 5
	var done []finished
	for i := range limit {
		done = append(done, finished{
			job:         ptr(deadlineJob("a", jobs.OpApply, t0.Add(-time.Duration(i)*time.Minute))),
			interrupted: true,
		})
	}
	bk := &Bookkeeping{View: JobsView{Failures: map[jobs.Op]int{}, LastFailure: map[jobs.Op]time.Time{}}}
	bk.countFailures(done)
	if got := bk.View.Failures[jobs.OpApply]; got != limit {
		t.Errorf("apply failures = %d, want %d", got, limit)
	}
}

// TestDeadlineJobIgnoresInterruptedAnnotation proves collectFinished does
// not read a bookkept deadline-killed Job as interrupted, and MarkBookkept
// never persists the interrupted mark for one.
func TestDeadlineJobIgnoresInterruptedAnnotation(t *testing.T) {
	t.Parallel()
	stale := deadlineJob("a3", jobs.OpApply, t0)
	stale.Annotations = map[string]string{BookkeptAnnotation: "true", InterruptedAnnotation: "true"}
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	e.runner.jobs = append(e.runner.jobs, stale)
	got, err := collectFinished(t.Context(), e.d, e.runner.jobs)
	if err != nil || len(got) != 1 || got[0].interrupted {
		t.Errorf("collectFinished = %+v, %v; want the deadline Job not interrupted", got, err)
	}

	fresh := deadlineJob("a5", jobs.OpApply, t0)
	c := fake.NewClientBuilder().WithObjects(&fresh).Build()
	m := &Bookkeeping{unmarked: []finished{{job: fresh.DeepCopy(), interrupted: true}}}
	if err := m.MarkBookkept(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	var after batchv1.Job
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(&fresh), &after); err != nil {
		t.Fatal(err)
	}
	if after.Annotations[BookkeptAnnotation] != "true" || after.Annotations[InterruptedAnnotation] != "" {
		t.Errorf("annotations = %v, want bookkept without interrupted", after.Annotations)
	}
}
