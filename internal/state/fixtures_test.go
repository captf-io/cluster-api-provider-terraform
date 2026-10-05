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

package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// Real backend Secrets written by Terraform 1.16.4 and OpenTofu 1.12.6
// (hack/fixtures-state.sh), in namespace "fixtures", for TerraformMachines
// named after each fixture.
const fixtureNamespace = "fixtures"

// fixtureDir holds the backend fixtures. They live in this package's
// testdata, and internal/outputs and internal/locks read them from here
// too, so the real captures exist once.
const fixtureDir = "testdata/fixtures"

// loadFixture decodes testdata/fixtures/<name>.yaml and returns its
// Secrets and Lease; t fails the test if the fixture cannot be read or
// decoded.
func loadFixture(t *testing.T, name string) ([]*corev1.Secret, *coordinationv1.Lease) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, name+".yaml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var secrets []*corev1.Secret
	var lease *coordinationv1.Lease
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
		switch meta.Kind {
		case "Secret":
			s := &corev1.Secret{}
			if err := json.Unmarshal(doc, s); err != nil {
				t.Fatalf("Secret: %v", err)
			}
			secrets = append(secrets, s)
		case "Lease":
			lease = &coordinationv1.Lease{}
			if err := json.Unmarshal(doc, lease); err != nil {
				t.Fatalf("Lease: %v", err)
			}
		default:
			t.Fatalf("unexpected kind %q in fixture", meta.Kind)
		}
	}
	return secrets, lease
}

// fixtureClient returns a fake client seeded with secrets and extra; t
// supplies the test scheme.
func fixtureClient(t *testing.T, secrets []*corev1.Secret, extra ...client.Object) client.Client {
	t.Helper()
	objs := slices.Clone(extra)
	for _, s := range secrets {
		objs = append(objs, s)
	}
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
}

// outputNames returns the sorted names of st's root outputs.
func outputNames(st *State) []string {
	names := make([]string, 0, len(st.Outputs))
	for k := range st.Outputs {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}

// TestFixtures reads every real backend Secret fixture and checks the
// labels, Lease and parsed State (version, serial, lineage, outputs,
// managed resource count and byte size) against what each fixture is known
// to hold.
func TestFixtures(t *testing.T) {
	t.Parallel()
	// failure_domain is declared with value null, and a null root output is
	// not persisted in state by either runtime: it is absent, not null.
	allOutputs := []string{"addresses", "blob", "health", "interruptible", "provider_id"}
	cases := []struct {
		name     string
		owner    string // the TerraformMachine the state belongs to; default name
		secrets  int
		version  string
		outputs  []string
		wantErr  error
		blobSize int
		managed  int
	}{
		{name: "terraform-single", secrets: 1, version: "1.16.4", outputs: allOutputs, managed: 1},
		// base64 of 1600 KiB of random bytes.
		{name: "terraform-chunked", secrets: 2, version: "1.16.4", outputs: allOutputs, managed: 1, blobSize: 4 * ((1600*1024 + 2) / 3)},
		{name: "opentofu-single", secrets: 1, version: "1.12.6", outputs: allOutputs, managed: 1},
		{name: "opentofu-encrypted", secrets: 1, wantErr: ErrStateEncrypted},
		// init alone persists an empty state: valid, no outputs.
		{name: "terraform-initonly", secrets: 1, version: "1.16.4", outputs: []string{}},
		{name: "opentofu-initonly", secrets: 1, version: "1.12.6", outputs: []string{}},
		// Captured while apply waited at its approval prompt with the lock
		// held, and after SIGTERM: still the empty state init wrote (7.03).
		{name: "terraform-locked", secrets: 1, version: "1.16.4", outputs: []string{}},
		{name: "opentofu-locked", secrets: 1, version: "1.12.6", outputs: []string{}},
		{name: "terraform-locked-stopped", owner: "terraform-locked", secrets: 1, version: "1.16.4", outputs: []string{}},
		{name: "opentofu-locked-stopped", owner: "opentofu-locked", secrets: 1, version: "1.12.6", outputs: []string{}},
		// Rewritten after a foreign label and annotation were added (7.03):
		// terraform-relabeled grew from one Secret to two.
		{name: "terraform-relabeled", secrets: 2, version: "1.16.4", outputs: allOutputs, managed: 1, blobSize: 4 * ((1600*1024 + 2) / 3)},
		{name: "terraform-relabeled-chunked", secrets: 2, version: "1.16.4", outputs: allOutputs, managed: 1, blobSize: 4 * ((1600*1024 + 2) / 3)},
		{name: "opentofu-relabeled", secrets: 1, version: "1.12.6", outputs: allOutputs, managed: 1, blobSize: len("rewritten")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			secrets, lease := loadFixture(t, c.name)
			if len(secrets) != c.secrets {
				t.Fatalf("fixture has %d Secrets, want %d", len(secrets), c.secrets)
			}
			// The shell script's suffix equals state.Suffix, and the backend
			// copied our labels map onto every chunk and the Lease.
			owner := c.owner
			if owner == "" {
				owner = c.name
			}
			suffix, err := Suffix(fixtureNamespace, KindTerraformMachine, owner)
			if err != nil {
				t.Fatalf("Suffix: %v", err)
			}
			want := BackendLabels(KindTerraformMachine, owner, "fixture")
			for _, s := range secrets {
				if s.Labels[BackendSuffixLabel] != suffix {
					t.Errorf("%s: suffix label %q, want Suffix() = %q", s.Name, s.Labels[BackendSuffixLabel], suffix)
				}
				for k, v := range want {
					if got, ok := s.Labels[k]; !ok || got != v {
						t.Errorf("%s: label %s = %q, want %q", s.Name, k, got, v)
					}
				}
				if s.Annotations["encoding"] != "gzip" {
					t.Errorf("%s: encoding annotation = %q", s.Name, s.Annotations["encoding"])
				}
			}
			if lease == nil || lease.Name != LeaseName(suffix) || lease.Labels[ManagedLabel] != "true" {
				t.Errorf("Lease = %v, want %s with the backend labels", lease, LeaseName(suffix))
			}

			st, err := NewReader(fixtureClient(t, secrets)).Read(context.Background(), fixtureNamespace, suffix)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("Read: err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if st.TerraformVersion != c.version || st.Serial < 1 || st.Lineage == "" || len(st.Secrets) != c.secrets {
				t.Errorf("State = version %q serial %d lineage %q secrets %v", st.TerraformVersion, st.Serial, st.Lineage, st.Secrets)
			}
			if got := outputNames(st); !slices.Equal(got, c.outputs) {
				t.Errorf("outputs = %v, want %v", got, c.outputs)
			}
			// captf_state_resources and captf_state_bytes.
			wantBytes := 0
			for _, s := range secrets {
				wantBytes += len(s.Data[DataKey])
			}
			if st.ManagedResources != c.managed || st.Bytes != wantBytes || st.Bytes == 0 {
				t.Errorf("managed resources %d, bytes %d; want %d, %d", st.ManagedResources, st.Bytes, c.managed, wantBytes)
			}
			if len(c.outputs) == 0 {
				return
			}
			// health and blob are sensitive in the committed module; blob became
			// sensitive after the first Terraform apply, so this also proves the
			// fixtures match the module.
			if !st.Outputs["health"].Sensitive || !st.Outputs["blob"].Sensitive || st.Outputs["provider_id"].Sensitive {
				t.Errorf("sensitive flags: health %v, blob %v, provider_id %v",
					st.Outputs["health"].Sensitive, st.Outputs["blob"].Sensitive, st.Outputs["provider_id"].Sensitive)
			}
			if string(st.Outputs["provider_id"].Value) != `"fixture://fixture-instance-1"` {
				t.Errorf("provider_id = %s", st.Outputs["provider_id"].Value)
			}
			if _, ok := st.Outputs["failure_domain"]; ok {
				t.Error("a null output was persisted; absent outputs must stay absent")
			}
			var blob string
			if err := json.Unmarshal(st.Outputs["blob"].Value, &blob); err != nil || len(blob) != c.blobSize {
				t.Errorf("blob output has %d characters (err %v), want %d", len(blob), err, c.blobSize)
			}
		})
	}
}

// TestFixtureForeignMetadata pins what Adopt relies on: a state rewrite
// keeps foreign labels and annotations on every Secret and
// Lease that already existed, on both runtimes, but a Terraform -part-N
// Secret created by the rewrite carries only the backend's labels, so Adopt
// must re-label after every apply.
func TestFixtureForeignMetadata(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		fresh string // a chunk created after the relabel; "" if none
	}{
		{"terraform-relabeled", "-part-1"},
		{"terraform-relabeled-chunked", ""},
		{"opentofu-relabeled", ""},
	} {
		secrets, lease := loadFixture(t, tt.name)
		objs := []metav1.Object{lease}
		for _, s := range secrets {
			objs = append(objs, s)
		}
		for _, o := range objs {
			wantKept := tt.fresh == "" || !strings.HasSuffix(o.GetName(), tt.fresh)
			kept := o.GetLabels()["captf.io/foreign"] == "kept" && o.GetAnnotations()["captf.io/foreign-note"] == "kept"
			if kept != wantKept {
				t.Errorf("%s: %s kept foreign metadata = %v, want %v", tt.name, o.GetName(), kept, wantKept)
			}
			if o.GetLabels()[ManagedLabel] != "true" {
				t.Errorf("%s: %s lost the backend labels", tt.name, o.GetName())
			}
		}
	}
}

// TestFixtureChunkedAdoptAndCleanup runs Adopt and Cleanup on real chunks:
// Terraform's -part-1 must get the ownerRef too.
func TestFixtureChunkedAdoptAndCleanup(t *testing.T) {
	t.Parallel()
	secrets, lease := loadFixture(t, "terraform-chunked")
	suffix, _ := Suffix(fixtureNamespace, KindTerraformMachine, "terraform-chunked")
	owner := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Name: "terraform-chunked", Namespace: fixtureNamespace, UID: "uid-9"}}
	c := fixtureClient(t, secrets, owner, lease)
	ctx := context.Background()
	if err := Adopt(ctx, c, owner, suffix, "h1:fixture"); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	st, err := NewReader(c).Read(ctx, fixtureNamespace, suffix)
	if err != nil || st.InputsHash != "h1:fixture" || len(st.Secrets) != 2 {
		t.Fatalf("Read after Adopt = %+v (err %v)", st, err)
	}
	for _, name := range st.Secrets {
		var s corev1.Secret
		if err := c.Get(ctx, name, &s); err != nil || len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != "uid-9" {
			t.Errorf("%s ownerRefs = %v (err %v)", name.Name, s.OwnerReferences, err)
		}
	}
	if err := Cleanup(ctx, c, fixtureNamespace, suffix); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := NewReader(c).Read(ctx, fixtureNamespace, suffix); !errors.Is(err, ErrNoState) {
		t.Errorf("Read after Cleanup: err = %v, want ErrNoState", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(lease), &coordinationv1.Lease{}); err == nil {
		t.Error("Lease survived Cleanup")
	}
}
