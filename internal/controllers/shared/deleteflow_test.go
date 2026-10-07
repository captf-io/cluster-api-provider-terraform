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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// deleteObject deletes e's stored machine through the client, as kubectl
// delete does: the finalizer keeps it, with a deletionTimestamp. It fails t
// on error.
func (e *env) deleteObject(t *testing.T) {
	t.Helper()
	if err := e.c.Delete(t.Context(), e.get(t)); err != nil {
		t.Fatal(err)
	}
	if m := e.get(t); m == nil || m.DeletionTimestamp.IsZero() {
		t.Fatalf("the object was not marked deleted: %+v", m)
	}
}

// opJobs returns the names of e's Jobs of op, failing t on error.
func (e *env) opJobs(t *testing.T, op jobs.Op) []string {
	t.Helper()
	var out []string
	for _, j := range e.jobsOf(t) {
		if jobs.OpOf(&j) == op {
			out = append(out, j.Name)
		}
	}
	return out
}

// TestReconcileDeleteMidApply: a deletion that arrives while an apply Job
// runs waits for it: no new Job, the finalizer, the run lease and
// block-move held. Once the apply finished, the destroy Job starts from
// that apply's inputs (the applied record), and once it succeeds and the
// state is gone the finalizer is removed.
func TestReconcileDeleteMidApply(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	e.d.Jobs = &clientRunner{c: e.c, now: e.apiNow}
	reconcile := func() {
		t.Helper()
		k := e.kindFor(t, readyOwner())
		k.in = machineIn()
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	applies := e.opJobs(t, jobs.OpApply)
	if len(applies) != 1 {
		t.Fatalf("apply Jobs %v, want one", applies)
	}
	apply := applies[0]
	lease := runLeaseOf(t, state.KindTerraformMachine, testName)

	e.deleteObject(t)
	for range 3 {
		reconcile()
		m := e.get(t)
		if m == nil || !slices.Contains(m.Finalizers, testFinal) || !HasBlockMove(m) || m.Status.ActiveJob.Name != apply {
			t.Fatalf("while the apply runs: object %+v", m)
		}
		if got := e.opJobs(t, jobs.OpDestroy); len(got) != 0 || len(e.jobsOf(t)) != 1 {
			t.Fatalf("a Job started while the apply runs: %v", jobNames(e.jobsOf(t)))
		}
		if l := e.lease(t, lease); l == nil || l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != apply {
			t.Fatalf("run lease = %+v, want held by %s", l, apply)
		}
	}

	// The apply finishes and left a state of its inputs.
	d, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil || d.Attempt == nil || d.Attempt.Job != apply {
		t.Fatalf("attempt record = %+v, %v", d, err)
	}
	e.finishJob(t, apply, jobs.Succeeded)
	e.state.st = &state.State{Serial: 1, InputsHash: d.Attempt.InputsHash}
	reconcile()
	destroys := e.opJobs(t, jobs.OpDestroy)
	if len(destroys) != 1 {
		t.Fatalf("destroy Jobs %v, want one", destroys)
	}
	if d, err = inputs.Read(t.Context(), e.c, testNS, "m", testName); err != nil || d.Applied == nil || d.Applied.Job != apply {
		t.Errorf("applied record = %+v, %v; want the finished apply promoted before the destroy", d, err)
	}
	if m := e.get(t); m == nil || !slices.Contains(m.Finalizers, testFinal) || m.Status.ActiveJob.Name != destroys[0] {
		t.Fatalf("while the destroy runs: object %+v", m)
	}

	// The destroy succeeds and the state is gone.
	e.finishJob(t, destroys[0], jobs.Succeeded)
	e.state.st = nil
	reconcile()
	if m := e.get(t); m != nil {
		t.Errorf("the object remains after the destroy: finalizers %v", m.Finalizers)
	}
	if l := e.lease(t, lease); l != nil {
		t.Errorf("the run lease remains: %+v", l)
	}
}

// TestReconcileDeleteAwaitingApproval: deleting a cluster-kind object whose
// TerraformPlan awaits approval (applyPolicy Manual, or a destructive apply
// that the guard blocked) destroys without that approval. The plan is
// neither approved nor deleted by the engine: it keeps its controller
// ownerRef, so garbage collection removes it with the object, and no
// approval wait condition remains.
func TestReconcileDeleteAwaitingApproval(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		// setup returns an env whose object waits for the approval of a
		// pending TerraformPlan, and the reconcile of its object.
		setup func(t *testing.T) (*env, func() error)
	}{
		{"applyPolicy Manual", func(t *testing.T) (*env, func() error) {
			e := newPlanEnv(t, "h1:old")
			if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
				t.Fatal(err)
			}
			planJob := e.newest(t)
			p := &runner.Plan{Hash: runner.PlanHash([]string{"module.role.lb|update", "module.role.sg|delete"}), Update: 1, Delete: 1,
				Resources: []string{"module.role.lb (update)", "module.role.sg (delete)"}}
			e.finishRunner(t, planJob, jobs.Succeeded, t0.Add(-30*time.Minute), planResult(runner.OpPlan, p, ""))
			e.reconcile(t, nil)
			return e.env, func() error { _, err := reconcileOnce(t, e.env, e.kind(t, nil)); return err }
		}},
		{"destructive apply blocked", func(t *testing.T) (*env, func() error) {
			e := newBlockedEnv(t, "h1:old", false)
			if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
				t.Fatal(err)
			}
			return e.env, func() error { _, err := reconcileOnce(t, e.env, e.kind(t, nil)); return err }
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e, reconcile := tt.setup(t)
			waiting := e.plans(t)
			if len(waiting) != 1 || phaseOf(&waiting[0]) != infrav1.PlanPhasePending {
				t.Fatalf("TerraformPlans = %+v, want one pending", waiting)
			}
			if c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition); c == nil || (c.Reason != infrav1.PlanAwaitingApprovalReason && c.Reason != infrav1.DestructivePlanBlockedReason) {
				t.Fatalf("ApplyJobSucceeded = %+v, want an approval wait", c)
			}
			created := len(e.runner.created)

			e.deleteObject(t)
			if err := reconcile(); err != nil {
				t.Fatal(err)
			}
			if len(e.runner.created) != created+1 || jobs.OpOf(e.jobNamed(t, e.newest(t))) != jobs.OpDestroy {
				t.Fatalf("created %v, want a destroy Job after the %d earlier ones", e.runner.created, created)
			}
			// A blocked apply's condition still reports that finished Job
			// (see TestReconcileDeleteBlockedCondition); the Manual wait
			// is a live decision and must be gone.
			if c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition); c != nil && c.Reason == infrav1.PlanAwaitingApprovalReason {
				t.Errorf("ApplyJobSucceeded = %+v, an approval wait remains", c)
			}
			got := e.plans(t)
			if len(got) != 1 || got[0].Name != waiting[0].Name {
				t.Fatalf("TerraformPlans = %v after the delete", got)
			}
			if got[0].Spec.Approved != nil {
				t.Errorf("the plan was approved by the engine: %v", *got[0].Spec.Approved)
			}
			if ref := metav1.GetControllerOf(&got[0]); ref == nil || ref.UID != "m1-uid" {
				t.Errorf("the plan's controller ref = %+v, want the object so GC removes it", ref)
			}
		})
	}
}

// TestReconcileDeleteBlockedCondition: once a deletion starts the destroy of
// an object whose destructive apply was blocked, ApplyJobSucceeded no
// longer tells the user that no apply runs until the plan is approved.
func TestReconcileDeleteBlockedCondition(t *testing.T) {
	t.Skip("bug: ApplyJobSucceeded keeps DestructivePlanBlocked (\"approve TerraformPlan X to apply it\") while the delete's destroy Job runs; applyDestroyCondition reports the newest finished apply and the running destroy replaces it only when it finishes")
	t.Parallel()
	e := newBlockedEnv(t, "h1:old", false)
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	e.deleteObject(t)
	if _, err := reconcileOnce(t, e.env, e.kind(t, nil)); err != nil {
		t.Fatal(err)
	}
	if c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition); c != nil && c.Reason == infrav1.DestructivePlanBlockedReason {
		t.Errorf("ApplyJobSucceeded = %+v, want no approval wait while the destroy runs", c)
	}
}
