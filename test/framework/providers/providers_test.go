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

package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine/enginetest"
)

// sum returns the hex sha256 of s.
func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// write writes content to path, creating directories, and fails t on error.
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// read returns the content of path, failing t on error.
func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestEnsureCache covers a miss, a hit, a corrupt file and a mismatch.
func TestEnsureCache(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte("good"))
		case "/bad":
			_, _ = w.Write([]byte("evil"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	good := framework.Artifact{Name: "good.yaml", URL: srv.URL + "/ok", SHA256: sum("good")}
	cache := t.TempDir()

	if err := EnsureCache(t.Context(), srv.Client(), cache, []framework.Artifact{good}); err != nil {
		t.Fatalf("miss: %v", err)
	}
	if got := read(t, CachedPath(cache, good)); got != "good" {
		t.Fatalf("content = %q", got)
	}
	if err := EnsureCache(t.Context(), srv.Client(), cache, []framework.Artifact{good}); err != nil {
		t.Fatalf("hit: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hit re-downloaded: %d requests", hits.Load())
	}

	write(t, CachedPath(cache, good), "corrupt")
	if err := EnsureCache(t.Context(), srv.Client(), cache, []framework.Artifact{good}); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if got := read(t, CachedPath(cache, good)); got != "good" || hits.Load() != 2 {
		t.Fatalf("corrupt file not replaced: %q after %d requests", got, hits.Load())
	}

	bad := framework.Artifact{Name: "bad.yaml", URL: srv.URL + "/bad", SHA256: sum("good")}
	err := EnsureCache(t.Context(), srv.Client(), cache, []framework.Artifact{bad})
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") || !strings.HasPrefix(err.Error(), "providers: cache") {
		t.Fatalf("mismatch err = %v", err)
	}
	// Nothing, not even a temp file, is left in the bad artifact's directory.
	if _, err := os.Stat(CachedPath(cache, bad)); err == nil {
		t.Fatal("mismatched download was cached")
	}
	entries, _ := os.ReadDir(filepath.Dir(CachedPath(cache, bad)))
	for _, e := range entries {
		if e.Name() != "good.yaml" {
			t.Fatalf("leftover file %s", e.Name())
		}
	}
}

// TestEnsureCacheErrors covers HTTP status, transport and mkdir failures.
func TestEnsureCacheErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	a := framework.Artifact{Name: "x.yaml", URL: srv.URL + "/x", SHA256: sum("x")}
	if err := EnsureCache(t.Context(), srv.Client(), t.TempDir(), []framework.Artifact{a}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404 err = %v", err)
	}
	srv.Close()
	if err := EnsureCache(t.Context(), srv.Client(), t.TempDir(), []framework.Artifact{a}); err == nil {
		t.Fatal("closed server: want error")
	}
	if err := EnsureCache(t.Context(), http.DefaultClient, t.TempDir(), []framework.Artifact{{Name: "x", URL: "://bad", SHA256: sum("x")}}); err == nil {
		t.Fatal("bad URL: want error")
	}
	blocker := filepath.Join(t.TempDir(), "file")
	write(t, blocker, "")
	if err := EnsureCache(t.Context(), http.DefaultClient, filepath.Join(blocker, "sub"), []framework.Artifact{a}); err == nil {
		t.Fatal("unwritable cache: want error")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := EnsureCache(ctx, http.DefaultClient, t.TempDir(), []framework.Artifact{a}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled err = %v", err)
	}
}

// TestCachedPath checks the content-addressed layout and DefaultCacheDir.
func TestCachedPath(t *testing.T) {
	t.Parallel()
	a := framework.Artifact{Name: "f.yaml", SHA256: "abc"}
	if got, want := CachedPath("/c", a), "/c/abc/f.yaml"; got != want {
		t.Fatalf("CachedPath = %q, want %q", got, want)
	}
	if d, err := DefaultCacheDir(); err == nil && filepath.Base(d) != "captf-testenv" {
		t.Fatalf("DefaultCacheDir = %q", d)
	}
}

// TestRenderCAPTF checks the make command line and error wrapping.
func TestRenderCAPTF(t *testing.T) {
	t.Parallel()
	line := "make manifests-release RELEASE_IMG=localhost/captf/manager:abc RELEASE_DIR=/out"
	fake := enginetest.New().On(line, "", nil)
	if err := RenderCAPTF(t.Context(), fake, "localhost/captf/manager:abc", "/out"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.Lines(), []string{line}) {
		t.Fatalf("lines = %q", fake.Lines())
	}
	fail := enginetest.New().On(line, "", enginetest.Exit(2, "boom"))
	if err := RenderCAPTF(t.Context(), fail, "localhost/captf/manager:abc", "/out"); err == nil || !strings.HasPrefix(err.Error(), "providers: render:") {
		t.Fatalf("err = %v", err)
	}
	if err := RenderCAPTF(t.Context(), fake, "", "/out"); err == nil {
		t.Fatal("empty image: want error")
	}
}

// fixture fails t on error; it caches fake artifacts for every pin in the
// cache directory it returns, and returns captf, a directory holding a
// fake rendered CAPTF provider.
func fixture(t *testing.T) (cache, captf string) {
	t.Helper()
	cache, captf = t.TempDir(), t.TempDir()
	for _, a := range framework.Artifacts() {
		path := CachedPath(cache, a)
		write(t, path, "content of "+a.Name)
	}
	write(t, filepath.Join(captf, "infrastructure-components.yaml"), "components")
	write(t, filepath.Join(captf, "metadata.yaml"), "meta")
	write(t, filepath.Join(captf, "cluster-template.yaml"), "template")
	if err := os.Mkdir(filepath.Join(captf, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	return cache, captf
}

// TestWriteRepository checks the exact layout and clusterctl.yaml.
func TestWriteRepository(t *testing.T) {
	t.Parallel()
	cache, captf := fixture(t)
	repo := filepath.Join(t.TempDir(), "repo")
	cfg, err := WriteRepository(cache, captf, repo, CAPTFVersion)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != filepath.Join(repo, "clusterctl.yaml") {
		t.Fatalf("configPath = %q", cfg)
	}
	v := framework.CAPIVersion
	files := map[string]string{
		"cluster-api/" + v + "/core-components.yaml":                          "content of core-components.yaml",
		"cluster-api/" + v + "/metadata.yaml":                                 "content of metadata.yaml",
		"bootstrap-kubeadm/" + v + "/bootstrap-components.yaml":               "content of bootstrap-components.yaml",
		"bootstrap-kubeadm/" + v + "/metadata.yaml":                           "content of metadata.yaml",
		"control-plane-kubeadm/" + v + "/control-plane-components.yaml":       "content of control-plane-components.yaml",
		"control-plane-kubeadm/" + v + "/metadata.yaml":                       "content of metadata.yaml",
		"infrastructure-terraform/v0.1.0/infrastructure-components.yaml":      "components",
		"infrastructure-terraform/v0.1.0/metadata.yaml":                       "meta",
		"infrastructure-terraform/v0.1.0/cluster-template.yaml":               "template",
		"cert-manager/" + framework.CertManagerVersion + "/cert-manager.yaml": "content of cert-manager.yaml",
	}
	var got []string
	err = filepath.WalkDir(repo, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(repo, p)
			got = append(got, rel)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"clusterctl.yaml"}
	for p, content := range files {
		want = append(want, p)
		if c := read(t, filepath.Join(repo, p)); c != content {
			t.Errorf("%s = %q, want %q", p, c, content)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("layout:\n got %q\nwant %q", got, want)
	}

	wantCfg := `providers:
- name: "cluster-api"
  type: CoreProvider
  url: "file://` + repo + `/cluster-api/` + v + `/core-components.yaml"
- name: "kubeadm"
  type: BootstrapProvider
  url: "file://` + repo + `/bootstrap-kubeadm/` + v + `/bootstrap-components.yaml"
- name: "kubeadm"
  type: ControlPlaneProvider
  url: "file://` + repo + `/control-plane-kubeadm/` + v + `/control-plane-components.yaml"
- name: "terraform"
  type: InfrastructureProvider
  url: "file://` + repo + `/infrastructure-terraform/v0.1.0/infrastructure-components.yaml"
cert-manager:
  url: "file://` + repo + `/cert-manager/` + framework.CertManagerVersion + `/cert-manager.yaml"
  version: "` + framework.CertManagerVersion + `"
`
	if c := read(t, cfg); c != wantCfg {
		t.Fatalf("clusterctl.yaml:\n%s\nwant:\n%s", c, wantCfg)
	}
}

// TestWriteRepositoryRelative checks a relative repoDir becomes absolute.
func TestWriteRepositoryRelative(t *testing.T) {
	t.Parallel()
	cache, captf := fixture(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := WriteRepository(cache, captf, rel, CAPTFVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg) || !strings.Contains(read(t, cfg), "url: \"file:///") {
		t.Fatalf("not absolute: %s", cfg)
	}
}

// TestWriteRepositoryErrors covers a missing cache entry, a missing
// rendered file and an empty CAPTF directory.
func TestWriteRepositoryErrors(t *testing.T) {
	t.Parallel()
	cache, captf := fixture(t)
	if _, err := WriteRepository(t.TempDir(), captf, t.TempDir(), CAPTFVersion); err == nil || !strings.HasPrefix(err.Error(), "providers: write repository:") {
		t.Fatalf("empty cache err = %v", err)
	}
	if _, err := WriteRepository(cache, filepath.Join(captf, "missing"), t.TempDir(), CAPTFVersion); err == nil {
		t.Fatal("missing captf dir: want error")
	}
	if err := os.Remove(filepath.Join(captf, "metadata.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteRepository(cache, captf, t.TempDir(), CAPTFVersion); err == nil || !strings.Contains(err.Error(), "lacks metadata.yaml") {
		t.Fatalf("missing metadata err = %v", err)
	}
	// Dropping the cert-manager entry only fails at the last step.
	cache2, captf2 := fixture(t)
	if err := os.Remove(CachedPath(cache2, framework.CertManagerManifest)); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteRepository(cache2, captf2, t.TempDir(), CAPTFVersion); err == nil || !strings.Contains(err.Error(), "cert-manager") {
		t.Fatalf("missing cert-manager err = %v", err)
	}
}

// TestInitArgs checks the exact argv.
func TestInitArgs(t *testing.T) {
	t.Parallel()
	got := InitArgs("/k", "/c.yaml", "v0.1.0")
	want := []string{
		"init", "--kubeconfig", "/k", "--config", "/c.yaml",
		"--core", "cluster-api:v1.14.2", "--bootstrap", "kubeadm:v1.14.2",
		"--control-plane", "kubeadm:v1.14.2", "--infrastructure", "terraform:v0.1.0",
		"--wait-providers",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("args = %q\nwant %q", got, want)
	}
}

// TestInitEnv checks the isolated environment.
func TestInitEnv(t *testing.T) {
	t.Parallel()
	want := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/h", "XDG_CONFIG_HOME=/h/.config", "KUBECONFIG=/k",
		"CLUSTER_TOPOLOGY=true", "EXP_MACHINE_POOL=true", "GOPROXY=off", "CLUSTERCTL_DISABLE_VERSIONCHECK=true",
	}
	if got := InitEnv("/k", "/h"); !slices.Equal(got, want) {
		t.Fatalf("env = %q", got)
	}
}

// TestInit runs Init over the fake and over a real OSRunner whose
// "clusterctl" prints its environment, to prove the isolation is applied.
func TestInit(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "home")
	line := "/bin/clusterctl " + strings.Join(InitArgs("/k", "/c", CAPTFVersion), " ")
	fake := enginetest.New().On(line, "", nil)
	if err := Init(t.Context(), fake, "/bin/clusterctl", "/k", "/c", home, CAPTFVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("home not created: %v", err)
	}
	fail := enginetest.New().On(line, "", enginetest.Exit(1, "no cluster"))
	if err := Init(t.Context(), fail, "/bin/clusterctl", "/k", "/c", home, CAPTFVersion); err == nil || !strings.HasPrefix(err.Error(), "providers: init:") {
		t.Fatalf("err = %v", err)
	}
	blocker := filepath.Join(t.TempDir(), "f")
	write(t, blocker, "")
	if err := Init(t.Context(), fake, "/bin/clusterctl", "/k", "/c", filepath.Join(blocker, "h"), CAPTFVersion); err == nil {
		t.Fatal("unwritable home: want error")
	}

	// A fake clusterctl script that dumps its environment into a file.
	out := filepath.Join(t.TempDir(), "env.txt")
	script := filepath.Join(t.TempDir(), "clusterctl")
	write(t, script, "#!/bin/sh\nenv > "+out+"\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]engine.Runner{
		"value":   engine.OSRunner{Env: []string{"LEAK=1"}},
		"pointer": &engine.OSRunner{Env: []string{"LEAK=1"}},
	} {
		if err := Init(t.Context(), r, script, "/k", "/c", home, CAPTFVersion); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		env := read(t, out)
		if strings.Contains(env, "LEAK=1") || !strings.Contains(env, "KUBECONFIG=/k") || !strings.Contains(env, "HOME="+home) {
			t.Fatalf("%s: env not isolated:\n%s", name, env)
		}
	}
}
