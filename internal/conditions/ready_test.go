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

package conditions

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// The Ready condition inputs for each kind and phase, restated literally
// so a change to infrav1's slices breaks this test.
var (
	beforeProvisioned = []string{
		"DependenciesReady", "IdentityAllowed", "CredentialsMirrored", "RunnerRBACReady",
		"ApplyJobSucceeded", "StateReadable", "OutputsValid", "InfrastructureHealthy", "Deleting",
	}
	afterProvisionedCluster = []string{"InfrastructureHealthy", "Deleting"}
	afterProvisionedMachine = []string{"InfrastructureHealthy", "Deleting"}
	afterProvisionedPool    = []string{"InfrastructureHealthy", "ApplyJobSucceeded", "Deleting"}
)

// TestReadyInputs proves ReadyInputs returns the expected condition list
// for each kind and phase, that it hands back a copy the
// caller cannot corrupt, and that the never-in-Ready condition types never
// appear in any kind or phase.
func TestReadyInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind  string
		phase Phase
		want  []string
	}{
		{KindCluster, BeforeProvisioned, beforeProvisioned},
		{KindMachine, BeforeProvisioned, beforeProvisioned},
		{KindCluster, AfterProvisioned, afterProvisionedCluster},
		{KindMachine, AfterProvisioned, afterProvisionedMachine},
		{KindMachinePool, BeforeProvisioned, beforeProvisioned},
		{KindMachinePool, AfterProvisioned, afterProvisionedPool},
		{"TerraformMachineTemplate", BeforeProvisioned, nil},
		{"TerraformMachinePoolTemplate", AfterProvisioned, nil},
	}
	for _, tt := range tests {
		got := ReadyInputs(tt.kind, tt.phase)
		if !slices.Equal(got, tt.want) {
			t.Errorf("ReadyInputs(%s, %d) = %v, want %v", tt.kind, tt.phase, got, tt.want)
		}
	}
	// A copy: callers cannot corrupt the table.
	ReadyInputs(KindMachine, AfterProvisioned)[0] = "X"
	if ReadyInputs(KindMachine, AfterProvisioned)[0] != "InfrastructureHealthy" {
		t.Error("ReadyInputs returned the shared slice")
	}
	// Never in Ready, whatever the phase.
	for _, never := range []string{"Paused", "DriftDetected", "DriftJobSucceeded", "DeletionBlocked", "EndpointAvailable"} {
		for _, k := range []string{KindCluster, KindMachine, KindMachinePool} {
			for _, p := range []Phase{BeforeProvisioned, AfterProvisioned} {
				if slices.Contains(ReadyInputs(k, p), never) {
					t.Errorf("%s is a Ready input of %s in phase %d", never, k, p)
				}
			}
		}
	}
}

// conds is a shorthand: condition type → status. Reasons are filled with a
// valid reason for the status from infrav1.ConditionReasons.
type conds map[string]metav1.ConditionStatus

// object builds a TerraformMachine carrying c's conditions, each with a
// valid reason for its status drawn from infrav1.ConditionReasons; it
// returns the built machine.
func object(c conds) *infrav1.TerraformMachine {
	table := infrav1.ConditionReasons()
	obj := &infrav1.TerraformMachine{}
	for _, typ := range slices.Sorted(maps.Keys(c)) {
		status := c[typ]
		reason := table[typ][status][0]
		conditions.Set(obj, metav1.Condition{Type: typ, Status: status, Reason: reason})
	}
	return obj
}

// allGood returns the healthy (True, except Deleting=False) value of every
// condition type in inputs.
func allGood(inputs []string) conds {
	c := conds{}
	for _, t := range inputs {
		c[t] = metav1.ConditionTrue
	}
	c["Deleting"] = metav1.ConditionFalse
	return c
}

// with returns a clone of base with overrides applied: each key set to its
// override value, or deleted when the override is missing (the empty
// status).
func with(base conds, overrides conds) conds {
	out := maps.Clone(base)
	for k, v := range overrides {
		if v == "" {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

// missing is the sentinel status passed to with to delete a condition type
// instead of setting it.
const missing metav1.ConditionStatus = ""

// TestSetReady proves SetReady summarizes each Ready input combination,
// before and after provisioning, into the correct Ready status and reason
// for both TerraformCluster and TerraformMachine, including the
// cluster-only inputs that must never affect Ready.
func TestSetReady(t *testing.T) {
	t.Parallel()
	const (
		T = metav1.ConditionTrue
		F = metav1.ConditionFalse
		U = metav1.ConditionUnknown
	)
	before := allGood(beforeProvisioned)
	after := allGood(afterProvisionedMachine)
	type tc struct {
		name   string
		phase  Phase
		in     conds
		status metav1.ConditionStatus
		reason string
	}
	tests := []tc{
		// Before provisioning.
		{"before: all inputs good", BeforeProvisioned, before, T, "Ready"},
		{"before: fresh object, nothing reported", BeforeProvisioned, conds{}, U, "ReadyUnknown"},
		{"before: only first-visit conditions", BeforeProvisioned, conds{"Paused": F, "Deleting": F, "DriftDetected": F}, U, "ReadyUnknown"},
		{"before: Deleting=False is not an issue (negative polarity)", BeforeProvisioned, before, T, "Ready"},
		{"before: Deleting missing is ignored", BeforeProvisioned, with(before, conds{"Deleting": missing}), T, "Ready"},
		{"before: Deleting=True", BeforeProvisioned, with(before, conds{"Deleting": T}), F, "NotReady"},
		{"before: waiting on dependencies", BeforeProvisioned, with(before, conds{"DependenciesReady": U, "InfrastructureHealthy": U}), U, "ReadyUnknown"},
		{"before: apply started (Provisioning)", BeforeProvisioned, with(before, conds{"InfrastructureHealthy": F}), F, "NotReady"},
		{"before: identity not allowed", BeforeProvisioned, with(before, conds{"IdentityAllowed": F}), F, "NotReady"},
		{"before: mirror failed", BeforeProvisioned, with(before, conds{"CredentialsMirrored": F}), F, "NotReady"},
		{"before: mirror pending", BeforeProvisioned, with(before, conds{"CredentialsMirrored": U}), U, "ReadyUnknown"},
		{"before: RBAC failed", BeforeProvisioned, with(before, conds{"RunnerRBACReady": F}), F, "NotReady"},
		{"before: apply failed", BeforeProvisioned, with(before, conds{"ApplyJobSucceeded": F}), F, "NotReady"},
		{"before: state not found", BeforeProvisioned, with(before, conds{"StateReadable": U}), U, "ReadyUnknown"},
		{"before: outputs invalid", BeforeProvisioned, with(before, conds{"OutputsValid": F}), F, "NotReady"},
		{"before: a missing positive input is Unknown", BeforeProvisioned, with(before, conds{"OutputsValid": missing}), U, "ReadyUnknown"},
		{"before: False wins over Unknown", BeforeProvisioned, with(before, conds{"StateReadable": U, "ApplyJobSucceeded": F}), F, "NotReady"},
		{"before: Paused=True does not affect Ready", BeforeProvisioned, with(before, conds{"Paused": T}), T, "Ready"},
		{"before: DriftDetected=True does not affect Ready", BeforeProvisioned, with(before, conds{"DriftDetected": T}), T, "Ready"},
		{"before: DriftJobSucceeded=False does not affect Ready", BeforeProvisioned, with(before, conds{"DriftJobSucceeded": F}), T, "Ready"},
		// After provisioning.
		{"after: healthy", AfterProvisioned, after, T, "Ready"},
		{"after: ApplyJobSucceeded=False does not affect Ready", AfterProvisioned, with(after, conds{"ApplyJobSucceeded": F}), T, "Ready"},
		{"after: identity not allowed does not affect Ready", AfterProvisioned, with(after, conds{"IdentityAllowed": F, "CredentialsMirrored": F, "RunnerRBACReady": F}), T, "Ready"},
		{"after: state or outputs problems do not affect Ready", AfterProvisioned, with(after, conds{"StateReadable": F, "OutputsValid": F}), T, "Ready"},
		{"after: DriftDetected=True does not affect Ready", AfterProvisioned, with(after, conds{"DriftDetected": T}), T, "Ready"},
		{"after: DriftJobSucceeded=False does not affect Ready", AfterProvisioned, with(after, conds{"DriftJobSucceeded": F}), T, "Ready"},
		{"after: Paused=True does not affect Ready", AfterProvisioned, with(after, conds{"Paused": T}), T, "Ready"},
		{"after: unhealthy", AfterProvisioned, with(after, conds{"InfrastructureHealthy": F}), F, "NotReady"},
		{"after: health unknown", AfterProvisioned, with(after, conds{"InfrastructureHealthy": U}), U, "ReadyUnknown"},
		{"after: health not reported", AfterProvisioned, with(after, conds{"InfrastructureHealthy": missing}), U, "ReadyUnknown"},
		{"after: Deleting=True", AfterProvisioned, with(after, conds{"Deleting": T}), F, "NotReady"},
	}
	clusterOnly := []tc{
		{"after: DeletionBlocked=True does not affect Ready", AfterProvisioned, with(after, conds{"DeletionBlocked": T}), T, "Ready"},
		{"after: EndpointAvailable=False does not affect Ready", AfterProvisioned, with(after, conds{"EndpointAvailable": F}), T, "Ready"},
	}
	run := func(kind string, cases []tc) {
		for _, tt := range cases {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				obj := object(tt.in)
				if err := SetReady(obj, kind, tt.phase); err != nil {
					t.Fatalf("SetReady: %v", err)
				}
				got := conditions.Get(obj, "Ready")
				if got == nil || got.Status != tt.status || got.Reason != tt.reason {
					t.Errorf("Ready = %+v, want %s/%s", got, tt.status, tt.reason)
				}
			})
		}
	}
	run(KindCluster, append(slices.Clone(tests), clusterOnly...))
	run(KindMachine, tests)
}

// TestSetReadyMessage: Ready's reason is one of three, and the input's own
// reason (for example Provisioning) is carried in the message.
func TestSetReadyMessage(t *testing.T) {
	t.Parallel()
	obj := object(allGood(beforeProvisioned))
	SetInfrastructureHealthy(obj, nil, HealthApplyStarted)
	if err := SetReady(obj, KindMachine, BeforeProvisioned); err != nil {
		t.Fatal(err)
	}
	got := conditions.Get(obj, "Ready")
	if got.Reason != "NotReady" || !strings.Contains(got.Message, "InfrastructureHealthy") || !strings.Contains(got.Message, "Provisioning") {
		t.Errorf("Ready = %+v", got)
	}
}

// TestSetReadyUnknownKind proves SetReady returns an error for a kind that
// has no Ready condition.
func TestSetReadyUnknownKind(t *testing.T) {
	t.Parallel()
	if err := SetReady(&infrav1.TerraformMachine{}, "TerraformMachineTemplate", BeforeProvisioned); err == nil {
		t.Error("SetReady accepted a kind without Ready")
	}
}

// pascalCase matches a PascalCase identifier: an upper-case letter followed
// by letters and digits.
var pascalCase = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)

// TestReasonsArePascalCase complements api/v1alpha1's table tests (every
// type has a reason for each status it takes).
func TestReasonsArePascalCase(t *testing.T) {
	t.Parallel()
	for typ, byStatus := range infrav1.ConditionReasons() {
		if !pascalCase.MatchString(typ) {
			t.Errorf("type %q is not PascalCase", typ)
		}
		for status, reasons := range byStatus {
			if len(reasons) == 0 {
				t.Errorf("%s/%s has no reason", typ, status)
			}
			for _, r := range reasons {
				if !pascalCase.MatchString(r) {
					t.Errorf("%s/%s reason %q is not PascalCase", typ, status, r)
				}
			}
		}
	}
}
