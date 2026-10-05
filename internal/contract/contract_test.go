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
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The embedded JSON schemas are the single source of truth: these tests pin the Go
// names and structs to them.

// schemaDoc is the subset of a JSON Schema document these tests inspect:
// its top-level properties and required keys, and the same for each
// definition under $defs.
type schemaDoc struct {
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
	Defs       map[string]struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	} `json:"$defs"`
}

// loadSchema parses schema bytes b into a schemaDoc, failing t if b is not
// valid JSON; it returns the parsed document.
func loadSchema(t *testing.T, b []byte) schemaDoc {
	t.Helper()
	var d schemaDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	return d
}

// sorted returns a sorted clone of s.
func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// keysOf returns m's keys, sorted.
func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// jsonKeys marshals v to JSON and returns its top-level object's keys,
// sorted, failing t if v does not marshal to a JSON object.
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return keysOf(m)
}

// TestSchemaFiles proves SchemaFiles lists exactly the embedded schema
// files and that each is valid JSON.
func TestSchemaFiles(t *testing.T) {
	t.Parallel()
	for _, name := range SchemaFiles() {
		if !json.Valid(mustRead(name)) {
			t.Errorf("%s is not valid JSON", name)
		}
	}
	entries, err := schemaFS.ReadDir("schemas")
	if err != nil || len(entries) != len(SchemaFiles()) {
		t.Errorf("embedded %d schema files (err %v), SchemaFiles lists %d", len(entries), err, len(SchemaFiles()))
	}
}

// TestInputNames proves InputNames matches each role's inputs schema
// (its required keys and property names), and that a fully populated and
// a zero-value inputs struct both render exactly those keys.
func TestInputNames(t *testing.T) {
	t.Parallel()
	for role, populated := range map[Role]any{RoleCluster: fullClusterInputs(), RoleMachine: fullMachineInputs(), RoleMachinePool: fullPoolInputs()} {
		names, err := InputNames(role)
		if err != nil {
			t.Fatalf("InputNames(%s): %v", role, err)
		}
		schema, err := InputsSchema(role)
		if err != nil {
			t.Fatalf("InputsSchema(%s): %v", role, err)
		}
		doc := loadSchema(t, schema)
		// Inputs are the schema's required keys; the cluster schema also
		// allows an optional captf_cluster_outputs, which is never rendered.
		if got, want := sorted(names), sorted(doc.Required); !slices.Equal(got, want) {
			t.Errorf("%s: InputNames = %v, schema required = %v", role, got, want)
		}
		for _, n := range names {
			if _, ok := doc.Properties[n]; !ok {
				t.Errorf("%s: input %s is not a schema property", role, n)
			}
		}
		// Every input key is always rendered, nullable ones as null.
		if got, want := jsonKeys(t, populated), sorted(names); !slices.Equal(got, want) {
			t.Errorf("%s: struct JSON keys = %v, want %v", role, got, want)
		}
		empty := map[Role]any{RoleCluster: ClusterInputs{}, RoleMachine: MachineInputs{}, RoleMachinePool: MachinePoolInputs{}}[role]
		if got, want := jsonKeys(t, empty), sorted(names); !slices.Equal(got, want) {
			t.Errorf("%s: zero struct JSON keys = %v, want %v (no omitempty on inputs)", role, got, want)
		}
	}
}

// TestRequiredOutputs proves RequiredOutputs matches each role's outputs
// schema's required keys, and that a zero-value outputs struct's JSON keys
// match the schema's properties.
func TestRequiredOutputs(t *testing.T) {
	t.Parallel()
	for role, zero := range map[Role]any{RoleCluster: ClusterOutputs{}, RoleMachine: MachineOutputs{}, RoleMachinePool: MachinePoolOutputs{}} {
		names, err := RequiredOutputs(role)
		if err != nil {
			t.Fatalf("RequiredOutputs(%s): %v", role, err)
		}
		schema, err := OutputsSchema(role)
		if err != nil {
			t.Fatalf("OutputsSchema(%s): %v", role, err)
		}
		doc := loadSchema(t, schema)
		if got, want := sorted(names), sorted(doc.Required); !slices.Equal(got, want) {
			t.Errorf("%s: RequiredOutputs = %v, schema required = %v", role, got, want)
		}
		if got, want := jsonKeys(t, zero), keysOf(doc.Properties); !slices.Equal(got, want) {
			t.Errorf("%s: outputs struct keys = %v, schema properties = %v", role, got, want)
		}
	}
}

// TestDefinitions proves the shared $defs of the schema (health.state's
// enum, captf_tags' required keys, and the endpoint, cluster_network,
// machine_address and health definitions) match HealthStates, TagKeys,
// Tags and the corresponding Go structs.
func TestDefinitions(t *testing.T) {
	t.Parallel()
	defs := loadSchema(t, Definitions()).Defs
	var states []string
	for _, s := range HealthStates() {
		states = append(states, string(s))
	}
	var state struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(defs["health"].Properties["state"], &state); err != nil {
		t.Fatalf("health.state: %v", err)
	}
	if want := state.Enum; !slices.Equal(states, want) {
		t.Errorf("HealthStates = %v, schema enum = %v", states, want)
	}
	if got, want := sorted(TagKeys()), sorted(defs["captf_tags"].Required); !slices.Equal(got, want) {
		t.Errorf("TagKeys = %v, schema captf_tags required = %v", got, want)
	}
	if got := keysOf(Tags("c", "ns", "TerraformMachine", "m", "")); !slices.Equal(got, sorted(TagKeys())) {
		t.Errorf("Tags keys = %v", got)
	}
	for name, def := range map[string]any{"endpoint": Endpoint{}, "cluster_network": ClusterNetwork{}, "machine_address": Address{}, "health": Health{}} {
		if got, want := jsonKeys(t, def), keysOf(defs[name].Properties); !slices.Equal(got, want) {
			t.Errorf("%s: struct keys = %v, schema properties = %v", name, got, want)
		}
	}
}

// TestRoles proves the implemented roles validate, and that an unknown
// role is rejected consistently by Validate, InputNames, RequiredOutputs,
// InputsSchema and OutputsSchema.
func TestRoles(t *testing.T) {
	t.Parallel()
	for _, r := range []Role{RoleCluster, RoleMachine, RoleMachinePool} {
		if err := r.Validate(); err != nil {
			t.Errorf("%s: %v", r, err)
		}
	}
	checks := []struct {
		role Role
		want error
	}{{Role("bogus"), ErrUnknownRole}}
	for _, c := range checks {
		if err := c.role.Validate(); !errors.Is(err, c.want) {
			t.Errorf("Validate(%s) = %v, want %v", c.role, err, c.want)
		}
		if _, err := InputNames(c.role); !errors.Is(err, c.want) {
			t.Errorf("InputNames(%s) = %v, want %v", c.role, err, c.want)
		}
		if _, err := RequiredOutputs(c.role); !errors.Is(err, c.want) {
			t.Errorf("RequiredOutputs(%s) = %v", c.role, err)
		}
		if _, err := InputsSchema(c.role); !errors.Is(err, c.want) {
			t.Errorf("InputsSchema(%s) = %v", c.role, err)
		}
		if _, err := OutputsSchema(c.role); !errors.Is(err, c.want) {
			t.Errorf("OutputsSchema(%s) = %v", c.role, err)
		}
	}
}

// TestClusterNetworkRendersEmptyLists proves a zero-value ClusterNetwork
// marshals its nil CIDR lists as [] rather than null.
func TestClusterNetworkRendersEmptyLists(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(ClusterNetwork{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"pods":[],"services":[],"service_domain":null,"api_server_port":null}`; string(b) != want {
		t.Errorf("ClusterNetwork{} = %s, want %s", b, want)
	}
}

// TestGoldenInputs pins the rendered JSON of fully populated inputs. The
// golden files are also validated against the schemas by
// hack/verify-schemas.sh, which is where the Go types meet the schema types.
// Regenerate with UPDATE_SNAPSHOTS=1.
func TestGoldenInputs(t *testing.T) {
	t.Parallel()
	for name, v := range map[string]any{
		"cluster-inputs.golden.json":               fullClusterInputs(),
		"cluster-inputs-minimal.golden.json":       minimalClusterInputs(),
		"machine-inputs.golden.json":               fullMachineInputs(),
		"machine-inputs-null-optional.golden.json": nullMachineInputs(),
		"machinepool-inputs.golden.json":           fullPoolInputs(),
		"machinepool-inputs-minimal.golden.json":   minimalPoolInputs(),
	} {
		got, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got = append(got, '\n')
		path := filepath.Join("testdata", name)
		if os.Getenv("UPDATE_SNAPSHOTS") == "1" {
			if err := os.WriteFile(path, got, 0o600); err != nil {
				t.Fatalf("write golden: %v", err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read golden (UPDATE_SNAPSHOTS=1 creates it): %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the golden file; rerun with UPDATE_SNAPSHOTS=1 and review the diff", name)
		}
	}
}

// common returns the CommonInputs a fixture of the given kind shares:
// contract version, a fixed cluster and object, and their tags.
func common(kind string) CommonInputs {
	return CommonInputs{
		Contract: Version,
		Cluster:  Cluster{Name: "prod", Namespace: "team-a"},
		Object:   Object{Kind: kind, Name: "prod-md-0-abcde", Namespace: "team-a"},
		Tags:     Tags("prod", "team-a", kind, "prod-md-0-abcde", "prod-md-0"),
	}
}

// fullClusterInputs returns a ClusterInputs fixture with every optional
// field populated, matching testdata/cluster-inputs.golden.json.
func fullClusterInputs() ClusterInputs {
	return ClusterInputs{
		CommonInputs:            common("TerraformCluster"),
		ControlPlaneEndpoint:    &Endpoint{Host: "api.prod.example.com", Port: 6443},
		KubernetesVersion:       new("v1.36.2"),
		ControlPlaneInitialized: true,
		ClusterNetwork: &ClusterNetwork{
			Pods: []string{"192.168.0.0/16"}, Services: []string{"10.96.0.0/12"},
			ServiceDomain: new("cluster.local"), APIServerPort: new(int32(6443)),
		},
	}
}

// minimalClusterInputs returns a ClusterInputs fixture with every optional
// field left unset, matching testdata/cluster-inputs-minimal.golden.json.
func minimalClusterInputs() ClusterInputs {
	return ClusterInputs{CommonInputs: common("TerraformCluster"), ClusterNetwork: &ClusterNetwork{}}
}

// fullMachineInputs returns a MachineInputs fixture with every optional
// field populated, matching testdata/machine-inputs.golden.json.
func fullMachineInputs() MachineInputs {
	return MachineInputs{
		CommonInputs:      common("TerraformMachine"),
		ClusterOutputs:    json.RawMessage(`{"network_id":"net-1","subnets":["a","b"]}`),
		MachineName:       "prod-md-0-abcde-xyz12",
		BootstrapData:     "I2Nsb3VkLWNvbmZpZw==",
		BootstrapFormat:   "cloud-config",
		FailureDomain:     new("zone-a"),
		KubernetesVersion: new("v1.36.2+rke2r1"),
		ControlPlane:      true,
	}
}

// nullMachineInputs returns a MachineInputs fixture with every optional
// field left unset, matching
// testdata/machine-inputs-null-optional.golden.json.
func nullMachineInputs() MachineInputs {
	return MachineInputs{
		CommonInputs:    common("TerraformMachine"),
		ClusterOutputs:  json.RawMessage(`{}`),
		MachineName:     "prod-md-0-abcde-xyz12",
		BootstrapData:   "e30=",
		BootstrapFormat: "ignition",
	}
}

// fullPoolInputs returns a MachinePoolInputs fixture with every optional
// field populated, matching testdata/machinepool-inputs.golden.json.
func fullPoolInputs() MachinePoolInputs {
	return MachinePoolInputs{
		CommonInputs:          common("TerraformMachinePool"),
		ClusterOutputs:        json.RawMessage(`{"network_id":"net-1","subnets":["a","b"]}`),
		MachinePoolName:       "prod-mp-0",
		Replicas:              3,
		BootstrapData:         "I2Nsb3VkLWNvbmZpZw==",
		BootstrapFormat:       "cloud-config",
		FailureDomains:        []string{"zone-a"},
		ClusterFailureDomains: []string{"zone-a", "zone-b"},
		KubernetesVersion:     new("v1.36.2+rke2r1"),
		NodeLabels:            map[string]string{"pool": "prod-mp-0"},
		Autoscaling:           Autoscaling{Enabled: true, Min: 1, Max: 5},
	}
}

// minimalPoolInputs returns a MachinePoolInputs fixture with every
// optional field left unset (nil slices/maps, null kubernetes_version,
// autoscaling disabled), matching
// testdata/machinepool-inputs-minimal.golden.json.
func minimalPoolInputs() MachinePoolInputs {
	return MachinePoolInputs{
		CommonInputs:    common("TerraformMachinePool"),
		ClusterOutputs:  json.RawMessage(`{}`),
		MachinePoolName: "prod-mp-0",
		BootstrapData:   "e30=",
		BootstrapFormat: "ignition",
	}
}
