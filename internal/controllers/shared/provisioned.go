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
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// Provisioned computes status.initialization.provisioned, derived from
// state and latched: once prev is true it stays true. Until then it needs
// hasInputsHash (an apply completed), outputsOK (valid outputs), and a
// health output that is present and not pending. It returns the new
// provisioned value.
func Provisioned(prev, hasInputsHash, outputsOK bool, health *contract.Health) bool {
	if prev {
		return true
	}
	return hasInputsHash && outputsOK && health != nil && health.State != contract.HealthPending
}
