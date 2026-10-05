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

package images_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/kind/pkg/cluster/nodes"
	kexec "sigs.k8s.io/kind/pkg/exec"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine/enginetest"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/images"
)

// containerdConfig is the `containerd config dump` output the fake node
// answers with; kind parses the snapshotter out of it.
const containerdConfig = `version = 2
[plugins]
  [plugins."io.containerd.grpc.v1.cri"]
    [plugins."io.containerd.grpc.v1.cri".containerd]
      snapshotter = "overlayfs"
`

// fakeNode is a nodes.Node that records its commands and what it was fed
// on stdin, and answers them from canned output.
type fakeNode struct {
	// name is the node's String.
	name string
	// mu guards every field below.
	mu sync.Mutex
	// cmds is the command line of every command run, in order.
	cmds []string
	// imported is every stdin payload `ctr images import` received.
	imported []string
	// failImport makes `ctr images import` fail.
	failImport bool
	// crictl is the stdout and error of `crictl inspecti`.
	crictl string
	// crictlErr is the error `crictl inspecti` returns.
	crictlErr error
	// pullErr is the error `crictl pull` returns.
	pullErr error
}

// Compile-time check that fakeNode is a nodes.Node.
var _ nodes.Node = (*fakeNode)(nil)

// fakeCmd is one command of a fakeNode.
type fakeCmd struct {
	// n is the node it runs on.
	n *fakeNode
	// line is the command line.
	line string
	// stdin, stdout and stderr are the streams set on it.
	stdin          io.Reader
	stdout, stderr io.Writer
}

// Command returns a command of the program name with args on n.
func (n *fakeNode) Command(name string, args ...string) kexec.Cmd {
	return &fakeCmd{n: n, line: strings.Join(append([]string{name}, args...), " ")}
}

// CommandContext returns what Command returns for the program name and
// args; the fake ignores the context.
func (n *fakeNode) CommandContext(_ context.Context, name string, args ...string) kexec.Cmd {
	return n.Command(name, args...)
}

// String returns the node's name.
func (n *fakeNode) String() string { return n.name }

// Role returns "control-plane".
func (n *fakeNode) Role() (string, error) { return "control-plane", nil }

// IP returns no addresses.
func (n *fakeNode) IP() (string, string, error) { return "", "", nil }

// SerialLogs writes nothing and returns nil.
func (n *fakeNode) SerialLogs(io.Writer) error { return nil }

// SetEnv ignores the environment and returns c.
func (c *fakeCmd) SetEnv(...string) kexec.Cmd { return c }

// SetStdin sets c's stdin to r and returns c.
func (c *fakeCmd) SetStdin(r io.Reader) kexec.Cmd { c.stdin = r; return c }

// SetStdout sets c's stdout to w and returns c.
func (c *fakeCmd) SetStdout(w io.Writer) kexec.Cmd { c.stdout = w; return c }

// SetStderr sets c's stderr to w and returns c.
func (c *fakeCmd) SetStderr(w io.Writer) kexec.Cmd { c.stderr = w; return c }

// Run records c and answers it: containerd's config for the snapshotter
// probe, the stdin payload for an import, crictl's canned output for an
// inspect. It returns the node's scripted error for the command, or nil.
func (c *fakeCmd) Run() error {
	c.n.mu.Lock()
	defer c.n.mu.Unlock()
	c.n.cmds = append(c.n.cmds, c.line)
	switch {
	case strings.HasPrefix(c.line, "containerd config dump"):
		_, _ = io.WriteString(c.stdout, containerdConfig)
	case strings.HasPrefix(c.line, "ctr "):
		if c.stdin != nil {
			b, _ := io.ReadAll(c.stdin)
			c.n.imported = append(c.n.imported, string(b))
		}
		if c.n.failImport {
			return errors.New("import failed")
		}
	case strings.HasPrefix(c.line, "crictl pull "):
		if c.n.pullErr != nil {
			_, _ = io.WriteString(c.stderr, "pull denied\n")
			return c.n.pullErr
		}
	case strings.HasPrefix(c.line, "crictl inspecti"):
		if c.n.crictlErr != nil {
			_, _ = io.WriteString(c.stderr, "no such image\n")
			return c.n.crictlErr
		}
		_, _ = io.WriteString(c.stdout, c.n.crictl)
	}
	return nil
}

// podman returns a podman Engine over r.
func podman(r engine.Runner) *engine.Engine {
	return &engine.Engine{Name: engine.Podman, Runner: r}
}

// cancelled returns a context that is already cancelled, derived from the
// test t's.
func cancelled(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

// gitFake returns a Runner scripted with the three git commands TreeID
// runs: sha is HEAD's short sha, diff the `git diff HEAD` output and
// untracked the untracked-file list.
func gitFake(sha, diff, untracked string) *enginetest.Runner {
	return enginetest.New().
		On("git rev-parse --short=12 HEAD", sha+"\n", nil).
		On("git diff HEAD", diff, nil).
		On("git ls-files --others --exclude-standard", untracked, nil)
}

// TestTreeID checks the clean and dirty forms and that the hash tracks
// both the diff and the untracked list.
func TestTreeID(t *testing.T) {
	t.Parallel()
	get := func(t *testing.T, r *enginetest.Runner) string {
		t.Helper()
		id, err := images.TreeID(t.Context(), r)
		if err != nil {
			t.Fatalf("TreeID: %v", err)
		}
		return id
	}
	if got := get(t, gitFake("0123456789ab", "", "")); got != "0123456789ab" {
		t.Errorf("clean tree = %q", got)
	}
	a := get(t, gitFake("0123456789ab", "diff --git a b", ""))
	if !strings.HasPrefix(a, "0123456789ab-dirty-") || len(a) != len("0123456789ab-dirty-")+12 {
		t.Errorf("dirty tree = %q", a)
	}
	if again := get(t, gitFake("0123456789ab", "diff --git a b", "")); again != a {
		t.Errorf("same tree gave %q then %q", a, again)
	}
	if b := get(t, gitFake("0123456789ab", "diff --git a c", "")); b == a {
		t.Error("different diff gave the same ID")
	}
	if c := get(t, gitFake("0123456789ab", "diff --git a b", "new.txt\n")); c == a {
		t.Error("untracked file did not change the ID")
	}
	if d := get(t, gitFake("0123456789ab", "", "new.txt\n")); !strings.Contains(d, "-dirty-") {
		t.Errorf("untracked-only tree = %q, want dirty", d)
	}
}

// TestTreeIDErrors checks each git failure and an empty sha.
func TestTreeIDErrors(t *testing.T) {
	t.Parallel()
	boom := enginetest.Exit(128, "not a git repository")
	tests := []struct {
		name string
		r    *enginetest.Runner
	}{
		{"rev-parse", enginetest.New().On("git rev-parse --short=12 HEAD", "", boom)},
		{"empty sha", gitFake("", "", "")},
		{"diff", enginetest.New().On("git rev-parse --short=12 HEAD", "abc\n", nil).On("git diff HEAD", "", boom)},
		{"ls-files", enginetest.New().
			On("git rev-parse --short=12 HEAD", "abc\n", nil).
			On("git diff HEAD", "", nil).
			On("git ls-files --others --exclude-standard", "", boom)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := images.TreeID(t.Context(), tt.r)
			if err == nil || !strings.HasPrefix(err.Error(), "images: tree id:") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// TestManagerRef checks the reference format.
func TestManagerRef(t *testing.T) {
	t.Parallel()
	if got, want := images.ManagerRef("abc-dirty-123"), "localhost/captf/manager:abc-dirty-123"; got != want {
		t.Errorf("ManagerRef = %q, want %q", got, want)
	}
}

// TestBuildManager checks the make command line and its guards.
func TestBuildManager(t *testing.T) {
	t.Parallel()
	const ref = "localhost/captf/manager:abc"
	const line = "make docker-build CONTAINER_TOOL=podman IMG=" + ref
	var log bytes.Buffer
	r := enginetest.New().On(line, "", nil)
	if err := images.BuildManager(t.Context(), r, podman(r), ref, images.WithLog(&log)); err != nil {
		t.Fatalf("BuildManager: %v", err)
	}
	if got := r.Lines(); !slices.Equal(got, []string{line}) {
		t.Errorf("lines = %q", got)
	}
	if !strings.Contains(log.String(), "building "+ref) {
		t.Errorf("log = %q", log.String())
	}

	fail := enginetest.New().On(line, "", enginetest.Exit(2, "boom"))
	if err := images.BuildManager(t.Context(), fail, podman(fail), ref); err == nil ||
		!strings.HasPrefix(err.Error(), "images: build manager:") {
		t.Errorf("failed make: err = %v", err)
	}

	for _, bad := range []string{"", "localhost/captf/manager", "localhost/captf/manager:latest", "localhost:5000/manager", "x:"} {
		n := enginetest.New()
		if err := images.BuildManager(t.Context(), n, podman(n), bad); err == nil {
			t.Errorf("ref %q accepted", bad)
		}
		if len(n.Calls()) != 0 {
			t.Errorf("ref %q ran %q", bad, n.Lines())
		}
	}

	n := enginetest.New()
	if err := images.BuildManager(cancelled(t), n, podman(n), ref); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v", err)
	}
}

// TestSmokeManager checks both binaries are run, in order, and that a
// failure of either is reported by name.
func TestSmokeManager(t *testing.T) {
	t.Parallel()
	const ref = "localhost/captf/manager:abc"
	mgr := "podman run --rm --pull=never --entrypoint /manager " + ref + " --help"
	run := "podman run --rm --pull=never --entrypoint /runner " + ref + " --help"

	var log bytes.Buffer
	r := enginetest.New().On(mgr, "", nil).On(run, "", nil)
	if err := images.SmokeManager(t.Context(), podman(r), ref, images.WithLog(&log)); err != nil {
		t.Fatalf("SmokeManager: %v", err)
	}
	if got := r.Lines(); !slices.Equal(got, []string{mgr, run}) {
		t.Errorf("lines = %q", got)
	}
	if strings.Count(log.String(), "smoke-checking") != 2 {
		t.Errorf("log = %q", log.String())
	}

	bad := enginetest.New().On(mgr, "", enginetest.Exit(1, "exec format error"))
	err := images.SmokeManager(t.Context(), podman(bad), ref)
	if err == nil || !strings.Contains(err.Error(), "images: smoke /manager:") {
		t.Errorf("manager failure: err = %v", err)
	}
	if len(bad.Calls()) != 1 {
		t.Errorf("kept going after /manager failed: %q", bad.Lines())
	}

	bad = enginetest.New().On(mgr, "", nil).On(run, "", enginetest.Exit(127, "not found"))
	if err := images.SmokeManager(t.Context(), podman(bad), ref); err == nil ||
		!strings.Contains(err.Error(), "images: smoke /runner:") {
		t.Errorf("runner failure: err = %v", err)
	}

	if err := images.SmokeManager(cancelled(t), podman(enginetest.New()), ref); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v", err)
	}
}

// saveFake returns a Runner that answers `podman save` by writing the
// image ref into the --output file, as the archive's content, and fails
// for the ref failRef. It returns the Runner.
func saveFake(failRef string) *enginetest.Runner {
	return enginetest.New().Respond(func(c enginetest.Call) enginetest.Response {
		if c.Args[0] != "save" {
			return enginetest.Response{Err: errors.New("unexpected " + c.Line())}
		}
		ref := c.Args[len(c.Args)-1]
		if ref == failRef {
			return enginetest.Response{Err: enginetest.Exit(125, "no such image")}
		}
		path := c.Args[slices.Index(c.Args, "--output")+1]
		if err := os.WriteFile(path, []byte("archive:"+ref), 0o600); err != nil {
			return enginetest.Response{Err: err}
		}
		return enginetest.Response{}
	})
}

// leftovers returns the names of the entries in dir, failing the test t
// if it cannot read it.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// TestSideLoad checks every ref reaches every node, streamed from the
// saved archive, and that no archive is left behind.
func TestSideLoad(t *testing.T) {
	t.Parallel()
	work := filepath.Join(t.TempDir(), "nested", "work")
	n1, n2 := &fakeNode{name: "cp"}, &fakeNode{name: "w1"}
	var log bytes.Buffer
	r := saveFake("")
	refs := []string{"localhost/captf/manager:abc", "ghcr.io/captf-io/noop-cluster:edge-terraform"}
	err := images.SideLoad(t.Context(), podman(r), []nodes.Node{n1, n2}, refs, work, images.WithLog(&log))
	if err != nil {
		t.Fatalf("SideLoad: %v", err)
	}
	for _, n := range []*fakeNode{n1, n2} {
		want := []string{"archive:" + refs[0], "archive:" + refs[1]}
		if !slices.Equal(n.imported, want) {
			t.Errorf("%s imported %q, want %q", n.name, n.imported, want)
		}
	}
	if got := leftovers(t, work); len(got) != 0 {
		t.Errorf("archives left behind: %q", got)
	}
	if got := r.Lines()[0]; !strings.HasPrefix(got, "podman save --format docker-archive --output "+work) {
		t.Errorf("first command = %q", got)
	}
	if !strings.Contains(log.String(), "loading "+refs[1]+" into w1") {
		t.Errorf("log = %q", log.String())
	}
}

// TestSideLoadErrors checks that each failure stops the run, is prefixed,
// and leaves no archive.
func TestSideLoadErrors(t *testing.T) {
	t.Parallel()
	refs := []string{"a:1", "b:1"}

	t.Run("save", func(t *testing.T) {
		t.Parallel()
		work := t.TempDir()
		n := &fakeNode{name: "cp"}
		err := images.SideLoad(t.Context(), podman(saveFake("b:1")), []nodes.Node{n}, refs, work)
		if err == nil || !strings.HasPrefix(err.Error(), "images: side-load:") {
			t.Fatalf("err = %v", err)
		}
		if len(n.imported) != 1 {
			t.Errorf("imported = %q, want only a:1", n.imported)
		}
		if got := leftovers(t, work); len(got) != 0 {
			t.Errorf("archives left behind: %q", got)
		}
	})

	t.Run("import", func(t *testing.T) {
		t.Parallel()
		work := t.TempDir()
		n := &fakeNode{name: "cp", failImport: true}
		err := images.SideLoad(t.Context(), podman(saveFake("")), []nodes.Node{n}, refs, work)
		if err == nil || !strings.Contains(err.Error(), "a:1 into cp") {
			t.Fatalf("err = %v", err)
		}
		if got := leftovers(t, work); len(got) != 0 {
			t.Errorf("archives left behind: %q", got)
		}
	})

	t.Run("snapshotter probe", func(t *testing.T) {
		t.Parallel()
		n := &brokenNode{fakeNode{name: "cp"}}
		err := images.SideLoad(t.Context(), podman(saveFake("")), []nodes.Node{n}, refs[:1], t.TempDir())
		if err == nil {
			t.Fatal("SideLoad succeeded")
		}
	})

	t.Run("work dir", func(t *testing.T) {
		t.Parallel()
		file := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := images.SideLoad(t.Context(), podman(saveFake("")), nil, refs, filepath.Join(file, "sub"))
		if err == nil || !strings.HasPrefix(err.Error(), "images: side-load:") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()
		r := saveFake("")
		err := images.SideLoad(cancelled(t), podman(r), []nodes.Node{&fakeNode{}}, refs, t.TempDir())
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
		if len(r.Calls()) != 0 {
			t.Errorf("ran %q", r.Lines())
		}
	})

	t.Run("cancelled between nodes", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		n1 := &cancelNode{fakeNode: fakeNode{name: "cp"}, cancel: cancel}
		n2 := &fakeNode{name: "w1"}
		err := images.SideLoad(ctx, podman(saveFake("")), []nodes.Node{n1, n2}, refs, t.TempDir())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if len(n2.cmds) != 0 {
			t.Errorf("second node was used after cancel: %q", n2.cmds)
		}
	})
}

// brokenNode is a fakeNode whose containerd config probe fails.
type brokenNode struct {
	// fakeNode is the embedded fake.
	fakeNode
}

// Command returns a failing command for the program name "containerd"
// and the fake's command for any other name, with args.
func (n *brokenNode) Command(name string, args ...string) kexec.Cmd {
	if name == "containerd" {
		return &failCmd{}
	}
	return n.fakeNode.Command(name, args...)
}

// failCmd is a command that always fails.
type failCmd struct{}

// SetEnv returns c.
func (c *failCmd) SetEnv(...string) kexec.Cmd { return c }

// SetStdin returns c.
func (c *failCmd) SetStdin(io.Reader) kexec.Cmd { return c }

// SetStdout returns c.
func (c *failCmd) SetStdout(io.Writer) kexec.Cmd { return c }

// SetStderr returns c.
func (c *failCmd) SetStderr(io.Writer) kexec.Cmd { return c }

// Run returns an error, always.
func (c *failCmd) Run() error { return errors.New("no containerd") }

// cancelNode is a fakeNode that cancels its context when it is first used.
type cancelNode struct {
	// fakeNode is the embedded fake.
	fakeNode
	// cancel cancels the context under test.
	cancel context.CancelFunc
}

// Command cancels the context, then returns the fake's command for the
// program name and args.
func (n *cancelNode) Command(name string, args ...string) kexec.Cmd {
	n.cancel()
	return n.fakeNode.Command(name, args...)
}

// TestNodePull checks the crictl command line, the log line, and that
// failures name the image and node, carry stderr and wrap the cause.
func TestNodePull(t *testing.T) {
	t.Parallel()
	const ref = "ghcr.io/captf-io/noop-cluster@sha256:abc"

	t.Run("pulls", func(t *testing.T) {
		t.Parallel()
		var log bytes.Buffer
		n := &fakeNode{name: "cp"}
		if err := images.NodePull(t.Context(), n, ref, images.WithLog(&log)); err != nil {
			t.Fatalf("NodePull: %v", err)
		}
		if !slices.Equal(n.cmds, []string{"crictl pull " + ref}) {
			t.Errorf("cmds = %q", n.cmds)
		}
		if !strings.Contains(log.String(), "pulling "+ref+" on cp") {
			t.Errorf("log = %q", log.String())
		}
	})

	t.Run("fails", func(t *testing.T) {
		t.Parallel()
		cause := errors.New("exit 1")
		err := images.NodePull(t.Context(), &fakeNode{name: "w1", pullErr: cause}, ref)
		if !errors.Is(err, cause) {
			t.Fatalf("err = %v, want it to wrap the cause", err)
		}
		for _, want := range []string{"images: node pull " + ref + " on w1:", "pull denied"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, lacks %q", err, want)
			}
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()
		n := &fakeNode{name: "cp"}
		if err := images.NodePull(cancelled(t), n, ref); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
		if len(n.cmds) != 0 {
			t.Errorf("ran %q after cancel", n.cmds)
		}
	})
}

// TestNodeImage checks the parsed fields and each failure.
func TestNodeImage(t *testing.T) {
	t.Parallel()
	const ref = "localhost/captf/manager:abc"
	good := `{"status":{"id":"sha256:aaa","repoTags":["` + ref + `"],"repoDigests":["localhost/captf/manager@sha256:bbb"]}}`
	n := &fakeNode{name: "cp", crictl: good}
	got, err := images.NodeImage(t.Context(), n, ref)
	if err != nil {
		t.Fatalf("NodeImage: %v", err)
	}
	want := images.NodeImageInfo{
		Ref: ref, ID: "sha256:aaa",
		RepoTags:    []string{ref},
		RepoDigests: []string{"localhost/captf/manager@sha256:bbb"},
	}
	if got.Ref != want.Ref || got.ID != want.ID || !slices.Equal(got.RepoTags, want.RepoTags) ||
		!slices.Equal(got.RepoDigests, want.RepoDigests) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if !slices.Equal(n.cmds, []string{"crictl inspecti -o json " + ref}) {
		t.Errorf("cmds = %q", n.cmds)
	}

	noDigests := &fakeNode{name: "cp", crictl: `{"status":{"id":"sha256:aaa","repoTags":["x:1"]}}`}
	if info, err := images.NodeImage(t.Context(), noDigests, "x:1"); err != nil || len(info.RepoDigests) != 0 {
		t.Errorf("no digests: info = %+v, err = %v", info, err)
	}

	for name, bad := range map[string]*fakeNode{
		"command fails": {name: "cp", crictlErr: errors.New("exit 1")},
		"bad json":      {name: "cp", crictl: "nope"},
		"no id":         {name: "cp", crictl: `{"status":{}}`},
	} {
		_, err := images.NodeImage(t.Context(), bad, ref)
		if err == nil || !strings.HasPrefix(err.Error(), "images: node image:") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
