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

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// activeJob returns a machine-mutator that records name as the running
// Job in status.activeJob.
func activeJob(name string) func(*infrav1.TerraformMachine) {
	return func(m *infrav1.TerraformMachine) {
		m.Status.ActiveJob = infrav1.ActiveJob{Name: name, Operation: infrav1.OperationApply, Attempt: 1}
	}
}

// staleEnv returns, failing t on error, an env holding a machine built
// from mut and, when running is set, the running Job j1 in the fake
// client, whose Job cache (a clientRunner) lists no Job at all.
func staleEnv(t *testing.T, running bool, mut ...func(*infrav1.TerraformMachine)) *env {
	t.Helper()
	e := newEnv(t, world(machine(mut...))...)
	e.d.Jobs = &clientRunner{c: e.c, stale: []batchv1.Job{}}
	if running {
		j := job("j1", jobs.OpApply, jobs.Running, t0)
		if err := e.c.Create(t.Context(), &j); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// TestReconcileActiveJobCacheLag: a Job status.activeJob names that the
// Job cache does not list yet but the API server has keeps block-move,
// status.activeJob and the finalizer, and requeues shortly; once the API
// server has no such Job either, both are cleared.
func TestReconcileActiveJobCacheLag(t *testing.T) {
	t.Parallel()
	withBlockMove := func(m *infrav1.TerraformMachine) { SetBlockMove(m) }
	pausedOwner := OwnerInfo{HasOwnerRef: true, Cluster: cluster(true)}
	for _, tt := range []struct {
		name    string
		owner   OwnerInfo
		mut     []func(*infrav1.TerraformMachine)
		running bool
		// lag is whether the reconcile must wait for the cache.
		lag bool
	}{
		{"running: cache lags", readyOwner, []func(*infrav1.TerraformMachine){withFinalizer, notPaused}, true, true},
		{"paused: cache lags", pausedOwner, []func(*infrav1.TerraformMachine){withFinalizer}, true, true},
		{"deleting without state: cache lags", readyOwner, []func(*infrav1.TerraformMachine){deleting, notPaused}, true, true},
		{"the Job is gone", readyOwner, []func(*infrav1.TerraformMachine){withFinalizer, notPaused}, false, false},
		{"paused, the Job is gone", pausedOwner, []func(*infrav1.TerraformMachine){withFinalizer}, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := staleEnv(t, tt.running, append(tt.mut, withBlockMove, activeJob("j1"))...)
			k := e.kindFor(t, tt.owner)
			k.in = machineIn()
			requeue, err := reconcileOnce(t, e, k)
			if err != nil {
				t.Fatal(err)
			}
			m := e.get(t)
			if m == nil {
				t.Fatal("the finalizer was dropped")
			}
			kept := HasBlockMove(m) && m.Status.ActiveJob.Name == "j1"
			if tt.lag && (!kept || requeue != LagRequeue) {
				t.Errorf("block-move %v, activeJob %+v, requeue %s", HasBlockMove(m), m.Status.ActiveJob, requeue)
			}
			// Unpaused, the reconcile goes on to start the next Job.
			if !tt.lag && (m.Status.ActiveJob.Name == "j1" || tt.owner.Cluster.Spec.Paused != nil && HasBlockMove(m)) {
				t.Errorf("block-move %v, activeJob %+v kept for a Job that is gone", HasBlockMove(m), m.Status.ActiveJob)
			}
			if n := len(e.jobsOf(t)); tt.lag && n != 1 {
				t.Errorf("%d Jobs, want only j1", n)
			}
		})
	}
}

// readCounter counts the run lease and Job reads made through an env's
// client. The state lock Lease, which bookkeeping reads every pass, is
// not counted.
type readCounter struct {
	// runLease is the name of the run lease whose reads count.
	runLease string
	// leases and jobs count Get calls on the run lease and on a Job.
	leases, jobs atomic.Int32
}

// funcs returns interceptor funcs that count rc's reads and pass every
// call through.
func (rc *readCounter) funcs() interceptor.Funcs {
	return interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		switch obj.(type) {
		case *coordinationv1.Lease:
			if key.Name == rc.runLease {
				rc.leases.Add(1)
			}
		case *batchv1.Job:
			rc.jobs.Add(1)
		}
		return c.Get(ctx, key, obj, opts...)
	}}
}

// liveHolderEnv returns, failing t on error, an env holding a machine
// built from mut, the running Job j1 of that machine in the fake client
// and a live run lease j1 holds; the client counts its reads in rc.
func liveHolderEnv(t *testing.T, rc *readCounter, mut ...func(*infrav1.TerraformMachine)) *env {
	t.Helper()
	rc.runLease = runLeaseOf(t, state.KindTerraformMachine, testName)
	e := newEnvWith(t, rc.funcs(), world(machine(mut...))...)
	j := job("j1", jobs.OpApply, jobs.Running, t0)
	j.Labels[state.OwnerKindLabel] = state.KindTerraformMachine
	j.Labels[state.OwnerNameLabel] = testName
	if err := e.c.Create(t.Context(), &j); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Create(t.Context(), foreignLease(runLeaseOf(t, state.KindTerraformMachine, testName), "j1", jobs.OpApply, t0)); err != nil {
		t.Fatal(err)
	}
	return e
}

// TestCacheLagReadsLeaseOnlyWhenItMatters: cacheLag reads the run lease
// and its holder Job only when its answer guards something (block-move
// set, status.activeJob set, or a deletion); an idle object reads
// neither, and the others still see the live holder the Job cache does
// not list.
func TestCacheLagReadsLeaseOnlyWhenItMatters(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		mut      []func(*infrav1.TerraformMachine)
		deleting bool
		// lag is cacheLag's expected answer; reads is whether it reads the
		// lease and a Job.
		lag, reads bool
	}{
		{"idle", nil, false, false, false},
		{"block-move", []func(*infrav1.TerraformMachine){func(m *infrav1.TerraformMachine) { SetBlockMove(m) }}, false, true, true},
		{"activeJob of a Job that is gone", []func(*infrav1.TerraformMachine){activeJob("j0")}, false, true, true},
		{"deleting", nil, true, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var rc readCounter
			e := liveHolderEnv(t, &rc, append([]func(*infrav1.TerraformMachine){withFinalizer}, tt.mut...)...)
			k := e.kindFor(t, readyOwner)
			rc.leases.Store(0)
			rc.jobs.Store(0)
			r := &reconciler{d: e.d, k: k, obj: k.Object(), st: k.Status(), suffix: suffixOf(t, state.KindTerraformMachine, testName), deleting: tt.deleting}
			lag, err := r.cacheLag(t.Context(), &Bookkeeping{})
			if err != nil {
				t.Fatal(err)
			}
			if lag != tt.lag {
				t.Errorf("cacheLag = %v, want %v", lag, tt.lag)
			}
			if read := rc.leases.Load() > 0 && rc.jobs.Load() > 0; read != tt.reads || !tt.reads && rc.leases.Load()+rc.jobs.Load() != 0 {
				t.Errorf("Lease reads %d, Job reads %d; want reads %v", rc.leases.Load(), rc.jobs.Load(), tt.reads)
			}
		})
	}
}

// TestReconcileIdleReadsNoLease: a paused reconcile of an idle object (no
// block-move, no status.activeJob) reads no run lease and no Job through
// the API server, even while a live lease exists; with block-move set the
// same reconcile reads them and waits for the live holder.
func TestReconcileIdleReadsNoLease(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		blockMove bool
	}{
		{"idle", false},
		{"block-move", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var rc readCounter
			mut := []func(*infrav1.TerraformMachine){withFinalizer}
			if tt.blockMove {
				mut = append(mut, func(m *infrav1.TerraformMachine) { SetBlockMove(m) })
			}
			e := liveHolderEnv(t, &rc, mut...)
			k := e.kindFor(t, OwnerInfo{HasOwnerRef: true, Cluster: cluster(true)})
			rc.leases.Store(0)
			rc.jobs.Store(0)
			requeue, err := reconcileOnce(t, e, k)
			if err != nil {
				t.Fatal(err)
			}
			n, m := rc.leases.Load(), rc.jobs.Load()
			switch {
			case !tt.blockMove && (n != 0 || m != 0):
				t.Errorf("Lease reads %d, Job reads %d; want none", n, m)
			case tt.blockMove && (n == 0 || m == 0 || requeue != LagRequeue || !HasBlockMove(e.get(t))):
				t.Errorf("Lease reads %d, Job reads %d, requeue %s; want both read and block-move kept", n, m, requeue)
			}
		})
	}
}

// TestReconcileLiveRunLeaseKeepsFinalizer: a deletion without state keeps
// the finalizer while a live Job the cache does not list holds the run
// lease.
func TestReconcileLiveRunLeaseKeepsFinalizer(t *testing.T) {
	t.Parallel()
	e := staleEnv(t, true, deleting, notPaused)
	if err := e.c.Create(t.Context(), foreignLease(runLeaseOf(t, state.KindTerraformMachine, testName), "j1", jobs.OpApply, t0)); err != nil {
		t.Fatal(err)
	}
	k := e.kindFor(t, readyOwner)
	requeue, err := reconcileOnce(t, e, k)
	if err != nil {
		t.Fatal(err)
	}
	if m := e.get(t); m == nil || !slices.Contains(m.Finalizers, testFinal) || requeue != LagRequeue {
		t.Errorf("object %+v, requeue %s", m, requeue)
	}
	if e.lease(t, runLeaseOf(t, state.KindTerraformMachine, testName)) == nil {
		t.Error("the live run lease was deleted")
	}
}

// TestReconcileLiveRunLeaseKeepsBlockMove: a paused object whose
// status.activeJob is empty (the object cache has block-move but not the
// later activeJob patch) keeps block-move while a live run lease names a
// Job of the object that the Job cache does not list; once the Job is gone
// the lease is no live holder and block-move clears.
func TestReconcileLiveRunLeaseKeepsBlockMove(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		running bool
		kept    bool
	}{
		{"the holder runs", true, true},
		{"the holder is gone", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := staleEnv(t, false, withFinalizer, func(m *infrav1.TerraformMachine) { SetBlockMove(m) })
			leaseName := runLeaseOf(t, state.KindTerraformMachine, testName)
			if tt.running {
				j := job("j1", jobs.OpApply, jobs.Running, t0)
				j.Labels[state.OwnerKindLabel] = state.KindTerraformMachine
				j.Labels[state.OwnerNameLabel] = testName
				if err := e.c.Create(t.Context(), &j); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.c.Create(t.Context(), foreignLease(leaseName, "j1", jobs.OpApply, t0)); err != nil {
				t.Fatal(err)
			}
			k := e.kindFor(t, OwnerInfo{HasOwnerRef: true, Cluster: cluster(true)})
			k.in = machineIn()
			requeue, err := reconcileOnce(t, e, k)
			if err != nil {
				t.Fatal(err)
			}
			m := e.get(t)
			if m == nil {
				t.Fatal("the finalizer was dropped")
			}
			if tt.kept && (!HasBlockMove(m) || requeue != LagRequeue) {
				t.Errorf("block-move %v, requeue %s; want it kept and a lag requeue", HasBlockMove(m), requeue)
			}
			if !tt.kept && HasBlockMove(m) {
				t.Error("block-move kept for a holder that is gone")
			}
		})
	}
}
