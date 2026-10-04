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

package env

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine/enginetest"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/providers"
)

// fakeHostOver returns the real Host over the fake runner r, and its
// output buffer.
func fakeHostOver(r engine.Runner) (*osHost, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return &osHost{runner: r, clusterctl: "/bin/clusterctl", http: &http.Client{Transport: failTransport{}}, out: out}, out
}

// failTransport is an http.RoundTripper that fails every request.
type failTransport struct{}

// RoundTrip returns errBoom for the request; it never touches the network.
func (failTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, errBoom }

// TestNewHost checks that the real Host runs in the repository root with
// an explicit KUBECONFIG.
func TestNewHost(t *testing.T) {
	t.Parallel()
	c := Config{Name: framework.DefaultClusterName, RepoRoot: "/repo", Clusterctl: "/c"}
	h, ok := NewHost(c, nil).(*osHost)
	if !ok {
		t.Fatal("NewHost did not return an *osHost")
	}
	r, ok := h.runner.(engine.OSRunner)
	if !ok || r.Dir != "/repo" || !slices.Contains(r.Env, "KUBECONFIG="+c.Kubeconfig()) || h.clusterctl != "/c" {
		t.Errorf("host: %+v", h)
	}
}

// TestChildEnv checks that KUBECONFIG is forced and make's variables are
// dropped.
func TestChildEnv(t *testing.T) {
	t.Parallel()
	got := childEnv([]string{"PATH=/bin", "KUBECONFIG=/home/me/.kube/config", "MAKEFLAGS=-j4", "MAKELEVEL=1", "MFLAGS=x", "HOME=/h"}, "/w/kubeconfig")
	want := []string{"PATH=/bin", "HOME=/h", "KUBECONFIG=/w/kubeconfig"}
	if !slices.Equal(got, want) {
		t.Errorf("childEnv = %v, want %v", got, want)
	}
}

// TestHostCommands checks the commands each Host method runs.
func TestHostCommands(t *testing.T) {
	t.Parallel()
	r := enginetest.New().
		On("podman info", "", nil).
		On("git rev-parse --short=12 HEAD", "abc\n", nil).
		On("git diff HEAD", "", nil).
		On("git ls-files --others --exclude-standard", "", nil).
		On("git show -s --format=%ct HEAD", "1700000000\n", nil).
		On("make docker-build CONTAINER_TOOL=podman IMG=localhost/captf/manager:abc", "", nil).
		On("podman run --rm --pull=never --entrypoint /manager localhost/captf/manager:abc --help", "", nil).
		On("podman run --rm --pull=never --entrypoint /runner localhost/captf/manager:abc --help", "", nil).
		On("make manifests-release RELEASE_IMG=localhost/captf/manager:abc RELEASE_DIR=/out", "", nil)
	h, _ := fakeHostOver(r)
	ctx := context.Background()
	e, err := h.DetectEngine(ctx, "")
	if err != nil || e.Name != engine.Podman {
		t.Fatalf("DetectEngine = %v, %v", e, err)
	}
	if id, err := h.TreeID(ctx); err != nil || id != "abc" {
		t.Errorf("TreeID = %q, %v", id, err)
	}
	if err := h.BuildManager(ctx, e, "localhost/captf/manager:abc"); err != nil {
		t.Errorf("BuildManager: %v", err)
	}
	if err := h.SmokeManager(ctx, e, "localhost/captf/manager:abc"); err != nil {
		t.Errorf("SmokeManager: %v", err)
	}
	if err := h.RenderCAPTF(ctx, "localhost/captf/manager:abc", "/out"); err != nil {
		t.Errorf("RenderCAPTF: %v", err)
	}
	r.On("/bin/clusterctl "+strings.Join(providers.InitArgs("/k", "/cfg", providers.CAPTFVersion), " "), "", nil)
	if err := h.InitProviders(ctx, "/k", "/cfg", filepath.Join(t.TempDir(), "home")); err != nil {
		t.Errorf("InitProviders: %v", err)
	}
}

// TestBuildStamp checks the DATE override, that VERSION is left to make's
// default, and the errors.
func TestBuildStamp(t *testing.T) {
	t.Parallel()
	r := enginetest.New().On("git show -s --format=%ct HEAD", "1700000000\n", nil)
	h, _ := fakeHostOver(r)
	got, err := h.buildStamp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"DATE=2023-11-14T22:13:20Z"}
	if !slices.Equal(got, want) {
		t.Errorf("buildStamp = %v, want %v", got, want)
	}
	bad := enginetest.New().On("git show -s --format=%ct HEAD", "soon\n", nil)
	hb, _ := fakeHostOver(bad)
	if err := hb.BuildManager(context.Background(), &engine.Engine{Name: engine.Podman, Runner: bad}, "m:t"); err == nil {
		t.Error("BuildManager with an unparsable commit time succeeded")
	}
	broken := enginetest.New()
	hx, _ := fakeHostOver(broken)
	if _, err := hx.buildStamp(context.Background()); err == nil {
		t.Error("buildStamp with a failing git succeeded")
	}
}

// TestEnsureCacheFails checks that a failing download is reported.
func TestEnsureCacheFails(t *testing.T) {
	t.Parallel()
	h, _ := fakeHostOver(enginetest.New())
	if err := h.EnsureCache(context.Background(), t.TempDir()); !errors.Is(err, errBoom) {
		t.Errorf("EnsureCache: got %v, want errBoom", err)
	}
}

// TestWriteRepository checks that the Host writes the repository from a
// filled cache.
func TestWriteRepository(t *testing.T) {
	t.Parallel()
	h, _ := fakeHostOver(enginetest.New())
	cache, captf, repo := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "repo")
	for _, a := range framework.Artifacts() {
		p := providers.CachedPath(cache, a)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(a.Name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"infrastructure-components.yaml", "metadata.yaml"} {
		if err := os.WriteFile(filepath.Join(captf, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := h.WriteRepository(cache, captf, repo)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != filepath.Join(repo, "clusterctl.yaml") {
		t.Errorf("config path = %s", cfg)
	}
}

// TestRemoveNetwork checks that only an exactly named, listed network is
// removed.
func TestRemoveNetwork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		list    string
		listErr error
		rmErr   error
		removed bool
		wantErr bool
	}{
		"listed":     {list: "podman\ncaptf-test\nkind\n", removed: true},
		"absent":     {list: "podman\ncaptf-test-old\nkind\n"},
		"list fails": {listErr: errBoom, wantErr: true},
		"rm fails":   {list: "captf-test\n", rmErr: errBoom, removed: true, wantErr: true},
	} {
		r := enginetest.New().
			On("podman network ls --format {{.Name}}", tc.list, tc.listErr).
			On("podman network rm captf-test", "", tc.rmErr)
		h, out := fakeHostOver(r)
		err := h.RemoveNetwork(ctx, &engine.Engine{Name: engine.Podman, Runner: r}, framework.KindNetwork)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, want error %v", name, err, tc.wantErr)
		}
		ran := slices.ContainsFunc(r.Calls(), func(c enginetest.Call) bool { return c.Line() == "podman network rm captf-test" })
		if ran != tc.removed {
			t.Errorf("%s: rm ran = %v, want %v (%s)", name, ran, tc.removed, out)
		}
	}
}
