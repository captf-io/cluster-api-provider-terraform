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

package outputs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// fixtureDir is the backend fixture directory owned by internal/state; it
// is shared so the real captures exist once.
const fixtureDir = "../state/testdata/fixtures"

// readFixture reads <fixtureDir>/<name>.yaml (real Terraform 1.16.4
// and OpenTofu 1.12.6 state) through the real state reader, failing t on
// any error; it returns the decoded state.
func readFixture(t *testing.T, name string) *state.State {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, name+".yaml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var objs []client.Object
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var doc json.RawMessage
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode fixture: %v", err)
		}
		var meta metav1.TypeMeta
		if err := json.Unmarshal(doc, &meta); err != nil {
			t.Fatalf("type meta: %v", err)
		}
		if meta.Kind != "Secret" {
			continue
		}
		s := &corev1.Secret{}
		if err := json.Unmarshal(doc, s); err != nil {
			t.Fatalf("Secret: %v", err)
		}
		objs = append(objs, s)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	suffix, err := state.Suffix("fixtures", state.KindTerraformMachine, name)
	if err != nil {
		t.Fatalf("Suffix: %v", err)
	}
	st, err := state.NewReader(fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()).Read(context.Background(), "fixtures", suffix)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return st
}

// TestFixtureOutputs: real state decodes cleanly. The fixture module sets
// failure_domain to null, which the runtimes do not persist; it must decode
// as a valid null, not as a missing output.
func TestFixtureOutputs(t *testing.T) {
	t.Parallel()
	var first []byte
	for _, name := range []string{"terraform-single", "terraform-chunked", "opentofu-single"} {
		o, res := DecodeMachine(readFixture(t, name))
		if !res.Valid() {
			t.Fatalf("%s: %s: %s", name, res.Reason(), res.Message())
		}
		if o.FailureDomain != nil || o.Interruptible == nil || *o.Interruptible || *o.ProviderID != "fixture://fixture-instance-1" {
			t.Errorf("%s: outputs = %+v", name, o)
		}
		if len(o.Addresses) != 2 || o.Addresses[0].Type != "InternalIP" || o.Addresses[1].Type != "Hostname" {
			t.Errorf("%s: addresses = %v", name, o.Addresses)
		}
		got, err := json.MarshalIndent(o, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if first == nil {
			first = got
		} else if !bytes.Equal(first, got) {
			t.Errorf("%s decodes differently from terraform-single", name)
		}
	}
	golden(t, "machine-outputs.golden.json", first)

	// The empty state init leaves behind has no outputs: health is missing.
	for _, name := range []string{"terraform-initonly", "opentofu-initonly"} {
		if _, res := DecodeMachine(readFixture(t, name)); res.Reason() != infrav1.OutputsMissingReason || res.Missing[0] != "health" {
			t.Errorf("%s: %s %v, want OutputsMissing for health", name, res.Reason(), res.Missing)
		}
	}
}

// TestClusterGolden pins a decoded cluster output set; hack/verify-schemas.sh
// validates both goldens against the role output schemas.
func TestClusterGolden(t *testing.T) {
	t.Parallel()
	o, res := DecodeCluster(stateOf(map[string]string{
		"control_plane_endpoint": `{"host":"api.example.com","port":6443}`,
		"failure_domains":        `[{"name":"zone-a"},{"name":"zone-b","control_plane":false,"attributes":{"rack":"2"}}]`,
		"exports":                `{"network_id":"net-1","subnets":["a","b"]}`,
		"health":                 `{"state":"running","healthy":true}`,
	}))
	if !res.Valid() {
		t.Fatalf("%s", res.Message())
	}
	got, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	golden(t, "cluster-outputs.golden.json", got)
}

// TestPoolGolden pins a decoded machinepool output set; hack/verify-schemas.sh
// validates the golden against the machinepool-outputs schema (it globs
// internal/outputs/testdata/*-outputs*.golden.json).
func TestPoolGolden(t *testing.T) {
	t.Parallel()
	o, res := DecodeMachinePool(stateOf(map[string]string{
		"provider_id":      `"libvirt-group:///pool-1"`,
		"provider_id_list": `["libvirt:///vm-2","libvirt:///vm-1"]`,
		"replicas":         `2`,
		"instances":        `[{"provider_id":"libvirt:///vm-1","state":"running"},{"provider_id":"libvirt:///vm-2","instance_id":"i-2","failure_domain":"zone-a","state":"running"}]`,
		"health":           `{"state":"running","healthy":true}`,
	}))
	if !res.Valid() {
		t.Fatalf("%s", res.Message())
	}
	got, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	golden(t, "machinepool-outputs.golden.json", got)
}

// golden compares got against testdata/name, failing t on a mismatch, or
// writes got as the new golden file when UPDATE_SNAPSHOTS=1 is set.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	got = append(bytes.Clone(got), '\n')
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_SNAPSHOTS") == "1" {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (UPDATE_SNAPSHOTS=1 creates it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file; rerun with UPDATE_SNAPSHOTS=1 and review the diff", name)
	}
}
