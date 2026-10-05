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

package kindcluster

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodes"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
)

// errBoom is the error the fake provider returns on request.
var errBoom = errors.New("boom")

// fakeNode is a nodes.Node that only carries a name.
type fakeNode struct {
	nodes.Node
	name string
}

// String returns the node name.
func (n fakeNode) String() string { return n.name }

// fakeProvider is a Provider that records calls and spawns nothing.
type fakeProvider struct {
	mu       sync.Mutex
	clusters []string
	err      error
	// netEnv is the network variable observed inside Create.
	netEnvName string
	netEnv     string
	created    []string
	deleted    []string
	logsDir    string
	internal   bool
	createOpts int
}

// Create records name, the number of options and the network variable's
// value, and returns the configured error, if any.
func (f *fakeProvider) Create(name string, options ...cluster.CreateOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.netEnv = os.Getenv(f.netEnvName)
	f.createOpts = len(options)
	if f.err != nil {
		return f.err
	}
	f.created = append(f.created, name)
	f.clusters = append(f.clusters, name)
	return nil
}

// Delete records name and returns the configured error, if any.
func (f *fakeProvider) Delete(name, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	return f.err
}

// List returns the configured clusters.
func (f *fakeProvider) List() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.clusters), f.err
}

// KubeConfig records internal and returns a fixed document and the
// configured error, if any.
func (f *fakeProvider) KubeConfig(_ string, internal bool) (string, error) {
	f.internal = internal
	return "kubeconfig-body", f.err
}

// ListNodes returns one node named after name, and the configured error,
// if any.
func (f *fakeProvider) ListNodes(name string) ([]nodes.Node, error) {
	return []nodes.Node{fakeNode{name: name + "-control-plane"}}, f.err
}

// CollectLogs records dir and returns the configured error, if any.
func (f *fakeProvider) CollectLogs(_, dir string) error {
	f.logsDir = dir
	return f.err
}

// newManager returns a Manager over a fresh fake for engine name n, with
// the clusters in existing already present, and that fake. It fails t if
// New fails.
func newManager(t *testing.T, n engine.Name, existing ...string) (*Manager, *fakeProvider) {
	t.Helper()
	e := &engine.Engine{Name: n}
	fp := &fakeProvider{clusters: existing, netEnvName: e.KindNetworkEnv()}
	m, err := New(e, WithProvider(fp))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m, fp
}

// TestValidateName checks the name guard, including the protected names.
func TestValidateName(t *testing.T) {
	t.Parallel()
	good := []string{"captf-test-dev", "captf-test-a", "captf-test-1"}
	for _, n := range good {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	bad := map[string]string{
		"kube-php-client":                       "operator's own",
		"kind":                                  "must start with",
		"":                                      "must start with",
		"captf-test-":                           "suffix",
		"captf-test":                            "must start with",
		"CAPTF-test-x":                          "must start with",
		"captf-test-Dev":                        "DNS-1123",
		"captf-test-a_b":                        "DNS-1123",
		"captf-test-a-":                         "DNS-1123",
		"captf-test-" + strings.Repeat("a", 60): "longer than",
		"other-captf-test-x":                    "must start with",
	}
	for n, want := range bad {
		err := ValidateName(n)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateName(%q) = %v, want error containing %q", n, err, want)
		}
	}
}

// TestRenderConfig checks nodes, image and networking, with and without
// defaults.
func TestRenderConfig(t *testing.T) {
	t.Parallel()
	c, err := RenderConfig(Options{Name: "captf-test-x", Workers: 2, NodeImage: "img:1"})
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	if c.Name != "captf-test-x" || c.Kind != "Cluster" || len(c.Nodes) != 3 {
		t.Fatalf("config = %+v", c)
	}
	if c.Nodes[0].Role != "control-plane" || c.Nodes[1].Role != "worker" || c.Nodes[2].Role != "worker" {
		t.Errorf("roles = %v", c.Nodes)
	}
	for _, n := range c.Nodes {
		if n.Image != "img:1" {
			t.Errorf("node image = %q", n.Image)
		}
	}
	if c.Networking.APIServerAddress != "127.0.0.1" || c.Networking.IPFamily != "ipv4" {
		t.Errorf("networking = %+v", c.Networking)
	}

	d, err := RenderConfig(Options{Name: framework.DefaultClusterName})
	if err != nil {
		t.Fatalf("RenderConfig default: %v", err)
	}
	if len(d.Nodes) != 1 || d.Nodes[0].Image != framework.KindNodeImage {
		t.Errorf("default config nodes = %+v", d.Nodes)
	}

	if _, err := RenderConfig(Options{Name: "kind"}); err == nil {
		t.Error("RenderConfig accepted a bad name")
	}
	if _, err := RenderConfig(Options{Name: "captf-test-x", Workers: -1}); err == nil {
		t.Error("RenderConfig accepted negative workers")
	}
}

// TestCreate checks a successful Create and the network variable.
func TestCreate(t *testing.T) {
	t.Parallel()
	m, fp := newManager(t, engine.Podman)
	err := m.Create(context.Background(), Options{Name: "captf-test-a", KubeconfigPath: "/tmp/kc"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fp.netEnv != framework.KindNetwork {
		t.Errorf("network env during Create = %q, want %q", fp.netEnv, framework.KindNetwork)
	}
	if fp.createOpts != 6 {
		t.Errorf("create options = %d, want 6", fp.createOpts)
	}
	if !slices.Equal(fp.created, []string{"captf-test-a"}) {
		t.Errorf("created = %v", fp.created)
	}
}

// TestCreateDockerNetworkOverride checks the docker variable name and a
// caller-chosen network.
func TestCreateDockerNetworkOverride(t *testing.T) {
	t.Parallel()
	m, fp := newManager(t, engine.Docker)
	err := m.Create(context.Background(), Options{Name: "captf-test-b", KubeconfigPath: "/tmp/kc", Network: "custom"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fp.netEnv != "custom" {
		t.Errorf("network env during Create = %q", fp.netEnv)
	}
}

// TestCreateRefusals checks every Create precondition, and that a refused
// Create never reaches the provider.
func TestCreateRefusals(t *testing.T) {
	t.Parallel()
	done, cancel := context.WithCancel(context.Background())
	cancel()
	cases := map[string]struct {
		ctx      context.Context
		o        Options
		existing []string
		want     string
	}{
		"protected": {context.Background(), Options{Name: "kube-php-client", KubeconfigPath: "/k"}, nil, "operator's own"},
		"kind":      {context.Background(), Options{Name: "kind", KubeconfigPath: "/k"}, nil, "must start with"},
		"no path":   {context.Background(), Options{Name: "captf-test-a"}, nil, "KubeconfigPath"},
		"cancelled": {done, Options{Name: "captf-test-a", KubeconfigPath: "/k"}, nil, "canceled"},
		"exists":    {context.Background(), Options{Name: "captf-test-a", KubeconfigPath: "/k"}, []string{"captf-test-a"}, "already exists"},
		"workers":   {context.Background(), Options{Name: "captf-test-a", KubeconfigPath: "/k", Workers: -2}, nil, "workers"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, fp := newManager(t, engine.Podman, tc.existing...)
			err := m.Create(tc.ctx, tc.o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Create = %v, want error containing %q", err, tc.want)
			}
			if len(fp.created) != 0 {
				t.Errorf("provider created %v", fp.created)
			}
		})
	}
}

// TestCreateErrors checks provider failures surface, from Create's own List
// and from kind's Create.
func TestCreateErrors(t *testing.T) {
	t.Parallel()
	m, fp := newManager(t, engine.Podman)
	fp.err = errBoom
	err := m.Create(context.Background(), Options{Name: "captf-test-a", KubeconfigPath: "/k"})
	if !errors.Is(err, errBoom) {
		t.Errorf("Create (list fails) = %v", err)
	}

	m2, fp2 := newManager(t, engine.Podman)
	// List succeeds, then Create fails: flip err once List has been called.
	m2.provider = &failOnCreate{fakeProvider: fp2}
	err = m2.Create(context.Background(), Options{Name: "captf-test-a", KubeconfigPath: "/k"})
	if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), `create "captf-test-a"`) {
		t.Errorf("Create (kind fails) = %v", err)
	}
}

// failOnCreate wraps a fakeProvider whose Create always fails.
type failOnCreate struct{ *fakeProvider }

// Create always returns errBoom.
func (failOnCreate) Create(string, ...cluster.CreateOption) error { return errBoom }

// TestWithEnvRestores checks withEnv restores a previous value, unsets a
// missing one and propagates fn's error, using unique variable names so the
// test is safe in parallel.
func TestWithEnvRestores(t *testing.T) {
	t.Parallel()
	const had, missing = "CAPTF_KINDCLUSTER_TEST_HAD", "CAPTF_KINDCLUSTER_TEST_MISSING"
	if err := os.Setenv(had, "before"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv(had) })

	err := withEnv(had, "during", func() error {
		if os.Getenv(had) != "during" {
			t.Error("value not set during fn")
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Errorf("withEnv error = %v", err)
	}
	if got := os.Getenv(had); got != "before" {
		t.Errorf("restored value = %q", got)
	}

	if err := withEnv(missing, "x", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := os.LookupEnv(missing); ok {
		t.Error("previously unset variable left set")
	}

	// An empty key makes os.Setenv fail.
	if err := withEnv("", "x", func() error { return nil }); err == nil {
		t.Error("withEnv accepted an empty key")
	}
}

// TestListAndExists checks List filters to prefixed clusters and sorts.
func TestListAndExists(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t, engine.Podman, "kind", "kube-php-client", "captf-test-b", "captf-test-a", "captf-test-")
	got, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"captf-test-a", "captf-test-b"}) {
		t.Errorf("List = %v", got)
	}
	ok, err := m.Exists("captf-test-a")
	if err != nil || !ok {
		t.Errorf("Exists(a) = %v, %v", ok, err)
	}
	ok, err = m.Exists("captf-test-zz")
	if err != nil || ok {
		t.Errorf("Exists(zz) = %v, %v", ok, err)
	}
	if _, err := m.Exists("kube-php-client"); err == nil {
		t.Error("Exists accepted the protected cluster")
	}
}

// TestListError checks provider errors surface from List and Exists.
func TestListError(t *testing.T) {
	t.Parallel()
	m, fp := newManager(t, engine.Podman)
	fp.err = errBoom
	if _, err := m.List(); !errors.Is(err, errBoom) {
		t.Errorf("List = %v", err)
	}
	if _, err := m.Exists("captf-test-a"); !errors.Is(err, errBoom) {
		t.Errorf("Exists = %v", err)
	}
}

// TestDelete checks Delete refuses unsafe names and deletes safe ones.
func TestDelete(t *testing.T) {
	t.Parallel()
	m, fp := newManager(t, engine.Podman)
	for _, n := range []string{"kube-php-client", "kind", "", "other"} {
		if err := m.Delete(n, "/k"); err == nil {
			t.Errorf("Delete(%q) succeeded", n)
		}
	}
	if len(fp.deleted) != 0 {
		t.Fatalf("provider deleted %v", fp.deleted)
	}
	if err := m.Delete("captf-test-a", "/k"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fp.deleted, []string{"captf-test-a"}) {
		t.Errorf("deleted = %v", fp.deleted)
	}
	fp.err = errBoom
	if err := m.Delete("captf-test-a", "/k"); !errors.Is(err, errBoom) {
		t.Errorf("Delete error = %v", err)
	}
}

// TestNodesKubeconfigCollectLogs checks the three pass-through methods,
// their name guard and their error wrapping.
func TestNodesKubeconfigCollectLogs(t *testing.T) {
	t.Parallel()
	m, fp := newManager(t, engine.Podman)

	ns, err := m.Nodes("captf-test-a")
	if err != nil || len(ns) != 1 || ns[0].String() != "captf-test-a-control-plane" {
		t.Errorf("Nodes = %v, %v", ns, err)
	}
	kc, err := m.Kubeconfig("captf-test-a")
	if err != nil || kc != "kubeconfig-body" || fp.internal {
		t.Errorf("Kubeconfig = %q, %v (internal %v)", kc, err, fp.internal)
	}
	if err := m.CollectLogs("captf-test-a", "/out"); err != nil || fp.logsDir != "/out" {
		t.Errorf("CollectLogs = %v (dir %q)", err, fp.logsDir)
	}

	if _, err := m.Nodes("kind"); err == nil {
		t.Error("Nodes accepted kind")
	}
	if _, err := m.Kubeconfig("kube-php-client"); err == nil {
		t.Error("Kubeconfig accepted the protected cluster")
	}
	if err := m.CollectLogs("kind", "/out"); err == nil {
		t.Error("CollectLogs accepted kind")
	}

	fp.err = errBoom
	if _, err := m.Nodes("captf-test-a"); !errors.Is(err, errBoom) {
		t.Errorf("Nodes error = %v", err)
	}
	if _, err := m.Kubeconfig("captf-test-a"); !errors.Is(err, errBoom) {
		t.Errorf("Kubeconfig error = %v", err)
	}
	if err := m.CollectLogs("captf-test-a", "/out"); !errors.Is(err, errBoom) {
		t.Errorf("CollectLogs error = %v", err)
	}
}

// TestNew checks New's argument checks and that the real kind provider is
// built (without starting anything) when no fake is given.
func TestNew(t *testing.T) {
	t.Parallel()
	if _, err := New(nil); err == nil {
		t.Error("New(nil) succeeded")
	}
	if _, err := New(&engine.Engine{Name: "nerdctl"}); err == nil {
		t.Error("New accepted an unknown engine")
	}
	var buf bytes.Buffer
	m, err := New(&engine.Engine{Name: engine.Podman}, WithLogWriter(&buf))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := m.provider.(*cluster.Provider); !ok {
		t.Errorf("provider is %T, want *cluster.Provider", m.provider)
	}
}

// TestWriterLogger checks the kind logger adapter.
func TestWriterLogger(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := newWriterLogger(&buf)
	l.Warn("w1")
	l.Warnf("w%d", 2)
	l.Error("e1")
	l.Errorf("e%d\n", 2)
	l.V(0).Info("i0")
	l.V(0).Infof("i%d", 1)
	l.V(1).Info("hidden")
	l.V(2).Infof("hidden %d", 2)
	want := "warning: w1\nwarning: w2\nerror: e1\nerror: e2\ni0\ni1\n"
	if buf.String() != want {
		t.Errorf("log = %q, want %q", buf.String(), want)
	}
	if !l.V(0).Enabled() || l.V(1).Enabled() {
		t.Error("Enabled mismatch")
	}
	newWriterLogger(nil).Warn("discarded")
}
