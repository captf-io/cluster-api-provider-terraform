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
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// TestPreamble runs Preamble through the externally-managed, owner, gate,
// finalizer and pause stages of the preamble, table by table, and checks
// the returned PreambleResult and the stored object at each stage.
func TestPreamble(t *testing.T) {
	t.Parallel()
	owned := OwnerInfo{HasOwnerRef: true, Cluster: cluster(false)}
	tests := []struct {
		name  string
		obj   *infrav1.TerraformMachine
		owner OwnerInfo
		// setup prepares state and Jobs.
		setup func(*env)
		want  PreambleResult
		// check inspects the stored object (nil when deleted).
		check func(*testing.T, *infrav1.TerraformMachine)
	}{
		{
			name: "externally managed: stop before any write",
			obj: machine(func(m *infrav1.TerraformMachine) {
				m.Annotations = map[string]string{clusterv1.ManagedByAnnotation: "someone"}
			}),
			owner: owned,
			want:  PreambleResult{Stop: true},
			check: func(t *testing.T, m *infrav1.TerraformMachine) {
				if len(m.Finalizers) != 0 || len(m.Status.Conditions) != 0 {
					t.Errorf("externally managed object was written: %+v", m)
				}
			},
		},
		{
			name:  "no ownerRef, not deleting: WaitingForOwner and requeue, no finalizer",
			obj:   machine(),
			owner: OwnerInfo{},
			want:  PreambleResult{Stop: true, Result: ctrlResult(GateRequeue)},
			check: func(t *testing.T, m *infrav1.TerraformMachine) {
				c := conditions.Get(m, infrav1.DependenciesReadyCondition)
				if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != infrav1.WaitingForOwnerReason || len(m.Finalizers) != 0 {
					t.Errorf("DependenciesReady = %+v, finalizers %v", c, m.Finalizers)
				}
				if c := conditions.Get(m, infrav1.ReadyCondition); c == nil || c.Status != metav1.ConditionUnknown || c.Reason != infrav1.ReadyUnknownReason {
					t.Errorf("Ready = %+v, want Unknown/ReadyUnknown", c)
				}
			},
		},
		{
			name:  "only a control-plane ownerRef: WaitingForOwnerMachine",
			obj:   machine(),
			owner: OwnerInfo{Gate: &Gate{Status: metav1.ConditionFalse, Reason: infrav1.WaitingForOwnerMachineReason}},
			want:  PreambleResult{Stop: true, Result: ctrlResult(GateRequeue)},
			check: func(t *testing.T, m *infrav1.TerraformMachine) {
				if c := conditions.Get(m, infrav1.DependenciesReadyCondition); c == nil || c.Reason != infrav1.WaitingForOwnerMachineReason {
					t.Errorf("DependenciesReady = %+v", c)
				}
			},
		},
		{
			name:  "no ownerRef, deleting, no state, no Job: finalizer dropped",
			obj:   machine(deleting),
			owner: OwnerInfo{},
			want:  PreambleResult{Stop: true},
			check: func(t *testing.T, m *infrav1.TerraformMachine) {
				if m != nil {
					t.Errorf("object still exists with finalizers %v", m.Finalizers)
				}
			},
		},
		{
			name:  "no ownerRef, deleting, state exists: continue to destroy",
			obj:   machine(deleting, notPaused),
			owner: OwnerInfo{},
			setup: func(e *env) { e.state.st = &state.State{InputsHash: "h1:x"} },
			want:  PreambleResult{},
			check: keepsFinalizer,
		},
		{
			name:  "no ownerRef, deleting, state unreadable: counts as state",
			obj:   machine(deleting, notPaused),
			owner: OwnerInfo{},
			setup: func(e *env) { e.state.err = state.ErrStateInconsistent },
			want:  PreambleResult{},
			check: keepsFinalizer,
		},
		{
			name:  "no ownerRef, deleting, Job active: continue",
			obj:   machine(deleting, notPaused),
			owner: OwnerInfo{},
			setup: func(e *env) { e.runner.jobs = append(e.runner.jobs, job("j1", jobs.OpApply, jobs.Running, t0)) },
			want:  PreambleResult{},
			check: keepsFinalizer,
		},
		{
			name:  "owner gone: continue, deletion destroys from durable inputs",
			obj:   machine(withFinalizer, notPaused),
			owner: OwnerInfo{HasOwnerRef: true, OwnerGone: true},
			want:  PreambleResult{Owner: OwnerInfo{HasOwnerRef: true, OwnerGone: true}},
			check: keepsFinalizer,
		},
		{
			// A forged or stale ownerRef (OwnerMismatch) behaves like owner
			// gone for deletion: the not-deleting branch below stops early
			// on the same gate (see "OwnerMismatch gate, not deleting"), but
			// deleting must fall through so destroy still runs from the
			// durable inputs, exactly as it does for an owner that is gone.
			name: "OwnerMismatch gate, deleting: continue, deletion destroys from durable inputs",
			obj:  machine(deleting, withFinalizer, notPaused),
			owner: OwnerInfo{HasOwnerRef: true, Gate: &Gate{
				Status: metav1.ConditionFalse, Reason: infrav1.OwnerMismatchReason, Message: "Machine victim does not reference this TerraformMachine as its infrastructure",
			}},
			want:  PreambleResult{},
			check: keepsFinalizer,
		},
		{
			name:  "OwnerMismatch gate, not deleting: DependenciesReady=False, stop, no finalizer",
			obj:   machine(),
			owner: OwnerInfo{HasOwnerRef: true, Gate: &Gate{Status: metav1.ConditionFalse, Reason: infrav1.OwnerMismatchReason, Message: "forged"}},
			want:  PreambleResult{Stop: true},
			check: func(t *testing.T, m *infrav1.TerraformMachine) {
				if c := conditions.Get(m, infrav1.DependenciesReadyCondition); c == nil || c.Status != metav1.ConditionFalse ||
					c.Reason != infrav1.OwnerMismatchReason || len(m.Finalizers) != 0 {
					t.Errorf("DependenciesReady = %+v, finalizers %v", c, m.Finalizers)
				}
			},
		},
		{
			name:  "ClusterNotTerraform gate: DependenciesReady=False, stop, no finalizer",
			obj:   machine(),
			owner: OwnerInfo{HasOwnerRef: true, Gate: &Gate{Status: metav1.ConditionFalse, Reason: infrav1.ClusterNotTerraformReason, Message: "uses Docker"}},
			want:  PreambleResult{Stop: true},
			check: func(t *testing.T, m *infrav1.TerraformMachine) {
				if c := conditions.Get(m, infrav1.DependenciesReadyCondition); c == nil || c.Status != metav1.ConditionFalse || len(m.Finalizers) != 0 {
					t.Errorf("DependenciesReady = %+v, finalizers %v", c, m.Finalizers)
				}
				// The stop path leaves a Ready condition that names the gate.
				if c := conditions.Get(m, infrav1.ReadyCondition); c == nil || c.Status != metav1.ConditionFalse ||
					c.Reason != infrav1.NotReadyReason || c.Message != infrav1.ClusterNotTerraformReason+": uses Docker" {
					t.Errorf("Ready = %+v, want False/NotReady naming the gate", c)
				}
			},
		},
		{
			name:  "gate on a provisioned object: its Ready is left alone",
			obj:   machine(func(m *infrav1.TerraformMachine) { m.Status.Initialization.Provisioned = new(true) }),
			owner: OwnerInfo{HasOwnerRef: true, Gate: &Gate{Status: metav1.ConditionFalse, Reason: infrav1.ClusterNotTerraformReason}},
			want:  PreambleResult{Stop: true},
			check: func(t *testing.T, m *infrav1.TerraformMachine) {
				if conditions.Has(m, infrav1.ReadyCondition) {
					t.Errorf("Ready = %+v, want none written", conditions.Get(m, infrav1.ReadyCondition))
				}
			},
		},
		{
			name:  "finalizer just added: stop",
			obj:   machine(),
			owner: owned,
			want:  PreambleResult{Stop: true},
			check: keepsFinalizer,
		},
		{
			name:  "paused Cluster: Paused=True, paused branch",
			obj:   machine(withFinalizer),
			owner: OwnerInfo{HasOwnerRef: true, Cluster: cluster(true)},
			want:  PreambleResult{Paused: true},
			check: func(t *testing.T, m *infrav1.TerraformMachine) {
				if c := conditions.Get(m, clusterv1.PausedCondition); c == nil || c.Status != metav1.ConditionTrue {
					t.Errorf("Paused = %+v", c)
				}
			},
		},
		{
			name: "paused annotation on the object",
			obj: machine(withFinalizer, func(m *infrav1.TerraformMachine) {
				m.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
			}),
			owner: owned,
			want:  PreambleResult{Paused: true},
		},
		{
			name:  "not paused, first Paused condition written: stop for the requeue",
			obj:   machine(withFinalizer),
			owner: owned,
			want:  PreambleResult{Stop: true},
		},
		{
			name:  "not paused, condition current: continue",
			obj:   machine(withFinalizer, notPaused),
			owner: owned,
			want:  PreambleResult{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, tt.obj)
			if tt.setup != nil {
				tt.setup(e)
			}
			k := e.kindFor(t, tt.owner)
			got, err := Preamble(t.Context(), e.d, k)
			if err != nil {
				t.Fatalf("Preamble: %v", err)
			}
			got.Owner, tt.want.Owner = OwnerInfo{}, OwnerInfo{}
			if got.Stop != tt.want.Stop || got.Paused != tt.want.Paused || got.Result != tt.want.Result {
				t.Errorf("Preamble = %+v, want %+v", got, tt.want)
			}
			if tt.check != nil {
				tt.check(t, e.get(t))
			}
		})
	}
}

// TestPreambleOwnerlessDeletionContinues: a deleting object without an
// ownerRef, no state and no listed Job still continues to the delete
// reconcile, keeping its finalizer, when dependents block its deletion,
// when it applied before (its state is lost, not absent), when the Job
// cache lags behind status.activeJob, or when a live Job holds its run
// lease.
func TestPreambleOwnerlessDeletionContinues(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		mut   []func(*infrav1.TerraformMachine)
		setup func(*testing.T, *env, *fakeKind)
	}{
		{"dependents block the deletion", nil, func(_ *testing.T, _ *env, k *fakeKind) { k.blocked = true }},
		{"provisioned", []func(*infrav1.TerraformMachine){provisioned}, nil},
		{"digest pinned", nil, func(t *testing.T, e *env, k *fakeKind) { e.writeDurable(t, k.obj) }},
		{"the Job cache lags", []func(*infrav1.TerraformMachine){activeJob("j1")}, func(t *testing.T, e *env, _ *fakeKind) {
			e.d.Jobs = &clientRunner{c: e.c, stale: []batchv1.Job{}}
			j := job("j1", jobs.OpApply, jobs.Running, t0)
			if err := e.c.Create(t.Context(), &j); err != nil {
				t.Fatal(err)
			}
		}},
		{"a live Job holds the run lease", nil, func(t *testing.T, e *env, _ *fakeKind) {
			e.d.Jobs = &clientRunner{c: e.c, stale: []batchv1.Job{}}
			j := job("j1", jobs.OpApply, jobs.Running, t0)
			if err := e.c.Create(t.Context(), &j); err != nil {
				t.Fatal(err)
			}
			if err := e.c.Create(t.Context(), foreignLease(runLeaseOf(t, state.KindTerraformMachine, testName), "j1", jobs.OpApply, t0)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, machine(append([]func(*infrav1.TerraformMachine){deleting, notPaused}, tt.mut...)...))
			k := e.kindFor(t, OwnerInfo{})
			if tt.setup != nil {
				tt.setup(t, e, k)
			}
			got, err := Preamble(t.Context(), e.d, k)
			if err != nil {
				t.Fatalf("Preamble: %v", err)
			}
			if got.Stop {
				t.Errorf("Preamble stopped: %+v", got)
			}
			keepsFinalizer(t, e.get(t))
		})
	}
}

// TestOwnerlessBlockedDeletionPersisted proves the DeletionBlocked
// transition of an ownerless deleting object, which the preamble evaluates
// before the reconcile's patch base and event snapshot, is persisted and
// reported by the reconcile; the preamble leaves the object's conditions
// as it found them.
func TestOwnerlessBlockedDeletionPersisted(t *testing.T) {
	t.Parallel()
	notBlocked := func(m *infrav1.TerraformMachine) {
		conditions.Set(m, metav1.Condition{Type: infrav1.DeletionBlockedCondition, Status: metav1.ConditionFalse, Reason: infrav1.NotBlockedReason})
	}
	e := newEnv(t, world(machine(deleting, notPaused, notBlocked))...)
	k := e.kindFor(t, OwnerInfo{})
	k.blocked, k.blockedCondition = true, true
	before := slices.Clone(k.obj.Status.Conditions)
	if _, err := Preamble(t.Context(), e.d, k); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(k.obj.Status.Conditions, before) {
		t.Errorf("Preamble changed the conditions in memory: %+v", k.obj.Status.Conditions)
	}

	k = e.kindFor(t, OwnerInfo{})
	k.blocked, k.blockedCondition = true, true
	if _, err := Reconcile(t.Context(), e.d, k); err != nil {
		t.Fatal(err)
	}
	m := e.get(t)
	keepsFinalizer(t, m)
	if c := conditions.Get(m, infrav1.DeletionBlockedCondition); c == nil || c.Status != metav1.ConditionTrue || c.Reason != infrav1.DependentsExistReason {
		t.Errorf("DeletionBlocked = %+v, want True/%s persisted", c, infrav1.DependentsExistReason)
	}
	var reported bool
	for _, ev := range e.rec.events {
		reported = reported || strings.Contains(ev.note, infrav1.DeletionBlockedCondition)
	}
	if !reported {
		t.Errorf("no event reports DeletionBlocked: %v", e.rec.reasons)
	}
}

// TestPreambleOwnerError proves Preamble returns the error from a failing
// owner lookup instead of swallowing it.
func TestPreambleOwnerError(t *testing.T) {
	t.Parallel()
	e := newEnv(t, machine())
	k := e.kindFor(t, OwnerInfo{})
	k.ownerErr = errors.New("boom")
	if _, err := Preamble(t.Context(), e.d, k); err == nil {
		t.Error("owner lookup error swallowed")
	}
}

// keepsFinalizer fails t if m is nil or lacks the testFinal finalizer.
func keepsFinalizer(t *testing.T, m *infrav1.TerraformMachine) {
	t.Helper()
	if m == nil || !slices.Contains(m.Finalizers, testFinal) {
		t.Errorf("finalizer missing: %+v", m)
	}
}

// ctrlResult returns a ctrl.Result that requeues after d.
func ctrlResult(d time.Duration) ctrl.Result {
	return ctrl.Result{RequeueAfter: d}
}

// notPaused gives m a current Paused=False condition, so
// EnsurePausedCondition does not stop the reconcile for its first write.
func notPaused(m *infrav1.TerraformMachine) {
	m.Status.Conditions = append(m.Status.Conditions, metav1.Condition{
		Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason,
	})
}
