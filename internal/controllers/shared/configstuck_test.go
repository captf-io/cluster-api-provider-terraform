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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
)

// TestConfigStuckJobDeleted proves an active Job whose pod has waited
// ContainerConfigGrace on a container the kubelet cannot create (its
// credential mirror revoked) is deleted, with StuckJobDeleted naming the
// kubelet's reason, and status.activeJob released; one that waited less
// is left alone.
func TestConfigStuckJobDeleted(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		age     time.Duration
		deleted bool
	}{
		{"past the grace", 3 * time.Minute, true},
		{"within the grace", time.Minute, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: inputs.RunName("a1")}}
			e := newEnv(t, world(machine(withFinalizer, notPaused, activeJob("a1")), runSecret)...)
			j := job("a1", jobs.OpApply, jobs.Running, t0)
			j.CreationTimestamp = metav1.NewTime(t0.Add(-tt.age))
			e.runner.jobs = append(e.runner.jobs, j)
			pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "a1-pod", CreationTimestamp: metav1.NewTime(t0.Add(-tt.age))}}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: jobs.SourceContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CreateContainerConfigError", Message: `secret "captf-creds-aws" not found`,
			}}}}
			e.runner.pods["a1"] = []corev1.Pod{pod}
			if _, err := reconcileOnce(t, e, e.kindFor(t, readyOwner())); err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(e.runner.deleted, "a1"); got != tt.deleted {
				t.Fatalf("deleted %v, want deleted %v", e.runner.deleted, tt.deleted)
			}
			if !tt.deleted {
				return
			}
			evs := e.rec.only(EventStuckJobDeleted)
			if len(evs) != 1 || evs[0].eventType != corev1.EventTypeWarning {
				t.Errorf("StuckJobDeleted = %+v", evs)
			}
			if m := e.get(t); m.Status.ActiveJob.Name != "" {
				t.Errorf("activeJob = %+v, want released", m.Status.ActiveJob)
			}
		})
	}
}
