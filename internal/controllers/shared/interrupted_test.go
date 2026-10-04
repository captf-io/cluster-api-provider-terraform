/*
Copyright 2026 The cluster-api-provider-terraform Authors.

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

	testingclock "k8s.io/utils/clock/testing"

	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runlease"
)

// TestInterruptedApplyMachine: a machine's first apply deleted while it
// runs is recorded as interrupted by no one: an immutable kind applies
// again anyway until its first apply succeeds, and never after, so its
// next apply carries no record of it. A mutable kind's is recorded.
func TestInterruptedApplyMachine(t *testing.T) {
	t.Parallel()
	for name, mutable := range map[string]bool{"machine": false, "mutable kind": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, world(machine(withFinalizer, notPaused))...)
			kind := func() *fakeKind {
				k := e.kindFor(t, readyOwner)
				k.in, k.mutable = machineIn(), mutable
				return k
			}
			if _, err := reconcileOnce(t, e, kind()); err != nil {
				t.Fatal(err)
			}
			if len(e.runner.created) != 1 {
				t.Fatalf("created %v, want the first apply", e.runner.created)
			}
			vanished := e.runner.created[0]
			e.runner.mu.Lock()
			e.runner.jobs = nil
			e.runner.mu.Unlock()
			e.d.Clock = testingclock.NewFakePassiveClock(t0.Add(2 * runlease.Grace))
			if _, err := reconcileOnce(t, e, kind()); err != nil {
				t.Fatal(err)
			}
			d, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if mutable {
				want = vanished
			}
			if d.InterruptedApply != want {
				t.Errorf("interrupted apply after Job %s vanished = %q, want %q", vanished, d.InterruptedApply, want)
			}
			if len(e.runner.created) != 2 || e.runner.jobs[0].Annotations[AfterInterruptedApplyAnnotation] != want {
				t.Errorf("after Job %s vanished: created %v, annotations %v", vanished, e.runner.created, e.runner.jobs)
			}
		})
	}
}
