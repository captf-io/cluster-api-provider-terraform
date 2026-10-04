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

package hash

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// withVariables returns machineFixture with vars set as its user variables.
func withVariables(vars contract.Variables) contract.MachineInputs {
	in := machineFixture()
	in.Variables = vars
	return in
}

// TestVariablesKeepTheGoldenHash: an empty (not only nil) variable set
// hashes exactly like an object from before user variables existed, so
// upgrading re-applies nothing.
func TestVariablesKeepTheGoldenHash(t *testing.T) {
	t.Parallel()
	for name, vars := range map[string]contract.Variables{"nil": nil, "empty": {}} {
		if got := mustInputs(t, contract.RoleMachine, testImage, withVariables(vars)); got != goldenMachineHash {
			t.Errorf("%s variables: hash = %s, want the golden %s", name, got, goldenMachineHash)
		}
		ptr := withVariables(vars)
		if got := mustInputs(t, contract.RoleMachine, testImage, &ptr); got != goldenMachineHash {
			t.Errorf("%s variables via pointer: hash = %s", name, got)
		}
	}
}

// TestVariablesChangeTheHash: a value, a name and the sensitivity each
// change the hash; formatting and key order do not; non-integer and huge
// numbers hash (the contract inputs reject them) and are never rounded.
func TestVariablesChangeTheHash(t *testing.T) {
	t.Parallel()
	base := contract.Variables{
		"instance_type": {Value: json.RawMessage(`"t3.large"`)},
		"ratio":         {Value: json.RawMessage(`0.5`)},
		"big":           {Value: json.RawMessage(`12345678901234567890`)},
		"tags":          {Value: json.RawMessage(`{"b":1,"a":[true,null]}`)},
		"db_password":   {Value: json.RawMessage(`"s3cr3t"`), Sensitive: true},
	}
	h := mustInputs(t, contract.RoleMachine, testImage, withVariables(base))
	if h == goldenMachineHash {
		t.Fatal("variables did not change the hash")
	}
	// Same values, different formatting and key order.
	same := contract.Variables{}
	for k, v := range base {
		same[k] = v
	}
	same["tags"] = contract.Variable{Value: json.RawMessage("{ \"a\": [true, null],\n \"b\": 1 }")}
	if got := mustInputs(t, contract.RoleMachine, testImage, withVariables(same)); got != h {
		t.Errorf("reformatted variables hash differently: %s vs %s", got, h)
	}
	for name, edit := range map[string]func(contract.Variables){
		"value": func(v contract.Variables) {
			v["instance_type"] = contract.Variable{Value: json.RawMessage(`"t3.xlarge"`)}
		},
		"fraction": func(v contract.Variables) { v["ratio"] = contract.Variable{Value: json.RawMessage(`0.25`)} },
		"big number": func(v contract.Variables) {
			v["big"] = contract.Variable{Value: json.RawMessage(`12345678901234567891`)}
		},
		"new name": func(v contract.Variables) { v["extra"] = contract.Variable{Value: json.RawMessage(`1`)} },
		"removed":  func(v contract.Variables) { delete(v, "tags") },
		"secret value": func(v contract.Variables) {
			v["db_password"] = contract.Variable{Value: json.RawMessage(`"other"`), Sensitive: true}
		},
		"sensitivity": func(v contract.Variables) { v["db_password"] = contract.Variable{Value: json.RawMessage(`"s3cr3t"`)} },
	} {
		vars := contract.Variables{}
		for k, v := range base {
			vars[k] = v
		}
		edit(vars)
		if got := mustInputs(t, contract.RoleMachine, testImage, withVariables(vars)); got == h {
			t.Errorf("changing the %s did not change the hash", name)
		}
	}
	// The cluster role takes variables the same way.
	cl := clusterFixture()
	clusterBase := mustInputs(t, contract.RoleCluster, testImage, cl)
	cl.Variables = contract.Variables{"ratio": {Value: json.RawMessage(`1.5`)}}
	if got := mustInputs(t, contract.RoleCluster, testImage, cl); got == clusterBase {
		t.Error("cluster variables did not change the hash")
	}
}

// TestVariablesInvalidJSON: a value that is not JSON fails the hash and the
// error names the variable, never the value.
func TestVariablesInvalidJSON(t *testing.T) {
	t.Parallel()
	_, err := Inputs(contract.RoleMachine, testImage, withVariables(contract.Variables{
		"db_password": {Value: json.RawMessage(`{"s3cr3t`), Sensitive: true},
	}))
	if err == nil || !strings.Contains(err.Error(), "db_password") || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("err = %v", err)
	}
}
