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
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// Check pairs a check ID with the function that runs it. Severity is its
// usual severity; a finding's own severity is authoritative (input/type
// reports a missing type as a warning, a mismatch as an error).
type Check struct {
	ID       string
	Severity Severity
	Run      func(*Module) []Finding
}

// Checks returns the checks of role. An unknown role is an error, which
// tfcapi-lint turns into a usage exit.
func Checks(role contract.Role) ([]Check, error) {
	inputs, err := contract.InputSpecs(role)
	if err != nil {
		return nil, err
	}
	outputs, err := contract.RequiredOutputs(role)
	if err != nil {
		return nil, err
	}
	c := contractChecks{inputs: inputs, outputs: outputs}
	checks := []Check{
		{IDInputRequired, SeverityError, c.inputRequired},
		{IDInputUserVariableDefault, SeverityWarning, c.inputUserVariableDefault},
		{IDInputType, SeverityError, c.inputType},
		{IDInputSensitive, SeverityWarning, c.inputSensitive},
		{IDInputDefault, SeverityWarning, c.inputDefault},
		{IDInputReserved, SeverityError, c.inputReserved},
		{IDInputTagsDeclared, SeverityError, c.inputTagsDeclared},
		{IDOutputRequired, SeverityError, c.outputRequired},
		{IDOutputReserved, SeverityWarning, c.outputReserved},
		{IDOutputHealth, SeverityError, c.outputHealth},
		{IDModuleVersion, SeverityInfo, moduleVersion},
		// The hcl/v2 pass. backend, cloud and provider-config also run
		// over every nested local module: defense in depth against one
		// hiding a backend, a cloud block or a credential literal.
		// tofu-shadow and the contract checks above
		// are root-only: a nested module is not itself required to
		// declare the contract's inputs and outputs.
		{IDModuleBackend, SeverityError, nested(moduleBackend)},
		{IDModuleSourceEscape, SeverityError, nested(moduleSourceEscape)},
		{IDModuleCloud, SeverityError, nested(moduleCloud)},
		{IDModuleProviderConfig, SeverityWarning, nested(moduleProviderConfig)},
		{IDModuleTofuShadow, SeverityWarning, moduleTofuShadow},
		{IDInputTagsUnused, SeverityWarning, inputTagsUnused},
	}
	if role == contract.RoleCluster {
		checks = append(checks, Check{IDOutputEndpointNeverSet, SeverityWarning, outputEndpointNeverSet})
	}
	if role == contract.RoleMachinePool {
		// outputProviderIDs is the declaration half of
		// output/provider-id-list-shape (an error: the output is
		// required); outputProviderIDShape is the expression half,
		// best-effort against native syntax only, so it can only warn.
		// Both share the ID: they are the same requirement checked two
		// ways.
		checks = append(checks,
			Check{IDOutputProviderIDs, SeverityError, outputProviderIDs},
			Check{IDOutputProviderIDs, SeverityWarning, outputProviderIDShape},
			Check{IDPoolAutoscaling, SeverityWarning, poolAutoscaling},
		)
	}
	return checks, nil
}

// Lint runs every check of role on m and returns the combined, ordered
// Report, or an error when role has no checks (Checks's error).
func Lint(m *Module, role contract.Role) (Report, error) {
	checks, err := Checks(role)
	if err != nil {
		return Report{}, err
	}
	var findings []Finding
	for _, c := range checks {
		findings = append(findings, c.Run(m)...)
	}
	r := NewReport(findings)
	r.FileSet = m.Set()
	return r, nil
}
