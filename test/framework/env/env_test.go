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
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
)

// TestNewValidates checks that New refuses an invalid Config and defaults
// a nil writer.
func TestNewValidates(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{Name: framework.ProtectedClusterName, RepoRoot: "/r"}, nil); err == nil {
		t.Error("New accepted the protected cluster name")
	}
	if _, err := New(Config{Name: framework.DefaultClusterName}, nil); !errors.Is(err, errNoRepoRoot) {
		t.Errorf("New without RepoRoot: got %v, want errNoRepoRoot", err)
	}
	e, err := New(Config{Name: framework.DefaultClusterName, RepoRoot: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.out == nil || e.deps.Host == nil || e.deps.Cluster == nil || e.deps.Kube == nil {
		t.Errorf("New left a nil writer or dependency: %+v", e)
	}
}

// TestUpFresh checks the whole Up sequence on a missing cluster, and the
// state.json and env.sh it writes.
func TestUpFresh(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(c *Config) { c.Workers = 1 })
	f.cluster.digests = true
	work := f.cfg.WorkDir()
	// A stale file from an earlier run goes; artifacts stay.
	if err := os.MkdirAll(filepath.Join(work, artifactsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "stale"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.env.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v\n%s", err, f.out)
	}
	want := []string{
		"host.DetectEngine", "host.TreeID", "host.BuildManager", "host.SmokeManager",
		"cluster.Exists", "cluster.Create", "cluster.SideLoad", "cluster.NodePull", "cluster.NodeImages",
		"host.EnsureCache", "host.RenderCAPTF", "host.WriteRepository", "host.InitProviders", "kube.WaitProviders",
	}
	if got := f.rec.names(); !slices.Equal(got, want) {
		t.Errorf("calls:\n got %v\nwant %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(work, "stale")); !os.IsNotExist(err) {
		t.Errorf("stale file survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, artifactsDir)); err != nil {
		t.Errorf("artifacts removed: %v", err)
	}
	if !f.rec.has("cluster.Create captf-test-dev 1 " + f.cfg.Kubeconfig()) {
		t.Errorf("Create did not get the name, workers and kubeconfig: %v", f.rec.calls)
	}
	if !f.rec.has("host.InitProviders " + f.cfg.Kubeconfig() + " " + filepath.Join(work, repoDir, "clusterctl.yaml") + " " + filepath.Join(work, homeDir)) {
		t.Errorf("InitProviders arguments: %v", f.rec.calls)
	}
	// Only the manager image is side-loaded; the noop images are pulled by
	// their pinned references, in the nodes.
	if !f.rec.has("cluster.SideLoad captf-test-dev " + managerRef + " ") {
		t.Errorf("SideLoad refs: %v", f.rec.calls)
	}
	var pinned []string
	for _, img := range framework.NoopImages() {
		pinned = append(pinned, img.Pinned())
	}
	if !f.rec.has("cluster.NodePull captf-test-dev " + strings.Join(pinned, ",")) {
		t.Errorf("NodePull refs: %v", f.rec.calls)
	}
	st, err := ReadState(work)
	if err != nil {
		t.Fatal(err)
	}
	if st.ManagerRef != managerRef || st.TreeID != "abc123" || st.Engine != "podman" || st.Workers != 1 ||
		st.Operation != "up" || st.Kubeconfig != f.cfg.Kubeconfig() || !st.Pins.Equal(CurrentPins()) {
		t.Errorf("state: %+v", st)
	}
	if len(st.Timings) != upStepCount-1 || len(st.NodeImages) != 1+len(framework.NoopImages()) || len(st.NoopImages) != len(framework.NoopImages()) {
		t.Errorf("state timings %d, node images %d, noop images %d", len(st.Timings), len(st.NodeImages), len(st.NoopImages))
	}
	script, err := os.ReadFile(filepath.Join(work, envFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "export KUBECONFIG='"+f.cfg.Kubeconfig()+"'") {
		t.Errorf("env.sh:\n%s", script)
	}
	for _, s := range []string{"up [1/8] preflight", "up [8/8] write state.json and env.sh done", "up [5/8] pull noop images in nodes done", "repo digests in the nodes: kept", "up: ready in"} {
		if !strings.Contains(f.out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, f.out)
		}
	}
}

// TestUpRefusesExisting checks that Up never touches an existing cluster
// without Reuse, and collects nothing since it created nothing.
func TestUpRefusesExisting(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.cluster.clusters[f.cfg.Name] = true
	err := f.env.Up(context.Background())
	if err == nil || !strings.Contains(err.Error(), ReuseEnv) {
		t.Fatalf("Up: got %v, want an error naming %s", err, ReuseEnv)
	}
	if f.rec.has("cluster.Create") || f.rec.has("kube.Collect") {
		t.Errorf("Up created or collected: %v", f.rec.calls)
	}
}

// writePrev writes a state.json for f's cluster with managerRef ref, as a
// previous Up would, changed by mutate when non-nil, and marks the cluster
// as existing; t fails on a write error.
func writePrev(t *testing.T, f *fixture, ref string, mutate func(*State)) {
	t.Helper()
	if err := os.MkdirAll(f.cfg.WorkDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	_, noops := noopRefs()
	st := &State{Cluster: f.cfg.Name, Engine: "podman", Pins: CurrentPins(), ManagerRef: ref, NoopImages: noops, Kubeconfig: f.cfg.Kubeconfig()}
	if mutate != nil {
		mutate(st)
	}
	if err := WriteState(f.cfg.WorkDir(), st); err != nil {
		t.Fatal(err)
	}
	f.cluster.clusters[f.cfg.Name] = true
	f.kube.image = ref
}

// TestUpReuseUnchanged checks that a reused cluster whose manager is
// current is neither re-loaded nor re-initialized.
func TestUpReuseUnchanged(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(c *Config) { c.Reuse = true })
	writePrev(t, f, managerRef, nil)
	if err := f.env.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v\n%s", err, f.out)
	}
	for _, c := range []string{"cluster.Create", "cluster.SideLoad", "host.InitProviders", "kube.SetManagerImage"} {
		if f.rec.has(c) {
			t.Errorf("reuse called %s: %v", c, f.rec.calls)
		}
	}
	for _, c := range []string{"cluster.Kubeconfig", "cluster.NodePull", "cluster.NodeImages", "kube.ManagerImage", "kube.WaitProviders"} {
		if !f.rec.has(c) {
			t.Errorf("reuse did not call %s: %v", c, f.rec.calls)
		}
	}
	kc, err := os.ReadFile(f.cfg.Kubeconfig())
	if err != nil || string(kc) != "kubeconfig from kind" {
		t.Errorf("kubeconfig not rewritten: %q, %v", kc, err)
	}
	st, err := ReadState(f.cfg.WorkDir())
	if err != nil || st.Operation != "up (reused)" {
		t.Errorf("state after reuse: %+v, %v", st, err)
	}
}

// TestUpReuseNewManager checks that a reused cluster gets a new manager
// image side-loaded and rolled out.
func TestUpReuseNewManager(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(c *Config) { c.Reuse = true })
	writePrev(t, f, "localhost/captf/manager:old", nil)
	if err := f.env.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v\n%s", err, f.out)
	}
	if !f.rec.has("cluster.SideLoad captf-test-dev " + managerRef + " ") {
		t.Errorf("SideLoad: %v", f.rec.calls)
	}
	if !f.rec.has("kube.SetManagerImage "+managerRef) || !f.rec.has("kube.WaitManagerRollout") {
		t.Errorf("no rollout: %v", f.rec.calls)
	}
}

// TestUpReuseRefusals checks every reason a cluster cannot be reused.
func TestUpReuseRefusals(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mutate  func(*State)
		noState bool
		want    string
	}{
		"pins":    {mutate: func(s *State) { s.Pins.CAPIVersion = "v0.0.1" }, want: "other pins"},
		"engine":  {mutate: func(s *State) { s.Engine = "docker" }, want: "runs on docker"},
		"workers": {mutate: func(s *State) { s.Workers = 2 }, want: "has 2 workers"},
		"state":   {noState: true, want: "cannot be reused"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, func(c *Config) { c.Reuse = true })
			if tc.noState {
				f.cluster.clusters[f.cfg.Name] = true
			} else {
				writePrev(t, f, managerRef, tc.mutate)
			}
			err := f.env.Up(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Up: got %v, want %q", err, tc.want)
			}
			if f.rec.has("kube.Collect") {
				t.Errorf("collected for a cluster Up did not take on: %v", f.rec.calls)
			}
		})
	}
}

// TestUpFailures checks which failures stop Up where, and that only those
// after the cluster exists collect diagnostics.
func TestUpFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		fail    string
		collect bool
	}{
		{"host.DetectEngine", false},
		{"deps.Cluster", false},
		{"host.TreeID", false},
		{"host.BuildManager", false},
		{"host.SmokeManager", false},
		{"cluster.Exists", false},
		{"cluster.Create", false},
		{"cluster.SideLoad", true},
		{"cluster.NodePull", true},
		{"cluster.NodeImages", true},
		{"host.EnsureCache", true},
		{"host.RenderCAPTF", true},
		{"host.WriteRepository", true},
		{"host.InitProviders", true},
		{"kube.WaitProviders", true},
	} {
		t.Run(tc.fail, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, nil)
			f.rec.fail[tc.fail] = errBoom
			err := f.env.Up(context.Background())
			if !errors.Is(err, errBoom) {
				t.Fatalf("Up: got %v, want errBoom", err)
			}
			if got := f.rec.has("kube.Collect"); got != tc.collect {
				t.Errorf("collected = %v, want %v: %v", got, tc.collect, f.rec.calls)
			}
			if tc.collect {
				if !strings.Contains(f.out.String(), "diagnostics written to "+filepath.Join(f.cfg.WorkDir(), artifactsDir)) {
					t.Errorf("no diagnostics path in output:\n%s", f.out)
				}
				if !f.rec.has("cluster.CollectLogs captf-test-dev " + filepath.Join(f.cfg.WorkDir(), artifactsDir)) {
					t.Errorf("node logs not collected: %v", f.rec.calls)
				}
			}
			if _, err := os.Stat(filepath.Join(f.cfg.WorkDir(), stateFile)); !os.IsNotExist(err) {
				t.Errorf("state.json written after a failure: %v", err)
			}
		})
	}
}

// TestUpDiagnosticsFailure checks that a failing collection is reported
// and the original error returned.
func TestUpDiagnosticsFailure(t *testing.T) {
	t.Parallel()
	for _, fail := range []string{"kube.Collect", "deps.Kube"} {
		t.Run(fail, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, nil)
			f.rec.fail["host.InitProviders"] = errBoom
			f.rec.fail[fail] = errors.New("collect broke")
			if err := f.env.Up(context.Background()); !errors.Is(err, errBoom) {
				t.Fatalf("Up: got %v, want errBoom", err)
			}
			if !strings.Contains(f.out.String(), "diagnostics failed: collect broke") {
				t.Errorf("output:\n%s", f.out)
			}
		})
	}
}

// TestUpClusterAlone checks that phase 1 stops after the side-load, writes
// no state.json and returns the in-progress state with its own timings.
func TestUpClusterAlone(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	res, err := f.env.UpCluster(context.Background())
	if err != nil {
		t.Fatalf("UpCluster: %v\n%s", err, f.out)
	}
	want := []string{
		"host.DetectEngine", "host.TreeID", "host.BuildManager", "host.SmokeManager",
		"cluster.Exists", "cluster.Create", "cluster.SideLoad", "cluster.NodePull", "cluster.NodeImages",
	}
	if got := f.rec.names(); !slices.Equal(got, want) {
		t.Errorf("calls:\n got %v\nwant %v", got, want)
	}
	st := res.State
	if res.reused || res.cl == nil || st.ManagerRef != managerRef || st.TreeID != "abc123" || st.Engine != "podman" || st.Operation != "up" {
		t.Errorf("result: %+v, state %+v", res, st)
	}
	if len(st.Timings) != clusterStepCount || len(st.NodeImages) == 0 {
		t.Errorf("timings %d, node images %d", len(st.Timings), len(st.NodeImages))
	}
	if _, err := os.Stat(filepath.Join(f.cfg.WorkDir(), stateFile)); !os.IsNotExist(err) {
		t.Errorf("state.json written by phase 1: %v", err)
	}
	if strings.Contains(f.out.String(), "up [6/8]") || strings.Contains(f.out.String(), "up: ready") {
		t.Errorf("phase 1 ran phase 2 steps:\n%s", f.out)
	}
}

// TestUpClusterReused checks that phase 1 marks a reused cluster.
func TestUpClusterReused(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(c *Config) { c.Reuse = true })
	writePrev(t, f, managerRef, nil)
	res, err := f.env.UpCluster(context.Background())
	if err != nil {
		t.Fatalf("UpCluster: %v\n%s", err, f.out)
	}
	if !res.reused || res.State.Operation != "up (reused)" {
		t.Errorf("result: %+v, state %+v", res, res.State)
	}
}

// TestInstallProvidersAlone checks that phase 2 runs on a prepared result,
// continues the step numbering and writes both phases' timings.
func TestInstallProvidersAlone(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	res, err := f.env.UpCluster(context.Background())
	if err != nil {
		t.Fatalf("UpCluster: %v\n%s", err, f.out)
	}
	f.rec.calls = nil
	if err := f.env.InstallProviders(context.Background(), res); err != nil {
		t.Fatalf("InstallProviders: %v\n%s", err, f.out)
	}
	want := []string{"host.EnsureCache", "host.RenderCAPTF", "host.WriteRepository", "host.InitProviders", "kube.WaitProviders"}
	if got := f.rec.names(); !slices.Equal(got, want) {
		t.Errorf("calls:\n got %v\nwant %v", got, want)
	}
	st, err := ReadState(f.cfg.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Timings) != upStepCount-1 || st.Timings[0].Step != "preflight" || st.Timings[clusterStepCount].Step != "install the providers (reuse: update the manager)" {
		t.Errorf("timings: %+v", st.Timings)
	}
	if !strings.Contains(f.out.String(), "up [6/8] install the providers") || !strings.Contains(f.out.String(), "up: ready in") {
		t.Errorf("output:\n%s", f.out)
	}
}

// TestPhaseFailuresCollect checks that a failure in either phase after the
// cluster exists collects diagnostics, and that phase 2 failures leave no
// state.json.
func TestPhaseFailuresCollect(t *testing.T) {
	t.Parallel()
	for _, fail := range []string{"cluster.SideLoad", "cluster.NodePull", "host.InitProviders", "kube.WaitProviders"} {
		t.Run(fail, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, nil)
			f.rec.fail[fail] = errBoom
			res, err := f.env.UpCluster(context.Background())
			if fail == "cluster.SideLoad" || fail == "cluster.NodePull" {
				if !errors.Is(err, errBoom) || res != nil {
					t.Fatalf("UpCluster: got %v, %v", res, err)
				}
			} else {
				if err != nil {
					t.Fatalf("UpCluster: %v", err)
				}
				if err := f.env.InstallProviders(context.Background(), res); !errors.Is(err, errBoom) {
					t.Fatalf("InstallProviders: got %v, want errBoom", err)
				}
			}
			if !f.rec.has("kube.Collect") {
				t.Errorf("no diagnostics collected: %v", f.rec.calls)
			}
			if _, err := os.Stat(filepath.Join(f.cfg.WorkDir(), stateFile)); !os.IsNotExist(err) {
				t.Errorf("state.json written after a failure: %v", err)
			}
		})
	}
}

// TestUpMissingTools checks that preflight requires executable tools.
func TestUpMissingTools(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.env.cfg.Kustomize = filepath.Join(f.cfg.RepoRoot, "missing")
	if err := f.env.Up(context.Background()); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Up: got %v, want a missing-tool error", err)
	}
	f.env.cfg.Kustomize = f.cfg.RepoRoot
	if err := f.env.Up(context.Background()); err == nil || !strings.Contains(err.Error(), "not an executable") {
		t.Fatalf("Up: got %v, want a not-executable error", err)
	}
	if f.rec.has("host.DetectEngine") {
		t.Errorf("preflight detected an engine without tools: %v", f.rec.calls)
	}
}

// TestDown checks that Down deletes the cluster with its explicit
// kubeconfig, keeps artifacts and removes the network when no test
// cluster is left.
func TestDown(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.cluster.clusters[f.cfg.Name] = true
	art := filepath.Join(f.cfg.WorkDir(), artifactsDir, "x")
	if err := os.MkdirAll(art, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.cfg.WorkDir(), stateFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.env.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.rec.has("cluster.Delete captf-test-dev " + f.cfg.Kubeconfig()) {
		t.Errorf("Delete: %v", f.rec.calls)
	}
	if !f.rec.has("host.RemoveNetwork " + framework.KindNetwork) {
		t.Errorf("network not removed: %v", f.rec.calls)
	}
	if _, err := os.Stat(art); err != nil {
		t.Errorf("artifacts removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.WorkDir(), stateFile)); !os.IsNotExist(err) {
		t.Errorf("state.json kept: %v", err)
	}
}

// TestDownWithoutArtifacts checks that a work directory without
// artifacts is removed entirely, and that Down on a missing cluster
// succeeds.
func TestDownWithoutArtifacts(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	if err := os.MkdirAll(filepath.Join(f.cfg.WorkDir(), "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.env.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.cfg.WorkDir()); !os.IsNotExist(err) {
		t.Errorf("work directory kept: %v", err)
	}
}

// TestDownKeepsNetwork checks that Down keeps the network while another
// test cluster uses it.
func TestDownKeepsNetwork(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.cluster.clusters[f.cfg.Name] = true
	f.cluster.clusters["captf-test-other"] = true
	if err := f.env.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.rec.has("host.RemoveNetwork") {
		t.Errorf("network removed while captf-test-other uses it: %v", f.rec.calls)
	}
	if !strings.Contains(f.out.String(), "still used by captf-test-other") {
		t.Errorf("output:\n%s", f.out)
	}
}

// TestDownAll checks that Down with All deletes every listed cluster.
func TestDownAll(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(c *Config) { c.All = true })
	f.cluster.clusters["captf-test-a"] = true
	f.cluster.clusters["captf-test-b"] = true
	if err := f.env.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"captf-test-a", "captf-test-b"} {
		if !f.rec.has("cluster.Delete " + n + " " + filepath.Join(f.cfg.TestenvRoot(), n, "kubeconfig")) {
			t.Errorf("%s not deleted: %v", n, f.rec.calls)
		}
	}
	if f.rec.has("cluster.Delete captf-test-dev") {
		t.Errorf("deleted the unlisted configured cluster: %v", f.rec.calls)
	}

	empty := newFixture(t, func(c *Config) { c.All = true })
	if err := empty.env.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(empty.out.String(), "no captf-test-* cluster exists") {
		t.Errorf("output:\n%s", empty.out)
	}
}

// TestDownErrors checks that Down reports kind and network failures.
func TestDownErrors(t *testing.T) {
	t.Parallel()
	for _, fail := range []string{"host.DetectEngine", "cluster.Delete", "cluster.List", "host.RemoveNetwork"} {
		t.Run(fail, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, nil)
			f.cluster.clusters[f.cfg.Name] = true
			f.rec.fail[fail] = errBoom
			if err := f.env.Down(context.Background()); !errors.Is(err, errBoom) {
				t.Fatalf("Down: got %v, want errBoom", err)
			}
		})
	}
	f := newFixture(t, func(c *Config) { c.All = true })
	f.rec.fail["cluster.List"] = errBoom
	if err := f.env.Down(context.Background()); !errors.Is(err, errBoom) {
		t.Fatalf("Down all: got %v, want errBoom", err)
	}
}

// TestRemoveWorkDirRefuses checks that removeWorkDir never touches an
// invalid name.
func TestRemoveWorkDirRefuses(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{framework.ProtectedClusterName, "../x", "", "captf-test-a/../../b"} {
		if err := removeWorkDir(root, name); err == nil {
			t.Errorf("removeWorkDir(%q) succeeded", name)
		}
	}
}

// TestStatus checks the report on a missing and on a running cluster.
func TestStatus(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	rep, err := f.env.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Exists || rep.State != nil || len(rep.Problems) != 1 || !strings.Contains(f.out.String(), "does not exist") {
		t.Errorf("missing cluster: %+v\n%s", rep, f.out)
	}

	g := newFixture(t, nil)
	writePrev(t, g, managerRef, func(s *State) { s.Pins.KindVersion = "v0.0.0" })
	rep, err = g.env.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Exists || rep.State == nil || rep.Pods != "no non-ready pods" || len(rep.Nodes) != 1 || len(rep.Problems) != 0 {
		t.Errorf("running cluster: %+v", rep)
	}
	for _, s := range []string{"nodes: captf-test-dev-control-plane", "manager: " + managerRef, "differ from this framework build", "noop repo digests on the nodes: unknown"} {
		if !strings.Contains(g.out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, g.out)
		}
	}
}

// TestStatusProblems checks that failing checks are reported, not
// returned, and that kind failures are returned.
func TestStatusProblems(t *testing.T) {
	t.Parallel()
	for _, fail := range []string{"cluster.Nodes", "deps.Kube", "kube.Report"} {
		t.Run(fail, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, nil)
			writePrev(t, f, managerRef, nil)
			f.rec.fail[fail] = errBoom
			rep, err := f.env.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Problems) != 1 || !strings.Contains(f.out.String(), "problem: ") {
				t.Errorf("problems %v\n%s", rep.Problems, f.out)
			}
		})
	}
	for _, fail := range []string{"cluster.Exists", "host.DetectEngine"} {
		f := newFixture(t, nil)
		f.rec.fail[fail] = errBoom
		if _, err := f.env.Status(context.Background()); !errors.Is(err, errBoom) {
			t.Errorf("%s: got %v, want errBoom", fail, err)
		}
	}
}

// TestCollect checks the on-demand diagnostics path and its refusals.
func TestCollect(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.cluster.clusters[f.cfg.Name] = true
	if err := os.MkdirAll(f.cfg.WorkDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := f.env.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(f.cfg.WorkDir(), artifactsDir, "20261004T120001Z"); dir != want {
		t.Errorf("dir = %s, want %s", dir, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "node-logs")); err != nil {
		t.Errorf("node logs: %v", err)
	}

	missing := newFixture(t, nil)
	if _, err := missing.env.Collect(context.Background()); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("Collect on a missing cluster: %v", err)
	}
	for _, fail := range []string{"host.DetectEngine", "cluster.Exists", "cluster.Kubeconfig", "kube.Collect"} {
		g := newFixture(t, nil)
		g.cluster.clusters[g.cfg.Name] = true
		if err := os.MkdirAll(g.cfg.WorkDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		g.rec.fail[fail] = errBoom
		if _, err := g.env.Collect(context.Background()); !errors.Is(err, errBoom) {
			t.Errorf("%s: got %v, want errBoom", fail, err)
		}
	}
}

// TestCollectKubeconfigUnwritable checks that a kubeconfig that cannot be
// written fails Collect.
func TestCollectKubeconfigUnwritable(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.cluster.clusters[f.cfg.Name] = true
	// No work directory: the kubeconfig has nowhere to go.
	if _, err := f.env.Collect(context.Background()); err == nil || !strings.Contains(err.Error(), "write kubeconfig") {
		t.Errorf("Collect: got %v, want a kubeconfig write error", err)
	}
}

// TestReload checks a reload that changes the manager image, and one that
// finds it unchanged.
func TestReload(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	writePrev(t, f, "localhost/captf/manager:old", func(s *State) {
		s.NodeImages = []NodeImage{{Node: "n", Ref: "localhost/captf/manager:old"}, {Node: "n", Ref: "ghcr.io/x:y"}}
	})
	if err := f.env.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v\n%s", err, f.out)
	}
	want := []string{
		"host.DetectEngine", "cluster.Exists", "cluster.Kubeconfig", "host.TreeID", "host.BuildManager", "host.SmokeManager",
		"cluster.SideLoad", "cluster.NodeImages", "kube.ManagerImage", "kube.SetManagerImage", "kube.WaitManagerRollout",
	}
	if got := f.rec.names(); !slices.Equal(got, want) {
		t.Errorf("calls:\n got %v\nwant %v", got, want)
	}
	st, err := ReadState(f.cfg.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	if st.ManagerRef != managerRef || st.TreeID != "abc123" || st.Operation != "reload" || len(st.Timings) != reloadStepCount-1 {
		t.Errorf("state: %+v", st)
	}
	var refs []string
	for _, n := range st.NodeImages {
		refs = append(refs, n.Ref)
	}
	if !slices.Equal(refs, []string{managerRef, "ghcr.io/x:y"}) {
		t.Errorf("node image refs: %v", refs)
	}

	g := newFixture(t, nil)
	writePrev(t, g, managerRef, nil)
	if err := g.env.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.rec.has("kube.SetManagerImage") || !strings.Contains(g.out.String(), "unchanged") {
		t.Errorf("unchanged reload rolled out: %v\n%s", g.rec.calls, g.out)
	}
}

// TestReloadFailures checks the reload refusals and that failures after
// preflight collect diagnostics.
func TestReloadFailures(t *testing.T) {
	t.Parallel()
	missing := newFixture(t, nil)
	if err := missing.env.Reload(context.Background()); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("Reload on a missing cluster: %v", err)
	}
	noState := newFixture(t, nil)
	noState.cluster.clusters[noState.cfg.Name] = true
	if err := os.MkdirAll(noState.cfg.WorkDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := noState.env.Reload(context.Background()); err == nil || !strings.Contains(err.Error(), "needs the state") {
		t.Errorf("Reload without state: %v", err)
	}
	for _, tc := range []struct {
		fail    string
		collect bool
	}{
		{"deps.Kube", false},
		{"host.BuildManager", true},
		{"cluster.SideLoad", true},
		{"cluster.NodeImages", true},
		{"kube.ManagerImage", true},
		{"kube.SetManagerImage", true},
		{"kube.WaitManagerRollout", true},
	} {
		t.Run(tc.fail, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, nil)
			writePrev(t, f, "localhost/captf/manager:old", nil)
			f.rec.fail[tc.fail] = errBoom
			if err := f.env.Reload(context.Background()); !errors.Is(err, errBoom) {
				t.Fatalf("Reload: got %v, want errBoom", err)
			}
			// A failed Kube factory also fails the collection; only the
			// attempt matters here.
			if got := f.rec.has("kube.Collect"); got != tc.collect {
				t.Errorf("collected = %v, want %v: %v", got, tc.collect, f.rec.calls)
			}
		})
	}
}
