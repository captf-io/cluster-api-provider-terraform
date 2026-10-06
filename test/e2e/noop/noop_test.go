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

package noop

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/env"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/greenlight"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/kindcluster"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// The suite's time budget. suiteTimeout plus cleanupTimeout and
// collectTimeout stay under the make target's 45m `go test -timeout`, so
// a slow run fails with diagnostics and a cleanup instead of a test-binary
// panic.
const (
	// suiteTimeout bounds the seven stages.
	suiteTimeout = 25 * time.Minute
	// collectTimeout bounds the diagnostics run after a failure.
	collectTimeout = 4 * time.Minute
	// cleanupTimeout bounds the best-effort cleanup: one Job deadline and
	// the destroys after it.
	cleanupTimeout = 15 * time.Minute
)

// jobDeadline is spec.jobs.activeDeadlineSeconds on every Terraform*
// object: a broken pull or a hung runner fails in minutes, not the
// default hour.
const jobDeadline = 600

// The fixed object names. They are short because a Job is named
// captf-<kind>-<name>-<op>-a<N>-<hash6> and is hashed past 57 characters;
// the namespace keeps runs apart.
const (
	// clusterName names the main Cluster and its TerraformCluster.
	clusterName = "c1"
	// failName names the second Cluster and TerraformCluster, whose first
	// apply fails on an undeclared variable.
	failName = "c2"
	// machineA names Machine A and its TerraformMachine (Terraform).
	machineA = "ma"
	// machineB names Machine B and its TerraformMachine (OpenTofu).
	machineB = "mb"
	// poolName names the MachinePool and its TerraformMachinePool.
	poolName = "mp"
	// bootstrapName names the bootstrap data Secret every machine uses.
	bootstrapName = "boot"
	// bootstrapValue is the bootstrap Secret's value; CAPTF passes its
	// base64 to the modules as bootstrap_data.
	bootstrapValue = "#cloud-config\n# captf noop e2e\n"
	// failureDomain is the failure domain the noop cluster module reports
	// and machine A asks for.
	failureDomain = "fd1"
	// unknownVariable is the variable the failing cluster sets; the noop
	// module does not declare it.
	unknownVariable = "e2e_unknown"
)

// TestMain refuses to run unless -run selects the suite: TestNoop creates
// objects on the shared e2e cluster and must be asked for explicitly. It
// returns through os.Exit with m.Run's code.
func TestMain(m *testing.M) {
	flag.Parse()
	if f := flag.Lookup("test.run"); f == nil || f.Value.String() == "" {
		fmt.Fprintln(os.Stderr, "noop: select the suite with -run '^TestNoop$'; see make e2e-noop")
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// image is one noop module image as the suite references it.
type image struct {
	// framework.NoopImage is the pinned image.
	framework.NoopImage
	// ref is spec.source.image: <repo>:<NoopVersion>-<runtime>@<digest>.
	ref string
}

// suite is the state the stages share, filled in as they pass.
type suite struct {
	// opts is the suite's configuration.
	opts options
	// cfg is the environment configuration derived from opts.
	cfg env.Config
	// env collects diagnostics after a failure.
	env *env.Env
	// kind tells whether the cluster exists.
	kind *kindcluster.Manager
	// c are the cluster's clients.
	c wait.Clients
	// start is when the suite started; the manager log is scanned from
	// here.
	start time.Time
	// ns is the run's namespace, e2e-noop-<random>.
	ns string
	// identity names the run's TerraformClusterIdentity and its Secret in
	// the manager namespace.
	identity string
	// clusterImg, machineAImg, machineBImg and poolImg are the module
	// images: Terraform for the cluster and machine A, OpenTofu for
	// machine B and the pool.
	clusterImg, machineAImg, machineBImg, poolImg image
	// manager is the manager pod at setup (stage 1).
	manager managerPod
	// backendID is the cluster state's exports.backend_id (stage 2).
	backendID string
	// failFixed is when the failing cluster's variable was removed; its
	// apply Jobs created before then may fail (stage 5).
	failFixed time.Time
	// tracker records every CAPTF Job pod of the namespace (stage 1 on).
	tracker *podTracker
	// stages are the stages run so far, with their durations.
	stages []greenlight.Stage
}

// stage is one ordered step of TestNoop.
type stage struct {
	// name is the stage name.
	name string
	// run runs the stage under ctx, failing t on any problem.
	run func(ctx context.Context, t *testing.T)
}

// TestNoop drives the noop modules through CAPI and CAPTF on the
// green-lit e2e cluster, stage by stage. A failed stage stops the suite
// and collects diagnostics; t.Cleanup then removes what the run left.
func TestNoop(t *testing.T) {
	s := newSuite(t)
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()
	t.Cleanup(func() { s.cleanup(t) })
	t.Logf("noop: cluster %s, namespace %s, identity %s; images: cluster %s, machine A %s, machine B %s, pool %s",
		s.cfg.Name, s.ns, s.identity, s.clusterImg.ref, s.machineAImg.ref, s.machineBImg.ref, s.poolImg.ref)
	if s.opts.badDigest {
		t.Logf("noop: %s=1: machine B points at the nonexistent digest %s; stage 3 must fail on its image pull", badDigestEnv, badDigest)
	}

	stages := []stage{
		{"setup", s.setup},
		{"cluster", s.cluster},
		{"machines", s.machines},
		{"pool", s.pool},
		{"drift", s.drift},
		{"teardown", s.teardown},
		{"health", s.health},
	}
	for i, st := range stages {
		start := time.Now()
		ok := t.Run(fmt.Sprintf("%d-%s", i+1, st.name), func(t *testing.T) { st.run(ctx, t) })
		s.stages = append(s.stages, greenlight.Stage{Name: st.name, Duration: time.Since(start), Passed: ok})
		if !ok {
			s.afterFailure(ctx, t, st.name)
			t.FailNow()
		}
	}
	var b strings.Builder
	var total time.Duration
	for _, st := range s.stages {
		fmt.Fprintf(&b, "\n  %-9s %s", st.Name, st.Duration.Round(time.Second))
		total += st.Duration
	}
	t.Logf("noop: all stages passed in %s:%s", total.Round(time.Second), b.String())
}

// newSuite returns the suite for the configuration in the environment. It
// fails t when the configuration is invalid, when the cluster's clients
// cannot be built, or, through greenlight.Require, when the cluster is
// not green-lit.
func newSuite(t *testing.T) *suite {
	t.Helper()
	o, err := optionsFrom(os.Getenv)
	if err != nil {
		t.Fatalf("noop: configuration: %v", err)
	}
	cfg, err := envConfig(o)
	if err != nil {
		t.Fatalf("noop: configuration: %v", err)
	}
	e, err := env.New(cfg, os.Stderr)
	if err != nil {
		t.Fatalf("noop: configuration: %v", err)
	}
	s := &suite{opts: o, cfg: cfg, env: e, start: time.Now()}
	s.requireGreenLight(t)
	if s.c, err = wait.ClientsFromKubeconfig(cfg.Kubeconfig()); err != nil {
		t.Fatalf("noop: the green-lit cluster's kubeconfig %s does not load: %v; run make e2e-foundation", cfg.Kubeconfig(), err)
	}
	suffix := randomSuffix()
	s.ns = "e2e-noop-" + suffix
	s.identity = "e2e-noop-" + suffix
	s.clusterImg = noopImage(t, framework.RoleCluster, framework.RuntimeTerraform, false)
	s.machineAImg = noopImage(t, framework.RoleMachine, framework.RuntimeTerraform, false)
	s.machineBImg = noopImage(t, framework.RoleMachine, framework.RuntimeOpenTofu, o.badDigest)
	s.poolImg = noopImage(t, framework.RoleMachinePool, framework.RuntimeOpenTofu, false)
	return s
}

// requireGreenLight fails t unless the cluster is green-lit: the
// foundation suite's record exists, is fresh, matches this build's pins
// and the manager in state.json, and its cluster exists.
func (s *suite) requireGreenLight(t *testing.T) {
	t.Helper()
	eng, err := engine.Detect(context.Background(), s.cfg.EngineOverride, engine.ExecRunner())
	if err != nil {
		t.Fatalf("noop: expected a usable container engine to check the green light: %v", err)
	}
	if s.kind, err = kindcluster.New(eng, kindcluster.WithLogWriter(os.Stderr)); err != nil {
		t.Fatalf("noop: kind: %v", err)
	}
	check, err := greenlight.CheckFromState(s.cfg.WorkDir(), s.kind.Exists)
	if err != nil {
		t.Fatalf("noop: cluster %s is not green-lit: %v; run make e2e-foundation", s.cfg.Name, err)
	}
	greenlight.Require(t, greenlight.Path(s.cfg.WorkDir()), check)
}

// noopImage returns the pinned noop image for role and runtime, referenced
// as <repo>:<NoopVersion>-<runtime>@<digest>; with bad the digest is badDigest
// (the negative check), and so is the expected pin. It fails t when no
// image is pinned for the pair.
func noopImage(t *testing.T, role framework.NoopRole, runtime framework.NoopRuntime, bad bool) image {
	t.Helper()
	img, ok := framework.NoopImageFor(role, runtime)
	if !ok {
		t.Fatalf("noop: no noop image pinned for role %s, runtime %s (framework.NoopImages)", role, runtime)
	}
	if bad {
		img.Digest = badDigest
	}
	return image{NoopImage: img, ref: img.Ref + "@" + img.Digest}
}

// kubectl returns a kubectl command line against the e2e cluster with
// args, for "inspect" hints in failure messages.
func (s *suite) kubectl(args string) string {
	return fmt.Sprintf("KUBECONFIG=%s kubectl %s", s.cfg.Kubeconfig(), args)
}

// kubectlNS returns a kubectl command line in the run's namespace with
// args, for "inspect" hints.
func (s *suite) kubectlNS(args string) string {
	return s.kubectl("-n " + s.ns + " " + args)
}

// afterFailure runs after the stage name failed: it writes the
// environment's diagnostics bundle plus the run namespace's pods, logs
// and events, on a context detached from ctx, and prints where they went.
// It reports through t.
func (s *suite) afterFailure(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), collectTimeout)
	defer cancel()
	dir, err := s.env.Collect(ctx)
	if err != nil {
		t.Logf("noop: stage %s failed; collecting diagnostics also failed: %v", name, err)
		return
	}
	if err := s.collectNamespace(ctx, dir); err != nil {
		t.Logf("noop: collecting namespace %s into %s: %v", s.ns, dir, err)
	}
	t.Logf("noop: stage %s failed; diagnostics (with namespace %s under %s) written to %s", name, s.ns, nsArtifactsDir, dir)
}
