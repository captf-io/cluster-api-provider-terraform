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

package runlease

import (
	"context"
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
)

// holderPod returns a fixture pod of the Job job in phase; terminating
// sets its deletionTimestamp.
func holderPod(job string, phase corev1.PodPhase, terminating bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: job + "-x7k2p", Labels: map[string]string{batchv1.JobNameLabel: job}},
		Status:     corev1.PodStatus{Phase: phase},
	}
	if terminating {
		p.DeletionTimestamp = new(metav1.NewTime(t0))
		p.Finalizers = []string{"test/hold"}
	}
	return p
}

// TestFreeHolderPods checks that Free keeps a lease whose holder Job is
// gone or finished while a pod of the Job has not reached a terminal
// phase, and frees it once the pods are terminal, absent, or the backstop
// has passed.
func TestFreeHolderPods(t *testing.T) {
	t.Parallel()
	const holder = "job-c"
	tests := []struct {
		name string
		age  time.Duration
		objs []client.Object
		want bool
	}{
		{"Job gone, pod running", 2 * Grace, []client.Object{holderPod(holder, corev1.PodRunning, false)}, false},
		{"Job gone, pod pending", 2 * Grace, []client.Object{holderPod(holder, corev1.PodPending, false)}, false},
		{"Job gone, no pod, past grace", 2 * Grace, nil, true},
		{"Job gone, no pod, within grace", Grace / 2, nil, false},
		{"Job gone, pod running, within grace", Grace / 2, []client.Object{holderPod(holder, corev1.PodRunning, false)}, false},
		{"Job gone, pod terminal", 2 * Grace, []client.Object{holderPod(holder, corev1.PodSucceeded, false)}, true},
		{"Job failed, pod terminating", 2 * Grace, []client.Object{jobIn(holder, jobs.Failed), holderPod(holder, corev1.PodRunning, true)}, false},
		{"Job failed, pod failed", 2 * Grace, []client.Object{jobIn(holder, jobs.Failed), holderPod(holder, corev1.PodFailed, false)}, true},
		{"Job succeeded, no pod", 2 * Grace, []client.Object{jobIn(holder, jobs.Succeeded)}, true},
		{"Job running, pods terminal", 2 * Grace, []client.Object{jobIn(holder, jobs.Running), holderPod(holder, corev1.PodFailed, false)}, false},
		{"another Job's pod ignored", 2 * Grace, []client.Object{holderPod("other", corev1.PodRunning, false)}, true},
		{"past backstop, pod running", backstop(held(spec("l", holder, jobs.OpApply), t0)) + time.Second, []client.Object{holderPod(holder, corev1.PodRunning, true)}, true},
		{"past backstop, Job failed, pod running", backstop(held(spec("l", holder, jobs.OpApply), t0)) + time.Second, []client.Object{jobIn(holder, jobs.Failed), holderPod(holder, corev1.PodRunning, false)}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			objs := make([]client.Object, len(tc.objs))
			for i, o := range tc.objs {
				objs[i] = o.DeepCopyObject().(client.Object)
			}
			c := newClient(t, interceptor.Funcs{}, objs...)
			l := held(spec("captf-run-x", holder, jobs.OpApply), t0)
			got, err := Free(t.Context(), c, l, t0.Add(tc.age))
			if err != nil || got != tc.want {
				t.Errorf("Free = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// TestFreePodListError checks that a failing pod list is an error, not a
// free lease.
func TestFreePodListError(t *testing.T) {
	t.Parallel()
	c := newClient(t, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return errBoom
	}}, jobIn("job-c", jobs.Failed))
	l := held(spec("captf-run-x", "job-c", jobs.OpApply), t0)
	if free, err := Free(t.Context(), c, l, t0.Add(2*Grace)); !errors.Is(err, errBoom) || free {
		t.Errorf("Free = %v, %v; want the list error", free, err)
	}
}
