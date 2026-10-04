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
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

const (
	// testImage is the spec.source.image used across this package's tests.
	testImage = "ghcr.io/example/machine-module:v1.2.3"
	// goldenMachineHash is the inputs hash of machineFixture with testImage
	// Changing it is a hash-scheme migration: every provisioned object would
	// re-apply. h2 dropped spec.source.command from Hashable.
	goldenMachineHash = "h2:e10a5fcbec6ca2745898f8cae279211c217179b1d90afa7e2ce09fb3e24b64b4"
)

// machineFixture returns a representative contract.MachineInputs, used as
// the base case for the golden hash and the mutation tests below.
func machineFixture() contract.MachineInputs {
	return contract.MachineInputs{
		CommonInputs: contract.CommonInputs{
			Contract: contract.Version,
			Cluster:  contract.Cluster{Name: "prod", Namespace: "team-a"},
			Object:   contract.Object{Kind: "TerraformMachine", Name: "prod-md-0-abcde", Namespace: "team-a"},
			Tags:     contract.Tags("prod", "team-a", "TerraformMachine", "prod-md-0-abcde", "prod-md-0"),
		},
		ClusterOutputs:    json.RawMessage(`{"network_id":"net-1","subnets":["a","b"],"mtu":1500}`),
		MachineName:       "prod-md-0-abcde-xyz12",
		BootstrapData:     "I2Nsb3VkLWNvbmZpZw==",
		BootstrapFormat:   "cloud-config",
		FailureDomain:     new("zone-a"),
		KubernetesVersion: new("v1.36.2"),
		ControlPlane:      false,
	}
}

// clusterFixture returns a representative contract.ClusterInputs, used as
// the base case for the cluster-role hash tests below.
func clusterFixture() contract.ClusterInputs {
	return contract.ClusterInputs{
		CommonInputs: contract.CommonInputs{
			Contract: contract.Version,
			Cluster:  contract.Cluster{Name: "prod", Namespace: "team-a"},
			Object:   contract.Object{Kind: "TerraformCluster", Name: "prod", Namespace: "team-a"},
			Tags:     contract.Tags("prod", "team-a", "TerraformCluster", "prod", ""),
		},
		ControlPlaneEndpoint:    &contract.Endpoint{Host: "api.prod.example.com", Port: 6443},
		KubernetesVersion:       new("v1.36.2"),
		ControlPlaneInitialized: false,
		ClusterNetwork:          &contract.ClusterNetwork{Pods: []string{"192.168.0.0/16"}},
	}
}

// poolFixture returns a representative contract.MachinePoolInputs, used as
// the base case for the machinepool-role hash tests below.
func poolFixture() contract.MachinePoolInputs {
	return contract.MachinePoolInputs{
		CommonInputs: contract.CommonInputs{
			Contract: contract.Version,
			Cluster:  contract.Cluster{Name: "prod", Namespace: "team-a"},
			Object:   contract.Object{Kind: "TerraformMachinePool", Name: "prod-mp-0", Namespace: "team-a"},
			Tags:     contract.Tags("prod", "team-a", "TerraformMachinePool", "prod-mp-0", ""),
		},
		ClusterOutputs:        json.RawMessage(`{"network_id":"net-1","subnets":["a","b"],"mtu":1500}`),
		MachinePoolName:       "prod-mp-0",
		Replicas:              3,
		BootstrapData:         "I2Nsb3VkLWNvbmZpZw==",
		BootstrapFormat:       "cloud-config",
		FailureDomains:        []string{"zone-a"},
		ClusterFailureDomains: []string{"zone-a", "zone-b"},
		KubernetesVersion:     new("v1.36.2"),
		NodeLabels:            map[string]string{"pool": "prod-mp-0"},
		Autoscaling:           contract.Autoscaling{Enabled: true, Min: 1, Max: 5},
	}
}

// poolFixtureDisabled returns poolFixture with autoscaling disabled: every
// leaf, including Replicas, is expected to change the hash when mutated,
// unlike the enabled fixture above.
func poolFixtureDisabled() contract.MachinePoolInputs {
	in := poolFixture()
	in.Autoscaling = contract.Autoscaling{}
	return in
}

// mustInputs calls Inputs with role, image and in, fails t if it errors,
// and returns the resulting hash.
func mustInputs(t *testing.T, role contract.Role, image string, in any) string {
	t.Helper()
	h, err := Inputs(role, image, in)
	if err != nil {
		t.Fatalf("Inputs: %v", err)
	}
	return h
}

// TestGolden pins the hash of a fixed MachineInputs. On a mismatch only the
// hash is printed, never the fixture (bootstrap data is sensitive).
func TestGolden(t *testing.T) {
	t.Parallel()
	got := mustInputs(t, contract.RoleMachine, testImage, machineFixture())
	if got != goldenMachineHash {
		t.Errorf("inputs hash = %s, want %s", got, goldenMachineHash)
	}
	if !strings.HasPrefix(got, Scheme+":") || len(got) != len(Scheme)+1+64 {
		t.Errorf("hash %q is not %s:<64 hex>", got, Scheme)
	}
}

// TestStable: equal inputs built differently hash the same.
func TestStable(t *testing.T) {
	t.Parallel()
	a := machineFixture()
	b := machineFixture()
	// Rebuild the tags map in a different insertion order and re-encode the
	// cluster outputs with other key order and whitespace.
	b.Tags = map[string]string{}
	for _, k := range slices.Backward(contract.TagKeys()) {
		b.Tags[k] = a.Tags[k]
	}
	b.ClusterOutputs = json.RawMessage("{ \"subnets\": [\"a\", \"b\"],\n \"mtu\": 1500, \"network_id\": \"net-1\" }")
	fd, kv := "zone-a", "v1.36.2"
	b.FailureDomain, b.KubernetesVersion = &fd, &kv
	if ha, hb := mustInputs(t, contract.RoleMachine, testImage, a), mustInputs(t, contract.RoleMachine, testImage, b); ha != hb {
		t.Errorf("equal inputs hash differently: %s vs %s", ha, hb)
	}
}

// TestEveryInputChangesTheHash mutates each leaf field of MachineInputs in
// turn, by reflection, so a field added later is covered without editing
// this test.
func TestEveryInputChangesTheHash(t *testing.T) {
	t.Parallel()
	base := mustInputs(t, contract.RoleMachine, testImage, machineFixture())
	var paths []string
	forEachLeaf(reflect.TypeFor[contract.MachineInputs](), nil, func(index []int, name string) {
		paths = append(paths, name)
		in := machineFixture()
		mutate(t, reflect.ValueOf(&in).Elem().FieldByIndex(index), name)
		if got := mustInputs(t, contract.RoleMachine, testImage, in); got == base {
			t.Errorf("changing %s did not change the hash", name)
		}
	})
	if len(paths) < 13 {
		t.Errorf("only %d leaf fields visited: %v", len(paths), paths)
	}

	// The same for every leaf of ClusterInputs, nested pointer structs
	// included (each pointer is mutated once as a whole).
	cluster := clusterFixture()
	clusterBase := mustInputs(t, contract.RoleCluster, testImage, cluster)
	forEachLeaf(reflect.TypeFor[contract.ClusterInputs](), nil, func(index []int, name string) {
		in := clusterFixture()
		mutate(t, reflect.ValueOf(&in).Elem().FieldByIndex(index), name)
		if got := mustInputs(t, contract.RoleCluster, testImage, in); got == clusterBase {
			t.Errorf("changing cluster input %s did not change the hash", name)
		}
	})

	for name, h := range map[string]string{
		"image":  mustInputs(t, contract.RoleMachine, "ghcr.io/example/machine-module:v1.2.4", machineFixture()),
		"digest": mustInputs(t, contract.RoleMachine, "ghcr.io/example/machine-module@sha256:"+strings.Repeat("a", 64), machineFixture()),
		"role":   mustInputs(t, contract.RoleCluster, testImage, machineFixture()),
	} {
		if h == base {
			t.Errorf("changing %s did not change the hash", name)
		}
	}

	// The same for every leaf of MachinePoolInputs, with autoscaling
	// enabled (poolFixture): Replicas is the one documented exception,
	// since HashView zeroes it while Autoscaling.Enabled — the observed
	// count then comes from the module's own refresh, not a user edit, and
	// must not force a re-apply.
	poolBase := mustInputs(t, contract.RoleMachinePool, testImage, poolFixture())
	forEachLeaf(reflect.TypeFor[contract.MachinePoolInputs](), nil, func(index []int, name string) {
		in := poolFixture()
		mutate(t, reflect.ValueOf(&in).Elem().FieldByIndex(index), name)
		got := mustInputs(t, contract.RoleMachinePool, testImage, in)
		if name == "Replicas" {
			if got != poolBase {
				t.Errorf("changing pool input Replicas changed the hash while autoscaling is enabled, want no change")
			}
			return
		}
		if got == poolBase {
			t.Errorf("changing pool input %s did not change the hash", name)
		}
	})

	// The same, autoscaling disabled: every leaf, including Replicas, must
	// change the hash — HashView is then the identity.
	poolDisabledBase := mustInputs(t, contract.RoleMachinePool, testImage, poolFixtureDisabled())
	forEachLeaf(reflect.TypeFor[contract.MachinePoolInputs](), nil, func(index []int, name string) {
		in := poolFixtureDisabled()
		mutate(t, reflect.ValueOf(&in).Elem().FieldByIndex(index), name)
		if got := mustInputs(t, contract.RoleMachinePool, testImage, in); got == poolDisabledBase {
			t.Errorf("changing disabled pool input %s did not change the hash", name)
		}
	})
}

// forEachLeaf walks typ's fields, recursing into every nested struct field
// with prefix as the accumulated field-index path, and calls f for every
// non-struct field it reaches through embedded and nested structs.
func forEachLeaf(typ reflect.Type, prefix []int, f func(index []int, name string)) {
	for i := range typ.NumField() {
		field := typ.Field(i)
		index := append(append([]int{}, prefix...), i)
		if field.Type.Kind() == reflect.Struct {
			forEachLeaf(field.Type, index, f)
			continue
		}
		f(index, field.Name)
	}
}

// mutate changes v, the field named name, to a value different from its
// current one, recursing through pointers and single-field structs; it
// fails t when it does not know how to mutate v's kind.
func mutate(t *testing.T, v reflect.Value, name string) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + "x")
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
			return
		}
		mutate(t, v.Elem(), name)
	case reflect.Map:
		if v.Type() == reflect.TypeFor[contract.Variables]() {
			// The user variables: nil in the fixtures, so adding one
			// must change the hash.
			v.Set(reflect.ValueOf(contract.Variables{"extra": {Value: json.RawMessage(`"x"`)}}))
			return
		}
		v.SetMapIndex(reflect.ValueOf("extra"), reflect.ValueOf("x"))
	case reflect.Slice:
		switch v.Type() {
		case reflect.TypeFor[json.RawMessage]():
			v.Set(reflect.ValueOf(json.RawMessage(`{"changed":true}`)))
		case reflect.TypeFor[[]string]():
			v.Set(reflect.Append(v, reflect.ValueOf("x")))
		default:
			t.Fatalf("no mutation for slice field %s", name)
		}
	case reflect.Int32:
		v.SetInt(v.Int() + 1)
	case reflect.Struct:
		// A pointed-to struct (endpoint, cluster network): change its first field.
		mutate(t, v.Field(0), name)
	default:
		t.Fatalf("no mutation for %s field %s", v.Kind(), name)
	}
}

// TestCanonical checks Canonical's key sorting, HTML-unescaped strings,
// exact preservation of large integers, rejection of fractional and
// exponent numbers with ErrNonInteger, and rejection of NaN and Inf.
func TestCanonical(t *testing.T) {
	t.Parallel()
	got, err := Canonical(map[string]any{"b": 1, "a": []any{"<&>", true, nil}, "c": map[string]any{"y": -2, "x": 0}})
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if want := `{"a":["<&>",true,null],"b":1,"c":{"x":0,"y":-2}}`; string(got) != want {
		t.Errorf("Canonical = %s, want %s", got, want)
	}
	big, err := Canonical(json.RawMessage(`{"n":123456789012345678901234567890}`))
	if err != nil || string(big) != `{"n":123456789012345678901234567890}` {
		t.Errorf("large integer = %s (err %v), want it kept exactly", big, err)
	}
	for name, v := range map[string]any{
		"fraction": 1.5,
		"exponent": json.RawMessage(`1e3`),
		"decimal":  json.RawMessage(`{"x":[2.0]}`),
	} {
		if _, err := Canonical(v); !errors.Is(err, ErrNonInteger) {
			t.Errorf("%s: err = %v, want ErrNonInteger", name, err)
		}
	}
	if _, err := Canonical(math.NaN()); err == nil {
		t.Error("NaN was accepted")
	}
	if _, err := Sum(math.Inf(1)); err == nil {
		t.Error("Inf was accepted")
	}
}

// TestInputsRejectsUnknownRole checks that Inputs errors for a role the
// contract does not know, and that a fractional number in module-authored
// cluster outputs is rejected with ErrNonInteger.
func TestInputsRejectsUnknownRole(t *testing.T) {
	t.Parallel()
	if _, err := Inputs("bogus", testImage, machineFixture()); !errors.Is(err, contract.ErrUnknownRole) {
		t.Errorf("Inputs(bogus) = %v, want ErrUnknownRole", err)
	}
	// A fractional number in module-authored exports makes inputs unhashable.
	in := machineFixture()
	in.ClusterOutputs = json.RawMessage(`{"ratio":0.5}`)
	if _, err := Inputs(contract.RoleMachine, testImage, in); !errors.Is(err, ErrNonInteger) {
		t.Errorf("fractional exports: err = %v, want ErrNonInteger", err)
	}
}

// TestPoolHashViewDisabled proves HashView is the identity when
// autoscaling is disabled: Inputs hashes poolFixtureDisabled exactly as it
// would hash the raw struct with no HashViewer involved at all, i.e. as it
// hashed before HashView existed.
func TestPoolHashViewDisabled(t *testing.T) {
	t.Parallel()
	in := poolFixtureDisabled()
	vars, err := canonicalVariables(contract.VariablesOf(in))
	if err != nil {
		t.Fatalf("canonicalVariables: %v", err)
	}
	want, err := Sum(Hashable{
		Scheme:    Scheme,
		Contract:  contract.Version,
		Role:      contract.RoleMachinePool,
		Image:     testImage,
		Inputs:    in, // the raw struct, not HashView(): must be the same value.
		Variables: vars,
	})
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	if got := mustInputs(t, contract.RoleMachinePool, testImage, in); got != want {
		t.Errorf("disabled pool hash = %s, want %s (HashView must be the identity when disabled)", got, want)
	}
}

// TestPoolHashViewPointer proves Inputs hashes a *MachinePoolInputs
// exactly as the value. HashView has a value receiver, so the pointer form
// satisfies HashViewer too; if it did not, the enabled case's pointer hash
// would include the raw Replicas instead of HashView's zeroed one and
// differ from the value hash.
func TestPoolHashViewPointer(t *testing.T) {
	t.Parallel()
	enabled := poolFixture()
	disabled := poolFixtureDisabled()
	for name, in := range map[string]contract.MachinePoolInputs{"enabled": enabled, "disabled": disabled} {
		value := mustInputs(t, contract.RoleMachinePool, testImage, in)
		pointer := mustInputs(t, contract.RoleMachinePool, testImage, &in)
		if value != pointer {
			t.Errorf("%s: value hash %s != pointer hash %s", name, value, pointer)
		}
	}
}

// TestPoolHashViewIgnoresReplicasOnlyWhenEnabled proves the autoscaled
// hash equals another autoscaled fixture that differs only in Replicas
// (the observed count), and differs from one that differs in Min, Max or
// Enabled.
func TestPoolHashViewIgnoresReplicasOnlyWhenEnabled(t *testing.T) {
	t.Parallel()
	base := poolFixture()
	baseHash := mustInputs(t, contract.RoleMachinePool, testImage, base)

	sameReplicas := base
	sameReplicas.Replicas = 999
	if got := mustInputs(t, contract.RoleMachinePool, testImage, sameReplicas); got != baseHash {
		t.Errorf("enabled: changing Replicas changed the hash: %s != %s", got, baseHash)
	}

	for name, mutant := range map[string]contract.MachinePoolInputs{
		"min":     withAutoscaling(base, contract.Autoscaling{Enabled: true, Min: 2, Max: 5}),
		"max":     withAutoscaling(base, contract.Autoscaling{Enabled: true, Min: 1, Max: 6}),
		"enabled": withAutoscaling(base, contract.Autoscaling{Enabled: false, Min: 1, Max: 5}),
	} {
		if got := mustInputs(t, contract.RoleMachinePool, testImage, mutant); got == baseHash {
			t.Errorf("changing autoscaling.%s did not change the hash", name)
		}
	}

	// Variables still change the enabled hash.
	withVars := base
	withVars.Variables = contract.Variables{"x": {Value: json.RawMessage(`"y"`)}}
	if got := mustInputs(t, contract.RoleMachinePool, testImage, withVars); got == baseHash {
		t.Error("adding a variable did not change the enabled pool hash")
	}
}

// withAutoscaling returns in with Autoscaling replaced by a.
func withAutoscaling(in contract.MachinePoolInputs, a contract.Autoscaling) contract.MachinePoolInputs {
	in.Autoscaling = a
	return in
}
