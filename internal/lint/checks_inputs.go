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

package lint

import (
	"maps"
	"slices"
	"strings"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// Check IDs.
const (
	// IDInputRequired: a contract input is not declared as a variable.
	IDInputRequired = "input/required"
	// IDInputUserVariableDefault: a variable outside the contract (a user
	// variable) has no default, so an object that does not set it in
	// spec.variables or variablesFrom fails to apply.
	IDInputUserVariableDefault = "input/user-variable-default"
	// IDInputType: a contract input's declared type does not accept what
	// the generated root passes, is missing, or could not be read.
	IDInputType = "input/type"
	// IDInputSensitive: bootstrap_data is declared but not
	// sensitive = true, though it carries the bootstrap payload.
	IDInputSensitive = "input/sensitive"
	// IDInputDefault: a contract input the controller always sets to a
	// non-null value nonetheless carries a default, which would mask a
	// controller mistake.
	IDInputDefault = "input/default"
	// IDInputReserved: a variable uses the reserved captf_ prefix but is
	// not itself a contract input.
	IDInputReserved = "input/reserved"
	// IDInputTagsDeclared: the module does not declare captf_tags, the
	// mandatory common input every module must accept.
	IDInputTagsDeclared = "input/tags-declared"
	// IDOutputRequired: a contract output is not declared.
	IDOutputRequired = "output/required"
	// IDOutputReserved: an output uses a name reserved for a future
	// contract output.
	IDOutputReserved = "output/reserved"
	// IDOutputHealth: the health output is declared with the wrong shape
	// for the contract's health check.
	IDOutputHealth = "output/health"
	// IDOutputProviderIDs: the machinepool role's provider_id_list output
	// is missing, or its expression does not look like it forwards one ID
	// per instance.
	IDOutputProviderIDs = "output/provider-id-list-shape"
	// IDModuleVersion: informational; reports the module's declared
	// required_version constraint, or its absence.
	IDModuleVersion = "module/version"
)

// Inputs and outputs with a check of their own; the generic required checks
// skip them, so one defect is one finding.
const (
	tagsInput      = "captf_tags"
	bootstrapInput = "bootstrap_data"
	healthOutput   = "health"
	providerIDList = "provider_id_list"
)

// contractChecks are the checks against one role's contract.
type contractChecks struct {
	inputs  map[string]contract.InputSpec
	outputs []string
}

// sortedInputs returns c's contract input names, sorted, so checks that
// walk them report findings in a stable order.
func (c contractChecks) sortedInputs() []string {
	return slices.Sorted(maps.Keys(c.inputs))
}

// inputRequired checks that every c contract input is declared in m
// (captf_tags has its own check). It returns an IDInputRequired finding
// for each missing input.
func (c contractChecks) inputRequired(m *Module) []Finding {
	var out []Finding
	for _, name := range c.sortedInputs() {
		if _, ok := m.Config.Variables[name]; !ok && name != tagsInput {
			out = append(out, Finding{ID: IDInputRequired, Severity: SeverityError,
				Message: "contract input " + name + " is not declared as a variable"})
		}
	}
	return out
}

// inputTagsDeclared checks that m declares captf_tags, the mandatory
// common input, and returns the IDInputTagsDeclared finding when it does
// not.
func (c contractChecks) inputTagsDeclared(m *Module) []Finding {
	if _, ok := m.Config.Variables[tagsInput]; ok {
		return nil
	}
	return []Finding{{ID: IDInputTagsDeclared, Severity: SeverityError,
		Message: "variable " + tagsInput + " is not declared: every module must accept the contract's tags"}}
}

// inputUserVariableDefault checks each of m's variables outside c's
// contract: such a user variable is set only when the object's
// spec.variables or variablesFrom names it, so it should have a default,
// letting the module plan for an object that sets none; without one, such
// an object fails its apply on the missing argument. A warning, not an
// error: a module may deliberately require a user variable (captf_ names
// are input/reserved's). It returns the IDInputUserVariableDefault
// finding for each such variable without a default.
func (c contractChecks) inputUserVariableDefault(m *Module) []Finding {
	var out []Finding
	for name, v := range m.Config.Variables {
		if _, ok := c.inputs[name]; ok || strings.HasPrefix(name, contract.ReservedPrefix) || !v.Required {
			continue
		}
		file, line := m.at(v.Pos)
		out = append(out, Finding{ID: IDInputUserVariableDefault, Severity: SeverityWarning, File: file, Line: line,
			Message: "variable " + name + " is not a contract input and has no default: an object that does not set it in spec.variables or variablesFrom fails to apply"})
	}
	return out
}

// clusterOutputsInput may carry default = null in every role: the
// contract's skeletons declare it that way, the cluster role never
// receives it, and a machine receiving null reads it the same as the
// default.
const clusterOutputsInput = "captf_cluster_outputs"

// inputReserved checks m's variables: the captf_ prefix belongs to the
// contract. captf_cluster_outputs with a default is allowed in roles that
// do not receive it. It returns the IDInputReserved finding for each
// other reserved-prefixed variable that is not a contract input in c.
func (c contractChecks) inputReserved(m *Module) []Finding {
	var out []Finding
	for name, v := range m.Config.Variables {
		if _, ok := c.inputs[name]; ok || !strings.HasPrefix(name, contract.ReservedPrefix) {
			continue
		}
		if name == clusterOutputsInput && !v.Required {
			continue
		}
		file, line := m.at(v.Pos)
		out = append(out, Finding{ID: IDInputReserved, Severity: SeverityError, File: file, Line: line,
			Message: "variable " + name + " uses the reserved " + contract.ReservedPrefix + " prefix but is not a contract input"})
	}
	return out
}

// inputType checks that each of c's contract inputs, as declared in m,
// has a type accepting what the generated root passes. No type is a
// warning, so is a type the matcher cannot read. It returns the
// IDInputType finding for each mismatch, missing or unreadable type.
func (c contractChecks) inputType(m *Module) []Finding {
	var out []Finding
	for _, name := range c.sortedInputs() {
		v, ok := m.Config.Variables[name]
		if !ok {
			continue
		}
		file, line := m.at(v.Pos)
		want := c.inputs[name].Type
		f := Finding{ID: IDInputType, File: file, Line: line}
		switch declared, err := parseType(v.Type); {
		case strings.TrimSpace(v.Type) == "":
			f.Severity, f.Message = SeverityWarning, "variable "+name+" has no type; the contract passes "+want
		case err != nil:
			f.Severity, f.Message = SeverityWarning, "variable "+name+": cannot check type "+v.Type+" against "+want+": "+err.Error()
		default:
			contractType, err := parseType(want)
			if err == nil && compatible(declared, contractType) {
				continue
			}
			f.Severity, f.Message = SeverityError, "variable "+name+" is declared as "+v.Type+", which does not accept the contract's "+want
		}
		out = append(out, f)
	}
	return out
}

// inputSensitive checks that m declares bootstrap_data (base64 of the
// bootstrap payload) sensitive, when c's contract carries that input. It
// returns the IDInputSensitive finding when the variable is declared but
// not sensitive.
func (c contractChecks) inputSensitive(m *Module) []Finding {
	v, ok := m.Config.Variables[bootstrapInput]
	if _, contracted := c.inputs[bootstrapInput]; !ok || !contracted || v.Sensitive {
		return nil
	}
	file, line := m.at(v.Pos)
	return []Finding{{ID: IDInputSensitive, Severity: SeverityWarning, File: file, Line: line,
		Message: "variable " + bootstrapInput + " should be sensitive = true: it carries the bootstrap payload"}}
}

// inputDefault checks m's contract inputs for one the root always sets to
// a non-null value that nonetheless carries a default, which would mask a
// controller mistake. Nullable inputs (per c) and captf_cluster_outputs
// may default (to null). It returns the IDInputDefault finding for each
// other input with a default.
func (c contractChecks) inputDefault(m *Module) []Finding {
	var out []Finding
	for _, name := range c.sortedInputs() {
		v, ok := m.Config.Variables[name]
		if !ok || v.Required || c.inputs[name].Nullable || name == clusterOutputsInput {
			continue
		}
		file, line := m.at(v.Pos)
		out = append(out, Finding{ID: IDInputDefault, Severity: SeverityWarning, File: file, Line: line,
			Message: "contract input " + name + " has a default; the controller always sets it, so a default only hides a missing value"})
	}
	return out
}
