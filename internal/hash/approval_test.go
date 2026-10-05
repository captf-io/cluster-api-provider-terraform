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

package hash

import (
	"encoding/json"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// mustApproval calls Approval with role, image and in, fails t if it
// errors, and returns the resulting hash.
func mustApproval(t *testing.T, role contract.Role, image string, in any) string {
	t.Helper()
	h, err := Approval(role, image, in)
	if err != nil {
		t.Fatalf("Approval: %v", err)
	}
	return h
}

// TestPoolApprovalIgnoresBootstrapRotation proves a pool's approval hash
// stays the same when only bootstrap_data rotates (while the inputs hash
// changes), changes with every other input the approval covers, and is
// the same for the value and the pointer form.
func TestPoolApprovalIgnoresBootstrapRotation(t *testing.T) {
	t.Parallel()
	for name, base := range map[string]contract.MachinePoolInputs{"autoscaled": poolFixture(), "fixed": poolFixtureDisabled()} {
		approval := mustApproval(t, contract.RoleMachinePool, testImage, base)
		if approval == mustInputs(t, contract.RoleMachinePool, testImage, base) {
			t.Errorf("%s: the approval hash equals the inputs hash; it must leave bootstrap_data out", name)
		}
		if p := mustApproval(t, contract.RoleMachinePool, testImage, &base); p != approval {
			t.Errorf("%s: pointer approval hash %s != value %s", name, p, approval)
		}

		rotated := base
		rotated.BootstrapData = "I2Nsb3VkLWNvbmZpZwojIG5ldw=="
		if mustInputs(t, contract.RoleMachinePool, testImage, rotated) == mustInputs(t, contract.RoleMachinePool, testImage, base) {
			t.Fatalf("%s: a rotation did not change the inputs hash", name)
		}
		if got := mustApproval(t, contract.RoleMachinePool, testImage, rotated); got != approval {
			t.Errorf("%s: a bootstrap rotation changed the approval hash: %s != %s", name, got, approval)
		}

		for field, mutant := range map[string]contract.MachinePoolInputs{
			"exports":          withPool(base, func(in *contract.MachinePoolInputs) { in.ClusterOutputs = json.RawMessage(`{"network_id":"net-2"}`) }),
			"version":          withPool(base, func(in *contract.MachinePoolInputs) { in.KubernetesVersion = new("v1.37.0") }),
			"bootstrap_format": withPool(base, func(in *contract.MachinePoolInputs) { in.BootstrapFormat = "ignition" }),
			"labels":           withPool(base, func(in *contract.MachinePoolInputs) { in.NodeLabels = map[string]string{"pool": "other"} }),
			"variables": withPool(base, func(in *contract.MachinePoolInputs) {
				in.Variables = contract.Variables{"x": {Value: json.RawMessage(`"y"`)}}
			}),
		} {
			if got := mustApproval(t, contract.RoleMachinePool, testImage, mutant); got == approval {
				t.Errorf("%s: changing %s did not change the approval hash", name, field)
			}
		}
		if mustApproval(t, contract.RoleMachinePool, "ghcr.io/example/pool:v2", base) == approval {
			t.Errorf("%s: changing the image did not change the approval hash", name)
		}
	}
}

// TestApprovalIsInputsHashWithoutView proves a cluster's and a machine's
// approval hash is their inputs hash: only an ApprovalViewer narrows it.
func TestApprovalIsInputsHashWithoutView(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		role contract.Role
		in   any
	}{
		"cluster": {contract.RoleCluster, clusterFixture()},
		"machine": {contract.RoleMachine, machineFixture()},
	} {
		if got, want := mustApproval(t, tc.role, testImage, tc.in), mustInputs(t, tc.role, testImage, tc.in); got != want {
			t.Errorf("%s: approval hash %s, want the inputs hash %s", name, got, want)
		}
	}
}

// TestExports proves Exports ignores key order and whitespace, keeps
// non-integer numbers as written, hashes null and empty as {}, tells
// different values apart and rejects a value that is not JSON.
func TestExports(t *testing.T) {
	t.Parallel()
	must := func(raw string) string {
		t.Helper()
		h, err := Exports(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("Exports(%s): %v", raw, err)
		}
		return h
	}
	base := must(`{"a":1,"b":["x","y"],"ratio":0.5}`)
	if got := must("{ \"ratio\": 0.5, \"b\": [\"x\", \"y\"],\n \"a\": 1 }"); got != base {
		t.Errorf("reordered and reformatted exports hash %s, want %s", got, base)
	}
	if must(`{"a":1,"b":["y","x"],"ratio":0.5}`) == base || must(`{"a":1,"b":["x","y"],"ratio":0.50}`) == base {
		t.Error("a changed value did not change the exports hash")
	}
	empty := must(`{}`)
	if must(``) != empty || must(`null`) != empty || must(" null ") != empty {
		t.Error("null or empty exports do not hash as {}")
	}
	if _, err := Exports(json.RawMessage(`{"a":`)); err == nil {
		t.Error("Exports accepted invalid JSON")
	}
}

// withPool returns a copy of in changed by mut.
func withPool(in contract.MachinePoolInputs, mut func(*contract.MachinePoolInputs)) contract.MachinePoolInputs {
	mut(&in)
	return in
}
