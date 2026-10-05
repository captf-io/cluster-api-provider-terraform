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
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/captf-io/cluster-api-provider-terraform/internal/runlease"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// TestStartJobPauseHandshake: a clusterctl move that pauses the Cluster
// (or the object) after this pass read it from the cache is seen by the
// live read that follows the block-move write: no Job starts, block-move
// is cleared again for the move, the leases go back, and the deferral
// (ErrStartDeferred) is a quiet requeue, not a reconcile error. An
// unpaused Cluster still gets its Job.
func TestStartJobPauseHandshake(t *testing.T) {
	t.Parallel()
	runName := runLeaseOf(t, state.KindTerraformMachine, testName)
	pauseObject := func(t *testing.T, e *env) {
		t.Helper()
		m := e.get(t)
		before := m.DeepCopy()
		m.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
		if err := e.c.Patch(t.Context(), m, client.MergeFrom(before)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		// clusterPaused is the stored Cluster's spec.paused; the owner
		// passed in is always unpaused.
		clusterPaused bool
		// pause, when set, pauses the stored object after the pass loaded
		// it.
		pause   func(*testing.T, *env)
		wantJob bool
	}{
		{name: "the Cluster is paused live", clusterPaused: true},
		{name: "the object is paused live", pause: pauseObject},
		{name: "unpaused", wantJob: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, _ := leaseEnv(t, true, interceptor.Funcs{}, cluster(tc.clusterPaused))
			k := e.kindNamed(t, testName)
			if tc.pause != nil {
				tc.pause(t, e)
			}
			res, err := Reconcile(t.Context(), e.d, k)
			created := e.jobsOf(t)
			if tc.wantJob {
				if err != nil || len(created) != 1 || !HasBlockMove(e.get(t)) {
					t.Fatalf("Reconcile = %v, Jobs %d, block-move %v; want one Job under block-move", err, len(created), HasBlockMove(e.get(t)))
				}
				if l := e.lease(t, runName); l == nil || runlease.HolderOf(l) != created[0].Name {
					t.Errorf("run lease %+v, want it held by %s", l, created[0].Name)
				}
				return
			}
			if err != nil || res.RequeueAfter != LagRequeue {
				t.Errorf("Reconcile = %+v, %v; want a quiet requeue after %s", res, err, LagRequeue)
			}
			if len(created) != 0 {
				t.Errorf("created %d Jobs while paused, want none", len(created))
			}
			if HasBlockMove(e.get(t)) {
				t.Error("block-move is still set: clusterctl move would wait for it")
			}
			if l := e.lease(t, runName); l != nil {
				t.Errorf("the run lease of a Job never created is held by %s", runlease.HolderOf(l))
			}
		})
	}
}
