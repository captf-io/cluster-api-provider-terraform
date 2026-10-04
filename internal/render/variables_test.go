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

package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// TestUserVariablesRoot: each user variable is a root variable without a
// type, sensitive exactly when it came from a Secret, passed to the module
// under its own name, and valued in the tfvars next to the contract inputs.
func TestUserVariablesRoot(t *testing.T) {
	t.Parallel()
	files, err := Root(contract.RoleMachine, machineInputsWithVariables())
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	var doc parsedMain
	if err := json.Unmarshal(files.MainTF, &doc); err != nil {
		t.Fatalf("parse main.tf.json: %v", err)
	}
	for name, sensitive := range map[string]bool{"instance_type": false, "disk_gib": false, "ratio": false, "subnet_ids": false, "db_password": true} {
		v, ok := doc.Variable[name]
		if !ok {
			t.Errorf("variable %s not declared", name)
			continue
		}
		if _, typed := v["type"]; typed {
			t.Errorf("variable %s has a type; the module's declaration converts it", name)
		}
		if got := string(v["sensitive"]) == "true"; got != sensitive {
			t.Errorf("variable %s sensitive = %v, want %v", name, got, sensitive)
		}
		if doc.Module["role"][name] != "${var."+name+"}" {
			t.Errorf("module argument %s = %q", name, doc.Module["role"][name])
		}
	}
	if doc.Module["role"]["source"] != ModuleSource {
		t.Errorf("module source = %q", doc.Module["role"]["source"])
	}
	if strings.Contains(string(files.MainTF), secretPassword) {
		t.Error("main.tf.json carries a variable value")
	}
	var vars map[string]json.RawMessage
	if err := json.Unmarshal(files.TFVars, &vars); err != nil {
		t.Fatalf("parse tfvars: %v", err)
	}
	for name, want := range map[string]string{
		"instance_type": `"t3.large"`, "disk_gib": `40`, "ratio": `0.5`, "subnet_ids": `["subnet-a","subnet-b"]`,
		"db_password": `"` + secretPassword + `"`, "machine_name": `"prod-md-0-abcde-xyz12"`,
	} {
		var got, exp any
		if json.Unmarshal(vars[name], &got) != nil || json.Unmarshal([]byte(want), &exp) != nil || !jsonEqual(got, exp) {
			t.Errorf("tfvars %s = %s, want %s", name, vars[name], want)
		}
	}

	// Module-authored exports keep their bytes on this path too.
	if !bytes.Contains(files.TFVars, []byte(`a<b & c>d`)) {
		t.Error("tfvars escape HTML characters in exports when variables are set")
	}

	// Trace logging redacts the Secret-won value, never the others.
	red := string(RedactedTFVars(files))
	if strings.Contains(red, secretPassword) || !strings.Contains(red, "t3.large") || !strings.Contains(red, "<redacted:len=") {
		t.Errorf("redacted tfvars = %s", red)
	}
}

// jsonEqual reports whether a and b marshal to the same JSON bytes.
func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// TestUserVariablesRejected: names the root owns and values that are not
// JSON never render, and no error quotes a value.
func TestUserVariablesRejected(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]error{
		"source":       contract.ErrVariableReserved,
		"count":        contract.ErrVariableReserved,
		"captf_extra":  contract.ErrVariableReserved,
		"machine_name": contract.ErrVariableReserved,
		"has.dot":      contract.ErrVariableName,
		"9lives":       contract.ErrVariableName,
	} {
		in := machineInputs()
		in.Variables = contract.Variables{name: {Value: json.RawMessage(`"` + secretPassword + `"`)}}
		_, err := Root(contract.RoleMachine, in)
		if !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", name, err, want)
		}
		if err != nil && strings.Contains(err.Error(), secretPassword) {
			t.Errorf("%s: error carries the value: %v", name, err)
		}
		if _, err := TFVarsJSON(in); !errors.Is(err, want) {
			t.Errorf("TFVarsJSON %s: err = %v, want %v", name, err, want)
		}
	}
	// A cluster contract input is a fine machine variable name, and the
	// reverse is reserved.
	in := machineInputs()
	in.Variables = contract.Variables{"control_plane_initialized": {Value: json.RawMessage(`true`)}}
	if _, err := Root(contract.RoleMachine, in); err != nil {
		t.Errorf("cluster input name on a machine: %v", err)
	}
	cl := clusterInputs()
	cl.Variables = contract.Variables{"control_plane_initialized": {Value: json.RawMessage(`true`)}}
	if _, err := Root(contract.RoleCluster, cl); !errors.Is(err, contract.ErrVariableReserved) {
		t.Errorf("cluster input as a cluster variable: err = %v", err)
	}

	bad := machineInputs()
	bad.Variables = contract.Variables{"db_password": {Value: json.RawMessage(`{"` + secretPassword)}}
	_, err := Root(contract.RoleMachine, bad)
	if !errors.Is(err, ErrVariableValue) || strings.Contains(err.Error(), secretPassword) {
		t.Errorf("invalid JSON value: err = %v", err)
	}
}

// TestUserVariablesTooLarge: variables count toward the rendered-inputs
// limit.
func TestUserVariablesTooLarge(t *testing.T) {
	t.Parallel()
	in := clusterInputs()
	in.Variables = contract.Variables{"blob": {Value: json.RawMessage(`"` + strings.Repeat("a", 1_000_000) + `"`)}}
	_, err := Root(contract.RoleCluster, in)
	var tooLarge *InputsTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Bytes <= maxInputsBytes {
		t.Errorf("err = %v, want InputsTooLargeError", err)
	}
}

// TestRedactedTFVarsFailsClosed: without a readable main.tf.json the
// sensitive variables are unknown, so nothing is logged.
func TestRedactedTFVarsFailsClosed(t *testing.T) {
	t.Parallel()
	if got := string(RedactedTFVars(Files{MainTF: []byte("{"), TFVars: []byte(`{"a":1}`)})); got != redactionFailed {
		t.Errorf("redacted = %s", got)
	}
}
