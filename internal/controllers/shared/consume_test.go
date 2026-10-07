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
	"sync/atomic"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// userRequests writes the restore request n onto the stored machine
// through c using ctx, as a user would, failing t on error.
func userRequests(ctx context.Context, t *testing.T, c client.Client, n string) {
	t.Helper()
	m := &infrav1.TerraformMachine{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: testName}, m); err != nil {
		t.Fatal(err)
	}
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	m.Annotations[infrav1.RestoreStateAnnotation] = n
	if err := c.Update(ctx, m); err != nil {
		t.Fatal(err)
	}
}

// TestConsumedAnnotationKeepsNewValue: removing a consumed restore request
// never deletes a value the user wrote since. Written before the removal,
// it conflicts: the reconcile requeues shortly, the finished Job stays
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
				userRequests(ctx, t, c, "7")
				return c.Patch(ctx, obj, p, opts...)
			}
			if err := c.Patch(ctx, obj, p, opts...); err != nil {
				return err
			}
			userRequests(ctx, t, c, "7")
			return nil
		}}
		restored := job("r", jobs.OpRestore, jobs.Succeeded, t0)
		restored.Annotations = map[string]string{jobs.RestoreSerialAnnotation: "5"}
		e := newEnvWith(t, funcs, world(machine(withFinalizer, notPaused, restoring("5")), restored.DeepCopy())...)
		e.runner.jobs = append(e.runner.jobs, restored)
		e.state.st = &state.State{Serial: 5, InputsHash: "h1:x"}
		k := e.kindFor(t, readyOwner())
		k.in = machineIn()
		res, err := Reconcile(t.Context(), e.d, k)
		if err != nil {
			t.Fatalf("before %v: %v", before, err)
		}
		if got := e.get(t).Annotations[infrav1.RestoreStateAnnotation]; got != "7" {
			t.Errorf("before %v: restore request = %q, want the user's 7", before, got)
		}
		stored := &batchv1.Job{}
		if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "r"}, stored); err != nil {
			t.Fatal(err)
		}
		marked := stored.Annotations[BookkeptAnnotation] == "true"
		if before && (res.RequeueAfter != LagRequeue || marked) {
			t.Errorf("conflict: requeue %s, Job marked %v", res.RequeueAfter, marked)
		}
		if !before && !marked {
			t.Errorf("removed: Job marked %v", marked)
		}
	}
}
