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

package inputs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
)

// poolImage is the module image the rendered pool fixtures carry.
const poolImage = "registry.example/pool:1.0"

// poolInputs returns pool inputs with every kind of field set, user
// variables (one sensitive) included, with each mut applied in order.
func poolInputs(mut ...func(*contract.MachinePoolInputs)) contract.MachinePoolInputs {
	in := contract.MachinePoolInputs{
		CommonInputs: contract.CommonInputs{
			Contract: contract.Version,
			Cluster:  contract.Cluster{Name: "c", Namespace: ns},
			Object:   contract.Object{Kind: "TerraformMachinePool", Name: "mp", Namespace: ns},
			Tags:     contract.Tags("c", ns, "TerraformMachinePool", "mp", ""),
			Variables: contract.Variables{
				"instance_type": {Value: json.RawMessage(`"m5.large"`)},
				"disk_gb":       {Value: json.RawMessage(`100`)},
				"api_token":     {Value: json.RawMessage(`"s3cr3t"`), Sensitive: true},
			},
		},
		ClusterOutputs:        json.RawMessage(`{"network_id":"net-1","subnets":["a","b"],"mtu":1500}`),
		MachinePoolName:       "mp",
		Replicas:              3,
		BootstrapData:         "I2Nsb3VkLWNvbmZpZw==",
		BootstrapFormat:       "cloud-config",
		FailureDomains:        []string{"zone-a"},
		ClusterFailureDomains: []string{"zone-a", "zone-b"},
		KubernetesVersion:     new("v1.36.2"),
		NodeLabels:            map[string]string{"pool": "mp"},
		Autoscaling:           contract.Autoscaling{Enabled: true, Min: 1, Max: 5},
	}
	for _, f := range mut {
		f(&in)
	}
	return in
}

// renderedPool returns the durable inputs an apply of in with image
// leaves, failing t when rendering fails.
func renderedPool(t *testing.T, image string, in contract.MachinePoolInputs) *Durable {
	t.Helper()
	files, err := render.Root(contract.RoleMachinePool, in)
	if err != nil {
		t.Fatal(err)
	}
	return &Durable{Files: files, Meta: Meta{Image: image}}
}

// TestPoolInputsHash proves the inputs hash read back from rendered pool
// files is the one the inputs hashed to when rendered, with or without
// user variables, empty lists and maps, or autoscaling, and that it
// tells other inputs, another image or a variable's sensitivity apart.
// Files that do not parse are an error.
func TestPoolInputsHash(t *testing.T) {
	t.Parallel()
	same := map[string]contract.MachinePoolInputs{
		"full": poolInputs(),
		"no variables, empty lists, autoscaling off": poolInputs(func(in *contract.MachinePoolInputs) {
			in.Variables, in.FailureDomains, in.ClusterFailureDomains, in.NodeLabels = nil, nil, nil, nil
			in.KubernetesVersion, in.Autoscaling = nil, contract.Autoscaling{}
		}),
	}
	for name, in := range same {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want, err := hash.Inputs(contract.RoleMachinePool, poolImage, in)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := PoolInputsHash(renderedPool(t, poolImage, in)); err != nil || got != want {
				t.Errorf("PoolInputsHash = %q, %v; want %s", got, err, want)
			}
		})
	}

	want, err := hash.Inputs(contract.RoleMachinePool, poolImage, poolInputs())
	if err != nil {
		t.Fatal(err)
	}
	other := map[string]*Durable{
		"other exports": renderedPool(t, poolImage, poolInputs(func(in *contract.MachinePoolInputs) {
			in.ClusterOutputs = json.RawMessage(`{"network_id":"net-2"}`)
		})),
		"other image": renderedPool(t, "registry.example/pool:2.0", poolInputs()),
		"variable not sensitive": renderedPool(t, poolImage, poolInputs(func(in *contract.MachinePoolInputs) {
			in.Variables["api_token"] = contract.Variable{Value: json.RawMessage(`"s3cr3t"`)}
		})),
	}
	for name, d := range other {
		if got, err := PoolInputsHash(d); err != nil || got == want {
			t.Errorf("%s: PoolInputsHash = %q, %v; want a hash other than %s", name, got, err, want)
		}
	}

	for name, files := range map[string]render.Files{
		"tfvars": {MainTF: []byte(`{}`), TFVars: []byte(`{`)},
		"root":   {MainTF: []byte(`{`), TFVars: []byte(`{}`)},
	} {
		if _, err := PoolInputsHash(&Durable{Files: files}); err == nil {
			t.Errorf("unparsable %s: no error", name)
		}
	}
}

// TestSeedClusterOutputs proves a seed records the exports and their hash
// as RecordClusterOutputs does, but keeps a pending and a partly applied
// change, writes nothing once a hash is recorded (ErrExportsRecorded),
// records only the hash of exports that do not fit, and reports a missing
// Secret as ErrNotFound.
func TestSeedClusterOutputs(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	m := machine("m1")
	if _, err := SeedClusterOutputs(ctx, c, m, json.RawMessage(`{}`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("seed without a Secret: %v, want ErrNotFound", err)
	}
	if err := Write(ctx, c, m, machineFiles(t, "a"), Meta{Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if err := SetPending(ctx, c, m, Pending{ExportsHash: "h2:new", ApprovalHash: "h2:a", Job: "j1"}); err != nil {
		t.Fatal(err)
	}
	if err := SetPartial(ctx, c, m, Partial{ExportsHash: "h2:new", Job: "j2"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := SeedClusterOutputs(ctx, c, m, json.RawMessage(`{"net":"n-1"}`)); err != nil || !ok {
		t.Fatalf("seed: %v, %v", ok, err)
	}
	d, err := Read(ctx, c, ns, "m", "m1")
	if err != nil || string(d.AppliedClusterOutputs) != `{"net":"n-1"}` || d.AppliedExportsHash != exportsHashOf(t, `{"net":"n-1"}`) ||
		d.Pending == nil || d.Partial == nil {
		t.Fatalf("after seed: applied %s (%s), pending %+v, partial %+v, %v", d.AppliedClusterOutputs, d.AppliedExportsHash, d.Pending, d.Partial, err)
	}
	if _, err := SeedClusterOutputs(ctx, c, m, json.RawMessage(`{"net":"n-2"}`)); !errors.Is(err, ErrExportsRecorded) {
		t.Errorf("second seed: %v, want ErrExportsRecorded", err)
	}
	if d, _ := Read(ctx, c, ns, "m", "m1"); d.AppliedExportsHash != exportsHashOf(t, `{"net":"n-1"}`) {
		t.Errorf("a second seed replaced the record: %s", d.AppliedExportsHash)
	}

	big := machine("m2")
	if err := Write(ctx, c, big, machineFiles(t, "b"), Meta{Image: "img"}); err != nil {
		t.Fatal(err)
	}
	huge := json.RawMessage(`"` + strings.Repeat("x", maxDataBytes) + `"`)
	if ok, err := SeedClusterOutputs(ctx, c, big, huge); err != nil || ok {
		t.Fatalf("oversize seed: %v, %v; want only the hash", ok, err)
	}
	if d, err := Read(ctx, c, ns, "m", "m2"); err != nil || d.AppliedClusterOutputs != nil || d.AppliedExportsHash != exportsHashOf(t, string(huge)) {
		t.Errorf("an oversize seed: %d bytes, hash %s, %v", len(d.AppliedClusterOutputs), d.AppliedExportsHash, err)
	}
}
