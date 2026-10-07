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
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
)

// TestPreambleExternallyManagedDeleting proves a deleting object carrying
// the managed-by annotation loses only our finalizer, with a Normal event,
// and no Job or state is touched; a live one is still skipped untouched.
func TestPreambleExternallyManagedDeleting(t *testing.T) {
	t.Parallel()
	managed := func(m *infrav1.TerraformMachine) {
		m.Annotations = map[string]string{clusterv1.ManagedByAnnotation: "someone"}
	}
	e := newEnv(t, machine(managed, deleting, func(m *infrav1.TerraformMachine) {
		m.Finalizers = []string{testFinal, "other.example/keep"}
	}))
	got, err := Preamble(t.Context(), e.d, e.kindFor(t, OwnerInfo{}))
	if err != nil || !got.Stop {
		t.Fatalf("Preamble = %+v, %v; want Stop", got, err)
	}
	if m := e.get(t); m == nil || !slices.Equal(m.Finalizers, []string{"other.example/keep"}) {
		t.Errorf("finalizers = %+v, want only the other controller's", m)
	}
	if !slices.Contains(e.rec.reasons, EventExternallyManagedReleased) {
		t.Errorf("events = %v, want %s", e.rec.reasons, EventExternallyManagedReleased)
	}
	if n := len(e.jobsOf(t)); n != 0 {
		t.Errorf("%d Jobs created", n)
	}
	secrets := &corev1.SecretList{}
	if err := e.c.List(t.Context(), secrets); err != nil || len(secrets.Items) != 0 {
		t.Errorf("Secrets = %d, %v; want none created or deleted", len(secrets.Items), err)
	}

	live := newEnv(t, machine(managed, func(m *infrav1.TerraformMachine) { m.Finalizers = []string{testFinal} }))
	if _, err := Preamble(t.Context(), live.d, live.kindFor(t, OwnerInfo{})); err != nil {
		t.Fatal(err)
	}
	if m := live.get(t); !slices.Contains(m.Finalizers, testFinal) || len(live.rec.reasons) != 0 {
		t.Errorf("live externally managed object changed: %+v, events %v", m.Finalizers, live.rec.reasons)
	}
}

// TestPreambleExternallyManagedKeepsState proves the release of a deleting
// managed-by object waits while one of its Jobs runs, then keeps its state
// chunks, backups and inputs records as deletionPolicy Retain does
// (unowned, unprotected, labeled with its uid), so the garbage collector
// does not take the state the event says is left to the external manager.
func TestPreambleExternallyManagedKeepsState(t *testing.T) {
	t.Parallel()
	e := retainEnv(t, deleting, func(m *infrav1.TerraformMachine) {
		m.Annotations = map[string]string{clusterv1.ManagedByAnnotation: "someone"}
	})
	e.d.Jobs = e.runner
	e.runner.jobs = []batchv1.Job{job("a", jobs.OpApply, jobs.Running, t0)}
	got, err := Preamble(t.Context(), e.d, e.kindFor(t, OwnerInfo{}))
	if err != nil || !got.Stop || got.Result.RequeueAfter == 0 {
		t.Fatalf("Preamble with a running Job = %+v, %v; want a requeue", got, err)
	}
	if m := e.get(t); m == nil || !slices.Contains(m.Finalizers, testFinal) {
		t.Fatal("the finalizer went while a Job ran")
	}

	e.runner.jobs = nil
	if _, err := Preamble(t.Context(), e.d, e.kindFor(t, OwnerInfo{})); err != nil {
		t.Fatal(err)
	}
	if e.get(t) != nil {
		t.Fatal("object kept after the release")
	}
	assertRetained(t, e, "m1-uid")
	if !slices.Contains(e.rec.reasons, EventExternallyManagedReleased) {
		t.Errorf("events = %v, want %s", e.rec.reasons, EventExternallyManagedReleased)
	}
}

// TestDropFinalizerConflict proves the finalizer removal is an
// optimistic-lock patch: a stale object fails with a Conflict, the stored
// finalizers stay intact, and the preamble requeues instead of failing.
func TestDropFinalizerConflict(t *testing.T) {
	t.Parallel()
	e := newEnv(t, machine(deleting))
	stale := e.get(t)
	// Another controller adds a finalizer after our read.
	fresh := e.get(t)
	fresh.Finalizers = append(fresh.Finalizers, "other.example/keep")
	if err := e.c.Update(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	removed, err := dropFinalizer(t.Context(), e.c, stale, testFinal)
	if removed || !apierrors.IsConflict(err) {
		t.Fatalf("dropFinalizer = %v, %v; want a Conflict", removed, err)
	}
	if m := e.get(t); !slices.Equal(m.Finalizers, []string{testFinal, "other.example/keep"}) {
		t.Errorf("finalizers = %v, want both intact", m.Finalizers)
	}

	conflict := newEnvWith(t, interceptor.Funcs{Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
		return apierrors.NewConflict(schema.GroupResource{}, obj.GetName(), errors.New("stale"))
	}}, machine(deleting, func(m *infrav1.TerraformMachine) {
		m.Annotations = map[string]string{clusterv1.ManagedByAnnotation: "someone"}
	}))
	got, err := Preamble(t.Context(), conflict.d, conflict.kindFor(t, OwnerInfo{}))
	if err != nil || !got.Stop || got.Result.RequeueAfter != time.Second {
		t.Errorf("Preamble on Conflict = %+v, %v; want Stop with a short requeue", got, err)
	}
}
