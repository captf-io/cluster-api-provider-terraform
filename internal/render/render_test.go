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
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// tags returns the fixed-tenant tag set (contract.Tags) for an object of
// kind and name.
func tags(kind, name string) map[string]string {
	return contract.Tags("prod", "team-a", kind, name, "")
}

// clusterInputs returns a representative contract.ClusterInputs, the base
// case for this file's cluster-role tests.
func clusterInputs() contract.ClusterInputs {
	return contract.ClusterInputs{
		CommonInputs: contract.CommonInputs{
			Contract: contract.Version,
			Cluster:  contract.Cluster{Name: "prod", Namespace: "team-a"},
			Object:   contract.Object{Kind: "TerraformCluster", Name: "prod", Namespace: "team-a"},
			Tags:     tags("TerraformCluster", "prod"),
		},
		ControlPlaneEndpoint:    &contract.Endpoint{Host: "api.prod.example.com", Port: 6443},
		KubernetesVersion:       new("v1.36.2"),
		ControlPlaneInitialized: true,
		ClusterNetwork: &contract.ClusterNetwork{
			Pods: []string{"192.168.0.0/16"}, Services: []string{"10.96.0.0/12"},
			ServiceDomain: new("cluster.local"), APIServerPort: new(int32(6443)),
		},
	}
}

// bootstrapValue is the raw bootstrap Secret value; the caller encodes it.
const bootstrapValue = "#cloud-config\nruncmd:\n  - echo <ready> & done\n"

// machineInputs returns a representative contract.MachineInputs, the base
// case for this file's machine-role tests.
func machineInputs() contract.MachineInputs {
	return contract.MachineInputs{
		CommonInputs: contract.CommonInputs{
			Contract: contract.Version,
			Cluster:  contract.Cluster{Name: "prod", Namespace: "team-a"},
			Object:   contract.Object{Kind: "TerraformMachine", Name: "prod-md-0-abcde", Namespace: "team-a"},
			Tags:     tags("TerraformMachine", "prod-md-0-abcde"),
		},
		ClusterOutputs:    json.RawMessage(`{"network_id":"net-1","note":"a<b & c>d"}`),
		MachineName:       "prod-md-0-abcde-xyz12",
		BootstrapData:     base64.StdEncoding.EncodeToString([]byte(bootstrapValue)),
		BootstrapFormat:   "cloud-config",
		FailureDomain:     nil,
		KubernetesVersion: new("v1.36.2"),
		ControlPlane:      false,
	}
}

// secretPassword is a user variable value from a Secret.
const secretPassword = "s3cr3t-<pw>"

// machineInputsWithVariables returns machineInputs with user variables
// added: from a ConfigMap (instance_type, as a string), inline JSON
// (disk_gib, ratio, subnet_ids) and from a Secret (db_password, sensitive).
func machineInputsWithVariables() contract.MachineInputs {
	in := machineInputs()
	in.Variables = contract.Variables{
		"instance_type": {Value: json.RawMessage(`"t3.large"`)},
		"disk_gib":      {Value: json.RawMessage(`40`)},
		"ratio":         {Value: json.RawMessage(`0.5`)},
		"subnet_ids":    {Value: json.RawMessage(`["subnet-a","subnet-b"]`)},
		"db_password":   {Value: json.RawMessage(`"` + secretPassword + `"`), Sensitive: true},
	}
	return in
}

// poolInputs returns a representative contract.MachinePoolInputs, the base
// case for this file's machinepool-role tests.
func poolInputs() contract.MachinePoolInputs {
	return contract.MachinePoolInputs{
		CommonInputs: contract.CommonInputs{
			Contract: contract.Version,
			Cluster:  contract.Cluster{Name: "prod", Namespace: "team-a"},
			Object:   contract.Object{Kind: "TerraformMachinePool", Name: "prod-mp-0", Namespace: "team-a"},
			Tags:     tags("TerraformMachinePool", "prod-mp-0"),
		},
		ClusterOutputs:        json.RawMessage(`{"network_id":"net-1","note":"a<b & c>d"}`),
		MachinePoolName:       "prod-mp-0",
		Replicas:              3,
		BootstrapData:         base64.StdEncoding.EncodeToString([]byte(bootstrapValue)),
		BootstrapFormat:       "cloud-config",
		FailureDomains:        []string{},
		ClusterFailureDomains: []string{"zone-a", "zone-b"},
		KubernetesVersion:     new("v1.36.2"),
		NodeLabels:            map[string]string{"pool": "prod-mp-0"},
		Autoscaling:           contract.Autoscaling{Enabled: false, Min: 0, Max: 0},
	}
}

// TestGolden pins both files per case; UPDATE_SNAPSHOTS=1 rewrites them.
// The goldens are also run through terraform and tofu validate.
func TestGolden(t *testing.T) {
	t.Parallel()
	for dir, c := range map[string]struct {
		role contract.Role
		in   any
	}{
		"cluster":           {contract.RoleCluster, clusterInputs()},
		"machine":           {contract.RoleMachine, machineInputs()},
		"machine-variables": {contract.RoleMachine, machineInputsWithVariables()},
		"machinepool":       {contract.RoleMachinePool, poolInputs()},
	} {
		files, err := Root(c.role, c.in)
		if err != nil {
			t.Fatalf("Root(%s): %v", dir, err)
		}
		for name, got := range map[string][]byte{MainTFFile: files.MainTF, TFVarsFile: files.TFVars} {
			path := filepath.Join("testdata", dir, name)
			if os.Getenv("UPDATE_SNAPSHOTS") == "1" {
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
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
				t.Errorf("%s differs from the golden file; rerun with UPDATE_SNAPSHOTS=1 and review the diff", path)
			}
		}
	}
}

// parsedMain is a loosely typed parse of a rendered main.tf.json, for
// assertions against its structure.
type parsedMain struct {
	Terraform struct {
		Backend map[string]map[string]any `json:"backend"`
	} `json:"terraform"`
	Variable map[string]map[string]json.RawMessage `json:"variable"`
	Module   map[string]map[string]string          `json:"module"`
	Output   map[string]struct {
		Value     string `json:"value"`
		Sensitive bool   `json:"sensitive"`
	} `json:"output"`
}

// keys returns the sorted keys of m.
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestMainTF checks the structure against the contract: one variable and one
// module argument per input, nothing else; one sensitive output per required
// output.
func TestMainTF(t *testing.T) {
	t.Parallel()
	for _, role := range []contract.Role{contract.RoleCluster, contract.RoleMachine, contract.RoleMachinePool} {
		b, err := MainTFJSON(role, nil)
		if err != nil {
			t.Fatalf("MainTFJSON(%s): %v", role, err)
		}
		var doc parsedMain
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("%s: parse: %v", role, err)
		}
		inputs, err := contract.InputNames(role)
		if err != nil {
			t.Fatalf("InputNames: %v", err)
		}
		slices.Sort(inputs)
		if got := keys(doc.Variable); !slices.Equal(got, inputs) {
			t.Errorf("%s: variables = %v, want the contract inputs %v", role, got, inputs)
		}
		if _, ok := doc.Terraform.Backend["kubernetes"]; !ok || len(doc.Terraform.Backend) != 1 {
			t.Errorf("%s: backend = %v, want only kubernetes", role, doc.Terraform.Backend)
		}
		mod := doc.Module["role"]
		if mod["source"] != ModuleSource || len(doc.Module) != 1 {
			t.Errorf("%s: module = %v, want only role with source %s", role, doc.Module, ModuleSource)
		}
		args := keys(mod)
		args = slices.DeleteFunc(args, func(s string) bool { return s == "source" })
		if !slices.Equal(args, inputs) {
			t.Errorf("%s: module arguments = %v, want %v", role, args, inputs)
		}
		for _, name := range inputs {
			if mod[name] != "${var."+name+"}" {
				t.Errorf("%s: module argument %s = %q", role, name, mod[name])
			}
		}
		nullable := map[string]bool{"control_plane_endpoint": true, "kubernetes_version": true, "cluster_network": true, "failure_domain": true}
		for name, v := range doc.Variable {
			def, hasDefault := v["default"]
			if hasNullDefault := hasDefault && string(def) == "null"; hasNullDefault != nullable[name] || (hasDefault && !hasNullDefault) {
				t.Errorf("%s: variable %s default = %s (present %v), want null only when nullable (%v)", role, name, def, hasDefault, nullable[name])
			}
			if sensitive := string(v["sensitive"]) == "true"; sensitive != (name == "bootstrap_data") {
				t.Errorf("%s: variable %s sensitive = %v", role, name, sensitive)
			}
			if len(v["type"]) < 3 {
				t.Errorf("%s: variable %s has no type", role, name)
			}
		}
		required, err := contract.RequiredOutputs(role)
		if err != nil {
			t.Fatalf("RequiredOutputs: %v", err)
		}
		slices.Sort(required)
		if got := keys(doc.Output); !slices.Equal(got, required) {
			t.Errorf("%s: outputs = %v, want %v", role, got, required)
		}
		for name, o := range doc.Output {
			if !o.Sensitive || o.Value != "${module.role."+name+"}" {
				t.Errorf("%s: output %s = %+v, want a sensitive re-export", role, name, o)
			}
		}
	}
	if b, _ := MainTFJSON(contract.RoleCluster, nil); strings.Contains(string(b), "captf_cluster_outputs") {
		t.Error("cluster root must not declare or pass captf_cluster_outputs")
	}
}

// TestTFVars: the values are the inputs struct; bootstrap data round-trips;
// no object metadata is rendered; HTML characters are not escaped.
func TestTFVars(t *testing.T) {
	t.Parallel()
	for role, in := range map[contract.Role]any{contract.RoleCluster: clusterInputs(), contract.RoleMachine: machineInputs(), contract.RoleMachinePool: poolInputs()} {
		files, err := Root(role, in)
		if err != nil {
			t.Fatalf("Root(%s): %v", role, err)
		}
		var vars map[string]json.RawMessage
		if err := json.Unmarshal(files.TFVars, &vars); err != nil {
			t.Fatalf("%s: parse tfvars: %v", role, err)
		}
		inputs, _ := contract.InputNames(role)
		slices.Sort(inputs)
		if got := keys(vars); !slices.Equal(got, inputs) {
			t.Errorf("%s: tfvars keys = %v, want %v", role, got, inputs)
		}
		if m := regexp.MustCompile(`"(uid|labels|generation|annotations)"`).Find(files.TFVars); m != nil {
			t.Errorf("%s: tfvars contain object metadata key %s", role, m)
		}
	}
	files, err := Root(contract.RoleMachine, machineInputs())
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	var vars struct {
		BootstrapData string `json:"bootstrap_data"`
	}
	if err := json.Unmarshal(files.TFVars, &vars); err != nil {
		t.Fatalf("parse: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(vars.BootstrapData)
	if err != nil || string(raw) != bootstrapValue {
		t.Errorf("bootstrap_data does not round-trip base64 to the raw value (err %v)", err)
	}
	if !bytes.Contains(files.TFVars, []byte(`a<b & c>d`)) {
		t.Error("tfvars escape HTML characters in module-authored exports")
	}
}

// TestRootRejects checks that Root and its helpers report ErrInputsMismatch
// for a role/inputs mismatch or an unsupported type, a base64 error for
// unencoded bootstrap data, and that both accept a pointer to the inputs.
func TestRootRejects(t *testing.T) {
	t.Parallel()
	bad := machineInputs()
	bad.BootstrapData = bootstrapValue // not encoded
	var nilCluster *contract.ClusterInputs
	cases := []struct {
		name string
		role contract.Role
		in   any
		want error
	}{
		{"machine inputs for cluster role", contract.RoleCluster, machineInputs(), ErrInputsMismatch},
		{"cluster inputs for machine role", contract.RoleMachine, clusterInputs(), ErrInputsMismatch},
		{"unsupported type", contract.RoleMachine, map[string]any{}, ErrInputsMismatch},
		{"nil pointer", contract.RoleCluster, nilCluster, ErrInputsMismatch},
		{"machine inputs for pool role", contract.RoleMachinePool, machineInputs(), ErrInputsMismatch},
		{"pool inputs for machine role", contract.RoleMachine, poolInputs(), ErrInputsMismatch},
	}
	for _, c := range cases {
		if _, err := Root(c.role, c.in); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := Root(contract.RoleMachine, bad); err == nil || !strings.Contains(err.Error(), "base64") {
		t.Errorf("unencoded bootstrap data: err = %v, want a base64 error", err)
	}
	if _, err := TFVarsJSON(42); !errors.Is(err, ErrInputsMismatch) {
		t.Errorf("TFVarsJSON(42): err = %v", err)
	}
	if _, err := MainTFJSON("bogus", nil); !errors.Is(err, contract.ErrUnknownRole) {
		t.Errorf("MainTFJSON(bogus): err = %v", err)
	}
	ptr := machineInputs()
	if _, err := Root(contract.RoleMachine, &ptr); err != nil {
		t.Errorf("pointer inputs: %v", err)
	}
	cptr := clusterInputs()
	if _, err := Root(contract.RoleCluster, &cptr); err != nil {
		t.Errorf("pointer cluster inputs: %v", err)
	}
	pptr := poolInputs()
	if _, err := Root(contract.RoleMachinePool, &pptr); err != nil {
		t.Errorf("pointer pool inputs: %v", err)
	}
	badPool := poolInputs()
	badPool.BootstrapData = bootstrapValue // not encoded
	if _, err := Root(contract.RoleMachinePool, badPool); err == nil || !strings.Contains(err.Error(), "base64") {
		t.Errorf("unencoded pool bootstrap data: err = %v, want a base64 error", err)
	}
}

// TestRootRejectsTooLarge: item 8. A Secret's ~1 MiB cap has no headroom for
// a raw apiserver "Too long" error to be the only diagnostic.
func TestRootRejectsTooLarge(t *testing.T) {
	t.Parallel()
	big := machineInputs()
	// Comfortably over the 1,000,000-byte cap once base64-encoded (4/3 the
	// raw size).
	big.BootstrapData = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), 800_000))
	_, err := Root(contract.RoleMachine, big)
	if !errors.Is(err, ErrInputsTooLarge) {
		t.Errorf("err = %v, want ErrInputsTooLarge", err)
	}
	// The size reaches captf_inputs_bytes.
	var tooLarge *InputsTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Bytes <= maxInputsBytes || !strings.Contains(err.Error(), "limit 1000000") {
		t.Errorf("err = %v, want the rendered size", err)
	}
	files, err := Root(contract.RoleMachine, machineInputs())
	if err != nil {
		t.Errorf("ordinary inputs rejected: %v", err)
	}
	if files.Size() != len(files.MainTF)+len(files.TFVars) || files.Size() == 0 {
		t.Errorf("Size = %d", files.Size())
	}
}

// TestCLIConfig checks CLIConfig's provider_installation block against a
// fixed rendering for ProvidersDir.
func TestCLIConfig(t *testing.T) {
	t.Parallel()
	want := `provider_installation {
  filesystem_mirror {
    path    = "/captf/providers"
    include = ["*/*/*"]
  }
  direct {
    exclude = ["*/*/*"]
  }
}
`
	if got := string(CLIConfig(ProvidersDir)); got != want {
		t.Errorf("CLIConfig =\n%s\nwant\n%s", got, want)
	}
}

// TestBackendRoot: the restore root declares the same partial backend as
// every rendered root, and nothing else.
func TestBackendRoot(t *testing.T) {
	t.Parallel()
	f := BackendRoot()
	var got, rendered mainTF
	if err := json.Unmarshal(f.MainTF, &got); err != nil {
		t.Fatal(err)
	}
	full, err := Root(contract.RoleCluster, clusterInputs())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(full.MainTF, &rendered); err != nil {
		t.Fatal(err)
	}
	if len(got.Terraform.Backend) != 1 || len(rendered.Terraform.Backend) != 1 || got.Module != nil || got.Variable != nil || got.Output != nil {
		t.Errorf("backend root = %s", f.MainTF)
	}
	for k := range rendered.Terraform.Backend {
		if _, ok := got.Terraform.Backend[k]; !ok {
			t.Errorf("backend root lacks the %s backend", k)
		}
	}
	if !json.Valid(f.TFVars) {
		t.Errorf("tfvars = %q", f.TFVars)
	}
}
