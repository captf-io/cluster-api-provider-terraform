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
	"fmt"
	"slices"

	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// Kinds with a Ready condition, as their GVK Kind (the form internal/state
// and internal/inputs use).
const (
	KindCluster     = "TerraformCluster"
	KindMachine     = "TerraformMachine"
	KindMachinePool = "TerraformMachinePool"
)

// Phase selects the Ready inputs: before or after
// status.initialization.provisioned first holds (it is latched).
type Phase int

const (
	// BeforeProvisioned is every reconcile until provisioned first holds.
	BeforeProvisioned Phase = iota
	// AfterProvisioned is every reconcile after that.
	AfterProvisioned
)

// ReadyInputs returns the conditions summarized into Ready for kind in
// phase, or nil for a kind without Ready. The slice is a copy.
func ReadyInputs(kind string, phase Phase) []string {
	var after []string
	switch kind {
	case KindCluster:
		after = infrav1.ReadyInputsClusterAfterProvisioned
	case KindMachine:
		after = infrav1.ReadyInputsMachineAfterProvisioned
	case KindMachinePool:
		after = infrav1.ReadyInputsMachinePoolAfterProvisioned
	default:
		return nil
	}
	if phase == BeforeProvisioned {
		return slices.Clone(infrav1.ReadyInputsBeforeProvisioned)
	}
	return slices.Clone(after)
}

// SetReady summarizes obj's ReadyInputs into its Ready condition:
//
//   - Deleting is negative polarity (True is the issue), passed to the merge
//     strategy's priority function: a CustomMergeStrategy replaces the
//     default strategy, which is the only place SetSummaryCondition applies
//     NegativePolarityConditionTypes itself;
//   - only the first-visit input types (Deleting) are ignored when missing.
//     Any other missing input counts as Unknown, so an object that has not
//     reported yet is Ready=Unknown, never True;
//   - the reason is Ready, NotReady or ReadyUnknown, as in CAPD. The input's
//     own reason (Provisioning, Deleting, …) is in the message.
//
// kind selects obj's ReadyInputs (see ReadyInputs) and phase selects
// whether they are the before- or after-provisioned set. It returns an
// error if kind has no Ready condition or the summary condition could not
// be set.
func SetReady(obj conditions.Setter, kind string, phase Phase) error {
	inputs := ReadyInputs(kind, phase)
	if inputs == nil {
		return fmt.Errorf("conditions: kind %q has no Ready condition", kind)
	}
	var ignore []string
	for _, t := range infrav1.SetOnFirstVisit {
		if slices.Contains(inputs, t) {
			ignore = append(ignore, t)
		}
	}
	if err := conditions.SetSummaryCondition(obj, obj, infrav1.ReadyCondition,
		conditions.ForConditionTypes(inputs),
		conditions.NegativePolarityConditionTypes(infrav1.NegativePolarityConditions),
		conditions.IgnoreTypesIfMissing(ignore),
		conditions.CustomMergeStrategy{
			MergeStrategy: conditions.DefaultMergeStrategy(
				conditions.GetPriorityFunc(conditions.GetDefaultMergePriorityFunc(infrav1.NegativePolarityConditions...)),
				conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
					infrav1.NotReadyReason,
					infrav1.ReadyUnknownReason,
					infrav1.ReadyReason,
				)),
			),
		},
	); err != nil {
		return fmt.Errorf("conditions: set %s: %w", infrav1.ReadyCondition, err)
	}
	return nil
}
