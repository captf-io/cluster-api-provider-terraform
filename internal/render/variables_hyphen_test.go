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
	"encoding/json"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// TestHyphenatedVariableName verifies that user variables with hyphens in
// their names (e.g. "foo-bar", accepted by contract.ValidateVariableName)
// render into a root module as valid Terraform references. HCL identifiers
// may contain hyphens, so var.foo-bar is a valid traversal.
func TestHyphenatedVariableName(t *testing.T) {
	t.Parallel()
	in := machineInputs()
	in.Variables = contract.Variables{
		"network-config": {Value: json.RawMessage(`{"subnet":"10.0.0.0/24"}`)},
		"cache-ttl":      {Value: json.RawMessage(`300`)},
		"debug-enabled":  {Value: json.RawMessage(`true`)},
	}
	files, err := Root(contract.RoleMachine, in)
	if err != nil {
		t.Fatalf("Root: %v", err)
	}

	// The generated main.tf.json must be valid JSON.
	var doc parsedMain
	if err := json.Unmarshal(files.MainTF, &doc); err != nil {
		t.Fatalf("parse main.tf.json: %v", err)
	}

	// Verify the variables are declared in the main.tf.json with hyphens
	// rendered as-is. HCL allows hyphens in identifiers, so var.foo-bar is
	// a valid traversal.
	for _, name := range []string{"network-config", "cache-ttl", "debug-enabled"} {
		if _, ok := doc.Variable[name]; !ok {
			t.Errorf("variable %s not declared", name)
			continue
		}
		// Verify the module receives the variable via the correct traversal.
		// The variable name with hyphens is used directly in ${var.name}.
		expected := "${var." + name + "}"
		if got := doc.Module["role"][name]; got != expected {
			t.Errorf("module argument %s = %q, want %q", name, got, expected)
		}
	}
}
