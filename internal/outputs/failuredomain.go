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

package outputs

import (
	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// CheckPlacement compares the failure domain a Machine requested with the
// one the module reports. The InfraMachine MUST be placed in the requested
// domain, so a non-null request with a different or null actual value is a
// FailureDomainMismatch. With no request any placement is fine. The
// controller appends the result to Result.Violations: decoding cannot see
// the Machine.
func CheckPlacement(requested, actual *string) *Violation {
	if requested == nil {
		return nil
	}
	if actual != nil && *actual == *requested {
		return nil
	}
	return &Violation{
		Output:  "failure_domain",
		Reason:  infrav1.FailureDomainMismatchReason,
		Message: "failure_domain: the machine was not placed in the failure domain its Machine requested",
	}
}

// ProviderIDChanged reports whether next, a provider_id output, differs
// from prev, the one already written to spec.providerID, which is
// immutable once set.
func ProviderIDChanged(prev, next string) bool {
	return prev != "" && next != "" && prev != next
}

// ProviderIDChangedViolation returns the violation the controller appends
// when ProviderIDChanged is true.
func ProviderIDChangedViolation() Violation {
	return Violation{
		Output:  "provider_id",
		Reason:  infrav1.ProviderIDChangedReason,
		Message: "provider_id: differs from the providerID already set, which cannot change",
	}
}
