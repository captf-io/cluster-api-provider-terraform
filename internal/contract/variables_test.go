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
	"slices"
	"testing"
)

// TestValidateVariableName proves ValidateVariableName accepts a
// well-formed, non-reserved variable name and rejects a malformed name
// (ErrVariableName) or one reserved by the contract or role
// (ErrVariableReserved), and that every contract input name of a role is
// itself reserved for that role.
func TestValidateVariableName(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		role Role
		name string
		want error
	}{
		{RoleMachine, "instance_type", nil},
		{RoleMachine, "_private", nil},
		{RoleMachine, "with-dash9", nil},
		{RoleMachine, "control_plane_endpoint", nil}, // a cluster input, not a machine one
		{RoleCluster, "machine_name", nil},
		{RoleMachine, "", ErrVariableName},
		{RoleMachine, "9lives", ErrVariableName},
		{RoleMachine, "has.dot", ErrVariableName},
		{RoleMachine, "has space", ErrVariableName},
		{RoleMachine, "captf_anything", ErrVariableReserved},
		{RoleCluster, "captf_cluster_outputs", ErrVariableReserved},
		{RoleMachine, "machine_name", ErrVariableReserved},
		{RoleMachine, "bootstrap_data", ErrVariableReserved},
		{RoleCluster, "control_plane_initialized", ErrVariableReserved},
		{RoleCluster, "source", ErrVariableReserved},
		{RoleMachine, "for_each", ErrVariableReserved},
		{RoleMachine, "depends_on", ErrVariableReserved},
		{RoleMachinePool, "instance_type", nil},
		{RoleMachinePool, "machinepool_name", ErrVariableReserved},
		{Role("bogus"), "instance_type", ErrUnknownRole},
	} {
		if err := ValidateVariableName(tt.role, tt.name); !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
			t.Errorf("ValidateVariableName(%s, %q) = %v, want %v", tt.role, tt.name, err, tt.want)
		}
	}
	// Every contract input of a role is reserved for it.
	for _, role := range []Role{RoleCluster, RoleMachine, RoleMachinePool} {
		names, err := InputNames(role)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if err := ValidateVariableName(role, n); !errors.Is(err, ErrVariableReserved) {
				t.Errorf("%s input %s: err = %v", role, n, err)
			}
		}
	}
}

// TestParseVariables proves ParseVariables parses a well-formed JSON
// object and returns ErrVariablesNotObject for anything else: null, an
// array, a scalar, invalid JSON or empty input.
func TestParseVariables(t *testing.T) {
	t.Parallel()
	got, err := ParseVariables([]byte(`{"a":1,"b":{"c":[true]}}`))
	if err != nil || len(got) != 2 || string(got["a"]) != "1" {
		t.Errorf("ParseVariables = %v, %v", got, err)
	}
	for _, raw := range []string{`null`, `[1]`, `"s"`, `3`, `{"a":`, ``} {
		if _, err := ParseVariables([]byte(raw)); !errors.Is(err, ErrVariablesNotObject) {
			t.Errorf("ParseVariables(%q): err = %v", raw, err)
		}
	}
}

// TestVariablesOf proves Variables.Names returns sorted names, that
// VariablesOf extracts a role's Variables from a value, a pointer, a nil
// pointer or an unrelated type, and that Variables never marshal alongside
// an inputs struct's contract fields.
func TestVariablesOf(t *testing.T) {
	t.Parallel()
	vars := Variables{"b": {Value: json.RawMessage(`1`)}, "a": {Value: json.RawMessage(`2`), Sensitive: true}}
	if got := vars.Names(); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("Names = %v", got)
	}
	c := ClusterInputs{CommonInputs: CommonInputs{Variables: vars}}
	m := MachineInputs{CommonInputs: CommonInputs{Variables: vars}}
	p := MachinePoolInputs{CommonInputs: CommonInputs{Variables: vars}}
	var nilC *ClusterInputs
	var nilM *MachineInputs
	var nilP *MachinePoolInputs
	for name, tt := range map[string]struct {
		in   any
		want int
	}{
		"cluster": {c, 2}, "cluster ptr": {&c, 2}, "machine": {m, 2}, "machine ptr": {&m, 2},
		"pool": {p, 2}, "pool ptr": {&p, 2},
		"nil cluster": {nilC, 0}, "nil machine": {nilM, 0}, "nil pool": {nilP, 0}, "other": {42, 0},
	} {
		if got := VariablesOf(tt.in); len(got) != tt.want {
			t.Errorf("%s: VariablesOf = %v", name, got)
		}
	}
	// The variables are never marshaled with the contract inputs.
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["a"]; ok {
		t.Errorf("variables marshaled with the inputs: %s", b)
	}
	if _, ok := top["Variables"]; ok {
		t.Errorf("Variables field marshaled: %s", b)
	}
}
