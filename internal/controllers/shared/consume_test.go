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
	"context"
	"sync/atomic"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// approving returns a machine-mutator that approves the destructive plan
// of inputs hash h.
func approving(h string) func(*infrav1.TerraformMachine) {
	return func(m *infrav1.TerraformMachine) {
		m.Annotations = map[string]string{infrav1.ApproveDestructivePlanAnnotation: h}
	}
}

// userApproves writes the approval h onto the stored machine through c
// using ctx, as a user would, failing t on error.
func userApproves(ctx context.Context, t *testing.T, c client.Client, h string) {
	t.Helper()
	m := &infrav1.TerraformMachine{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: testName}, m); err != nil {
		t.Fatal(err)
	}
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	m.Annotations[infrav1.ApproveDestructivePlanAnnotation] = h
	if err := c.Update(ctx, m); err != nil {
		t.Fatal(err)
	}
}

// TestConsumedAnnotationKeepsNewValue: removing a consumed approval never
// deletes a value the user wrote since. Written before the removal, it
// conflicts: the reconcile requeues shortly, the finished Job stays
// unmarked, and the new value stays. Written after the removal, the
// deferred status patch does not remove it again.
func TestConsumedAnnotationKeepsNewValue(t *testing.T) {
	t.Parallel()
	for _, before := range []bool{true, false} {
		var patched atomic.Bool
		funcs := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*infrav1.TerraformMachine); !ok || patched.Swap(true) {
				return c.Patch(ctx, obj, p, opts...)
			}
			if before {
				userApproves(ctx, t, c, "h1:new")
				return c.Patch(ctx, obj, p, opts...)
			}
			if err := c.Patch(ctx, obj, p, opts...); err != nil {
				return err
			}
			userApproves(ctx, t, c, "h1:new")
			return nil
		}}
		applied := job("a", jobs.OpApply, jobs.Succeeded, t0)
		applied.Annotations = map[string]string{state.InputsHashAnnotation: "h1:x"}
		e := newEnvWith(t, funcs, world(machine(withFinalizer, notPaused, approving("h1:x")), applied.DeepCopy())...)
		e.runner.jobs = append(e.runner.jobs, applied)
		e.state.st = &state.State{Serial: 1, InputsHash: "h1:x"}
		k := e.kindFor(t, readyOwner)
		k.in = machineIn()
		res, err := Reconcile(t.Context(), e.d, k)
		if err != nil {
			t.Fatalf("before %v: %v", before, err)
		}
		if got := e.get(t).Annotations[infrav1.ApproveDestructivePlanAnnotation]; got != "h1:new" {
			t.Errorf("before %v: approval = %q, want the user's h1:new", before, got)
		}
		stored := &batchv1.Job{}
		if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "a"}, stored); err != nil {
			t.Fatal(err)
		}
		marked := stored.Annotations[BookkeptAnnotation] == "true"
		consumed := e.rec.count(EventDestructivePlanApprovalConsumed)
		if before && (res.RequeueAfter != LagRequeue || marked || consumed != 0) {
			t.Errorf("conflict: requeue %s, Job marked %v, consumed events %d", res.RequeueAfter, marked, consumed)
		}
		if !before && (!marked || consumed != 1) {
			t.Errorf("removed: Job marked %v, consumed events %d", marked, consumed)
		}
	}
}
