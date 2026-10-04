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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// Hashable is exactly what captf.io/inputs-hash covers: the hash scheme, the
// contract version, the role, spec.source.image as written (tag or digest,
// never the resolved digest), every rendered role input, and the user
// variables when there are any. Anything not in this struct cannot change
// the hash: object metadata, the resolved digest, Job metadata, attempts and
// drift ticks.
type Hashable struct {
	Scheme   string        `json:"scheme"`
	Contract string        `json:"contract"`
	Role     contract.Role `json:"role"`
	Image    string        `json:"image"`
	// Inputs is contract.ClusterInputs, contract.MachineInputs or
	// contract.MachinePoolInputs — or, when the kind implements
	// contract.HashViewer (MachinePoolInputs does), the HashView() it
	// returns instead. Their Variables field is not marshaled; it is
	// Variables below.
	Inputs any `json:"inputs"`
	// Variables is the canonical encoding of the user variables (values and
	// sensitive names), omitted when there are none: an object without
	// variables hashes exactly as before they existed, so adding the
	// feature re-applies nothing. It is carried as a string because user
	// values may hold non-integer numbers, which Canonical rejects.
	Variables string `json:"variables,omitempty"`
}

// Inputs returns the inputs hash for role, image and inputs. The user
// variables are taken from inputs (contract.VariablesOf) — always the
// original inputs, never the hashed view. When inputs implements
// contract.HashViewer, the hashed Inputs field is HashView() instead of
// inputs itself.
func Inputs(role contract.Role, image string, inputs any) (string, error) {
	if err := role.Validate(); err != nil {
		return "", err
	}
	vars, err := canonicalVariables(contract.VariablesOf(inputs))
	if err != nil {
		return "", err
	}
	hashed := inputs
	if v, ok := inputs.(contract.HashViewer); ok {
		hashed = v.HashView()
	}
	return Sum(Hashable{
		Scheme:    Scheme,
		Contract:  contract.Version,
		Role:      role,
		Image:     image,
		Inputs:    hashed,
		Variables: vars,
	})
}

// Approval returns the hash a destructive-plan approval
// (captf.io/approve-destructive-plan) of role's inputs, rendered with
// image, must name: Inputs of their ApprovalView() when inputs implements
// contract.ApprovalViewer (a pool's inputs without bootstrap_data), else
// Inputs of inputs itself, so a cluster's approval hash is its inputs
// hash. It returns the hash, or any error from Inputs.
func Approval(role contract.Role, image string, inputs any) (string, error) {
	if v, ok := inputs.(contract.ApprovalViewer); ok {
		inputs = v.ApprovalView()
	}
	return Inputs(role, image, inputs)
}

// Exports returns the hash of raw, a cluster's exports output as a
// machine or pool renders it (captf_cluster_outputs): Scheme + ":" + hex(sha256) of
// its canonical encoding, every number as written. Key order and
// whitespace do not change it; an empty or null value hashes as {}, as the
// contract treats it. It returns the hash, or an error for a value that is
// not JSON.
func Exports(raw json.RawMessage) (string, error) {
	if t := bytes.TrimSpace(raw); len(t) == 0 || bytes.Equal(t, []byte("null")) {
		raw = json.RawMessage("{}")
	}
	b, err := canonicalAnyNumber(raw)
	if err != nil {
		return "", fmt.Errorf("hash: exports: %w", err)
	}
	sum := sha256.Sum256(b)
	return Scheme + ":" + hex.EncodeToString(sum[:]), nil
}

// canonicalVariables returns vars encoded as
// {"sensitive":[names],"values":{...}} with sorted keys and numbers as
// written, or "" when vars is empty. The sensitive names are covered because
// they change the rendered main.tf.json. Sensitive values are hashed like
// any other: the sum is not reversible, and the encoding itself never
// leaves this function.
func canonicalVariables(vars contract.Variables) (string, error) {
	if len(vars) == 0 {
		return "", nil
	}
	values := make(map[string]any, len(vars))
	sensitive := []string{}
	for _, name := range vars.Names() {
		v := vars[name]
		dec := json.NewDecoder(bytes.NewReader(v.Value))
		dec.UseNumber()
		var tree any
		if dec.Decode(&tree) != nil {
			// The decode error would quote part of the value.
			return "", fmt.Errorf("hash: variable %s is not valid JSON", name)
		}
		values[name] = tree
		if v.Sensitive {
			sensitive = append(sensitive, name)
		}
	}
	b, err := canonicalAnyNumber(map[string]any{"sensitive": sensitive, "values": values})
	if err != nil {
		return "", err
	}
	return string(b), nil
}
