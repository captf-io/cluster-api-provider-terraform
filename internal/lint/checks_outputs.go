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

package lint

import (
	"slices"
	"strings"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// outputRequired checks that every c contract output is declared in m.
// health and the pool's provider_id_list have their own checks. It
// returns an IDOutputRequired finding for each missing output.
func (c contractChecks) outputRequired(m *Module) []Finding {
	var out []Finding
	for _, name := range slices.Sorted(slices.Values(c.outputs)) {
		if _, ok := m.Config.Outputs[name]; ok || name == healthOutput || name == providerIDList {
			continue
		}
		out = append(out, Finding{ID: IDOutputRequired, Severity: SeverityError,
			Message: "contract output " + name + " is not declared"})
	}
	return out
}

// outputHealth checks that m declares the common health output, and
// returns the IDOutputHealth finding when it does not.
func (c contractChecks) outputHealth(m *Module) []Finding {
	if _, ok := m.Config.Outputs[healthOutput]; ok {
		return nil
	}
	return []Finding{{ID: IDOutputHealth, Severity: SeverityError,
		Message: "output " + healthOutput + " is not declared: every module must report its health"}}
}

// outputReserved checks m's outputs: the captf_ prefix is reserved for
// future contract outputs. It returns the IDOutputReserved finding for
// each output using that prefix.
func (c contractChecks) outputReserved(m *Module) []Finding {
	var out []Finding
	for name, o := range m.Config.Outputs {
		if !strings.HasPrefix(name, contract.ReservedPrefix) {
			continue
		}
		file, line := m.at(o.Pos)
		out = append(out, Finding{ID: IDOutputReserved, Severity: SeverityWarning, File: file, Line: line,
			Message: "output " + name + " uses the " + contract.ReservedPrefix + " prefix, which is reserved for the contract"})
	}
	return out
}

// outputProviderIDs is the declaration half of
// output/provider-id-list-shape, checked against m: a pool module declares
// provider_id_list. The expression half is the hcl/v2 pass's
// (outputProviderIDShape). outputRequired skips provider_id_list because
// this check covers it instead, with its own message. Checks lists it for
// the machinepool role only. It returns the IDOutputProviderIDs finding
// when the output is not declared, else nil.
func outputProviderIDs(m *Module) []Finding {
	if _, ok := m.Config.Outputs[providerIDList]; ok {
		return nil
	}
	return []Finding{{ID: IDOutputProviderIDs, Severity: SeverityError,
		Message: "output " + providerIDList + " is not declared: a pool reports every non-terminated member"}}
}

// moduleVersion checks that m declares required_version. Like every check
// in this package, it states the requirement and the finding is its
// absence. It returns the IDModuleVersion finding when
// required_version is not declared, else nil.
func moduleVersion(m *Module) []Finding {
	if len(m.Config.RequiredCore) > 0 {
		return nil
	}
	return []Finding{{ID: IDModuleVersion, Severity: SeverityInfo,
		Message: "no required_version: declare the runtime versions the module was tested with"}}
}
