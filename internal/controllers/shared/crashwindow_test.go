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
	"errors"
	"sync/atomic"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
)

// TestReconcileCrashAfterJobCreate: a status write that fails right after
// the Job was created (the crash window between the create and
// status.activeJob) makes the next Reconcile adopt that Job: still one Job
// in all, status.activeJob names it, its per-run Secret is owned by it, the
// attempt counter does not advance and the attempt record is written once,
// naming it.
func TestReconcileCrashAfterJobCreate(t *testing.T) {
	t.Parallel()
	boom := errors.New("apiserver unavailable")
	// crashed fails every status patch of the machine, as the crash that
	// loses the pass's whole patch (the helper issues several) does.
	var crashed atomic.Bool
	crashed.Store(true)
	var attemptWrites atomic.Int32
	e := newEnvWith(t, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if s, ok := obj.(*corev1.Secret); ok && s.Name == inputs.Name("m", testName) {
			attemptWrites.Add(1)
		}
		return c.Update(ctx, obj, opts...)
	}, Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if s, ok := obj.(*corev1.Secret); ok && s.Name == inputs.Name("m", testName) {
			attemptWrites.Add(1)
		}
		return c.Create(ctx, obj, opts...)
	}, SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
		if _, ok := obj.(*infrav1.TerraformMachine); ok && crashed.Load() {
			return boom
		}
		return c.SubResource(sub).Patch(ctx, obj, p, opts...)
	}}, world(machine(withFinalizer, notPaused))...)
	e.d.Jobs = &clientRunner{c: e.c, now: e.apiNow}

	k := e.kindFor(t, readyOwner())
	k.in = machineIn()
	if _, err := reconcileOnce(t, e, k); !errors.Is(err, boom) {
		t.Fatalf("Reconcile #1 = %v, want the status patch error", err)
	}
	crashed.Store(false)
	created := e.jobsOf(t)
	if len(created) != 1 {
		t.Fatalf("%d Jobs after the crash, want 1", len(created))
	}
	if got := e.get(t).Status.ActiveJob.Name; got != "" {
		t.Fatalf("activeJob = %q after the failed status write, want none", got)
	}

	k = e.kindFor(t, readyOwner())
	k.in = machineIn()
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}
	all := e.jobsOf(t)
	if len(all) != 1 || all[0].Name != created[0].Name {
		t.Fatalf("Jobs = %v, want only %s", jobNames(all), created[0].Name)
	}
	m := e.get(t)
	if m.Status.ActiveJob.Name != all[0].Name || m.Status.ActiveJob.Attempt != 1 {
		t.Errorf("activeJob = %+v, want %s attempt 1", m.Status.ActiveJob, all[0].Name)
	}
	run := &corev1.Secret{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: inputs.RunName(all[0].Name)}, run); err != nil {
		t.Fatalf("per-run Secret: %v", err)
	}
	if len(run.OwnerReferences) != 1 || run.OwnerReferences[0].UID != all[0].UID {
		t.Errorf("run Secret owners = %+v, want Job UID %s", run.OwnerReferences, all[0].UID)
	}
	d, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil || d.Attempt == nil || d.Attempt.Job != all[0].Name {
		t.Errorf("attempt record = %+v, %v; want it to name %s", d, err, all[0].Name)
	}
	if n := attemptWrites.Load(); n != 1 {
		t.Errorf("%d writes of the attempt record, want 1", n)
	}
	if n := int(e.rec.count(EventJobCreated)); n != 1 {
		t.Errorf("%d JobCreated events, want 1", n)
	}
}

// jobNames returns the names of js.
func jobNames(js []batchv1.Job) []string {
	out := make([]string, len(js))
	for i := range js {
		out[i] = js[i].Name
	}
	return out
}
