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

package inputs

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
)

// PoolInputsHash returns the inputs hash (hash.Inputs) of the
// TerraformMachinePool inputs d's rendered files carry, with d's image
// (written with them): the hash the apply that wrote them computed, so it
// equals an apply Job's inputs hash only when d still holds that Job's
// inputs. The contract inputs are read back from the
// tfvars; the user variables are the tfvars keys the role's root does not
// declare as contract inputs, sensitive as main.tf.json declares them. It
// returns an error when d is nil, the files do not parse or the inputs do
// not hash.
func PoolInputsHash(d *Record) (string, error) {
	if d == nil {
		return "", errors.New("inputs: no rendered pool inputs")
	}
	var in contract.MachinePoolInputs
	if err := json.Unmarshal(d.Files.TFVars, &in); err != nil {
		return "", fmt.Errorf("inputs: read the rendered pool inputs: %w", err)
	}
	vars, err := renderedVariables(contract.RoleMachinePool, d.Files)
	if err != nil {
		return "", err
	}
	in.Variables = vars
	h, err := hash.Inputs(contract.RoleMachinePool, d.Image, in)
	if err != nil {
		return "", fmt.Errorf("inputs: %w", err)
	}
	return h, nil
}

// renderedVariables returns the user variables files, rendered for role,
// carry: every tfvars key that is not one of role's contract inputs
// (contract.InputSpecs), with its value and the sensitivity main.tf.json
// declares for it; nil when there are none. It returns an error when
// either file does not parse.
func renderedVariables(role contract.Role, files render.Files) (contract.Variables, error) {
	specs, err := contract.InputSpecs(role)
	if err != nil {
		return nil, fmt.Errorf("inputs: %w", err)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(files.TFVars, &values); err != nil {
		return nil, fmt.Errorf("inputs: read the rendered tfvars: %w", err)
	}
	var root struct {
		Variable map[string]struct {
			Sensitive bool `json:"sensitive"`
		} `json:"variable"`
	}
	if err := json.Unmarshal(files.MainTF, &root); err != nil {
		return nil, fmt.Errorf("inputs: read the rendered root: %w", err)
	}
	var vars contract.Variables
	for name, v := range values {
		if _, ok := specs[name]; ok {
			continue
		}
		if vars == nil {
			vars = contract.Variables{}
		}
		vars[name] = contract.Variable{Value: v, Sensitive: root.Variable[name].Sensitive}
	}
	return vars, nil
}
