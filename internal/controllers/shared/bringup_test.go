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

package shared

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runlease"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// testDigest is the image digest every simulated Job's pod reports.
var testDigest = "registry.example/mod@sha256:" + strings.Repeat("c", 64)

// ownedRunner is the fakeRunner with List scoped to one owner, as the real
// runner's label selector is, so several objects share one env.
type ownedRunner struct{ *fakeRunner }

// List returns, using ctx, the fakeRunner's Jobs of kind that belong to
// owner, dropping the ones whose owner-kind/owner-name labels name a
// different object.
func (r ownedRunner) List(ctx context.Context, owner client.Object, kind string) ([]batchv1.Job, error) {
	all, err := r.fakeRunner.List(ctx, owner, kind)
	return slices.DeleteFunc(all, func(j batchv1.Job) bool {
		return j.Labels[state.OwnerKindLabel] != kind || j.Labels[state.OwnerNameLabel] != state.LabelValue(owner.GetName())
	}), err
}

// baseSecret returns, failing t on error, the kubernetes backend's base
// state Secret of kind/name, which state.Adopt annotates.
func baseSecret(t *testing.T, kind, name string) *corev1.Secret {
	t.Helper()
	suffix, err := state.Suffix(testNS, kind, name)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: state.SecretName(suffix), Labels: map[string]string{
		state.BackendStateLabel: "true", state.BackendSuffixLabel: suffix, state.BackendWorkspaceLabel: state.Workspace,
	}}}
}

// resultPod returns a finished runner pod reporting r and testDigest.
func resultPod(r runner.Result) corev1.Pod {
	p := podWith(string(runner.Encode(r)), "")
	p.Spec.Containers = []corev1.Container{{Name: jobs.SourceContainer, Image: "registry.example/mod:1.0"}}
	p.Status.ContainerStatuses[0].ImageID = "docker-pullable://" + testDigest
	return *p
}

// steps returns a runner result's step list, one runner.Step per name in
// names, each recorded as taking one second.
func steps(names ...string) []runner.Step {
	out := make([]runner.Step, 0, len(names))
	for _, n := range names {
		out = append(out, runner.Step{Name: n, Seconds: 1})
	}
	return out
}

// noChangeApply returns the result of a guarded apply whose plan had no
// changes: it ends after the plan, with zero changes.
func noChangeApply() runner.Result {
	return runner.Result{Version: runner.ResultVersion, Op: runner.OpApply, Steps: steps("init", "validate", "plan"), Changes: &runner.Changes{}}
}

// TestNoChangeClusterApply: a guarded cluster apply whose plan had no
// changes ends after the plan; the controller treats it as any successful
// apply: the inputs hash is adopted, the digest pinned, the run lease and the
// cluster write lease released as soon as it is bookkept, and nothing else
// starts. Its outcome says it changed nothing.
func TestNoChangeClusterApply(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused), baseSecret(t, state.KindTerraformCluster, testName))...)
	e.d.ClusterOperationGate = true
	e.state.st = &state.State{Serial: 3, InputsHash: "h1:old"}
	healthy := &contract.Health{State: contract.HealthRunning, Healthy: true}
	kind := func() *fakeKind {
		return &fakeKind{obj: e.get(t), owner: readyOwner, in: machineIn(), mutable: true, asCluster: true, health: healthy}
	}
	if _, err := reconcileOnce(t, e, kind()); err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 1 {
		t.Fatalf("created %v, want the apply", e.runner.created)
	}
	name := e.runner.created[0]
	j := e.jobNamed(t, name)
	if jobs.OpOf(j) != jobs.OpApply || !slices.Contains(sourceArgs(j), "--guard-deletes") {
		t.Fatalf("Job %s: op %s, args %v; want a guarded apply", name, jobs.OpOf(j), sourceArgs(j))
	}
	hash := j.Annotations[state.InputsHashAnnotation]
	runLease := runLeaseOf(t, state.KindTerraformCluster, testName)
	clusterLease := runlease.ClusterName(testNS, "c1")
	if runlease.HolderOf(e.lease(t, runLease)) != name || runlease.HolderOf(e.lease(t, clusterLease)) != name {
		t.Fatalf("the apply does not hold its leases")
	}

	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(t0.Add(time.Minute))}}
	e.runner.pods[name] = []corev1.Pod{resultPod(noChangeApply())}
	e.d.Clock = testingclock.NewFakePassiveClock(t0.Add(time.Minute + time.Second))
	requeue, err := reconcileOnce(t, e, kind())
	if err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 1 || requeue < 20*time.Minute {
		t.Errorf("after the apply: created %v, requeue %s; want nothing more until drift", e.runner.created, requeue)
	}
	if e.lease(t, runLease) != nil || e.lease(t, clusterLease) != nil {
		t.Error("the leases outlived the bookkept no-change apply")
	}
	stored := &corev1.Secret{}
	if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(baseSecret(t, state.KindTerraformCluster, testName)), stored); err != nil {
		t.Fatal(err)
	}
	if stored.Annotations[state.InputsHashAnnotation] != hash {
		t.Errorf("state inputs hash = %q, want the apply's %s", stored.Annotations[state.InputsHashAnnotation], hash)
	}
	// The durable Secret is named for the stored object's kind, which the
	// fake cluster adapter leaves a TerraformMachine.
	durable, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil || durable.Meta.ImageDigest != testDigest {
		t.Errorf("durable = %+v, %v; want the digest pinned", durable, err)
	}
	m := e.get(t)
	if c := conditions.Get(m, infrav1.ApplyJobSucceededCondition); c == nil || c.Status != metav1.ConditionTrue || c.Reason != infrav1.ApplySucceededReason {
		t.Errorf("ApplyJobSucceeded = %+v", c)
	}
	var names []string
	for _, s := range m.Status.LastRun.Steps {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"init", "validate", "plan"}) || m.Status.ObservedStateSerial != 3 {
		t.Errorf("lastRun steps %v, observed serial %d", names, m.Status.ObservedStateSerial)
	}
	if ev := e.rec.only(EventJobSucceeded); len(ev) != 1 || !strings.Contains(ev[0].note, "resources: 0 added, 0 changed, 0 destroyed") {
		t.Errorf("JobSucceeded events = %+v", ev)
	}
}

// bringup drives the objects of one cluster through a fake bring-up: every
// Job a reconcile starts runs to success before the next reconcile, a
// minute each.
type bringup struct {
	t   *testing.T
	e   *env
	now time.Time
	// in and health are what each object's adapter builds and reads.
	in     map[string]any
	health map[string]*contract.Health
	// applyReading is what a machine apply's own outputs read; a refresh
	// reads healthy.
	applyReading *contract.Health
	// noChange makes the next apply's plan have no changes.
	noChange bool
}

// bringupCluster is the bring-up scenario's TerraformCluster name.
const bringupCluster = "tc"

// healthyReading is a definite, healthy contract.Health reading.
var healthyReading = &contract.Health{State: contract.HealthRunning, Healthy: true}

// objectNamed returns a machine-mutator that sets m's Name and a
// deterministic UID derived from name.
func objectNamed(name string) func(*infrav1.TerraformMachine) {
	return func(m *infrav1.TerraformMachine) { m.Name, m.UID = name, types.UID(name+"-uid") }
}

// bringupMachines are the three control-plane and three worker machines.
var bringupMachines = []string{"cp-0", "cp-1", "cp-2", "w-0", "w-1", "w-2"}

// newBringup returns, failing t on error, a bringup env holding the cluster
// and the six bringupMachines, none provisioned yet, whose machine applies
// will read applyReading.
func newBringup(t *testing.T, applyReading *contract.Health) *bringup {
	t.Helper()
	objs := []client.Object{
		machine(withFinalizer, notPaused, objectNamed(bringupCluster)),
		baseSecret(t, state.KindTerraformCluster, bringupCluster),
	}
	for _, n := range bringupMachines {
		objs = append(objs, machine(withFinalizer, notPaused, objectNamed(n)), baseSecret(t, state.KindTerraformMachine, n))
	}
	e := newEnv(t, world(objs...)...)
	e.d.Jobs = ownedRunner{e.runner}
	e.d.ClusterOperationGate = true
	e.state.bySuffix = map[string]*state.State{}
	b := &bringup{t: t, e: e, now: t0, in: map[string]any{}, health: map[string]*contract.Health{}, applyReading: applyReading}
	for _, n := range append([]string{bringupCluster}, bringupMachines...) {
		b.in[n] = machineIn()
		b.e.state.bySuffix[b.suffix(n)] = nil
	}
	return b
}

// kindOf returns the state kind of the object named name: the cluster kind
// for bringupCluster, the machine kind otherwise.
func (b *bringup) kindOf(name string) string {
	if name == bringupCluster {
		return state.KindTerraformCluster
	}
	return state.KindTerraformMachine
}

// suffix returns the backend suffix of the object named name, failing b.t
// on error.
func (b *bringup) suffix(name string) string {
	s, err := state.Suffix(testNS, b.kindOf(name), name)
	if err != nil {
		b.t.Fatal(err)
	}
	return s
}

// kind returns, failing b.t on error, the fakeKind adapter for the object
// named name, loaded fresh from the fake client and set up as a cluster
// (mutable) or as a refreshing machine.
func (b *bringup) kind(name string) *fakeKind {
	b.t.Helper()
	m := &infrav1.TerraformMachine{}
	if err := b.e.c.Get(b.t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, m); err != nil {
		b.t.Fatal(err)
	}
	k := &fakeKind{obj: m, owner: readyOwner, in: b.in[name], health: b.health[name]}
	if name == bringupCluster {
		k.asCluster, k.mutable = true, true
	} else {
		k.refresh = true
	}
	return k
}

// settle reconciles name until a reconcile starts no Job, running each Job
// it starts; it returns the ops started and the last requeue.
func (b *bringup) settle(name string) ([]jobs.Op, time.Duration) {
	b.t.Helper()
	var ops []jobs.Op
	for range 20 {
		before := len(b.e.runner.created)
		b.e.d.Clock = testingclock.NewFakePassiveClock(b.now)
		res, err := Reconcile(b.t.Context(), b.e.d, b.kind(name))
		if err != nil {
			b.t.Fatalf("Reconcile %s: %v", name, err)
		}
		if len(b.e.runner.created) == before {
			return ops, res.RequeueAfter
		}
		ops = append(ops, b.run(name, b.e.runner.created[len(b.e.runner.created)-1]))
	}
	b.t.Fatalf("%s did not settle: started %v", name, ops)
	return nil, 0
}

// run finishes Job jobName of object name a minute later with the result
// and state it would leave, and returns its op.
func (b *bringup) run(name, jobName string) jobs.Op {
	b.t.Helper()
	b.now = b.now.Add(time.Minute)
	j := b.e.jobNamed(b.t, jobName)
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(b.now)}}
	op := jobs.OpOf(j)
	r := runner.Result{Version: runner.ResultVersion, Op: string(op)}
	suffix := b.suffix(name)
	switch op {
	case jobs.OpApply:
		st := b.e.state.bySuffix[suffix]
		if st == nil {
			st = &state.State{}
			b.e.state.bySuffix[suffix] = st
		}
		if b.noChange {
			r = noChangeApply()
			b.noChange = false
		} else {
			r.Steps, r.Changes = steps("init", "validate", "apply"), &runner.Changes{Add: 1}
			st.Serial++
		}
		b.health[name] = healthyReading
		if name != bringupCluster {
			b.health[name] = b.applyReading
		}
	case jobs.OpRefresh:
		r.Steps = steps("init", "apply-refresh-only")
		b.health[name] = healthyReading
	default:
		b.t.Fatalf("unexpected %s Job %s during bring-up", op, jobName)
	}
	b.e.runner.pods[jobName] = []corev1.Pod{resultPod(r)}
	b.now = b.now.Add(time.Second)
	return op
}

// bringUp runs the scenario and returns the Jobs it created: one
// TerraformCluster apply, three control-plane and three worker machines
// provisioned one after another, then the cluster's re-apply when the
// control plane is initialized (new inputs, an empty plan).
func (b *bringup) bringUp() int {
	b.t.Helper()
	for _, n := range append([]string{bringupCluster}, bringupMachines...) {
		ops, requeue := b.settle(n)
		m := b.kind(n).obj
		if p := m.Status.Initialization.Provisioned; p == nil || !*p || len(ops) == 0 || ops[0] != jobs.OpApply {
			b.t.Fatalf("%s: ops %v, provisioned %v", n, ops, p)
		}
		// Steady state: nothing more until the first drift check.
		if requeue < 20*time.Minute {
			b.t.Errorf("%s settled with requeue %s, want the drift deadline", n, requeue)
		}
	}
	reinit := machineIn()
	reinit.MachineName = "control-plane-initialized"
	b.in[bringupCluster] = reinit
	b.noChange = true
	if ops, _ := b.settle(bringupCluster); !slices.Equal(ops, []jobs.Op{jobs.OpApply}) {
		b.t.Errorf("control_plane_initialized re-apply: ops %v, want one apply", ops)
	}
	if b.e.lease(b.t, runlease.ClusterName(testNS, "c1")) != nil {
		b.t.Error("the no-change re-apply kept the cluster write lease")
	}
	return len(b.e.runner.created)
}

// TestBringUpJobCount counts the Jobs of a bring-up.
//
// Before (the old rules): the cluster applies once (it never refreshes
// after an apply, and its outputs read healthy); each of the six machines
// applies and then always refreshes (RefreshAfterApply), 2 × 6 = 12; the
// control_plane_initialized re-apply is one more Job, which also ran the
// apply step on its empty saved plan: 1 + 12 + 1 = 14.
//
// After: a machine whose apply outputs read a definite health state skips
// the refresh, 1 × 6 = 6; the re-apply is still a Job, now ending after its
// plan: 1 + 6 + 1 = 8. When a machine's apply reads pending, the refresh
// stays, which reproduces the old count.
func TestBringUpJobCount(t *testing.T) {
	t.Parallel()
	const before, after = 14, 8
	t.Run("healthy apply outputs", func(t *testing.T) {
		t.Parallel()
		if n := newBringup(t, healthyReading).bringUp(); n != after {
			t.Errorf("bring-up created %d Jobs, want %d (was %d)", n, after, before)
		}
	})
	t.Run("pending apply outputs keep the refresh", func(t *testing.T) {
		t.Parallel()
		if n := newBringup(t, &contract.Health{State: contract.HealthPending}).bringUp(); n != before {
			t.Errorf("bring-up created %d Jobs, want %d", n, before)
		}
	})
}
