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
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// recordsEnv is a mutable machine, with deletionPolicy Destroy, whose
// first apply succeeded and was promoted: the applied record holds
// applied, the files of Job first, and the state records their hash.
type recordsEnv struct {
	*env
	// in is what the adapter builds next.
	in      contract.MachineInputs
	applied render.Files
	first   string
}

// recordsMachine sets m's deletionPolicy to Destroy and turns its drift
// checks off, so no drift Job starts between the applies.
func recordsMachine(m *infrav1.TerraformMachine) {
	m.Spec.DeletionPolicy = infrav1.DeletionPolicyDestroy
	m.Spec.Drift = &infrav1.MachineDriftPolicy{IntervalSeconds: new(int32(0))}
}

// newRecordsEnv returns a recordsEnv, built through e's client with the
// interceptors funcs, failing t on error.
func newRecordsEnv(t *testing.T, funcs interceptor.Funcs) *recordsEnv {
	t.Helper()
	r := &recordsEnv{env: newEnvWith(t, funcs, world(machine(withFinalizer, notPaused, recordsMachine))...), in: machineIn()}
	r.first = r.startsApply(t)
	files, err := render.Root(contract.RoleMachine, r.in)
	if err != nil {
		t.Fatal(err)
	}
	r.applied = files
	r.finishRunner(t, r.first, jobs.Succeeded, t0.Add(-time.Hour), "")
	r.state.st = &state.State{Serial: 1, InputsHash: r.jobNamed(t, r.first).Annotations[state.InputsHashAnnotation]}
	if _, err := reconcileOnce(t, r.env, r.kind(t)); err != nil {
		t.Fatal(err)
	}
	if d := r.records(t); d.Applied == nil || d.Applied.Job != r.first {
		t.Fatalf("applied record = %+v, want the first apply %s promoted", d.Applied, r.first)
	}
	return r
}

// kind returns the adapter of e's stored object: mutable, building r.in.
func (r *recordsEnv) kind(t *testing.T) *fakeKind {
	t.Helper()
	k := r.kindFor(t, readyOwner)
	k.mutable, k.in = true, r.in
	return k
}

// startsApply reconciles once, failing t unless exactly one apply Job
// started, and returns its name.
func (r *recordsEnv) startsApply(t *testing.T) string {
	t.Helper()
	before := len(r.runner.created)
	if _, err := reconcileOnce(t, r.env, r.kind(t)); err != nil {
		t.Fatal(err)
	}
	if got := r.runner.created[before:]; len(got) != 1 || !strings.Contains(got[0], "-apply-") {
		t.Fatalf("started %v, want one apply", got)
	}
	return r.newest(t)
}

// editRegion changes what the adapter builds, as a changed spec would.
func (r *recordsEnv) editRegion() {
	r.in.BootstrapData = base64.StdEncoding.EncodeToString([]byte("#cloud-config\nregion: eu-west-1\n"))
}

// records returns the object's inputs records, failing t on error.
func (r *recordsEnv) records(t *testing.T) *inputs.Durable {
	t.Helper()
	d, err := inputs.Read(t.Context(), r.c, testNS, "m", testName)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// destroys deletes the stored object and reconciles once, failing t
// unless a destroy Job started; it returns the files its per-run Secret
// holds.
func (r *recordsEnv) destroys(t *testing.T) render.Files {
	t.Helper()
	if err := r.c.Delete(t.Context(), r.get(t)); err != nil {
		t.Fatal(err)
	}
	before := len(r.runner.created)
	if _, err := reconcileOnce(t, r.env, r.kind(t)); err != nil {
		t.Fatal(err)
	}
	got := r.runner.created[before:]
	if len(got) != 1 || jobs.OpOf(r.jobNamed(t, got[0])) != jobs.OpDestroy {
		t.Fatalf("started %v, want one destroy", got)
	}
	run := &corev1.Secret{}
	if err := r.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: inputs.RunName(got[0])}, run); err != nil {
		t.Fatal(err)
	}
	return render.Files{MainTF: run.Data[inputs.MainTFKey], TFVars: run.Data[inputs.TFVarsKey]}
}

// TestDestroyUsesAppliedAfterBlockedApply: an apply of changed inputs
// that the runner blocked before a destructive plan changed nothing, yet
// it is the attempt record now. Deleting the object then destroys from
// the applied record, the inputs the state was written with, not the
// blocked change's: a destroy rendered from those would look for the
// resources in the wrong place, find none, and orphan them.
func TestDestroyUsesAppliedAfterBlockedApply(t *testing.T) {
	t.Parallel()
	r := newRecordsEnv(t, interceptor.Funcs{})
	r.editRegion()
	blocked := r.startsApply(t)
	if d := r.records(t); d.Attempt.Job != blocked || d.Applied.Job != r.first {
		t.Fatalf("records = attempt %s, applied %s; want %s and %s", d.Attempt.Job, d.Applied.Job, blocked, r.first)
	}
	r.finishRunner(t, blocked, jobs.Failed, t0.Add(-time.Minute), blockedResult())
	if got := r.destroys(t); !bytes.Equal(got.TFVars, r.applied.TFVars) || !bytes.Equal(got.MainTF, r.applied.MainTF) {
		t.Errorf("destroy rendered %s, want the applied inputs %s", got.TFVars, r.applied.TFVars)
	}
}

// TestPromoteCrashBetweenSteps: a pass that promoted a successful apply
// but failed to delete its per-run Secret leaves both for the next pass,
// which finds the record its own and promotes nothing again: the applied
// record is written once, naming the Job.
func TestPromoteCrashBetweenSteps(t *testing.T) {
	t.Parallel()
	appliedName := inputs.AppliedName("m", testName)
	var failRunDelete atomic.Bool
	var writes atomic.Int32
	funcs := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() == appliedName {
				writes.Add(1)
			}
			return c.Create(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if obj.GetName() == appliedName {
				writes.Add(1)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if strings.HasPrefix(obj.GetName(), "captf-run-") && failRunDelete.CompareAndSwap(true, false) {
				return apierrors.NewServiceUnavailable("injected")
			}
			return c.Delete(ctx, obj, opts...)
		},
	}
	r := &recordsEnv{env: newEnvWith(t, funcs, world(machine(withFinalizer, notPaused, recordsMachine))...), in: machineIn()}
	first := r.startsApply(t)
	r.finishRunner(t, first, jobs.Succeeded, t0.Add(-time.Hour), "")
	r.state.st = &state.State{Serial: 1, InputsHash: r.jobNamed(t, first).Annotations[state.InputsHashAnnotation]}

	failRunDelete.Store(true)
	if _, err := reconcileOnce(t, r.env, r.kind(t)); err == nil {
		t.Fatal("the per-run Secret delete failed, but the pass succeeded")
	}
	if d := r.records(t); d.Applied == nil || d.Applied.Job != first {
		t.Fatalf("applied record after the crash = %+v, want %s", d.Applied, first)
	}
	if err := r.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: inputs.RunName(first)}, &corev1.Secret{}); err != nil {
		t.Fatalf("per-run Secret after the crash: %v, want it kept", err)
	}
	if _, err := reconcileOnce(t, r.env, r.kind(t)); err != nil {
		t.Fatal(err)
	}
	if n := writes.Load(); n != 1 {
		t.Errorf("applied record written %d times, want once", n)
	}
	if d := r.records(t); d.Applied == nil || d.Applied.Job != first {
		t.Errorf("applied record = %+v, want %s", d.Applied, first)
	}
	if err := r.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: inputs.RunName(first)}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("per-run Secret: %v, want it deleted on the second pass", err)
	}
}

// TestWriteAttemptAfterCreate: the attempt record is written only once
// the apply Job exists. A create the API server refused, and a start
// deferred before the create (the Cluster paused meanwhile), leave the
// record of the apply that last ran.
func TestWriteAttemptAfterCreate(t *testing.T) {
	t.Parallel()
	t.Run("create refused", func(t *testing.T) {
		t.Parallel()
		r := newRecordsEnv(t, interceptor.Funcs{})
		r.editRegion()
		r.runner.createErr = apierrors.NewInvalid(schema.GroupKind{Group: "batch", Kind: "Job"}, "x", field.ErrorList{field.Invalid(field.NewPath("metadata"), "x", "refused")})
		if _, err := reconcileOnce(t, r.env, r.kind(t)); err == nil {
			t.Fatal("the create was refused, but the pass succeeded")
		}
		if d := r.records(t); d.Attempt.Job != r.first || !bytes.Equal(d.Attempt.Files.TFVars, r.applied.TFVars) {
			t.Errorf("attempt record = %s, want the first apply's %s", d.Attempt.Job, r.first)
		}
	})
	t.Run("start deferred", func(t *testing.T) {
		t.Parallel()
		r := newRecordsEnv(t, interceptor.Funcs{})
		r.editRegion()
		if err := r.c.Create(t.Context(), cluster(true)); err != nil {
			t.Fatal(err)
		}
		before := len(r.runner.created)
		if requeue, err := reconcileOnce(t, r.env, r.kind(t)); err != nil || requeue != LagRequeue {
			t.Fatalf("Reconcile = %s, %v; want a deferred start", requeue, err)
		}
		if len(r.runner.created) != before {
			t.Fatalf("created %v while the Cluster is paused", r.runner.created[before:])
		}
		if d := r.records(t); d.Attempt.Job != r.first || !bytes.Equal(d.Attempt.Files.TFVars, r.applied.TFVars) {
			t.Errorf("attempt record = %s, want the first apply's %s", d.Attempt.Job, r.first)
		}
	})
	t.Run("created", func(t *testing.T) {
		t.Parallel()
		r := newRecordsEnv(t, interceptor.Funcs{})
		r.editRegion()
		next := r.startsApply(t)
		if d := r.records(t); d.Attempt.Job != next || bytes.Equal(d.Attempt.Files.TFVars, r.applied.TFVars) {
			t.Errorf("attempt record = %s, want the started apply %s", d.Attempt.Job, next)
		}
	})
}
