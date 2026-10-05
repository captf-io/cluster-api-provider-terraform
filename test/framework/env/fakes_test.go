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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/kindcluster"
)

// errBoom is the error fakes return on request.
var errBoom = errors.New("boom")

// recorder records the calls every fake makes, in order, as one shared
// log, so a test can check the sequence across Host, Cluster and Kube.
type recorder struct {
	mu    sync.Mutex
	calls []string
	// fail maps a call name ("host.BuildManager") to the error it returns.
	fail map[string]error
}

// call records the call name with the detail formatted from format and
// args, and returns the error scripted for name.
func (r *recorder) call(name, format string, args ...any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	line := name
	if format != "" {
		line += " " + fmt.Sprintf(format, args...)
	}
	r.calls = append(r.calls, line)
	return r.fail[name]
}

// names returns the recorded call names without their details.
func (r *recorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		name, _, _ := strings.Cut(c, " ")
		out = append(out, name)
	}
	return out
}

// has reports whether a call line starting with prefix was recorded.
func (r *recorder) has(prefix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.ContainsFunc(r.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

// fakeHost is a Host that records its calls and spawns nothing.
type fakeHost struct {
	rec    *recorder
	treeID string
}

// DetectEngine records the call and returns podman over a nil runner,
// or the scripted error; ctx and override are ignored.
func (h *fakeHost) DetectEngine(_ context.Context, override string) (*engine.Engine, error) {
	if err := h.rec.call("host.DetectEngine", "%s", override); err != nil {
		return nil, err
	}
	return &engine.Engine{Name: engine.Podman}, nil
}

// TreeID records the call and returns the fake's tree ID or the scripted
// error.
func (h *fakeHost) TreeID(context.Context) (string, error) {
	return h.treeID, h.rec.call("host.TreeID", "")
}

// BuildManager records ref and returns the scripted error.
func (h *fakeHost) BuildManager(_ context.Context, _ *engine.Engine, ref string) error {
	return h.rec.call("host.BuildManager", "%s", ref)
}

// SmokeManager records ref and returns the scripted error.
func (h *fakeHost) SmokeManager(_ context.Context, _ *engine.Engine, ref string) error {
	return h.rec.call("host.SmokeManager", "%s", ref)
}

// EnsureCache records cacheDir and returns the scripted error.
func (h *fakeHost) EnsureCache(_ context.Context, cacheDir string) error {
	return h.rec.call("host.EnsureCache", "%s", cacheDir)
}

// RenderCAPTF records image and outDir, creates outDir, and returns the
// scripted error.
func (h *fakeHost) RenderCAPTF(_ context.Context, image, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	return h.rec.call("host.RenderCAPTF", "%s %s", image, outDir)
}

// WriteRepository records its arguments cacheDir, captfDir and repoDir and
// returns repoDir/clusterctl.yaml or the scripted error.
func (h *fakeHost) WriteRepository(cacheDir, captfDir, repoDir string) (string, error) {
	return filepath.Join(repoDir, "clusterctl.yaml"), h.rec.call("host.WriteRepository", "%s %s %s", cacheDir, captfDir, repoDir)
}

// InitProviders records kubeconfig, configPath and homeDir and returns the
// scripted error.
func (h *fakeHost) InitProviders(_ context.Context, kubeconfig, configPath, homeDir string) error {
	return h.rec.call("host.InitProviders", "%s %s %s", kubeconfig, configPath, homeDir)
}

// RemoveNetwork records name and returns the scripted error.
func (h *fakeHost) RemoveNetwork(_ context.Context, _ *engine.Engine, name string) error {
	return h.rec.call("host.RemoveNetwork", "%s", name)
}

// fakeCluster is a Cluster over an in-memory set of clusters.
type fakeCluster struct {
	rec      *recorder
	mu       sync.Mutex
	clusters map[string]bool
	nodes    []string
	// digests, when true, makes NodeImages report each noop image's
	// pinned digest among its repo digests.
	digests bool
}

// Create records o's name and workers, writes o.KubeconfigPath, adds the
// cluster, and returns the scripted error; ctx is ignored.
func (c *fakeCluster) Create(_ context.Context, o kindcluster.Options) error {
	if err := c.rec.call("cluster.Create", "%s %d %s", o.Name, o.Workers, o.KubeconfigPath); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clusters[o.Name] = true
	return os.WriteFile(o.KubeconfigPath, []byte("kubeconfig"), 0o600)
}

// Exists records name and reports whether the cluster exists, or the
// scripted error.
func (c *fakeCluster) Exists(name string) (bool, error) {
	err := c.rec.call("cluster.Exists", "%s", name)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clusters[name], err
}

// List records the call and returns the sorted cluster names, or the
// scripted error.
func (c *fakeCluster) List() ([]string, error) {
	if err := c.rec.call("cluster.List", ""); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for n := range c.clusters {
		out = append(out, n)
	}
	slices.Sort(out)
	return out, nil
}

// Delete records name and kubeconfigPath, removes the cluster, and returns
// the scripted error.
func (c *fakeCluster) Delete(name, kubeconfigPath string) error {
	if err := c.rec.call("cluster.Delete", "%s %s", name, kubeconfigPath); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.clusters, name)
	return nil
}

// Kubeconfig records name and returns a fixed kubeconfig, or the scripted
// error.
func (c *fakeCluster) Kubeconfig(name string) (string, error) {
	return "kubeconfig from kind", c.rec.call("cluster.Kubeconfig", "%s", name)
}

// Nodes records name and returns the fake's nodes, or the scripted error.
func (c *fakeCluster) Nodes(name string) ([]string, error) {
	return c.nodes, c.rec.call("cluster.Nodes", "%s", name)
}

// SideLoad records name, refs and workDir and returns the scripted error.
func (c *fakeCluster) SideLoad(_ context.Context, name string, refs []string, workDir string) error {
	return c.rec.call("cluster.SideLoad", "%s %s %s", name, strings.Join(refs, ","), workDir)
}

// NodePull records name and refs and returns the scripted error.
func (c *fakeCluster) NodePull(_ context.Context, name string, refs []string) error {
	return c.rec.call("cluster.NodePull", "%s %s", name, strings.Join(refs, ","))
}

// NodeImages records name and refs, and returns one record per node and
// ref (noop images, named by their pinned ref, carry their pinned digest
// when c.digests is set), or the scripted error.
func (c *fakeCluster) NodeImages(_ context.Context, name string, refs []string) ([]NodeImage, error) {
	if err := c.rec.call("cluster.NodeImages", "%s %s", name, strings.Join(refs, ",")); err != nil {
		return nil, err
	}
	_, noops := noopRefs()
	var out []NodeImage
	for _, n := range c.nodes {
		for _, ref := range refs {
			rec := NodeImage{Node: n, Ref: ref, ID: "sha256:" + ref, RepoTags: []string{ref}}
			for _, nr := range noops {
				if nr.Pinned == ref && c.digests {
					rec.RepoDigests = []string{nr.Pinned}
				}
			}
			out = append(out, rec)
		}
	}
	return out, nil
}

// CollectLogs records name and dir, creates dir, and returns the scripted
// error.
func (c *fakeCluster) CollectLogs(name, dir string) error {
	if err := c.rec.call("cluster.CollectLogs", "%s %s", name, dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o750)
}

// fakeKube is a Kube with an in-memory manager image.
type fakeKube struct {
	rec   *recorder
	mu    sync.Mutex
	image string
}

// WaitProviders records the call and returns the scripted error.
func (k *fakeKube) WaitProviders(context.Context) error {
	return k.rec.call("kube.WaitProviders", "")
}

// Report records the call and returns a fixed report or the scripted
// error.
func (k *fakeKube) Report(context.Context) (string, error) {
	return "no non-ready pods", k.rec.call("kube.Report", "")
}

// Collect records dir, writes dir/collected and runs nodeLogs on
// dir/node-logs, and returns the scripted error.
func (k *fakeKube) Collect(_ context.Context, dir string, nodeLogs func(dir string) error) error {
	if err := k.rec.call("kube.Collect", "%s", dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "collected"), nil, 0o600); err != nil {
		return err
	}
	return nodeLogs(filepath.Join(dir, "node-logs"))
}

// ManagerImage records the call and returns the current image or the
// scripted error.
func (k *fakeKube) ManagerImage(context.Context) (string, error) {
	err := k.rec.call("kube.ManagerImage", "")
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.image, err
}

// SetManagerImage records ref, sets the image, and returns the scripted
// error.
func (k *fakeKube) SetManagerImage(_ context.Context, ref string) error {
	if err := k.rec.call("kube.SetManagerImage", "%s", ref); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.image = ref
	return nil
}

// WaitManagerRollout records the call and returns the scripted error.
func (k *fakeKube) WaitManagerRollout(context.Context) error {
	return k.rec.call("kube.WaitManagerRollout", "")
}

// fixture is one Env over fakes, in a temporary repository.
type fixture struct {
	t       *testing.T
	cfg     Config
	rec     *recorder
	host    *fakeHost
	cluster *fakeCluster
	kube    *fakeKube
	out     *bytes.Buffer
	env     *Env
}

// newFixture returns a fixture for t with a temporary repository root
// holding go.work and executable clusterctl and kustomize stubs, the
// cluster captf-test-dev and one node. mutate, when non-nil, changes the
// Config before the Env is built.
func newFixture(t *testing.T, mutate func(*Config)) *fixture {
	t.Helper()
	root := t.TempDir()
	tools := filepath.Join(root, "hack", "tools", "bin")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(root, "go.work"), filepath.Join(tools, "clusterctl"), filepath.Join(tools, "kustomize")} {
		if err := os.WriteFile(f, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{
		Name:       framework.DefaultClusterName,
		RepoRoot:   root,
		CacheDir:   filepath.Join(root, "cache"),
		Clusterctl: filepath.Join(tools, "clusterctl"),
		Kustomize:  filepath.Join(tools, "kustomize"),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	rec := &recorder{fail: map[string]error{}}
	f := &fixture{
		t:       t,
		cfg:     cfg,
		rec:     rec,
		host:    &fakeHost{rec: rec, treeID: "abc123"},
		cluster: &fakeCluster{rec: rec, clusters: map[string]bool{}, nodes: []string{"captf-test-dev-control-plane"}},
		kube:    &fakeKube{rec: rec},
		out:     &bytes.Buffer{},
	}
	var tick time.Duration
	clock := func() time.Time {
		tick += time.Second
		return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC).Add(tick)
	}
	e, err := New(cfg, f.out, WithClock(clock), WithDeps(Deps{
		Host:    f.host,
		Cluster: func(*engine.Engine) (Cluster, error) { return f.cluster, rec.fail["deps.Cluster"] },
		Kube:    func(string) (Kube, error) { return f.kube, rec.fail["deps.Kube"] },
	}))
	if err != nil {
		t.Fatal(err)
	}
	f.env = e
	return f
}

// managerRef is the manager reference the fixture's tree ID yields.
const managerRef = "localhost/captf/manager:abc123"
