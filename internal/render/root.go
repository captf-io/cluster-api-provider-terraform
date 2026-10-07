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

package render

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// ErrInputsMismatch is returned when the inputs value does not belong to the
// role.
var ErrInputsMismatch = errors.New("render: inputs do not match the role")

// ErrInputsTooLarge is returned when the rendered root would not fit in a
// Secret with headroom to spare: the tfvars carry base64 bootstrap data (4/3
// its raw size) plus the cluster's exports, and the only report otherwise is
// a raw apiserver "Too long" error from whichever Secret write hits the
// limit first.
var ErrInputsTooLarge = errors.New("render: rendered inputs too large")

// InputsTooLargeError is ErrInputsTooLarge with the rendered size, for
// captf_inputs_bytes.
type InputsTooLargeError struct {
	// Bytes is the size of main.tf.json plus terraform.tfvars.json.
	Bytes int
}

// Error returns the message naming e's rendered size against the limit.
func (e *InputsTooLargeError) Error() string {
	return fmt.Sprintf("%v: %d bytes, limit %d", ErrInputsTooLarge, e.Bytes, maxInputsBytes)
}

// Unwrap returns ErrInputsTooLarge, so errors.Is(err, ErrInputsTooLarge)
// holds for e.
func (e *InputsTooLargeError) Unwrap() error { return ErrInputsTooLarge }

// Size is the byte count captf_inputs_bytes reports for files.
func (f Files) Size() int { return len(f.MainTF) + len(f.TFVars) }

// maxInputsBytes is comfortably under the ~1 MiB a Kubernetes object can
// hold, leaving headroom for the Secret's own metadata and the other key
// the per-run Secret carries alongside these two files.
const maxInputsBytes = 1_000_000

// Files is a rendered root module.
type Files struct {
	// MainTF is main.tf.json.
	MainTF []byte
	// TFVars is terraform.tfvars.json. It carries bootstrap data: treat it
	// as a Secret.
	TFVars []byte
}

// mainTF is the JSON shape of main.tf.json.
type mainTF struct {
	Terraform tfBlock                   `json:"terraform"`
	Variable  map[string]variableBlock  `json:"variable"`
	Module    map[string]map[string]any `json:"module"`
	Output    map[string]outputBlock    `json:"output"`
}

// tfBlock is the JSON shape of the terraform block's backend stanza.
type tfBlock struct {
	// Backend is the partial kubernetes backend; the runner completes it
	// with -backend-config.
	Backend map[string]struct{} `json:"backend"`
}

// variableBlock declares a root variable. User variables have no type: the
// module's own declaration converts the value.
type variableBlock struct {
	Type      string          `json:"type,omitempty"`
	Default   json.RawMessage `json:"default,omitempty"`
	Sensitive bool            `json:"sensitive,omitempty"`
}

// outputBlock is the JSON shape of one output block's value and
// sensitivity.
type outputBlock struct {
	Value     string `json:"value"`
	Sensitive bool   `json:"sensitive"`
}

// MainTFJSON returns main.tf.json for role and the user variables user. It
// depends on the variable names and sensitivity only, never on a value.
// Each user variable is declared without a type (sensitive when it came
// from a Secret) and passed to the module under its own name; Terraform
// rejects a name the module does not declare ("Unsupported argument").
func MainTFJSON(role contract.Role, user contract.Variables) ([]byte, error) {
	if err := validateVariables(role, user); err != nil {
		return nil, err
	}
	// The root variables are declared as contract.InputSpecs says, the table
	// tfcapi-lint checks modules against.
	vars, err := contract.InputSpecs(role)
	if err != nil {
		return nil, err
	}
	outputs, err := contract.RequiredOutputs(role)
	if err != nil {
		return nil, err
	}
	doc := mainTF{
		Terraform: tfBlock{Backend: map[string]struct{}{"kubernetes": {}}},
		Variable:  make(map[string]variableBlock, len(vars)),
		Module:    map[string]map[string]any{"role": {"source": ModuleSource}},
		Output:    make(map[string]outputBlock, len(outputs)),
	}
	for name, v := range vars {
		b := variableBlock{Type: v.Type, Sensitive: v.Sensitive}
		if v.Nullable {
			b.Default = json.RawMessage("null")
		}
		doc.Variable[name] = b
		doc.Module["role"][name] = fmt.Sprintf("${var.%s}", name)
	}
	for name, v := range user {
		doc.Variable[name] = variableBlock{Sensitive: v.Sensitive}
		doc.Module["role"][name] = fmt.Sprintf("${var.%s}", name)
	}
	// Every re-export is sensitive, so a module may mark any contract
	// output sensitive without failing plan.
	for _, name := range outputs {
		doc.Output[name] = outputBlock{Value: fmt.Sprintf("${module.role.%s}", name), Sensitive: true}
	}
	return encode(doc)
}

// TFVarsJSON returns terraform.tfvars.json for in, a contract.ClusterInputs,
// contract.MachineInputs or contract.MachinePoolInputs (or a pointer to
// one): the contract inputs, plus the user variables when there are any
// (keys sorted then).
func TFVarsJSON(in any) ([]byte, error) {
	role, err := roleOf(in)
	if err != nil {
		return nil, err
	}
	vars := contract.VariablesOf(in)
	if len(vars) == 0 {
		return encode(in)
	}
	if err := validateVariables(role, vars); err != nil {
		return nil, err
	}
	// encode, not json.Marshal: module-authored values keep their bytes
	// (no HTML escaping); the final encode re-indents the raw values.
	raw, err := encode(in)
	if err != nil {
		return nil, err
	}
	all := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("render: encode: %w", err)
	}
	for name, v := range vars {
		all[name] = v.Value
	}
	return encode(all)
}

// ErrVariableValue is returned for a user variable whose value is not
// valid JSON.
var ErrVariableValue = errors.New("render: variable value is not valid JSON")

// validateVariables rejects, among vars, any name role's root owns and any
// value that is not JSON: the controller resolves variables before
// rendering, so this is defense in depth (a user variable "source" would
// replace the module source). It returns a non-nil error naming the
// first such variable in name order, never its value.
func validateVariables(role contract.Role, vars contract.Variables) error {
	for _, name := range vars.Names() {
		v := vars[name]
		if err := contract.ValidateVariableName(role, name); err != nil {
			return fmt.Errorf("render: %w", err)
		}
		if !json.Valid(v.Value) {
			return fmt.Errorf("%w: %s", ErrVariableValue, name)
		}
	}
	return nil
}

// Root renders both files for role from in, which must be role's inputs,
// and returns them as Files. It returns a non-nil error when role or in is
// invalid, in does not match role, its bootstrap data is not valid base64,
// or the rendered files exceed the size limit (ErrInputsTooLarge).
func Root(role contract.Role, in any) (Files, error) {
	if err := role.Validate(); err != nil {
		return Files{}, err
	}
	got, err := roleOf(in)
	if err != nil {
		return Files{}, err
	}
	if got != role {
		return Files{}, fmt.Errorf("%w: %T for role %s", ErrInputsMismatch, in, role)
	}
	if data, ok := bootstrapDataOf(in); ok {
		if _, err := base64.StdEncoding.Strict().DecodeString(data); err != nil {
			return Files{}, fmt.Errorf("render: bootstrap_data is not standard padded base64: %w", err)
		}
	}
	mainTFBytes, err := MainTFJSON(role, contract.VariablesOf(in))
	if err != nil {
		return Files{}, err
	}
	vars, err := TFVarsJSON(in)
	if err != nil {
		return Files{}, err
	}
	if n := len(mainTFBytes) + len(vars); n > maxInputsBytes {
		return Files{}, &InputsTooLargeError{Bytes: n}
	}
	return Files{MainTF: mainTFBytes, TFVars: vars}, nil
}

// roleOf returns the Role that in belongs to, given as a
// contract.ClusterInputs, contract.MachineInputs or
// contract.MachinePoolInputs, or a non-nil pointer to one of them; it
// returns a non-nil ErrInputsMismatch error for any other type or a nil
// pointer.
func roleOf(in any) (contract.Role, error) {
	switch v := in.(type) {
	case contract.ClusterInputs:
		return contract.RoleCluster, nil
	case *contract.ClusterInputs:
		if v != nil {
			return contract.RoleCluster, nil
		}
	case contract.MachineInputs:
		return contract.RoleMachine, nil
	case *contract.MachineInputs:
		if v != nil {
			return contract.RoleMachine, nil
		}
	case contract.MachinePoolInputs:
		return contract.RoleMachinePool, nil
	case *contract.MachinePoolInputs:
		if v != nil {
			return contract.RoleMachinePool, nil
		}
	}
	return "", fmt.Errorf("%w: unsupported inputs type %T", ErrInputsMismatch, in)
}

// bootstrapDataOf returns the bootstrap_data carried by in, a
// contract.MachineInputs or contract.MachinePoolInputs (or a pointer to
// either), and whether in held one at all: every role that renders
// bootstrap data validates it is base64 before it reaches a Secret.
func bootstrapDataOf(in any) (string, bool) {
	switch v := in.(type) {
	case contract.MachineInputs:
		return v.BootstrapData, true
	case *contract.MachineInputs:
		if v != nil {
			return v.BootstrapData, true
		}
	case contract.MachinePoolInputs:
		return v.BootstrapData, true
	case *contract.MachinePoolInputs:
		if v != nil {
			return v.BootstrapData, true
		}
	}
	return "", false
}

// BackendRoot returns the root a state restore runs in: only the partial
// kubernetes backend, which init completes with -backend-config as for
// every operation. It calls no module and declares no variable, so a
// restore needs neither the durable inputs (which a disaster may have
// taken too) nor any provider: `state push` loads no provider schema
// outside HCP Terraform (Terraform v1.16.4 internal/command/state_push.go,
// OpenTofu v1.12.6 likewise).
func BackendRoot() Files {
	return Files{
		MainTF: []byte("{\n  \"terraform\": {\n    \"backend\": {\n      \"kubernetes\": {}\n    }\n  }\n}\n"),
		TFVars: []byte("{}\n"),
	}
}

// encode returns v marshaled indented, without HTML escaping, so
// module-authored values such as exports keep their bytes in the durable
// inputs Secret.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("render: encode: %w", err)
	}
	return buf.Bytes(), nil
}
