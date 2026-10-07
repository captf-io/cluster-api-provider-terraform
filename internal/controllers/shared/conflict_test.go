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
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
)

// conflictOnce is an interceptor that fails the first write of a kind it
// is armed for with a Conflict, then passes every call through.
type conflictOnce struct {
	// armed is true until the conflict was returned.
	armed atomic.Bool
	// hits counts the writes the conflict was returned for.
	hits atomic.Int32
	// match reports whether a write of obj is the one to fail.
	match func(obj client.Object) bool
}

// fail returns the Conflict for obj when c is armed and matches it, and
// disarms c; otherwise nil.
func (c *conflictOnce) fail(obj client.Object) error {
	if c.match(obj) && c.armed.CompareAndSwap(true, false) {
		c.hits.Add(1)
		return apierrors.NewConflict(schema.GroupResource{Group: infrav1.GroupVersion.Group, Resource: "terraformmachines"}, obj.GetName(), nil)
	}
	return nil
}

// funcs returns the interceptor funcs of c: every write of the client is
// subject to it.
func (c *conflictOnce) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			if err := c.fail(obj); err != nil {
				return err
			}
			return cl.Patch(ctx, obj, p, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := c.fail(obj); err != nil {
				return err
			}
			return cl.Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
			if err := c.fail(obj); err != nil {
				return err
			}
			return cl.SubResource(sub).Patch(ctx, obj, p, opts...)
		},
	}
}

// onKind returns a match function for writes of objects of type T.
func onKind[T client.Object]() func(client.Object) bool {
	return func(obj client.Object) bool { _, ok := obj.(T); return ok }
}

// assertNoFailure fails t when e's object recorded a failed run (lastRun
// error) or a JobFailed event.
func assertNoFailure(t *testing.T, e *env) {
	t.Helper()
	m := e.get(t)
	if lr := m.Status.LastRun; lr.Error.Kind != "" || lr.Error.Summary != "" {
		t.Errorf("lastRun.error = %+v after a conflict", lr.Error)
	}
	if n := e.rec.count(EventJobFailed); n != 0 {
		t.Errorf("%d JobFailed events after a conflict", n)
	}
}

// TestReconcileConflictOnFinalizerAdd: a Conflict adding the finalizer is
// returned for a retry, records no failure and starts no Job; the retries
// then add it and start exactly one apply, attempt 1.
func TestReconcileConflictOnFinalizerAdd(t *testing.T) {
	t.Parallel()
	c := &conflictOnce{match: onKind[*infrav1.TerraformMachine]()}
	c.armed.Store(true)
	e := newEnvWith(t, c.funcs(), world(machine(notPaused))...)
	e.d.Jobs = &clientRunner{c: e.c, now: e.apiNow}
	reconcile := func() error {
		k := e.kindFor(t, readyOwner())
		k.in = machineIn()
		_, err := reconcileOnce(t, e, k)
		return err
	}
	if err := reconcile(); !apierrors.IsConflict(err) {
		t.Fatalf("Reconcile = %v, want the Conflict", err)
	}
	if m := e.get(t); slices.Contains(m.Finalizers, testFinal) || len(e.jobsOf(t)) != 0 {
		t.Fatalf("after the conflict: finalizers %v, Jobs %v", m.Finalizers, jobNames(e.jobsOf(t)))
	}
	assertNoFailure(t, e)
	for range 3 {
		if err := reconcile(); err != nil {
			t.Fatal(err)
		}
	}
	all := e.jobsOf(t)
	if len(all) != 1 || jobs.OpOf(&all[0]) != jobs.OpApply {
		t.Fatalf("Jobs = %v, want one apply", jobNames(all))
	}
	if m := e.get(t); !slices.Contains(m.Finalizers, testFinal) || m.Status.ActiveJob.Attempt != 1 {
		t.Errorf("finalizers %v, activeJob %+v", m.Finalizers, m.Status.ActiveJob)
	}
	assertNoFailure(t, e)
}

// TestReconcileConflictOnPlanWrites: a Conflict on a TerraformPlan write
// (its phase label or status, when the plan is created and when it is
// approved) is returned for a retry, records no failure, and starts no
// duplicate Job; the retry carries the flow on with the same attempt a
// flow without the Conflict reaches.
func TestReconcileConflictOnPlanWrites(t *testing.T) {
	t.Parallel()
	// flow runs a Manual plan, its approval and the apply it starts; stage
	// is where the conflict is armed ("create" or "approve"; "" never).
	// It returns the created Jobs and the apply's attempt.
	flow := func(t *testing.T, stage string) (created []string, attempt int32) {
		t.Helper()
		c := &conflictOnce{match: onKind[*infrav1.TerraformPlan]()}
		e := newPlanEnvWith(t, c.funcs(), "h1:old")
		step := func(armed bool) {
			t.Helper()
			c.armed.Store(armed)
			_, err := reconcileOnce(t, e.env, e.kind(t, nil))
			switch {
			case armed && !apierrors.IsConflict(err):
				t.Fatalf("Reconcile = %v, want the Conflict", err)
			case !armed && err != nil:
				t.Fatal(err)
			}
			if armed {
				if c.hits.Load() == 0 {
					t.Fatal("the plan write did not conflict")
				}
				before := len(e.runner.created)
				assertNoFailure(t, e.env)
				// The error path starts nothing more.
				if len(e.runner.created) != before {
					t.Fatalf("created %v after the conflict", e.runner.created)
				}
			}
		}
		step(false)
		planJob := e.newest(t)
		p := &runner.Plan{Hash: runner.PlanHash([]string{"module.role.lb|update"}), Update: 1, Resources: []string{"module.role.lb (update)"}}
		e.finishRunner(t, planJob, jobs.Succeeded, t0.Add(-30*time.Minute), planResult(runner.OpPlan, p, ""))
		step(stage == "create")
		for range 2 {
			step(false)
		}
		plans := e.plans(t)
		if len(plans) != 1 {
			t.Fatalf("%d TerraformPlans, want 1", len(plans))
		}
		if len(e.runner.created) != 1 {
			t.Fatalf("created %v while the plan waits", e.runner.created)
		}
		e.approve(t, plans[0].Name, "alice")
		step(stage == "approve")
		for range 2 {
			step(false)
		}
		if len(e.runner.created) != 2 {
			t.Fatalf("created %v, want the plan Job and one apply", e.runner.created)
		}
		return slices.Clone(e.runner.created), e.get(t).Status.ActiveJob.Attempt
	}
	_, want := flow(t, "")
	for _, stage := range []string{"create", "approve"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			created, attempt := flow(t, stage)
			if attempt != want || len(created) != 2 {
				t.Errorf("created %v, apply attempt %d; want 2 Jobs and attempt %d", created, attempt, want)
			}
		})
	}
}
