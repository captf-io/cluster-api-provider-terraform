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
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/images"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/kindcluster"
)

// collectTimeout bounds the diagnostics run after a failure. It runs on a
// context detached from the failed operation's, which may have expired.
const collectTimeout = 3 * time.Minute

// The work-directory entries besides state.json and env.sh.
const (
	// artifactsDir holds diagnostics bundles; Down keeps it.
	artifactsDir = "artifacts"
	// imagesDir holds the side-load archives while they are loaded.
	imagesDir = "images"
	// captfDir holds the rendered CAPTF provider.
	captfDir = "captf"
	// repoDir is the clusterctl local repository.
	repoDir = "repo"
	// homeDir is clusterctl's isolated HOME.
	homeDir = "home"
)

// Env runs the environment operations for one Config. Build it with New.
type Env struct {
	// cfg is the resolved configuration.
	cfg Config
	// out receives every progress line.
	out io.Writer
	// deps are the side effects.
	deps Deps
	// now is the clock.
	now func() time.Time
}

// Option configures New.
type Option func(*Env)

// WithDeps replaces the real side effects with d; it is for tests. Nil
// fields keep the real implementation. It returns the Option.
func WithDeps(d Deps) Option {
	return func(e *Env) {
		if d.Host != nil {
			e.deps.Host = d.Host
		}
		if d.Cluster != nil {
			e.deps.Cluster = d.Cluster
		}
		if d.Kube != nil {
			e.deps.Kube = d.Kube
		}
	}
}

// WithClock makes the Env read the time from now; it is for tests. It
// returns the Option.
func WithClock(now func() time.Time) Option {
	return func(e *Env) { e.now = now }
}

// New returns the Env for cfg writing progress to out (nil discards it),
// with the real Host, Cluster and Kube unless opts replace them. It
// returns an error if cfg's name fails kindcluster.ValidateName or its
// RepoRoot is empty.
func New(cfg Config, out io.Writer, opts ...Option) (*Env, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if out == nil {
		out = io.Discard
	}
	e := &Env{cfg: cfg, out: out, now: time.Now}
	e.deps = Deps{
		Host:    NewHost(cfg, out),
		Cluster: func(eng *engine.Engine) (Cluster, error) { return NewCluster(eng, out) },
		Kube:    func(kubeconfig string) (Kube, error) { return NewKube(kubeconfig, out) },
	}
	for _, o := range opts {
		o(e)
	}
	return e, nil
}

// logf writes one "testenv: " line formatted from format and args.
func (e *Env) logf(format string, args ...any) {
	fmt.Fprintf(e.out, "testenv: "+format+"\n", args...)
}

// run is one operation in progress: its step timings.
type run struct {
	// env is the Env running the operation.
	env *Env
	// op names the operation in the log ("up", "reload", ...).
	op string
	// total is the operation's step count, for "[i/total]".
	total int
	// n is the number of steps started so far.
	n int
	// timings are the finished steps.
	timings []Timing
}

// newRun returns a run of the operation op with total steps.
func (e *Env) newRun(op string, total int) *run {
	return &run{env: e, op: op, total: total}
}

// step runs fn as the next step called name, logging its start, its
// duration and any error, and recording its timing. It returns fn's error.
func (r *run) step(name string, fn func() error) error {
	r.n++
	r.env.logf("%s [%d/%d] %s", r.op, r.n, r.total, name)
	start := r.env.now()
	err := fn()
	d := r.env.now().Sub(start)
	r.timings = append(r.timings, Timing{Step: name, Seconds: d.Round(time.Millisecond).Seconds()})
	if err != nil {
		r.env.logf("%s [%d/%d] %s failed after %s: %v", r.op, r.n, r.total, name, d.Round(time.Millisecond), err)
		return err
	}
	r.env.logf("%s [%d/%d] %s done in %s", r.op, r.n, r.total, name, d.Round(time.Millisecond))
	return nil
}

// preflight checks the cluster name, detects the engine under ctx and
// builds the Cluster for it; withTools also requires the pinned clusterctl
// and kustomize binaries. It returns the engine and Cluster, or an error.
func (e *Env) preflight(ctx context.Context, withTools bool) (*engine.Engine, Cluster, error) {
	if err := kindcluster.ValidateName(e.cfg.Name); err != nil {
		return nil, nil, fmt.Errorf("env: preflight: %w", err)
	}
	if withTools {
		for _, tool := range []string{e.cfg.Clusterctl, e.cfg.Kustomize} {
			if err := checkTool(tool); err != nil {
				return nil, nil, fmt.Errorf("env: preflight: %w (run make %s, or use the testenv-* make targets)", err, filepath.Base(tool))
			}
		}
	}
	eng, err := e.deps.Host.DetectEngine(ctx, e.cfg.EngineOverride)
	if err != nil {
		return nil, nil, fmt.Errorf("env: preflight: %w", err)
	}
	cl, err := e.deps.Cluster(eng)
	if err != nil {
		return nil, nil, fmt.Errorf("env: preflight: %w", err)
	}
	e.logf("engine %s, cluster %s, work directory %s", eng.Name, e.cfg.Name, e.cfg.WorkDir())
	return eng, cl, nil
}

// checkTool returns nil if path is an executable regular file (symlinks
// followed), else an error naming it.
func checkTool(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("tool %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("tool %s is not an executable file", path)
	}
	return nil
}

// noopRefs returns the references the nodes pull (the digest-pinned
// repository@digest form) of the noop images, and the NoopRef records for
// state.json.
func noopRefs() ([]string, []NoopRef) {
	var refs []string
	var recs []NoopRef
	for _, img := range framework.NoopImages() {
		refs = append(refs, img.Pinned())
		recs = append(recs, NoopRef{Ref: img.Ref, Pinned: img.Pinned()})
	}
	return refs, recs
}

// buildManager returns the manager image reference for the current tree
// after building and smoke-checking it with engine eng under ctx, or an
// error. treeID receives the tree identity.
func (e *Env) buildManager(ctx context.Context, eng *engine.Engine, treeID *string) (string, error) {
	id, err := e.deps.Host.TreeID(ctx)
	if err != nil {
		return "", err
	}
	*treeID = id
	ref := images.ManagerRef(id)
	if err := e.deps.Host.BuildManager(ctx, eng, ref); err != nil {
		return "", err
	}
	if err := e.deps.Host.SmokeManager(ctx, eng, ref); err != nil {
		return "", err
	}
	return ref, nil
}

// upStepCount is the number of steps Up logs.
const upStepCount = 8

// clusterStepCount is how many of Up's steps UpCluster runs; the rest
// belong to InstallProviders.
const clusterStepCount = 5

// ClusterResult is what UpCluster hands to InstallProviders: the
// in-progress State and the context phase 2 needs but state.json does not
// record. It is only valid for the Env that produced it.
type ClusterResult struct {
	// State is the in-progress state: everything known after phase 1, with
	// the phase 1 step timings. InstallProviders completes and writes it.
	State *State
	// cl is the Cluster built for the engine; phase 2 collects through it.
	cl Cluster
	// reused reports that the cluster already existed and was reused.
	reused bool
	// started is when Up began, for the final "ready in" line.
	started time.Time
}

// Up brings the environment up under ctx: UpCluster, then InstallProviders
// on its result. Once the cluster exists, any failure first writes a
// diagnostics bundle into <work>/artifacts/ and prints its path. It returns
// the first error.
func (e *Env) Up(ctx context.Context) error {
	res, err := e.UpCluster(ctx)
	if err != nil {
		return err
	}
	return e.InstallProviders(ctx, res)
}

// UpCluster is phase 1 of Up, under ctx: preflight, build and smoke the
// manager image, create (or, with Reuse, reuse) the cluster, side-load the
// manager image, then pull the noop images inside every node (reuse pulls
// too) and record what each node holds. Once
// the cluster exists, a failure writes a diagnostics bundle first. Nothing
// is written to state.json. It returns the result for InstallProviders, or
// the first error.
func (e *Env) UpCluster(ctx context.Context) (res *ClusterResult, err error) {
	started := e.now()
	r := e.newRun("up", upStepCount)
	work := e.cfg.WorkDir()
	st := &State{Cluster: e.cfg.Name, Workers: e.cfg.Workers, Kubeconfig: e.cfg.Kubeconfig(), Pins: CurrentPins(), Operation: "up"}
	noops, noopRecs := noopRefs()
	st.NoopImages = noopRecs

	var eng *engine.Engine
	var cl Cluster
	if err := r.step("preflight", func() error {
		var err error
		eng, cl, err = e.preflight(ctx, true)
		return err
	}); err != nil {
		return nil, err
	}
	st.Engine = string(eng.Name)

	if err := r.step("build and smoke-check the manager image", func() error {
		var err error
		st.ManagerRef, err = e.buildManager(ctx, eng, &st.TreeID)
		return err
	}); err != nil {
		return nil, err
	}

	// Past this point the cluster exists: collect diagnostics on failure.
	clusterUp := false
	defer func() {
		if err != nil && clusterUp {
			e.collectAfterFailure(ctx, cl)
		}
	}()

	var prev *State
	if err := r.step("create or reuse the kind cluster", func() error {
		var err error
		prev, err = e.ensureCluster(ctx, eng, cl)
		clusterUp = err == nil
		return err
	}); err != nil {
		return nil, err
	}
	reused := prev != nil
	if reused {
		st.Operation = "up (reused)"
	}

	if err := r.step("side-load the manager image", func() error {
		if reused && prev.ManagerRef == st.ManagerRef {
			e.logf("manager image %s is already loaded", st.ManagerRef)
			return nil
		}
		return cl.SideLoad(ctx, e.cfg.Name, []string{st.ManagerRef}, filepath.Join(work, imagesDir))
	}); err != nil {
		return nil, err
	}

	// The noop module images are pulled in the nodes, never side-loaded: a
	// side-load drops the repo digests, and CAPTF's digest pinning needs
	// the registry's. A reused cluster runs the pull too; crictl pull is
	// idempotent and fast for an image the node holds.
	if err := r.step("pull noop images in nodes", func() error {
		if err := cl.NodePull(ctx, e.cfg.Name, noops); err != nil {
			return err
		}
		recs, err := cl.NodeImages(ctx, e.cfg.Name, append([]string{st.ManagerRef}, noops...))
		if err != nil {
			return err
		}
		st.NodeImages = recs
		e.logf("noop image repo digests in the nodes: %s", DigestFinding(recs, st.NoopImages))
		return nil
	}); err != nil {
		return nil, err
	}
	st.Timings = r.timings
	return &ClusterResult{State: st, cl: cl, reused: reused, started: started}, nil
}

// InstallProviders is phase 2 of Up, under ctx, on the result res of
// UpCluster: install the providers (when the cluster was reused, point the
// manager at the new image instead), wait for readiness, and write
// state.json and env.sh with the timings of both phases. A failure writes a
// diagnostics bundle first. It returns the first error.
func (e *Env) InstallProviders(ctx context.Context, res *ClusterResult) (err error) {
	defer func() {
		if err != nil {
			e.collectAfterFailure(ctx, res.cl)
		}
	}()
	st := res.State
	work := e.cfg.WorkDir()
	kubeconfig := e.cfg.Kubeconfig()
	r := e.newRun("up", upStepCount)
	r.n = clusterStepCount
	r.timings = slices.Clone(st.Timings)

	var kube Kube
	if err := r.step("install the providers (reuse: update the manager)", func() error {
		if res.reused {
			var err error
			if kube, err = e.deps.Kube(kubeconfig); err != nil {
				return err
			}
			return e.pointManagerAt(ctx, kube, st.ManagerRef)
		}
		return e.installProviders(ctx, st.ManagerRef)
	}); err != nil {
		return err
	}

	if err := r.step("wait for readiness", func() error {
		if kube == nil {
			var err error
			if kube, err = e.deps.Kube(kubeconfig); err != nil {
				return err
			}
		}
		return kube.WaitProviders(ctx)
	}); err != nil {
		return err
	}

	if err := r.step("write state.json and env.sh", func() error {
		st.Timings = r.timings
		st.UpdatedAt = e.now().UTC()
		if err := WriteState(work, st); err != nil {
			return err
		}
		return writeEnvScript(work, st)
	}); err != nil {
		return err
	}
	e.logf("up: ready in %s (%s); kubeconfig %s", e.now().Sub(res.started).Round(time.Second), st.Operation, kubeconfig)
	e.logf("up: source %s", filepath.Join(work, envFile))
	return nil
}

// ensureCluster makes the cluster exist under ctx with engine eng through
// cl. A missing cluster is created (after clearing the work directory,
// artifacts kept) and ensureCluster returns a nil State. An existing one
// is reused only when Reuse is set and its state.json matches the pins,
// engine and worker count; then its kubeconfig is rewritten from kind and
// ensureCluster returns the previous State. Otherwise it returns an error
// that says how to proceed; it never deletes a cluster.
func (e *Env) ensureCluster(ctx context.Context, eng *engine.Engine, cl Cluster) (*State, error) {
	work := e.cfg.WorkDir()
	exists, err := cl.Exists(e.cfg.Name)
	if err != nil {
		return nil, err
	}
	if exists {
		if !e.cfg.Reuse {
			return nil, fmt.Errorf("env: cluster %s already exists: set %s=1 to reuse it, or run make testenv-down", e.cfg.Name, ReuseEnv)
		}
		prev, err := ReadState(work)
		if err != nil {
			return nil, fmt.Errorf("env: cluster %s exists but cannot be reused (%w): run make testenv-down", e.cfg.Name, err)
		}
		if why := reuseMismatch(prev, eng, e.cfg.Workers); why != "" {
			return nil, fmt.Errorf("env: cluster %s exists but cannot be reused: %s; run make testenv-down", e.cfg.Name, why)
		}
		kc, err := cl.Kubeconfig(e.cfg.Name)
		if err != nil {
			return nil, err
		}
		if err := writeFileAtomic(e.cfg.Kubeconfig(), []byte(kc), 0o600); err != nil {
			return nil, fmt.Errorf("env: write kubeconfig: %w", err)
		}
		e.logf("reusing cluster %s (state.json matches the pins)", e.cfg.Name)
		return prev, nil
	}
	if err := removeWorkDir(e.cfg.TestenvRoot(), e.cfg.Name); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, fmt.Errorf("env: create work directory: %w", err)
	}
	return nil, cl.Create(ctx, kindcluster.Options{Name: e.cfg.Name, Workers: e.cfg.Workers, KubeconfigPath: e.cfg.Kubeconfig()})
}

// reuseMismatch returns why the cluster prev describes cannot be reused
// with engine eng and workers worker nodes, or "" when it can.
func reuseMismatch(prev *State, eng *engine.Engine, workers int) string {
	switch {
	case !prev.Pins.Equal(CurrentPins()):
		return "its state.json records other pins"
	case prev.Engine != string(eng.Name):
		return fmt.Sprintf("it runs on %s, not %s", prev.Engine, eng.Name)
	case prev.Workers != workers:
		return fmt.Sprintf("it has %d workers, not %d", prev.Workers, workers)
	default:
		return ""
	}
}

// installProviders, under ctx, fills the download cache, renders CAPTF
// with the manager image ref into <work>/captf, writes the local
// repository into <work>/repo and runs clusterctl init with the isolated
// HOME <work>/home. It returns the first error.
func (e *Env) installProviders(ctx context.Context, ref string) error {
	work := e.cfg.WorkDir()
	if err := e.deps.Host.EnsureCache(ctx, e.cfg.CacheDir); err != nil {
		return err
	}
	captf, repo := filepath.Join(work, captfDir), filepath.Join(work, repoDir)
	for _, d := range []string{captf, repo} {
		if err := os.RemoveAll(d); err != nil {
			return fmt.Errorf("env: install providers: %w", err)
		}
	}
	if err := e.deps.Host.RenderCAPTF(ctx, ref, captf); err != nil {
		return err
	}
	configPath, err := e.deps.Host.WriteRepository(e.cfg.CacheDir, captf, repo)
	if err != nil {
		return err
	}
	return e.deps.Host.InitProviders(ctx, e.cfg.Kubeconfig(), configPath, filepath.Join(work, homeDir))
}

// pointManagerAt makes the manager Deployment run ref through kube under
// ctx: when it already does, it logs that and changes nothing; otherwise
// it patches the image and waits for the rollout. It returns the first
// error.
func (e *Env) pointManagerAt(ctx context.Context, kube Kube, ref string) error {
	cur, err := kube.ManagerImage(ctx)
	if err != nil {
		return err
	}
	if cur == ref {
		e.logf("%s already runs %s: unchanged", ManagerDeployment, ref)
		return nil
	}
	e.logf("pointing %s at %s (was %s)", ManagerDeployment, ref, cur)
	if err := kube.SetManagerImage(ctx, ref); err != nil {
		return err
	}
	return kube.WaitManagerRollout(ctx)
}

// collectAfterFailure writes a diagnostics bundle for the cluster behind
// cl on a context detached from ctx (which may have expired), bounded by
// collectTimeout, and logs its path or why it failed.
func (e *Env) collectAfterFailure(ctx context.Context, cl Cluster) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), collectTimeout)
	defer cancel()
	e.logf("collecting diagnostics after the failure")
	dir, err := e.collect(ctx, cl)
	if err != nil {
		e.logf("diagnostics failed: %v", err)
		return
	}
	e.logf("diagnostics written to %s", dir)
}

// collect writes a diagnostics bundle for the cluster behind cl into
// <work>/artifacts/<UTC timestamp>/ under ctx, with kind's node logs, and
// returns its path or an error.
func (e *Env) collect(ctx context.Context, cl Cluster) (string, error) {
	kube, err := e.deps.Kube(e.cfg.Kubeconfig())
	if err != nil {
		return "", err
	}
	dir := filepath.Join(e.cfg.WorkDir(), artifactsDir, e.now().UTC().Format("20060102T150405Z"))
	if err := kube.Collect(ctx, dir, func(d string) error { return cl.CollectLogs(e.cfg.Name, d) }); err != nil {
		return dir, err
	}
	return dir, nil
}

// removeWorkDir removes the work directory root/name except its artifacts
// directory, and the directory itself when nothing is left. It refuses a
// name that fails kindcluster.ValidateName (a DNS label, so never a path),
// and treats a missing directory as done. It returns an error if removal
// fails.
func removeWorkDir(root, name string) error {
	if err := kindcluster.ValidateName(name); err != nil {
		return fmt.Errorf("env: remove work directory: %w", err)
	}
	dir := filepath.Join(root, name)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("env: remove work directory: %w", err)
	}
	kept := false
	for _, ent := range entries {
		if ent.Name() == artifactsDir {
			kept = true
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, ent.Name())); err != nil {
			return fmt.Errorf("env: remove work directory: %w", err)
		}
	}
	if !kept {
		if err := os.Remove(dir); err != nil {
			return fmt.Errorf("env: remove work directory: %w", err)
		}
	}
	return nil
}
