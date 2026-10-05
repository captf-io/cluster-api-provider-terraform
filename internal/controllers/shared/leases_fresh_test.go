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
	"errors"
	"testing"
	"time"

	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runlease"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// TestStartJobLeaseFreshness: the run lease a pass took at t0 is checked
// against the clock just before the Job create. A lease taken less than
// leaseFresh ago is kept as is; an older one is taken again, refreshing
// its acquire time, unless the gate check that repeats now waits (the
// cluster's operation started while the lease could lapse): then no Job
// starts, the lease goes back, and the error wraps ErrStartDeferred. A run
// lease another Job took meanwhile, fresh or not, defers the start too,
// and is left to that Job.
func TestStartJobLeaseFreshness(t *testing.T) {
	t.Parallel()
	runName := runLeaseOf(t, state.KindTerraformMachine, testName)
	const clusterJob = "tc-apply-job"
	// clusterOperating has the TerraformCluster take the cluster write
	// lease at at and run its Job.
	clusterOperating := func(t *testing.T, e *env, at time.Time) {
		t.Helper()
		if err := e.c.Create(t.Context(), jobObj(clusterJob, jobs.Running)); err != nil {
			t.Fatal(err)
		}
		if res, err := runlease.Acquire(t.Context(), e.c, e.c, at, runlease.Spec{
			Namespace: testNS, Name: runlease.ClusterName(testNS, "c1"), Kind: runlease.KindCluster, Holder: clusterJob,
			Op: jobs.OpApply, OwnerKind: state.KindTerraformCluster, OwnerName: "tc", ClusterName: "c1",
		}); err != nil || !res.Acquired {
			t.Fatalf("Acquire cluster lease = %+v, %v", res, err)
		}
	}
	for _, tc := range []struct {
		name string
		// age is how long after t0, when the run lease was taken, the Job
		// is created.
		age time.Duration
		// holder takes the run lease instead of the Job; "" means the Job.
		holder string
		// setup runs after the run lease is taken.
		setup        func(*testing.T, *env, time.Time)
		wantJob      bool
		wantAcquired time.Time
	}{
		{name: "fresh", age: 10 * time.Second, wantJob: true, wantAcquired: t0},
		{name: "stale is taken again", age: 45 * time.Second, wantJob: true, wantAcquired: t0.Add(45 * time.Second)},
		{name: "lapsed while the cluster started", age: 70 * time.Second, setup: func(t *testing.T, e *env, now time.Time) {
			clusterOperating(t, e, now.Add(-5*time.Second))
		}},
		{name: "held by another Job", age: 10 * time.Second, holder: "other-job"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, jr := leaseEnv(t, true, interceptor.Funcs{})
			k := e.kindNamed(t, testName)
			suffix, err := state.Suffix(testNS, state.KindTerraformMachine, testName)
			if err != nil {
				t.Fatal(err)
			}
			req := JobRequest{
				Op: jobs.OpApply, Files: renderMachine(t), InputsHash: "h1:x", Attempt: 1, Suffix: suffix, ClusterName: "c1",
				Source: infrav1.Source{Image: "registry.example/mod:1.0"}, Identity: testIdentity,
			}
			holder := tc.holder
			if holder == "" {
				holder = JobName(k, req)
			}
			if res, err := runlease.Acquire(t.Context(), e.c, e.c, t0, runlease.Spec{
				Namespace: testNS, Name: runName, Kind: runlease.KindRun, Holder: holder, Op: jobs.OpApply,
				OwnerKind: state.KindTerraformMachine, OwnerName: testName, ClusterName: "c1",
			}); err != nil || !res.Acquired {
				t.Fatalf("Acquire = %+v, %v", res, err)
			}
			now := t0.Add(tc.age)
			if tc.setup != nil {
				tc.setup(t, e, now)
			}
			e.d.Clock = testingclock.NewFakePassiveClock(now)
			_, err = StartJob(t.Context(), e.d, k, req)
			l := e.lease(t, runName)
			if tc.wantJob {
				if err != nil || jr.created.Load() != 1 {
					t.Fatalf("StartJob = %v, created %d, want one Job", err, jr.created.Load())
				}
				if l == nil || runlease.HolderOf(l) != holder || !runlease.AcquiredAt(l).Equal(tc.wantAcquired) {
					t.Errorf("run lease %+v, want held by %s acquired at %s", l, holder, tc.wantAcquired)
				}
				return
			}
			if !errors.Is(err, ErrStartDeferred) || jr.created.Load() != 0 {
				t.Fatalf("StartJob = %v, created %d, want ErrStartDeferred and no Job", err, jr.created.Load())
			}
			switch {
			case tc.holder != "" && (l == nil || runlease.HolderOf(l) != tc.holder || !runlease.AcquiredAt(l).Equal(t0)):
				t.Errorf("run lease %+v, want it left to %s as taken at %s", l, tc.holder, t0)
			case tc.holder == "" && l != nil:
				t.Errorf("the run lease of a Job never created is held by %s", runlease.HolderOf(l))
			}
		})
	}
}
