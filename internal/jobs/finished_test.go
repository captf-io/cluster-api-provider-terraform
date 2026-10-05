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

package jobs

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestFinishedAtDeadlineOp checks FinishedAt's fallback order (condition
// time, then completionTime, then creation time), DeadlineExceeded's
// reason match, and OpOf's op label read.
func TestFinishedAtDeadlineOp(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	failedAt := created.Add(5 * time.Minute)
	completedAt := created.Add(7 * time.Minute)
	job := func(conds ...batchv1.JobCondition) *batchv1.Job {
		return &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created), Labels: map[string]string{OpLabel: "destroy"}},
			Status:     batchv1.JobStatus{Conditions: conds},
		}
	}
	failed := job(batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded, LastTransitionTime: metav1.NewTime(failedAt)})
	if got := FinishedAt(failed); !got.Equal(failedAt) {
		t.Errorf("failed Job FinishedAt = %s, want the Failed condition time %s", got, failedAt)
	}
	if !DeadlineExceeded(failed) || OpOf(failed) != OpDestroy {
		t.Errorf("DeadlineExceeded/OpOf wrong for %+v", failed.Status)
	}

	done := job(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	done.Status.CompletionTime = &metav1.Time{Time: completedAt}
	if got := FinishedAt(done); !got.Equal(completedAt) || DeadlineExceeded(done) {
		t.Errorf("completed Job without condition time: FinishedAt = %s, want completionTime", got)
	}

	bare := job(batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"})
	if got := FinishedAt(bare); !got.Equal(created) || DeadlineExceeded(bare) {
		t.Errorf("FinishedAt fallback = %s, want creation time", got)
	}

	running := job(batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionFalse})
	if !FinishedAt(running).IsZero() {
		t.Error("running Job has a finish time")
	}
}

// TestSourceStartedAt: captf_job_queue_seconds ends at the source
// container's start, terminated or running; other containers do not count.
func TestSourceStartedAt(t *testing.T) {
	t.Parallel()
	at := metav1.NewTime(time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC))
	pod := func(cs ...corev1.ContainerStatus) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: cs}}
	}
	cases := []struct {
		name string
		pod  *corev1.Pod
		ok   bool
	}{
		{name: "terminated", pod: pod(corev1.ContainerStatus{Name: SourceContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{StartedAt: at}}}), ok: true},
		{name: "running", pod: pod(corev1.ContainerStatus{Name: SourceContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: at}}}), ok: true},
		{name: "never started", pod: pod(corev1.ContainerStatus{Name: SourceContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}})},
		{name: "waiting", pod: pod(corev1.ContainerStatus{Name: SourceContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}})},
		{name: "another container", pod: pod(corev1.ContainerStatus{Name: "other", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: at}}})},
		{name: "no status", pod: pod()},
	}
	for _, c := range cases {
		got, ok := SourceStartedAt(c.pod)
		if ok != c.ok || (ok && !got.Equal(at.Time)) {
			t.Errorf("%s: %v, %v", c.name, got, ok)
		}
	}
}
