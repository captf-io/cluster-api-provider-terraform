//go:build e2e

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

package foundation

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/env"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/greenlight"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/kindcluster"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// The suite's time budget. suiteTimeout stays under the make target's
// 60m `go test -timeout`, so a slow run fails with diagnostics instead of
// a test-binary panic.
const (
	// suiteTimeout bounds the whole suite.
	suiteTimeout = 55 * time.Minute
	// collectTimeout bounds the diagnostics run after a failure.
	collectTimeout = 4 * time.Minute
	// teardownTimeout bounds the optional teardown.
	teardownTimeout = 5 * time.Minute
)

// TestMain refuses to run unless -run selects the suite: TestFoundation
// creates and changes a shared kind cluster and must be asked for
// explicitly. It returns through os.Exit with m.Run's code.
func TestMain(m *testing.M) {
	flag.Parse()
	if f := flag.Lookup("test.run"); f == nil || f.Value.String() == "" {
		fmt.Fprintln(os.Stderr, "foundation: select the suite with -run '^TestFoundation$'; see make e2e-foundation")
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// suite is the state the stages share, filled in as they pass.
type suite struct {
	// opts is the suite's configuration.
	opts options
	// cfg is the environment configuration derived from opts.
	cfg env.Config
	// env runs the environment phases.
	env *env.Env
	// eng is the detected container engine (stage 1).
	eng *engine.Engine
	// kind manages the kind cluster (stage 1).
	kind *kindcluster.Manager
	// res is UpCluster's result, for InstallProviders (stage 1).
	res *env.ClusterResult
	// c are the cluster's clients (stage 1).
	c wait.Clients
	// serverVersion is the API server's gitVersion (stage 1).
	serverVersion string
	// managerRef is the manager image the cluster runs (stages 1 and 3).
	managerRef string
	// treeID is the tree the manager image was built from (stage 1).
	treeID string
	// managerPods are the names of the CAPTF manager pods (stage 4).
	managerPods []string
	// managerPod is the manager pod holding the leader Lease (stage 4).
	managerPod string
	// envCollected records that an env phase failed and so already wrote
	// a diagnostics bundle.
	envCollected bool
	// stages are the stages run so far, with their durations.
	stages []greenlight.Stage
}

// stage is one ordered step of TestFoundation.
type stage struct {
	// name is the stage name, as recorded in greenlight.json.
	name string
	// run runs the stage under ctx, failing t on any problem.
	run func(ctx context.Context, t *testing.T)
}

// TestFoundation builds the e2e cluster and green-lights it, stage by
// stage. A failed stage stops the suite, collects diagnostics and keeps
// the cluster.
func TestFoundation(t *testing.T) {
	s := newSuite(t)
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()
	t.Logf("foundation: cluster %s, work directory %s, reuse %t, teardown %t, stability window %s",
		s.cfg.Name, s.cfg.WorkDir(), s.opts.reuse, s.opts.teardown, s.opts.stability)

	stages := []stage{
		{"cluster-build", s.clusterBuild},
		{"base-components", s.baseComponents},
		{"captf-install", s.captfInstall},
		{"captf-components", s.captfComponents},
		{"together", s.together},
		{"green-light", s.greenLight},
	}
	for i, st := range stages {
		start := time.Now()
		ok := t.Run(fmt.Sprintf("%d-%s", i+1, st.name), func(t *testing.T) { st.run(ctx, t) })
		if st.name != "green-light" || !ok {
			s.stages = append(s.stages, greenlight.Stage{Name: st.name, Duration: time.Since(start), Passed: ok})
		}
		if !ok {
			s.afterFailure(ctx, t, st.name)
			t.FailNow()
		}
	}
	s.afterSuccess(ctx, t)
}

// newSuite returns the suite for the configuration in the environment,
// failing t when it is invalid.
func newSuite(t *testing.T) *suite {
	t.Helper()
	o, err := optionsFrom(os.Getenv)
	if err != nil {
		t.Fatalf("foundation: configuration: %v", err)
	}
	cfg, err := envConfig(o)
	if err != nil {
		t.Fatalf("foundation: configuration: %v", err)
	}
	e, err := env.New(cfg, os.Stderr)
	if err != nil {
		t.Fatalf("foundation: configuration: %v", err)
	}
	return &suite{opts: o, cfg: cfg, env: e}
}

// nodeCount returns the number of nodes the cluster should have: the
// control plane plus the workers.
func (s *suite) nodeCount() int {
	return 1 + s.opts.workers
}

// kubectl returns a kubectl command line against the e2e cluster with
// args, for "inspect" hints in failure messages.
func (s *suite) kubectl(args string) string {
	return fmt.Sprintf("KUBECONFIG=%s kubectl %s", s.cfg.Kubeconfig(), args)
}

// afterFailure runs after the stage name failed: it writes a diagnostics
// bundle (unless an env phase already did, or the cluster does not exist)
// on a context detached from ctx, prints where it went, and says how to
// reach and delete the kept cluster. It reports through t.
func (s *suite) afterFailure(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), collectTimeout)
	defer cancel()
	artifacts := filepath.Join(s.cfg.WorkDir(), "artifacts")
	switch {
	case s.envCollected:
		t.Logf("foundation: stage %s failed; the environment already wrote a diagnostics bundle under %s (see the \"testenv: diagnostics written to\" line above)", name, artifacts)
	case !s.clusterExists():
		t.Logf("foundation: stage %s failed before cluster %s existed; nothing to collect", name, s.cfg.Name)
		return
	default:
		dir, err := s.env.Collect(ctx)
		if err != nil {
			t.Logf("foundation: stage %s failed; collecting diagnostics also failed: %v", name, err)
		} else {
			t.Logf("foundation: stage %s failed; diagnostics written to %s", name, dir)
		}
	}
	t.Logf("foundation: cluster %s is kept for inspection: export KUBECONFIG=%s; delete it with make e2e-down", s.cfg.Name, s.cfg.Kubeconfig())
}

// clusterExists reports whether the e2e cluster exists, false when that
// cannot be told (no engine yet, or kind fails).
func (s *suite) clusterExists() bool {
	if s.kind == nil {
		return false
	}
	ok, err := s.kind.Exists(s.cfg.Name)
	return err == nil && ok
}

// afterSuccess runs after every stage passed: it prints the stage
// timings, then deletes the cluster when teardown is set, or says it is
// kept and green-lit. It reports through t and fails it when the
// teardown, which runs under ctx, fails.
func (s *suite) afterSuccess(ctx context.Context, t *testing.T) {
	t.Helper()
	var b strings.Builder
	var total time.Duration
	for _, st := range s.stages {
		fmt.Fprintf(&b, "\n  %-17s %s", st.Name, st.Duration.Round(time.Second))
		total += st.Duration
	}
	t.Logf("foundation: all stages passed in %s:%s", total.Round(time.Second), b.String())
	if !s.opts.teardown {
		t.Logf("foundation: cluster %s is green-lit and kept (%s); later tests call greenlight.Require; make e2e-down deletes it",
			s.cfg.Name, greenlight.Path(s.cfg.WorkDir()))
		return
	}
	ctx, cancel := context.WithTimeout(ctx, teardownTimeout)
	defer cancel()
	t.Logf("foundation: %s=1: deleting cluster %s", teardownEnv, s.cfg.Name)
	if err := s.env.Down(ctx); err != nil {
		t.Fatalf("foundation: teardown of %s failed: %v; delete it with make e2e-down", s.cfg.Name, err)
	}
}
