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

package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// MaxVariables is the most inline user variables an object may set
// (spec.variables); the CRD and the webhook enforce it.
const MaxVariables = 256

// ErrVariableName is returned for a user variable name that is not a
// Terraform identifier.
var ErrVariableName = errors.New("contract: variable name is not a Terraform identifier")

// ErrVariableReserved is returned for a user variable name the generated
// root owns.
var ErrVariableReserved = errors.New("contract: variable name is reserved")

// variableName is a Terraform identifier as the API documents it.
var variableName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_-]*$`)

// moduleMetaArguments are the arguments a module block gives a meaning of
// its own. Terraform refuses them as variable names, and a user variable
// "source" would otherwise replace the generated module source.
var moduleMetaArguments = []string{"source", "version", "providers", "count", "for_each", "depends_on", "lifecycle", "locals"}

// ValidateVariableName reports whether name may be a user variable of role:
// a Terraform identifier that is not a captf_ name, not one of role's
// contract inputs and not a module meta-argument. The error names the
// variable, never a value.
func ValidateVariableName(role Role, name string) error {
	if !variableName.MatchString(name) {
		return fmt.Errorf("%w: %q (want %s)", ErrVariableName, name, variableName.String())
	}
	if strings.HasPrefix(name, ReservedPrefix) {
		return fmt.Errorf("%w: %q uses the %s prefix", ErrVariableReserved, name, ReservedPrefix)
	}
	if slices.Contains(moduleMetaArguments, name) {
		return fmt.Errorf("%w: %q is a module meta-argument", ErrVariableReserved, name)
	}
	inputs, err := InputNames(role)
	if err != nil {
		return err
	}
	if slices.Contains(inputs, name) {
		return fmt.Errorf("%w: %q is a contract input of the %s role", ErrVariableReserved, name, role)
	}
	return nil
}

// ErrVariablesNotObject is returned for spec.variables that is not a JSON
// object.
var ErrVariablesNotObject = errors.New("variables must be a JSON object")

// ParseVariables parses raw, spec.variables' JSON bytes, into its keys and
// values. It checks the shape only; names are checked per role
// (ValidateVariableName). It returns the parsed map, or ErrVariablesNotObject
// if raw is not a JSON object; the error never quotes the input.
func ParseVariables(raw []byte) (map[string]json.RawMessage, error) {
	var vars map[string]json.RawMessage
	if err := json.Unmarshal(raw, &vars); err != nil || vars == nil {
		return nil, ErrVariablesNotObject
	}
	return vars, nil
}

// Variable is one user-supplied module variable (spec.variables and
// spec.variablesFrom).
type Variable struct {
	// Value is the variable's JSON value.
	Value json.RawMessage
	// Sensitive is true when the winning value came from a Secret: the
	// generated root declares the variable sensitive.
	Sensitive bool
}

// Variables are the merged user variables of an object, by name. They are
// not contract inputs: the generated root passes each to the role module
// as a named argument, and the inputs hash covers them only when there are
// any, so an object without variables keeps its hash.
type Variables map[string]Variable

// Names returns the variable names, sorted.
func (v Variables) Names() []string {
	return slices.Sorted(maps.Keys(v))
}

// VariablesOf returns the user variables carried by in, a ClusterInputs,
// MachineInputs or MachinePoolInputs (or a pointer to one); nil for
// anything else.
func VariablesOf(in any) Variables {
	switch v := in.(type) {
	case ClusterInputs:
		return v.Variables
	case *ClusterInputs:
		if v != nil {
			return v.Variables
		}
	case MachineInputs:
		return v.Variables
	case *MachineInputs:
		if v != nil {
			return v.Variables
		}
	case MachinePoolInputs:
		return v.Variables
	case *MachinePoolInputs:
		if v != nil {
			return v.Variables
		}
	}
	return nil
}
