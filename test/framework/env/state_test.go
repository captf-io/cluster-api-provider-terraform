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

package env

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
)

// TestStateRoundTrip checks that WriteState and ReadState agree.
func TestStateRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, noops := noopRefs()
	in := &State{
		Cluster: "captf-test-dev", Engine: "podman", Workers: 1, Kubeconfig: "/k",
		Pins: CurrentPins(), TreeID: "abc", ManagerRef: "localhost/captf/manager:abc", NoopImages: noops,
		NodeImages: []NodeImage{{Node: "n", Ref: "r", ID: "sha256:1", RepoTags: []string{"r"}, RepoDigests: []string{"d"}}},
		Timings:    []Timing{{Step: "preflight", Seconds: 0.25}},
		Operation:  "up",
		UpdatedAt:  time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC),
	}
	if err := WriteState(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip:\n in %+v\nout %+v", in, out)
	}
	if out.Version != stateVersion {
		t.Errorf("Version = %d", out.Version)
	}
}

// TestReadStateErrors checks the missing, malformed and old-layout cases.
func TestReadStateErrors(t *testing.T) {
	t.Parallel()
	if _, err := ReadState(t.TempDir()); err == nil {
		t.Error("ReadState of a missing file succeeded")
	}
	for name, content := range map[string]string{
		"malformed": "{",
		"version":   `{"version": 0}`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadState(dir); err == nil {
			t.Errorf("%s: ReadState succeeded", name)
		}
	}
}

// TestWriteStateErrors checks that an unwritable directory fails.
func TestWriteStateErrors(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	if err := WriteState(missing, &State{}); err == nil {
		t.Error("WriteState into a missing directory succeeded")
	}
	if err := writeEnvScript(missing, &State{}); err == nil {
		t.Error("writeEnvScript into a missing directory succeeded")
	}
	// A directory in the way of the rename.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, stateFile, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteState(dir, &State{}); err == nil {
		t.Error("WriteState over a directory succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("temporary file left behind: %v", entries)
	}
}

// TestPinsEqual checks that every field takes part in Equal.
func TestPinsEqual(t *testing.T) {
	t.Parallel()
	if !CurrentPins().Equal(CurrentPins()) {
		t.Fatal("CurrentPins differs from itself")
	}
	for name, mutate := range map[string]func(*Pins){
		"kind":         func(p *Pins) { p.KindVersion = "x" },
		"node":         func(p *Pins) { p.KindNodeImage = "x" },
		"capi":         func(p *Pins) { p.CAPIVersion = "x" },
		"cert-manager": func(p *Pins) { p.CertManagerVersion = "x" },
		"captf":        func(p *Pins) { p.CAPTFVersion = "x" },
		"noop":         func(p *Pins) { p.NoopImages = p.NoopImages[1:] },
	} {
		p := CurrentPins()
		mutate(&p)
		if p.Equal(CurrentPins()) {
			t.Errorf("%s: changed pins compare equal", name)
		}
	}
	if got := len(CurrentPins().NoopImages); got != len(framework.NoopImages()) {
		t.Errorf("CurrentPins has %d noop images", got)
	}
}

// TestScript checks the exported variables and the quoting.
func TestScript(t *testing.T) {
	t.Parallel()
	got := Script(&State{Cluster: "captf-test-dev", Kubeconfig: "/a b/it's/kubeconfig"})
	for _, want := range []string{
		`export KUBECONFIG='/a b/it'\''s/kubeconfig'`,
		"export TERRAFORM_CLUSTER_IMAGE='ghcr.io/captf-io/module-images/noop-cluster:" + framework.NoopVersion + "-terraform@sha256:",
		"export TERRAFORM_MACHINE_IMAGE='ghcr.io/captf-io/module-images/noop-machine:" + framework.NoopVersion + "-terraform@sha256:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("env.sh lacks %q:\n%s", want, got)
		}
	}
}

// TestDigestFinding checks every verdict.
func TestDigestFinding(t *testing.T) {
	t.Parallel()
	noops := []NoopRef{{Ref: "a:t", Pinned: "a@sha256:1"}, {Ref: "b:t", Pinned: "b@sha256:2"}}
	rec := func(ref string, digests ...string) NodeImage {
		return NodeImage{Node: "n", Ref: ref, RepoDigests: digests}
	}
	for name, tc := range map[string]struct {
		records []NodeImage
		want    string
	}{
		"none":    {[]NodeImage{rec("manager:x")}, "unknown"},
		"kept":    {[]NodeImage{rec("a:t", "a@sha256:1"), rec("b:t", "b@sha256:2"), rec("manager:x")}, "kept"},
		"dropped": {[]NodeImage{rec("a:t"), rec("b:t", "b@sha256:other")}, "dropped"},
		"partial": {[]NodeImage{rec("a:t", "a@sha256:1"), rec("b:t")}, "partial (1/2)"},
		"by pin":  {[]NodeImage{rec("a@sha256:1", "a@sha256:1"), rec("b@sha256:2", "b@sha256:2")}, "kept"},
	} {
		if got := DigestFinding(tc.records, noops); got != tc.want {
			t.Errorf("%s: DigestFinding = %q, want %q", name, got, tc.want)
		}
	}
}
