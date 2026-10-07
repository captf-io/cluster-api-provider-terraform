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

package terraformcluster

import (
	"slices"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
)

// stored returns the stored TerraformCluster, or nil once it is gone,
// failing the test on any other error.
func (e *clusterEnv) stored() *infrav1.TerraformCluster {
	e.t.Helper()
	tc := &infrav1.TerraformCluster{}
	if err := e.c.Get(e.t.Context(), e.req.NamespacedName, tc); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil
		}
		e.t.Fatal(err)
	}
	return tc
}

// deleteCluster deletes the TerraformCluster as kubectl delete does: its
// finalizer keeps it, with a deletionTimestamp.
func (e *clusterEnv) deleteCluster() {
	e.t.Helper()
	if err := e.c.Delete(e.t.Context(), e.cluster()); err != nil {
		e.t.Fatal(err)
	}
}

// opJobNames returns the names of the Jobs of op in the client.
func (e *clusterEnv) opJobNames(op jobs.Op) []string {
	e.t.Helper()
	var l batchv1.JobList
	if err := e.c.List(e.t.Context(), &l, client.InNamespace(ns)); err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for i := range l.Items {
		if jobs.OpOf(&l.Items[i]) == op {
			out = append(out, l.Items[i].Name)
		}
	}
	return out
}

// TestEngineParity runs the engine's lifecycle cases through the real
// TerraformCluster adapter, as the shared package's tests run them through
// its fake kind: destroy of an applied cluster, a deletion with no state,
// a paused cluster, and a first apply that vanished mid-run.
func TestEngineParity(t *testing.T) {
	t.Parallel()
	t.Run("destroy", func(t *testing.T) {
		t.Parallel()
		e := newClusterEnv(t)
		e.deleteCluster()
		before := len(e.runner.created())
		e.reconcile()
		destroys := e.opJobNames(jobs.OpDestroy)
		if len(destroys) != 1 || len(e.runner.created()) != before+1 {
			t.Fatalf("destroy Jobs %v, created %v", destroys, e.runner.created())
		}
		tc := e.stored()
		if tc == nil || !slices.Contains(tc.Finalizers, Finalizer) || tc.Status.ActiveJob.Name != destroys[0] {
			t.Fatalf("while the destroy runs: %+v", tc)
		}
		e.reconcile()
		if got := e.opJobNames(jobs.OpDestroy); len(got) != 1 {
			t.Fatalf("destroy Jobs %v after a second pass, want one", got)
		}
		var j batchv1.Job
		if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: destroys[0]}, &j); err != nil {
			t.Fatal(err)
		}
		e.finish(&j, batchv1.JobComplete)
		e.st.mu.Lock()
		e.st.st = nil
		e.st.mu.Unlock()
		e.reconcile()
		if tc := e.stored(); tc != nil {
			t.Errorf("the cluster remains after the destroy: finalizers %v", tc.Finalizers)
		}
	})
	t.Run("delete with no state", func(t *testing.T) {
		t.Parallel()
		e := newUnappliedClusterEnv(t)
		e.deleteCluster()
		e.reconcile()
		if tc := e.stored(); tc != nil || len(e.runner.created()) != 0 {
			t.Errorf("cluster %+v, created %v; want it gone without a Job", tc, e.runner.created())
		}
	})
	t.Run("paused", func(t *testing.T) {
		t.Parallel()
		e := newClusterEnv(t)
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
		e.setVersion("v1.37.0")
		for range 3 {
			e.reconcileNoApply()
		}
		if got := e.opJobNames(jobs.OpApply); len(got) != 1 {
			t.Fatalf("apply Jobs %v while paused, want only the first", got)
		}
		if c := conditions.Get(e.cluster(), clusterv1.PausedCondition); c == nil || c.Status != metav1.ConditionTrue {
			t.Errorf("Paused = %+v", c)
		}
		setPaused(false)
		// The unpaused cluster applies the new version.
		for range 3 {
			if len(e.opJobNames(jobs.OpApply)) == 2 {
				break
			}
			e.reconcile()
		}
		if got := e.opJobNames(jobs.OpApply); len(got) != 2 {
			t.Errorf("apply Jobs %v once unpaused, want the first and the new version's", got)
		}
	})
	t.Run("first apply vanished, then deleted", func(t *testing.T) {
		t.Parallel()
		e := newUnappliedClusterEnv(t)
		j := e.reconcileStarts()
		e.deleteJob(j)
		e.reconcileNoApply()
		if got := e.interrupted(); got != j.Name {
			t.Fatalf("unconfirmed apply = %q, want %s", got, j.Name)
		}
		e.deleteCluster()
		e.reconcileNoApply()
		tc := e.stored()
		if tc == nil || !slices.Contains(tc.Finalizers, Finalizer) {
			t.Fatalf("the deletion did not hold: %+v", tc)
		}
		if c := conditions.Get(tc, infrav1.StateReadableCondition); c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.ApplyOutcomeUnknownReason {
			t.Errorf("StateReadable = %+v, want False/ApplyOutcomeUnknown", c)
		}
		if got := e.opJobNames(jobs.OpDestroy); len(got) != 0 {
			t.Errorf("destroy Jobs %v for a cluster whose first apply is unconfirmed", got)
		}
		tc.Annotations = map[string]string{infrav1.ConfirmNoResourcesAnnotation: j.Name}
		if err := e.c.Update(t.Context(), tc); err != nil {
			t.Fatal(err)
		}
		e.reconcile()
		if tc := e.stored(); tc != nil {
			t.Errorf("the cluster remains after confirming no resources: %+v", tc.Finalizers)
		}
	})
}
