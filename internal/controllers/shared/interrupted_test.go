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
	"strings"
	"testing"

	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runlease"
)

// TestInterruptedApplyMachine: a first apply deleted while it runs is
// recorded as unconfirmed for every kind, immutable machines included:
// it may have created resources before any state was written. No second
// first apply runs (ApplyOutcomeUnknown) until the operator confirms the
// Job created nothing; the apply after that carries no record of it.
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
			if d.InterruptedApply != vanished || !d.Attempt.MayHaveApplied {
				t.Errorf("after Job %s vanished: unconfirmed apply %q, may have applied %v", vanished, d.InterruptedApply, d.Attempt.MayHaveApplied)
			}
			if len(e.runner.created) != 1 {
				t.Errorf("after Job %s vanished: created %v, want no second first apply", vanished, e.runner.created)
			}
			if c := conditions.Get(e.get(t), infrav1.StateReadableCondition); c == nil || c.Reason != infrav1.ApplyOutcomeUnknownReason || !strings.Contains(c.Message, vanished) {
				t.Errorf("StateReadable = %+v, want ApplyOutcomeUnknown naming %s", c, vanished)
			}

			e.annotate(t, infrav1.ConfirmNoResourcesAnnotation, vanished)
			if _, err := reconcileOnce(t, e, kind()); err != nil {
				t.Fatal(err)
			}
			if d, _ := inputs.Read(t.Context(), e.c, testNS, "m", testName); d.InterruptedApply != "" {
				t.Errorf("unconfirmed apply after the confirmation = %q", d.InterruptedApply)
			}
			if _, ok := e.get(t).Annotations[infrav1.ConfirmNoResourcesAnnotation]; ok {
				t.Error("the confirmation was not consumed")
			}
			if len(e.runner.created) != 2 || e.jobNamed(t, e.runner.created[1]).Annotations[AfterInterruptedApplyAnnotation] != "" {
				t.Errorf("after the confirmation: created %v, want a first apply that names no unconfirmed Job", e.runner.created)
			}
		})
	}
}
