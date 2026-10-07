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
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// writeUnapplied writes m's durable inputs with no applied marker and no
// pinned digest, as a first apply that has not finished leaves them, and
// records an interrupted apply of job when job is not empty, failing t on
// error.
func (e *env) writeUnapplied(t *testing.T, m *infrav1.TerraformMachine, job string) {
	t.Helper()
	if err := writeInputs(t.Context(), e.c, m, renderMachine(t), testMeta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
		t.Fatal(err)
	}
	if job != "" {
		if err := inputs.SetInterruptedApply(t.Context(), e.c, m, job); err != nil {
			t.Fatal(err)
		}
	}
}

// TestInterruptedFirstApplyOnDelete: a deleting object whose first apply's
// Job vanished before any state was written is held (it may have created
// resources) with a message naming the Job; one whose apply never started
// drops its finalizer at once, in the reconcile and in the ownerless
// preamble alike.
func TestInterruptedFirstApplyOnDelete(t *testing.T) {
	t.Parallel()
	t.Run("reconcile: interrupted apply holds", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused))...)
		e.d.Jobs, e.d.State = &clientRunner{c: e.c}, state.NewReader(e.c)
		e.writeUnapplied(t, e.get(t), "apply-1")
		res, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName))
		if err != nil {
			t.Fatal(err)
		}
		assertHeld(t, e, res.RequeueAfter, infrav1.StateLostReason)
		msg := conditions.Get(e.get(t), infrav1.StateReadableCondition).Message
		if !strings.Contains(msg, "apply-1") || !strings.Contains(msg, "may have created resources") || strings.Contains(msg, "applied before") {
			t.Errorf("message = %q", msg)
		}
	})
	t.Run("reconcile: never started drops", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused))...)
		e.d.Jobs, e.d.State = &clientRunner{c: e.c}, state.NewReader(e.c)
		e.writeUnapplied(t, e.get(t), "")
		if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
			t.Fatal(err)
		}
		if m := e.get(t); m != nil {
			t.Errorf("object kept: %+v", m.Finalizers)
		}
	})
	t.Run("preamble: interrupted apply continues", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, machine(deleting, notPaused))
		k := e.kindFor(t, OwnerInfo{})
		e.writeUnapplied(t, k.obj, "apply-1")
		got, err := Preamble(t.Context(), e.d, k)
		if err != nil {
			t.Fatal(err)
		}
		if got.Stop {
			t.Errorf("Preamble stopped: %+v", got)
		}
		keepsFinalizer(t, e.get(t))
	})
	t.Run("preamble: never started drops", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, machine(deleting, notPaused))
		k := e.kindFor(t, OwnerInfo{})
		e.writeUnapplied(t, k.obj, "")
		got, err := Preamble(t.Context(), e.d, k)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Stop {
			t.Errorf("Preamble continued: %+v", got)
		}
		if m := e.get(t); m != nil {
			t.Errorf("object kept: %+v", m.Finalizers)
		}
	})
}

// TestCleanupMirrorConflict: a Conflict from the mirror's delete is the
// expected race: Cleanup returns an error that errMirrorConflict marks
// (and that is still a Conflict), keeps the finalizer, and emits no
// event; the mirror is still there for the next pass. The reconcile
// requeues after LagRequeue without an error.
func TestCleanupMirrorConflict(t *testing.T) {
	t.Parallel()
	mirror := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: identity.MirrorName(testIdentity)}}
	e := newEnvWith(t, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if obj.GetName() == mirror.Name {
			return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, obj.GetName(), errors.New("changed"))
		}
		return c.Delete(ctx, obj, opts...)
	}}, world(machine(deleting, withFinalizer, notPaused))...)
	if err := e.c.Create(t.Context(), mirror); err != nil {
		t.Fatal(err)
	}
	k := e.kindFor(t, readyOwner)
	suffix, err := state.Suffix(testNS, k.Kind(), testName)
	if err != nil {
		t.Fatal(err)
	}
	err = Cleanup(t.Context(), e.d, k, suffix, testIdentity)
	if !errors.Is(err, errMirrorConflict) || !apierrors.IsConflict(err) {
		t.Fatalf("Cleanup = %v, want a marked Conflict", err)
	}
	if !slices.Contains(k.obj.GetFinalizers(), testFinal) {
		t.Error("the finalizer was removed")
	}
	if n := len(e.rec.only(EventMirrorRemoved)); n != 0 {
		t.Errorf("EventMirrorRemoved emitted %d times", n)
	}
	if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(mirror), &corev1.Secret{}); err != nil {
		t.Errorf("mirror: %v", err)
	}

	// The reconcile that runs the cleanup takes the race as a quiet
	// requeue, not a reconcile error, and keeps the finalizer.
	res, err := Reconcile(t.Context(), e.d, e.kindFor(t, readyOwner))
	if err != nil || res.RequeueAfter != LagRequeue {
		t.Fatalf("Reconcile = %+v, %v; want a quiet requeue after %s", res, err, LagRequeue)
	}
	if m := e.get(t); m == nil || !slices.Contains(m.Finalizers, testFinal) {
		t.Errorf("the finalizer was removed: %+v", m)
	}
}
