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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestInputsApplied proves each state of the InputsApplied condition from
// the facts a pass learned.
func TestInputsApplied(t *testing.T) {
	t.Parallel()
	applied := StateView{Exists: true, InputsHash: "h1", CurrentHash: "h1"}
	for _, tt := range []struct {
		name    string
		mutable bool
		view    StateView
		gate    *Gate
		active  bool
		plan    bool
		failed  bool
		want    metav1.ConditionStatus
		reason  string
	}{
		{"hash equal", true, applied, nil, false, false, false, metav1.ConditionTrue, infrav1.InputsAppliedReason},
		{"hash differs, no Job", true, StateView{Exists: true, InputsHash: "h1", CurrentHash: "h2"}, nil, false, false, false,
			metav1.ConditionFalse, infrav1.ApplyPendingReason},
		{"no state yet", true, StateView{CurrentHash: "h2"}, nil, false, false, false, metav1.ConditionFalse, infrav1.ApplyPendingReason},
		{"plan waits", true, StateView{Exists: true, InputsHash: "h1", CurrentHash: "h2"}, nil, false, true, false,
			metav1.ConditionFalse, infrav1.AwaitingApprovalReason},
		{"apply active", true, StateView{Exists: true, InputsHash: "h1", CurrentHash: "h2"}, nil, true, false, false,
			metav1.ConditionFalse, infrav1.ApplyRunningReason},
		{"last apply failed", true, StateView{Exists: true, InputsHash: "h1", CurrentHash: "h2"}, nil, false, false, true,
			metav1.ConditionFalse, infrav1.InputsApplyFailedReason},
		{"hash equal but last apply failed", true, applied, nil, false, false, true, metav1.ConditionFalse, infrav1.InputsApplyFailedReason},
		{"inputs gated", true, StateView{Exists: true, InputsHash: "h1"}, &Gate{Reason: "WaitingForBootstrapData", Message: "waiting"}, false, false, false,
			metav1.ConditionUnknown, infrav1.InputsUnavailableReason},
		{"immutable, applied", false, StateView{Exists: true, InputsHash: "h1"}, nil, false, false, false, metav1.ConditionTrue, infrav1.InputsAppliedReason},
		{"immutable, first apply gated", false, StateView{}, &Gate{Reason: "X"}, false, false, false, metav1.ConditionUnknown, infrav1.InputsUnavailableReason},
		{"immutable, first apply not started", false, StateView{}, nil, false, false, false, metav1.ConditionFalse, infrav1.ApplyPendingReason},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fk := &fakeKind{obj: machine(), mutable: tt.mutable}
			fk.obj.Generation = 7
			r := &reconciler{k: fk, obj: fk.obj, st: fk.Status(), facts: inputsFacts{read: true, view: tt.view, gate: tt.gate}}
			if tt.active {
				r.st.ActiveJob = infrav1.ActiveJob{Name: "j1", Operation: infrav1.OperationApply}
			}
			if tt.plan {
				r.planWait = &metav1.Condition{Message: "TerraformPlan p waits"}
			}
			bk := &Bookkeeping{}
			bk.View.LastApplyFailed = tt.failed
			c := r.inputsApplied(bk)
			if c == nil || c.Status != tt.want || c.Reason != tt.reason {
				t.Fatalf("InputsApplied = %+v, want %s/%s", c, tt.want, tt.reason)
			}
			conditions.Set(fk.obj, *c)
			if got := conditions.Get(fk.obj, infrav1.InputsAppliedCondition); got == nil || got.ObservedGeneration != 7 {
				t.Errorf("stored condition = %+v, want observedGeneration 7", got)
			}
		})
	}
	// A deleting, paused or unread pass leaves the condition alone.
	fk := &fakeKind{obj: machine(), mutable: true}
	for name, r := range map[string]*reconciler{
		"deleting": {k: fk, obj: fk.obj, st: fk.Status(), deleting: true, facts: inputsFacts{read: true, view: applied}},
		"paused":   {k: fk, obj: fk.obj, st: fk.Status(), isPaused: true, facts: inputsFacts{read: true, view: applied}},
		"unread":   {k: fk, obj: fk.obj, st: fk.Status()},
	} {
		if c := r.inputsApplied(&Bookkeeping{}); c != nil {
			t.Errorf("%s: InputsApplied = %+v, want none", name, c)
		}
	}
}

// TestReconcileSetsInputsApplied proves a reconcile pass writes the
// condition in its status patch: gated inputs are Unknown, and the pass
// that starts the first apply already reports it Running.
func TestReconcileSetsInputsApplied(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	k := e.kindFor(t, readyOwner())
	k.gate = &Gate{Status: metav1.ConditionUnknown, Reason: infrav1.WaitingForBootstrapDataReason, Message: "waiting"}
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	m := e.get(t)
	if c := conditions.Get(m, infrav1.InputsAppliedCondition); c == nil || c.Status != metav1.ConditionUnknown ||
		c.Reason != infrav1.InputsUnavailableReason || c.ObservedGeneration != m.Generation {
		t.Errorf("gated: InputsApplied = %+v", c)
	}

	e = newEnv(t, world(machine(withFinalizer, notPaused))...)
	k = e.kindFor(t, readyOwner())
	k.in = machineIn()
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	if c := conditions.Get(e.get(t), infrav1.InputsAppliedCondition); c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.ApplyRunningReason {
		t.Errorf("apply started: InputsApplied = %+v", c)
	}
}
