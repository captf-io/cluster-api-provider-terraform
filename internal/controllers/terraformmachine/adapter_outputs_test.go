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

package terraformmachine

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// TestApplyOutputsInterruptibleConcern proves a non-boolean interruptible
// output keeps the previous status value instead of clearing a true.
func TestApplyOutputsInterruptibleConcern(t *testing.T) {
	t.Parallel()
	a := newTestAdapter(t, testTM(func(tm *infrav1.TerraformMachine) { tm.Status.Interruptible = new(true) }), stateReader{})
	bad := machineState(map[string]string{"provider_id": `"aws:///us-east-1a/i-1"`, "health": healthy, "interruptible": `"yes"`})
	res, _, err := a.ApplyOutputs(t.Context(), shared.OwnerInfo{}, bad, nil)
	if err != nil || res.Valid() {
		t.Fatalf("ApplyOutputs = %+v, %v; want a violation", res, err)
	}
	if i := a.obj.Status.Interruptible; i == nil || !*i {
		t.Errorf("status.interruptible = %v after a violation, want the previous true", i)
	}
}

// remediating returns a copy of tm with remediation.annotateMachine on.
func remediating(tm *infrav1.TerraformMachine) *infrav1.TerraformMachine {
	c := tm.DeepCopy()
	c.Spec.Remediation = &infrav1.MachineRemediation{AnnotateMachine: new(true)}
	return c
}

// TestApplyOutputsProviderIDDebounce proves a provider_id that goes null
// after provisioning is Unknown for the first sample, Terminated only for
// a later sample (a refresh completed since) that is null too, never for a
// reconcile of the same outputs, and Healthy again once it returns.
func TestApplyOutputsProviderIDDebounce(t *testing.T) {
	t.Parallel()
	id := `"aws:///us-east-1a/i-1"`
	present := machineState(map[string]string{"provider_id": id, "health": healthy})
	missing := machineState(map[string]string{"health": healthy})
	a := newTestAdapter(t, testTM(func(tm *infrav1.TerraformMachine) {
		tm.Spec.ProviderID = "aws:///us-east-1a/i-1"
		tm.Status.Initialization.Provisioned = new(true)
		tm.Status.LastRefresh = new(metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	}), stateReader{})
	// settle plays the shared flow's part: the condition the health
	// produces, with the reason the setter maps the state to.
	settle := func(h *contract.Health) {
		reason := infrav1.HealthyReason
		status := metav1.ConditionTrue
		switch h.State {
		case contract.HealthProviderIDMissing:
			reason, status = infrav1.ProviderIDMissingReason, metav1.ConditionUnknown
		case contract.HealthTerminated:
			reason, status = infrav1.InstanceTerminatedReason, metav1.ConditionFalse
		}
		msg := ""
		if h.Message != nil {
			msg = *h.Message
		}
		conditions.Set(a.obj, metav1.Condition{Type: infrav1.InfrastructureHealthyCondition, Status: status, Reason: reason, Message: msg})
	}
	apply := func(st *state.State) *contract.Health {
		t.Helper()
		_, h, err := a.ApplyOutputs(t.Context(), shared.OwnerInfo{}, st, nil)
		if err != nil || h == nil {
			t.Fatalf("ApplyOutputs = %+v, %v", h, err)
		}
		settle(h)
		return h
	}

	if h := apply(present); h.State != contract.HealthRunning {
		t.Fatalf("present: %+v", h)
	}
	h := apply(missing)
	if h.State != contract.HealthProviderIDMissing || h.Message == nil || !strings.Contains(*h.Message, "next sample") {
		t.Fatalf("first missing sample: %+v", h)
	}
	if RemediationReason(remediating(a.obj)) != "" {
		t.Error("the first missing sample asked for remediation")
	}
	if h := apply(missing); h.State != contract.HealthProviderIDMissing {
		t.Errorf("same refresh reconciled again: %+v, want still ProviderIDMissing", h)
	}
	// A refresh completed since: the second consecutive sample.
	a.obj.Status.LastRefresh = new(metav1.NewTime(time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)))
	if h := apply(missing); h.State != contract.HealthTerminated {
		t.Errorf("second consecutive missing sample: %+v, want Terminated", h)
	}
	// Further reconciles with a still-null provider_id stay Terminated
	// (no flapping back to Unknown), for the same and for a newer refresh,
	// so remediation keeps seeing InstanceTerminated.
	if h := apply(missing); h.State != contract.HealthTerminated {
		t.Errorf("same refresh after Terminated: %+v, want Terminated", h)
	}
	a.obj.Status.LastRefresh = new(metav1.NewTime(time.Date(2026, 1, 1, 0, 7, 0, 0, time.UTC)))
	if h := apply(missing); h.State != contract.HealthTerminated {
		t.Errorf("newer refresh after Terminated: %+v, want Terminated", h)
	}

	// Recovered: Healthy again, and a later gap starts over at Unknown.
	if h := apply(present); h.State != contract.HealthRunning {
		t.Errorf("recovered: %+v", h)
	}
	a.obj.Status.LastRefresh = new(metav1.NewTime(time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)))
	if h := apply(missing); h.State != contract.HealthProviderIDMissing {
		t.Errorf("a new gap: %+v, want ProviderIDMissing", h)
	}
}

// TestProviderIDMissingSampleMarker proves the second-sample check reads
// the marker's stamp by value: a message with different wording around the
// same stamp is still the first sample, an older "(refresh ...)" marker is
// still read, and a condition without a marker never terminates.
func TestProviderIDMissingSampleMarker(t *testing.T) {
	t.Parallel()
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		msg  string
		want contract.HealthState
	}{
		{"same stamp, reworded", "reworded (sample 2026-01-01T00:00:00Z) text", contract.HealthProviderIDMissing},
		{"different stamp", "x (sample 2025-12-31T23:00:00Z) y", contract.HealthTerminated},
		{"previous marker form, different stamp", "x (refresh 2025-12-31T23:00:00Z) y", contract.HealthTerminated},
		{"previous marker form, same stamp", "x (refresh 2026-01-01T00:00:00Z) y", contract.HealthProviderIDMissing},
		{"no marker", "something else", contract.HealthProviderIDMissing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := newTestAdapter(t, testTM(func(tm *infrav1.TerraformMachine) {
				tm.Status.LastRefresh = new(metav1.NewTime(stamp))
			}), stateReader{})
			conditions.Set(a.obj, metav1.Condition{Type: infrav1.InfrastructureHealthyCondition, Status: metav1.ConditionUnknown,
				Reason: infrav1.ProviderIDMissingReason, Message: tt.msg})
			if h := a.providerIDMissing(); h.State != tt.want {
				t.Errorf("state = %v, want %v", h.State, tt.want)
			}
		})
	}
}

// TestApplyOutputsProviderIDAfterApply proves an unapplied state's
// provider_id is neither written nor judged: a failed first apply can leave
// a tainted instance X that the retry replaces with Y. Only a state written
// by a successful apply (an inputs hash) latches the ID, and a later
// different ID is then a ProviderIDChanged violation.
func TestApplyOutputsProviderIDAfterApply(t *testing.T) {
	t.Parallel()
	idState := func(hash, id string) *state.State {
		st := machineState(map[string]string{"provider_id": `"` + id + `"`, "health": healthy})
		st.InputsHash = hash
		return st
	}
	a := newTestAdapter(t, testTM(), stateReader{})
	rec := &recorder{}
	a.d.Recorder = rec
	ctx := t.Context()

	res, _, err := a.ApplyOutputs(ctx, shared.OwnerInfo{}, idState("", "i-x"), nil)
	if err != nil || !res.Valid() || a.obj.Spec.ProviderID != "" || len(rec.reasons) != 0 {
		t.Fatalf("unapplied state: res=%+v err=%v providerID=%q events=%v; want no write, no violation, no event",
			res, err, a.obj.Spec.ProviderID, rec.reasons)
	}
	res, _, err = a.ApplyOutputs(ctx, shared.OwnerInfo{}, idState("h1:m", "i-y"), nil)
	if err != nil || !res.Valid() || a.obj.Spec.ProviderID != "i-y" {
		t.Fatalf("applied state: res=%+v err=%v providerID=%q; want i-y written, valid", res, err, a.obj.Spec.ProviderID)
	}
	res, _, err = a.ApplyOutputs(ctx, shared.OwnerInfo{}, idState("h1:m", "i-z"), nil)
	if err != nil || res.Valid() || a.obj.Spec.ProviderID != "i-y" {
		t.Fatalf("changed ID: res=%+v err=%v providerID=%q; want a violation, i-y kept", res, err, a.obj.Spec.ProviderID)
	}
}
