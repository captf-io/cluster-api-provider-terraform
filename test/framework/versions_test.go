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

package framework_test

import (
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
)

// The well-formedness patterns every pin must match.
var (
	// sha256Hex is a bare lowercase hex sha256.
	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// digest is an OCI sha256 digest.
	digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// semver is a v-prefixed release version.
	semver = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
)

// checkHTTPS fails t unless raw is an absolute https URL whose last path
// element is wantFile.
func checkHTTPS(t *testing.T, raw, wantFile string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("URL %q: %v", raw, err)
	}
	if u.Scheme != "https" || u.Host == "" {
		t.Errorf("URL %q: want an absolute https URL", raw)
	}
	if got := path.Base(u.Path); got != wantFile {
		t.Errorf("URL %q: file is %q, want %q", raw, got, wantFile)
	}
}

// TestVersions checks the version constants are v-prefixed releases.
func TestVersions(t *testing.T) {
	t.Parallel()
	for name, v := range map[string]string{
		"KindVersion":        framework.KindVersion,
		"KubernetesVersion":  framework.KubernetesVersion,
		"CAPIVersion":        framework.CAPIVersion,
		"CertManagerVersion": framework.CertManagerVersion,
	} {
		if !semver.MatchString(v) {
			t.Errorf("%s = %q, want vX.Y.Z", name, v)
		}
	}
	if !strings.HasPrefix(framework.KubernetesVersion, "v1.36.") {
		t.Errorf("KubernetesVersion = %q, want v1.36.x to match the product's client-go v0.36", framework.KubernetesVersion)
	}
}

// TestKindVersionMatchesGoMod checks KindVersion is the sigs.k8s.io/kind
// version test/go.mod requires.
func TestKindVersionMatchesGoMod(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatalf("read test/go.mod: %v", err)
	}
	// Either a line in a require block or a single-line require.
	want := "sigs.k8s.io/kind " + framework.KindVersion
	for line := range strings.Lines(string(b)) {
		line = strings.TrimSpace(line)
		if strings.TrimPrefix(line, "require ") == want {
			return
		}
	}
	t.Errorf("test/go.mod does not require %q", want)
}

// TestKindNodeImage checks the node image is kindest/node at
// KubernetesVersion with a digest.
func TestKindNodeImage(t *testing.T) {
	t.Parallel()
	ref, dgst, ok := strings.Cut(framework.KindNodeImage, "@")
	if !ok {
		t.Fatalf("KindNodeImage %q has no digest", framework.KindNodeImage)
	}
	if want := "docker.io/kindest/node:" + framework.KubernetesVersion; ref != want {
		t.Errorf("KindNodeImage ref = %q, want %q", ref, want)
	}
	if !digest.MatchString(dgst) {
		t.Errorf("KindNodeImage digest = %q, want sha256:<64 hex>", dgst)
	}
}

// TestArtifacts checks every download has a name, an https URL ending in
// that name and a well-formed sha256, and that names and hashes are unique.
func TestArtifacts(t *testing.T) {
	t.Parallel()
	all := framework.Artifacts()
	if got, want := len(all), len(framework.CAPIArtifacts())+1; got != want {
		t.Fatalf("Artifacts() has %d entries, want %d", got, want)
	}
	names := map[string]bool{}
	sums := map[string]bool{}
	for _, a := range all {
		t.Run(a.Name, func(t *testing.T) {
			t.Parallel()
			if a.Name == "" {
				t.Fatal("empty Name")
			}
			checkHTTPS(t, a.URL, a.Name)
			if !sha256Hex.MatchString(a.SHA256) {
				t.Errorf("SHA256 = %q, want 64 lowercase hex", a.SHA256)
			}
		})
		if names[a.Name] || sums[a.SHA256] {
			t.Errorf("duplicate artifact %q (%s)", a.Name, a.SHA256)
		}
		names[a.Name], sums[a.SHA256] = true, true
	}
	for _, a := range framework.CAPIArtifacts() {
		if !strings.Contains(a.URL, "/cluster-api/releases/download/"+framework.CAPIVersion+"/") {
			t.Errorf("%s URL %q is not from the CAPI %s release", a.Name, a.URL, framework.CAPIVersion)
		}
	}
	if !strings.Contains(framework.CertManagerManifest.URL, "/download/"+framework.CertManagerVersion+"/") {
		t.Errorf("cert-manager URL %q is not from release %s", framework.CertManagerManifest.URL, framework.CertManagerVersion)
	}
}

// TestCAPIProviders checks the three providers are core, bootstrap and
// control plane at CAPIVersion, each with a components file and metadata.
func TestCAPIProviders(t *testing.T) {
	t.Parallel()
	want := []struct{ name, typ, label, components string }{
		{"cluster-api", "CoreProvider", "cluster-api", "core-components.yaml"},
		{"kubeadm", "BootstrapProvider", "bootstrap-kubeadm", "bootstrap-components.yaml"},
		{"kubeadm", "ControlPlaneProvider", "control-plane-kubeadm", "control-plane-components.yaml"},
	}
	got := framework.CAPIProviders()
	if len(got) != len(want) {
		t.Fatalf("CAPIProviders() has %d entries, want %d", len(got), len(want))
	}
	for i, w := range want {
		p := got[i]
		if p.Name != w.name || p.Type != w.typ || p.Label != w.label || p.Components.Name != w.components {
			t.Errorf("provider %d = {%s %s %s %s}, want {%s %s %s %s}", i,
				p.Name, p.Type, p.Label, p.Components.Name, w.name, w.typ, w.label, w.components)
		}
		if p.Version != framework.CAPIVersion {
			t.Errorf("provider %s version = %q, want %q", p.Label, p.Version, framework.CAPIVersion)
		}
		if p.Metadata != framework.CAPIMetadata {
			t.Errorf("provider %s metadata = %+v, want CAPIMetadata", p.Label, p.Metadata)
		}
	}
}

// TestNoopImages checks all six role and runtime pairs are pinned once,
// with a readable ref, a digest and a tagless pull reference.
func TestNoopImages(t *testing.T) {
	t.Parallel()
	roles := []framework.NoopRole{framework.RoleCluster, framework.RoleMachine, framework.RoleMachinePool}
	runtimes := []framework.NoopRuntime{framework.RuntimeTerraform, framework.RuntimeOpenTofu}
	if got, want := len(framework.NoopImages()), len(roles)*len(runtimes); got != want {
		t.Fatalf("NoopImages() has %d entries, want %d", got, want)
	}
	digests := map[string]bool{}
	for _, role := range roles {
		for _, rt := range runtimes {
			img, ok := framework.NoopImageFor(role, rt)
			if !ok {
				t.Errorf("no noop image for %s/%s", role, rt)
				continue
			}
			repo := "ghcr.io/captf-io/module-images/noop-" + string(role)
			if img.Repository != repo || img.Ref != repo+":"+framework.NoopVersion+"-"+string(rt) {
				t.Errorf("%s/%s: Repository %q Ref %q", role, rt, img.Repository, img.Ref)
			}
			if !digest.MatchString(img.Digest) {
				t.Errorf("%s/%s: Digest = %q, want sha256:<64 hex>", role, rt, img.Digest)
			}
			if want := repo + "@" + img.Digest; img.Pinned() != want {
				t.Errorf("%s/%s: Pinned() = %q, want %q", role, rt, img.Pinned(), want)
			}
			if digests[img.Digest] {
				t.Errorf("%s/%s: digest %s is pinned twice", role, rt, img.Digest)
			}
			digests[img.Digest] = true
		}
	}
	if _, ok := framework.NoopImageFor("bogus", framework.RuntimeTerraform); ok {
		t.Error("NoopImageFor(bogus) reported ok")
	}
}

// TestNames checks the shared safety names agree with each other.
func TestNames(t *testing.T) {
	t.Parallel()
	if !strings.HasPrefix(framework.DefaultClusterName, framework.ClusterNamePrefix) {
		t.Errorf("DefaultClusterName %q lacks prefix %q", framework.DefaultClusterName, framework.ClusterNamePrefix)
	}
	if strings.HasPrefix(framework.ProtectedClusterName, framework.ClusterNamePrefix) {
		t.Errorf("ProtectedClusterName %q must not carry the test prefix", framework.ProtectedClusterName)
	}
	if framework.KindNetwork == "kind" || framework.KindNetwork == "" {
		t.Errorf("KindNetwork = %q, want a dedicated network", framework.KindNetwork)
	}
}
