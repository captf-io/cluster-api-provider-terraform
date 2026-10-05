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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
	kexec "sigs.k8s.io/kind/pkg/exec"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine/enginetest"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/kindcluster"
)

// fakeProvider is a kindcluster.Provider with fixed nodes; it spawns
// nothing.
type fakeProvider struct {
	nodes   []nodes.Node
	logsDir string
}

// Create returns nil; name and options are ignored.
func (p *fakeProvider) Create(string, ...cluster.CreateOption) error { return nil }

// Delete returns nil.
func (p *fakeProvider) Delete(string, string) error { return nil }

// List returns the one test cluster and the protected one.
func (p *fakeProvider) List() ([]string, error) {
	return []string{"captf-test-dev", "kube-php-client"}, nil
}

// KubeConfig returns a fixed kubeconfig.
func (p *fakeProvider) KubeConfig(string, bool) (string, error) { return "kc", nil }

// ListNodes returns the fake nodes.
func (p *fakeProvider) ListNodes(string) ([]nodes.Node, error) { return p.nodes, nil }

// CollectLogs records dir and returns nil.
func (p *fakeProvider) CollectLogs(_, dir string) error {
	p.logsDir = dir
	return nil
}

// fakeNode is a nodes.Node whose commands print stdout and fail with err.
type fakeNode struct {
	nodes.Node
	name   string
	stdout string
	err    error
}

// String returns the node name.
func (n *fakeNode) String() string { return n.name }

// Command returns a fake command; name and args are ignored.
func (n *fakeNode) Command(string, ...string) kexec.Cmd { return &fakeCmd{n: n} }

// CommandContext returns a fake command; ctx, name and args are ignored.
func (n *fakeNode) CommandContext(context.Context, string, ...string) kexec.Cmd {
	return &fakeCmd{n: n}
}

// fakeCmd is one fakeNode command.
type fakeCmd struct {
	n      *fakeNode
	stdout io.Writer
}

// SetEnv returns c.
func (c *fakeCmd) SetEnv(...string) kexec.Cmd { return c }

// SetStdin returns c.
func (c *fakeCmd) SetStdin(io.Reader) kexec.Cmd { return c }

// SetStdout records w and returns c.
func (c *fakeCmd) SetStdout(w io.Writer) kexec.Cmd { c.stdout = w; return c }

// SetStderr returns c.
func (c *fakeCmd) SetStderr(io.Writer) kexec.Cmd { return c }

// Run writes the node's stdout and returns its error.
func (c *fakeCmd) Run() error {
	if c.stdout != nil {
		_, _ = io.WriteString(c.stdout, c.n.stdout)
	}
	return c.n.err
}

// newKindCluster returns the real Cluster over a fakeProvider with the
// node n, its engine running through r; t fails if it cannot be built.
func newKindCluster(t *testing.T, r engine.Runner, n nodes.Node) (Cluster, *fakeProvider) {
	t.Helper()
	p := &fakeProvider{nodes: []nodes.Node{n}}
	c, err := NewCluster(&engine.Engine{Name: engine.Podman, Runner: r}, &bytes.Buffer{}, kindcluster.WithProvider(p))
	if err != nil {
		t.Fatal(err)
	}
	return c, p
}

// TestKindClusterNodes checks the node names and the node image records.
func TestKindClusterNodes(t *testing.T) {
	t.Parallel()
	n := &fakeNode{name: "captf-test-dev-control-plane", stdout: `{"status":{"id":"sha256:1","repoTags":["a:t"],"repoDigests":["a@sha256:2"]}}`}
	c, _ := newKindCluster(t, enginetest.New(), n)
	names, err := c.Nodes("captf-test-dev")
	if err != nil || len(names) != 1 || names[0] != n.name {
		t.Errorf("Nodes = %v, %v", names, err)
	}
	recs, err := c.NodeImages(context.Background(), "captf-test-dev", []string{"a:t"})
	if err != nil {
		t.Fatal(err)
	}
	want := NodeImage{Node: n.name, Ref: "a:t", ID: "sha256:1", RepoTags: []string{"a:t"}, RepoDigests: []string{"a@sha256:2"}}
	if len(recs) != 1 || recs[0].ID != want.ID || recs[0].Node != want.Node || recs[0].RepoDigests[0] != want.RepoDigests[0] {
		t.Errorf("NodeImages = %+v, want [%+v]", recs, want)
	}
	n.err = errBoom
	if _, err := c.NodeImages(context.Background(), "captf-test-dev", []string{"a:t"}); err == nil {
		t.Error("NodeImages with a failing crictl succeeded")
	}
}

// TestKindClusterNodePull checks that NodePull succeeds on a healthy node
// and returns the node's error, naming the image, when crictl fails.
func TestKindClusterNodePull(t *testing.T) {
	t.Parallel()
	n := &fakeNode{name: "captf-test-dev-control-plane"}
	c, _ := newKindCluster(t, enginetest.New(), n)
	if err := c.NodePull(context.Background(), "captf-test-dev", []string{"a@sha256:1", "b@sha256:2"}); err != nil {
		t.Fatalf("NodePull: %v", err)
	}
	n.err = errBoom
	err := c.NodePull(context.Background(), "captf-test-dev", []string{"a@sha256:1"})
	if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "a@sha256:1") {
		t.Errorf("NodePull with a failing crictl: %v", err)
	}
}

// TestKindClusterGuards checks that the name guard applies to the node
// operations.
func TestKindClusterGuards(t *testing.T) {
	t.Parallel()
	c, _ := newKindCluster(t, enginetest.New(), &fakeNode{name: "n"})
	ctx := context.Background()
	if _, err := c.Nodes("kube-php-client"); err == nil {
		t.Error("Nodes accepted the protected cluster")
	}
	if err := c.SideLoad(ctx, "kube-php-client", []string{"a:t"}, t.TempDir()); err == nil {
		t.Error("SideLoad accepted the protected cluster")
	}
	if err := c.NodePull(ctx, "kube-php-client", []string{"a:t"}); err == nil {
		t.Error("NodePull accepted the protected cluster")
	}
	if _, err := c.NodeImages(ctx, "kube-php-client", []string{"a:t"}); err == nil {
		t.Error("NodeImages accepted the protected cluster")
	}
	if err := c.CollectLogs("kube-php-client", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error("CollectLogs accepted the protected cluster")
	}
	names, err := c.List()
	if err != nil || len(names) != 1 || names[0] != "captf-test-dev" {
		t.Errorf("List = %v, %v", names, err)
	}
}

// TestKindClusterSideLoad checks that SideLoad saves through the engine
// and reports a failing save.
func TestKindClusterSideLoad(t *testing.T) {
	t.Parallel()
	r := enginetest.New().Respond(func(enginetest.Call) enginetest.Response {
		return enginetest.Response{Err: errBoom}
	})
	c, _ := newKindCluster(t, r, &fakeNode{name: "n"})
	err := c.SideLoad(context.Background(), "captf-test-dev", []string{"a:t"}, t.TempDir())
	if !errors.Is(err, errBoom) {
		t.Errorf("SideLoad: got %v, want errBoom", err)
	}
	if calls := r.Calls(); len(calls) != 1 || !strings.HasPrefix(calls[0].Line(), "podman save --format docker-archive") {
		t.Errorf("calls: %v", calls)
	}
}

// TestKindClusterCollectLogs checks that the hook creates its directory
// before kind writes into it.
func TestKindClusterCollectLogs(t *testing.T) {
	t.Parallel()
	c, p := newKindCluster(t, enginetest.New(), &fakeNode{name: "n"})
	dir := filepath.Join(t.TempDir(), "a", "node-logs")
	if err := c.CollectLogs("captf-test-dev", dir); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() || p.logsDir != dir {
		t.Errorf("node-logs dir %s: %v (kind got %q)", dir, err, p.logsDir)
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.CollectLogs("captf-test-dev", filepath.Join(file, "x")); err == nil {
		t.Error("CollectLogs under a file succeeded")
	}
}

// TestNewClusterErrors checks that an unusable engine is refused.
func TestNewClusterErrors(t *testing.T) {
	t.Parallel()
	if _, err := NewCluster(nil, nil); err == nil {
		t.Error("NewCluster(nil) succeeded")
	}
}
