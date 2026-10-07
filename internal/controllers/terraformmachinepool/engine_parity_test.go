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

package terraformmachinepool

import (
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// stored returns the stored pool, or nil once it is gone, failing the test
// on any other error.
func (e *holdEnv) stored() *infrav1.TerraformMachinePool {
	e.t.Helper()
	p := &infrav1.TerraformMachinePool{}
	if err := e.c.Get(e.t.Context(), e.req.NamespacedName, p); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil
		}
		e.t.Fatal(err)
	}
	return p
}

// deletePool deletes the pool as kubectl delete does: its finalizer keeps
// it, with a deletionTimestamp.
func (e *holdEnv) deletePool() {
	e.t.Helper()
	if err := e.c.Delete(e.t.Context(), e.stored()); err != nil {
		e.t.Fatal(err)
	}
}

// opJobs returns the Jobs of op in the client.
func (e *holdEnv) opJobs(op jobs.Op) []batchv1.Job {
	e.t.Helper()
	var l batchv1.JobList
	if err := e.c.List(e.t.Context(), &l, client.InNamespace(ns)); err != nil {
		e.t.Fatal(err)
	}
	return slices.DeleteFunc(l.Items, func(j batchv1.Job) bool { return jobs.OpOf(&j) != op })
}

// clearState drops the pool's state, as a finished destroy leaves it.
func (e *holdEnv) clearState() {
	e.t.Helper()
	suffix, err := state.Suffix(ns, state.KindTerraformMachinePool, e.req.Name)
	if err != nil {
		e.t.Fatal(err)
	}
	e.sr.mu.Lock()
	delete(e.sr.bySuffix, suffix)
	e.sr.mu.Unlock()
}

// TestEngineParity runs the engine's lifecycle cases through the real
// TerraformMachinePool adapter, as the shared package's tests run them
// through its fake kind: destroy of an applied pool, a deletion with no
// state, a paused pool, and a first apply that vanished mid-run.
func TestEngineParity(t *testing.T) {
	t.Parallel()
	t.Run("destroy", func(t *testing.T) {
		t.Parallel()
		e := newHoldEnv(t)
		e.deletePool()
		d := e.reconcileStarts(jobs.OpDestroy)
		if p := e.stored(); p == nil || !slices.Contains(p.Finalizers, Finalizer) || p.Status.ActiveJob.Name != d.Name {
			t.Fatalf("while the destroy runs: %+v", p)
		}
		e.reconcileIdle()
		e.finish(d, batchv1.JobComplete)
		e.clearState()
		e.reconcile()
		if p := e.stored(); p != nil {
			t.Errorf("the pool remains after the destroy: finalizers %v", p.Finalizers)
		}
	})
	t.Run("delete with no state", func(t *testing.T) {
		t.Parallel()
		e := newPoolEnv(t)
		e.deletePool()
		e.reconcileIdle()
		if p := e.stored(); p != nil {
			t.Errorf("the pool remains: %+v", p.Finalizers)
		}
	})
	t.Run("paused", func(t *testing.T) {
		t.Parallel()
		e := newHoldEnv(t)
		setPaused := func(paused bool) {
			t.Helper()
			c := &clusterv1.Cluster{}
			if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "c1"}, c); err != nil {
				t.Fatal(err)
			}
			c.Spec.Paused = &paused
			if err := e.c.Update(t.Context(), c); err != nil {
				t.Fatal(err)
			}
		}
		setPaused(true)
		e.rotate("#cloud-config\n# rotated\n")
		for range 3 {
			e.reconcileNoApply()
		}
		if c := conditions.Get(e.stored(), clusterv1.PausedCondition); c == nil || c.Status != metav1.ConditionTrue {
			t.Errorf("Paused = %+v", c)
		}
		setPaused(false)
		for range 3 {
			if len(e.opJobs(jobs.OpApply)) == 2 {
				break
			}
			e.reconcile()
		}
		if got := e.opJobs(jobs.OpApply); len(got) != 2 {
			t.Errorf("%d apply Jobs once unpaused, want the first and the rotated inputs'", len(got))
		}
	})
	t.Run("first apply vanished, then deleted", func(t *testing.T) {
		t.Parallel()
		e := newPoolEnv(t)
		j := e.reconcileStarts(jobs.OpApply)
		e.deleteJob(j)
		e.reconcileIdle()
		e.deletePool()
		e.reconcileIdle()
		p := e.stored()
		if p == nil || !slices.Contains(p.Finalizers, Finalizer) {
			t.Fatalf("the deletion did not hold: %+v", p)
		}
		if c := conditions.Get(p, infrav1.StateReadableCondition); c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.ApplyOutcomeUnknownReason ||
			!strings.Contains(c.Message, j.Name) {
			t.Errorf("StateReadable = %+v, want False/ApplyOutcomeUnknown naming %s", c, j.Name)
		}
		p.Annotations = map[string]string{infrav1.ConfirmNoResourcesAnnotation: j.Name}
		if err := e.c.Update(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		e.reconcile()
		if p := e.stored(); p != nil {
			t.Errorf("the pool remains after confirming no resources: %+v", p.Finalizers)
		}
	})
}
