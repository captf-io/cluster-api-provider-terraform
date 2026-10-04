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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/outputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// TestCountSample proves countSample increments unhealthySamples on an
// unhealthy, degraded or stopped reading, resets it on a healthy one, leaves
// it on a pending, terminated or unknown reading or a repeated sample, and
// no-ops on a kind without the field.
func TestCountSample(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		reason    string
		newSerial bool
		start     int32
		want      int32
	}{
		{"unhealthy counts", infrav1.InstanceUnhealthyReason, true, 0, 1},
		{"degraded counts", infrav1.InstanceDegradedReason, true, 2, 3},
		{"stopped counts", infrav1.InstanceStoppedReason, true, 1, 2},
		{"healthy resets", infrav1.HealthyReason, true, 3, 0},
		{"pending leaves it", infrav1.InstancePendingReason, true, 2, 2},
		{"terminated leaves it", infrav1.InstanceTerminatedReason, true, 2, 2},
		{"unknown leaves it", infrav1.HealthUnknownReason, true, 2, 2},
		{"same serial is not a new sample", infrav1.InstanceDegradedReason, false, 2, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := machine()
			m.Status.UnhealthySamples = tt.start
			conditions.Set(m, metav1.Condition{Type: infrav1.InfrastructureHealthyCondition, Status: metav1.ConditionFalse, Reason: tt.reason})
			countSample(m, CommonStatus{UnhealthySamples: &m.Status.UnhealthySamples}, tt.newSerial)
			if m.Status.UnhealthySamples != tt.want {
				t.Errorf("unhealthySamples = %d, want %d", m.Status.UnhealthySamples, tt.want)
			}
		})
	}
	// Kinds without the field and objects without the condition are left.
	m := machine()
	countSample(m, CommonStatus{}, true)
	countSample(m, CommonStatus{UnhealthySamples: &m.Status.UnhealthySamples}, true)
	if m.Status.UnhealthySamples != 0 {
		t.Errorf("unhealthySamples = %d", m.Status.UnhealthySamples)
	}
}

// TestCountPending proves countPending adds one to pendingRefreshes for a
// pending sample, resets it to one on the first pending sample after an
// apply, resets it to zero on any non-pending sample or on a fresh apply
// before its first sample, and leaves it with no sample and no apply.
func TestCountPending(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name             string
		reason           string
		sampled, applied bool
		start, want      int32
	}{
		{"a pending sample adds one", infrav1.InstancePendingReason, true, false, 2, 3},
		{"the first pending sample after an apply is one", infrav1.InstancePendingReason, true, true, 4, 1},
		{"a healthy sample resets", infrav1.HealthyReason, true, false, 4, 0},
		{"an unhealthy sample resets", infrav1.InstanceUnhealthyReason, true, false, 4, 0},
		{"an unknown sample resets", infrav1.HealthUnknownReason, true, false, 4, 0},
		{"a new apply resets before its first sample", infrav1.InstancePendingReason, false, true, 4, 0},
		{"no sample and no apply leaves it", infrav1.InstancePendingReason, false, false, 4, 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := machine()
			m.Status.PendingRefreshes = tt.start
			conditions.Set(m, metav1.Condition{Type: infrav1.InfrastructureHealthyCondition, Status: metav1.ConditionFalse, Reason: tt.reason})
			countPending(m, CommonStatus{WorkspaceStatus: &m.Status.WorkspaceStatus}, tt.sampled, tt.applied)
			if m.Status.PendingRefreshes != tt.want {
				t.Errorf("pendingRefreshes = %d, want %d", m.Status.PendingRefreshes, tt.want)
			}
		})
	}
}

// appliedMachineEnv returns, using t for setup, a machine env whose apply
// (with durable inputs) finished a minute before t0, with the state it
// wrote.
func appliedMachineEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	if err := inputs.Write(t.Context(), e.c, e.get(t), renderMachine(t), inputs.Meta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
		t.Fatal(err)
	}
	a := job("a", jobs.OpApply, jobs.Succeeded, t0.Add(-time.Minute))
	a.Annotations = map[string]string{state.InputsHashAnnotation: "h1:x"}
	e.runner.jobs = append(e.runner.jobs, a)
	e.state.st = &state.State{Serial: 1, InputsHash: "h1:x"}
	return e
}

// TestApplyReadingReplacesRefresh: a machine (RefreshAfterApply) whose
// apply's own outputs are valid with a definite health reading starts no
// post-apply refresh; the reading is its one sample (status.lastRefresh is
// the apply's finish, unhealthySamples moves once over two reconciles). A
// pending or unknown reading, or invalid outputs, keep the refresh.
func TestApplyReadingReplacesRefresh(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		health      *contract.Health
		invalid     bool
		refresh     bool
		wantSamples int32
	}{
		{"healthy", &contract.Health{State: contract.HealthRunning, Healthy: true}, false, false, 0},
		{"unhealthy counts once", &contract.Health{State: contract.HealthRunning}, false, false, 1},
		{"stopped counts once", &contract.Health{State: contract.HealthStopped}, false, false, 1},
		{"terminated", &contract.Health{State: contract.HealthTerminated}, false, false, 0},
		{"pending keeps the refresh", &contract.Health{State: contract.HealthPending}, false, true, 0},
		{"unknown keeps the refresh", &contract.Health{State: contract.HealthUnknown}, false, true, 0},
		{"no health output keeps the refresh", nil, false, true, 0},
		{"invalid outputs keep the refresh", &contract.Health{State: contract.HealthRunning, Healthy: true}, true, true, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := appliedMachineEnv(t)
			reconcile := func() *infrav1.TerraformMachine {
				t.Helper()
				k := e.kindFor(t, readyOwner)
				k.refresh, k.health = true, tt.health
				if tt.invalid {
					k.result = outputs.Result{Missing: []string{"addresses"}}
				}
				if _, err := reconcileOnce(t, e, k); err != nil {
					t.Fatal(err)
				}
				return e.get(t)
			}
			m := reconcile()
			if tt.refresh {
				if len(e.runner.created) != 1 || jobs.OpOf(e.jobNamed(t, e.runner.created[0])) != jobs.OpRefresh {
					t.Fatalf("created %v, want the post-apply refresh", e.runner.created)
				}
				if m.Status.LastRefresh != nil || m.Status.UnhealthySamples != 0 {
					t.Errorf("lastRefresh %v, unhealthySamples %d: the apply's reading was taken", m.Status.LastRefresh, m.Status.UnhealthySamples)
				}
				return
			}
			if len(e.runner.created) != 0 {
				t.Fatalf("created %v, want no refresh after a definite reading", e.runner.created)
			}
			if m.Status.LastRefresh == nil || !m.Status.LastRefresh.Time.Equal(t0.Add(-time.Minute)) {
				t.Errorf("lastRefresh = %v, want the apply's finish", m.Status.LastRefresh)
			}
			if m.Status.UnhealthySamples != tt.wantSamples {
				t.Errorf("unhealthySamples = %d, want %d", m.Status.UnhealthySamples, tt.wantSamples)
			}
			// The next reconcile neither refreshes nor counts the reading again.
			m = reconcile()
			if len(e.runner.created) != 0 || m.Status.UnhealthySamples != tt.wantSamples {
				t.Errorf("second reconcile: created %v, unhealthySamples %d, want %d", e.runner.created, m.Status.UnhealthySamples, tt.wantSamples)
			}
		})
	}
	// A kind that does not refresh after an apply (the cluster) keeps
	// lastRefresh unset: its drift and health schedule is unchanged.
	e := appliedMachineEnv(t)
	k := e.kindFor(t, readyOwner)
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	if m := e.get(t); m.Status.LastRefresh != nil || len(e.runner.created) != 0 {
		t.Errorf("lastRefresh %v, created %v", m.Status.LastRefresh, e.runner.created)
	}
}

// TestPendingRefreshBackoff: while health reads pending, each completed
// refresh adds one to status.pendingRefreshes and the next refresh waits
// PendingRefreshDelay (30s, 1m, 2m, 4m, 5m) plus the jitter; a non-pending
// reading resets the count.
func TestPendingRefreshBackoff(t *testing.T) {
	t.Parallel()
	e := appliedMachineEnv(t)
	pending := &contract.Health{State: contract.HealthPending}
	reconcile := func(h *contract.Health) (time.Duration, *infrav1.TerraformMachine) {
		t.Helper()
		k := e.kindFor(t, readyOwner)
		k.refresh, k.health = true, h
		requeue, err := reconcileOnce(t, e, k)
		if err != nil {
			t.Fatal(err)
		}
		return requeue, e.get(t)
	}
	// The apply read pending: its refresh starts, the count is 0.
	if _, m := reconcile(pending); len(e.runner.created) != 1 || m.Status.PendingRefreshes != 0 {
		t.Fatalf("after the apply: created %v, pendingRefreshes %d", e.runner.created, m.Status.PendingRefreshes)
	}
	now := t0
	for i, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		// The running refresh finishes, still pending.
		now = now.Add(10 * time.Second)
		name := e.runner.created[len(e.runner.created)-1]
		finish(t, e, name, now)
		e.d.Clock = testingclock.NewFakePassiveClock(now)
		requeue, m := reconcile(pending)
		if m.Status.PendingRefreshes != int32(i+1) {
			t.Fatalf("after refresh %d: pendingRefreshes %d", i+1, m.Status.PendingRefreshes)
		}
		wait := want + Jitter(string(m.UID), want)
		if requeue != wait {
			t.Fatalf("after refresh %d: requeue %s, want %s", i+1, requeue, wait)
		}
		// Due then: the next refresh starts. Status times are whole seconds,
		// as are a real Job's.
		now = now.Add((wait + time.Second).Truncate(time.Second))
		e.d.Clock = testingclock.NewFakePassiveClock(now)
		if _, _ = reconcile(pending); jobs.OpOf(e.jobNamed(t, e.runner.created[len(e.runner.created)-1])) != jobs.OpRefresh ||
			e.runner.created[len(e.runner.created)-1] == name {
			t.Fatalf("after refresh %d: no new refresh at the deadline (created %v)", i+1, e.runner.created)
		}
	}
	// The instance comes up: the count resets.
	now = now.Add(10 * time.Second)
	finish(t, e, e.runner.created[len(e.runner.created)-1], now)
	e.d.Clock = testingclock.NewFakePassiveClock(now)
	if _, m := reconcile(&contract.Health{State: contract.HealthRunning, Healthy: true}); m.Status.PendingRefreshes != 0 {
		t.Errorf("after a healthy reading: pendingRefreshes %d", m.Status.PendingRefreshes)
	}
}

// finish marks the fake runner's Job name succeeded at at, using t for
// lookup and e for the runner's Job list.
func finish(t *testing.T, e *env, name string, at time.Time) {
	t.Helper()
	j := e.jobNamed(t, name)
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(at)}}
}
