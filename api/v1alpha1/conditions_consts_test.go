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

package v1alpha1

import (
	"os"
	"regexp"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// conditionStatuses lists, per type, the statuses that type may take. A
// status left out of a type's slice means the type never takes it.
var conditionStatuses = map[string][]metav1.ConditionStatus{
	ReadyCondition:                 {metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown},
	clusterv1.PausedCondition:      {metav1.ConditionTrue, metav1.ConditionFalse},
	DependenciesReadyCondition:     {metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown},
	IdentityAllowedCondition:       {metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown},
	CredentialsMirroredCondition:   {metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown},
	RunnerRBACReadyCondition:       {metav1.ConditionTrue, metav1.ConditionFalse},
	ApplyJobSucceededCondition:     {metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown},
	StateReadableCondition:         {metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown},
	RestoreJobSucceededCondition:   {metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown},
	OutputsValidCondition:          {metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown},
	InfrastructureHealthyCondition: {metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown},
	DriftJobSucceededCondition:     {metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown},
	DriftDetectedCondition:         {metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown},
	DeletionBlockedCondition:       {metav1.ConditionTrue, metav1.ConditionFalse},
	EndpointAvailableCondition:     {metav1.ConditionTrue, metav1.ConditionFalse},
	clusterv1.DeletingCondition:    {metav1.ConditionTrue, metav1.ConditionFalse},
	CapacityResolvedCondition:      {metav1.ConditionTrue, metav1.ConditionFalse},
	AutoscalingActiveCondition:     {metav1.ConditionTrue, metav1.ConditionFalse},
	PlanApprovedCondition:          {metav1.ConditionTrue, metav1.ConditionFalse},
	VariablesValidCondition:        {metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown},
	InputsAppliedCondition:         {metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown},
	ReconcilingCondition:           {metav1.ConditionTrue, metav1.ConditionFalse},
}

// TestEveryTypeHasReasonsForEachStatus proves ConditionReasons has an
// entry for every condition type in conditionStatuses, with a non-empty
// reason for each status that type is allowed to take and no reasons for a
// status it is not.
func TestEveryTypeHasReasonsForEachStatus(t *testing.T) {
	t.Parallel()
	table := ConditionReasons()
	if len(table) != len(conditionStatuses) {
		t.Errorf("ConditionReasons has %d types, the table has %d", len(table), len(conditionStatuses))
	}
	for typ, statuses := range conditionStatuses {
		byStatus, ok := table[typ]
		if !ok {
			t.Errorf("%s: missing from ConditionReasons", typ)
			continue
		}
		for _, st := range []metav1.ConditionStatus{metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown} {
			allowed := slices.Contains(statuses, st)
			reasons := byStatus[st]
			if allowed && len(reasons) == 0 {
				t.Errorf("%s=%s: no reason", typ, st)
			}
			if !allowed && len(reasons) != 0 {
				t.Errorf("%s=%s: status not allowed but has reasons %v", typ, st, reasons)
			}
			for _, r := range reasons {
				if r == "" {
					t.Errorf("%s=%s: empty reason", typ, st)
				}
			}
		}
	}
}

// TestReadyInputs proves each of the three ReadyInputs slices always
// includes Deleting and InfrastructureHealthy, never includes a type from
// NeverInReady, and only names types that have entries in
// ConditionReasons; it also proves ApplyJobSucceeded is a before-, but not
// an after-provisioned cluster input, and that the cluster and
// machine after-provisioned slices are identical.
func TestReadyInputs(t *testing.T) {
	t.Parallel()
	table := ConditionReasons()
	all := map[string][]string{
		"ReadyInputsBeforeProvisioned":       ReadyInputsBeforeProvisioned,
		"ReadyInputsClusterAfterProvisioned": ReadyInputsClusterAfterProvisioned,
		"ReadyInputsMachineAfterProvisioned": ReadyInputsMachineAfterProvisioned,
	}
	for name, inputs := range all {
		if !slices.Contains(inputs, clusterv1.DeletingCondition) {
			t.Errorf("%s: Deleting missing", name)
		}
		if !slices.Contains(inputs, InfrastructureHealthyCondition) {
			t.Errorf("%s: InfrastructureHealthy missing", name)
		}
		for _, never := range NeverInReady {
			if slices.Contains(inputs, never) {
				t.Errorf("%s: contains %s, which is never a Ready input", name, never)
			}
		}
		for _, in := range inputs {
			if _, ok := table[in]; !ok {
				t.Errorf("%s: %s has no reasons in ConditionReasons", name, in)
			}
		}
	}
	// The cluster drops ApplyJobSucceeded after provisioning.
	if !slices.Contains(ReadyInputsBeforeProvisioned, ApplyJobSucceededCondition) {
		t.Errorf("ApplyJobSucceeded must be a Ready input before provisioning")
	}
	if slices.Contains(ReadyInputsClusterAfterProvisioned, ApplyJobSucceededCondition) {
		t.Errorf("ApplyJobSucceeded must not be a cluster Ready input after provisioning")
	}
	// Identical today; a divergence must be deliberate.
	if !slices.Equal(ReadyInputsClusterAfterProvisioned, ReadyInputsMachineAfterProvisioned) {
		t.Errorf("cluster and machine after-provisioned inputs diverged: %v vs %v",
			ReadyInputsClusterAfterProvisioned, ReadyInputsMachineAfterProvisioned)
	}
	if !slices.Equal(NegativePolarityConditions, []string{clusterv1.DeletingCondition}) {
		t.Errorf("NegativePolarityConditions = %v, want [Deleting]", NegativePolarityConditions)
	}
	for _, c := range SetOnFirstVisit {
		if _, ok := table[c]; !ok {
			t.Errorf("SetOnFirstVisit: %s has no reasons", c)
		}
	}
}

// TestDestructivePlanBlockedReason: a blocked destructive apply is an
// ApplyJobSucceeded=False reason, and only that.
func TestDestructivePlanBlockedReason(t *testing.T) {
	t.Parallel()
	for typ, byStatus := range ConditionReasons() {
		for st, reasons := range byStatus {
			in := slices.Contains(reasons, DestructivePlanBlockedReason)
			want := typ == ApplyJobSucceededCondition && st == metav1.ConditionFalse
			if in != want {
				t.Errorf("%s=%s lists DestructivePlanBlocked: %v, want %v", typ, st, in, want)
			}
		}
	}
}

// TestEveryReasonConstantIsInTheTable guards against drift: a reason constant
// declared in conditions_consts.go but missing from ConditionReasons (or the
// reverse) fails.
func TestEveryReasonConstantIsInTheTable(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("conditions_consts.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s+\w+Reason = "(\w+)"`).FindAllSubmatch(src, -1) {
		declared[string(m[1])] = true
	}
	inTable := map[string]bool{}
	for _, byStatus := range ConditionReasons() {
		for _, reasons := range byStatus {
			for _, r := range reasons {
				inTable[r] = true
			}
		}
	}
	for r := range declared {
		if !inTable[r] {
			t.Errorf("reason %q is declared but not in ConditionReasons", r)
		}
	}
	for r := range inTable {
		if !declared[r] && !slices.Contains([]string{
			clusterv1.PausedReason, clusterv1.NotPausedReason, clusterv1.DeletingReason,
			clusterv1.NotDeletingReason,
		}, r) {
			t.Errorf("reason %q is in ConditionReasons but not declared here or in clusterv1", r)
		}
	}
	if len(declared) < 60 {
		t.Errorf("only %d reasons declared, want at least 60", len(declared))
	}
}

// TestReasonsBelongToOneType guards against a reason string silently shared
// by two condition types. DriftNotChecked is shared by the drift types,
// SecretNotFound and CredentialsIncomplete by IdentityAllowed and the
// identity's own Ready, the lease and Job-slot waits by the Job conditions
// (the op that waits: the restore condition shares them with
// ApplyJobSucceeded), as is ImagePullFailed (the op whose module image
// cannot be pulled), and a TerraformPlan's Pending and Approved by its
// Ready and Approved conditions.
func TestReasonsBelongToOneType(t *testing.T) {
	t.Parallel()
	shared := map[string]bool{
		DriftNotCheckedReason: true, SecretNotFoundReason: true, CredentialsIncompleteReason: true, WaitingForRunLeaseReason: true,
		WaitingForClusterOperationReason: true, WaitingForMachineOperationsReason: true, WaitingForJobSlotReason: true,
		PlanPendingReason: true, PlanApprovedReason: true, ImagePullFailedReason: true,
	}
	types := map[string]map[string]bool{}
	for typ, byStatus := range ConditionReasons() {
		for _, reasons := range byStatus {
			for _, r := range reasons {
				if types[r] == nil {
					types[r] = map[string]bool{}
				}
				types[r][typ] = true
			}
		}
	}
	for r, ts := range types {
		if len(ts) > 1 && !shared[r] {
			t.Errorf("reason %q is used by %d condition types: %v", r, len(ts), ts)
		}
	}
}

// TestConditionReasonsReturnsACopy proves mutating the map ConditionReasons
// returns does not affect a later call's result.
func TestConditionReasonsReturnsACopy(t *testing.T) {
	t.Parallel()
	a := ConditionReasons()
	a[ReadyCondition][metav1.ConditionTrue] = append(a[ReadyCondition][metav1.ConditionTrue], "Mutated")
	if slices.Contains(ConditionReasons()[ReadyCondition][metav1.ConditionTrue], "Mutated") {
		t.Errorf("ConditionReasons returned shared state")
	}
}
