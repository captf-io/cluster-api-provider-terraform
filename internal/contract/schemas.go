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
	"embed"
	"fmt"
)

// The schemas are the source of truth for the book's published copies
// (module-author/contract/v1alpha1/schemas in github.com/captf-io/docs),
// which are updated by hand.
//
//go:embed schemas/*.json
var schemaFS embed.FS

// SchemaFiles lists the embedded schema file names; it returns
// definitions.json followed by the cluster, machine and machinepool
// inputs and outputs schema file names.
func SchemaFiles() []string {
	return []string{
		"definitions.json",
		"cluster-inputs.json", "cluster-outputs.json",
		"machine-inputs.json", "machine-outputs.json",
		"machinepool-inputs.json", "machinepool-outputs.json",
	}
}

// Definitions returns definitions.json, which the role schemas reference.
func Definitions() []byte {
	return mustRead("definitions.json")
}

// InputsSchema returns the JSON Schema of role's terraform.tfvars.json.
func InputsSchema(role Role) ([]byte, error) {
	if err := role.Validate(); err != nil {
		return nil, err
	}
	return mustRead(fmt.Sprintf("%s-inputs.json", role)), nil
}

// OutputsSchema returns the JSON Schema of role's outputs.
func OutputsSchema(role Role) ([]byte, error) {
	if err := role.Validate(); err != nil {
		return nil, err
	}
	return mustRead(fmt.Sprintf("%s-outputs.json", role)), nil
}

// mustRead reads name from the embedded schema filesystem, panicking if it
// is missing (only reachable if the embed pattern and SchemaFiles
// disagree); it returns the file's contents.
func mustRead(name string) []byte {
	b, err := schemaFS.ReadFile("schemas/" + name)
	if err != nil {
		// Only reachable if the embed pattern and SchemaFiles disagree.
		panic(fmt.Sprintf("contract: embedded schema %s: %v", name, err))
	}
	return b
}
