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

package terraformmachinepool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	testingclock "k8s.io/utils/clock/testing"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runlease"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Cluster exports the hold tests switch between.
const (
	exportsE0 = `{"net":"n-1"}`
	exportsE1 = `{"net":"n-2"}`
	exportsE2 = `{"net":"n-3"}`
)

// blockedTail is the runner's summary of the blocked plan.
const blockedTail = "the plan deletes or replaces 1 resource(s): module.role.aws_autoscaling_group.this (replace)"

// clientRunner is a jobs.Runner that keeps the Jobs in the fake client, so
// the marks bookkeeping patches onto them stick as in a real cluster.
type clientRunner struct {
	c    client.Client
	mu   sync.Mutex
	pods map[string][]corev1.Pod
	// names are the names of the Jobs created, in order.
	names []string
}

var _ jobs.Runner = &clientRunner{}

// created returns the names of the Jobs created so far, in order.
func (r *clientRunner) created() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.names)
}

// Create stores job in the client with a fake UID, using ctx, and returns
// any create error.
func (r *clientRunner) Create(ctx context.Context, _ client.Object, job *batchv1.Job) error {
	r.mu.Lock()
	r.names = append(r.names, job.Name)
	job.UID = types.UID(fmt.Sprintf("job-uid-%03d", len(r.names)))
	// History pruning orders Jobs by creation, which the API server sets.
	job.CreationTimestamp = metav1.NewTime(t0.Add(-2*time.Hour + time.Duration(len(r.names))*time.Second))
	r.mu.Unlock()
	return r.c.Create(ctx, job)
}

// List returns every Job in ns, read through the client using ctx, or the
// list error.
func (r *clientRunner) List(ctx context.Context, _ client.Object, _ string) ([]batchv1.Job, error) {
	var l batchv1.JobList
	if err := r.c.List(ctx, &l, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// Delete removes job from the client using ctx, ignoring not-found, and
// returns any other error.
func (r *clientRunner) Delete(ctx context.Context, job *batchv1.Job) error {
	return client.IgnoreNotFound(r.c.Delete(ctx, job))
}

// Pods returns the pods recorded for job and a nil error.
func (r *clientRunner) Pods(_ context.Context, job *batchv1.Job) ([]corev1.Pod, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.pods[job.Name]), nil
}

// holdEnv drives one TerraformMachinePool through the shared reconcile
// with a fake client, state reader and runner.
type holdEnv struct {
	t      *testing.T
	c      client.Client
	sr     *stateReader
	runner *clientRunner
	rec    *recorder
	clock  *testingclock.FakePassiveClock
	r      *Reconciler
	req    ctrl.Request
	// finished counts finished Jobs, which finish a second apart, an hour
	// before the clock's now.
	finished int
}

// newHoldEnv returns a pool whose cluster exports exportsE0, after its
// first apply: started unguarded, succeeded, and bookkept, so exportsE0
// are its applied exports. t fails the test on any error.
func newHoldEnv(t *testing.T) *holdEnv {
	t.Helper()
	e := newPoolEnv(t)
	first := e.reconcileStarts(jobs.OpApply)
	if e.guarded(first) || first.Annotations[shared.ApprovalHashAnnotation] != "" {
		t.Fatalf("the first apply is guarded: %v", first.Annotations)
	}
	e.succeed(first)
	e.reconcileIdle()
	if d := e.durable(); d.Pending != nil || !sameExports(t, d.AppliedClusterOutputs, exportsE0) {
		t.Fatalf("after the first apply: applied %s, pending %+v", d.AppliedClusterOutputs, d.Pending)
	}
	return e
}

// newPoolEnv returns a pool whose cluster exports exportsE0, before its
// first apply. t fails the test on any error.
func newPoolEnv(t *testing.T) *holdEnv {
	t.Helper()
	tmp := testTMP(func(p *infrav1.TerraformMachinePool) {
		p.Finalizers = []string{Finalizer}
		p.Status.Conditions = []metav1.Condition{{
			Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason, LastTransitionTime: metav1.NewTime(t0),
		}}
		now := metav1.NewTime(t0)
		p.Status.LastRefresh, p.Status.LastDriftCheck = &now, &now
	})
	s := scheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(world(tmp)...).WithStatusSubresource(&infrav1.TerraformMachinePool{}, &infrav1.TerraformPlan{}).
		WithIndex(&infrav1.TerraformPlan{}, shared.PlanTargetIndex, shared.PlanTargetIndexer).Build()
	e := &holdEnv{
		t: t, c: c, sr: &stateReader{}, runner: &clientRunner{c: c, pods: map[string][]corev1.Pod{}}, rec: &recorder{},
		clock: testingclock.NewFakePassiveClock(t0), req: ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tmp)},
	}
	e.setExports(exportsE0)
	e.r = &Reconciler{Deps: shared.Deps{
		Client: c, APIReader: c, Scheme: s, Jobs: e.runner, State: e.sr, Recorder: e.rec,
		Clock: e.clock, RunnerImage: "registry.example/captf:dev", DriftDefault: 30 * time.Minute,
	}}
	return e
}

// reconcile runs one reconcile of the pool, failing the test on error.
func (e *holdEnv) reconcile() {
	e.t.Helper()
	if _, err := e.r.Reconcile(e.t.Context(), e.req); err != nil {
		e.t.Fatal(err)
	}
}

// reconcileStarts reconciles once and returns the one Job of op it
// started, failing the test unless exactly that Job started.
func (e *holdEnv) reconcileStarts(op jobs.Op) *batchv1.Job {
	e.t.Helper()
	before := e.runner.created()
	e.reconcile()
	started := e.runner.created()[len(before):]
	if len(started) != 1 {
		e.t.Fatalf("started %v, want one %s Job", started, op)
	}
	j := &batchv1.Job{}
	if err := e.c.Get(e.t.Context(), client.ObjectKey{Namespace: ns, Name: started[0]}, j); err != nil {
		e.t.Fatal(err)
	}
	if jobs.OpOf(j) != op {
		e.t.Fatalf("started a %s Job, want %s", jobs.OpOf(j), op)
	}
	return j
}

// reconcileIdle reconciles once, failing the test if a Job started.
func (e *holdEnv) reconcileIdle() {
	e.t.Helper()
	before := e.runner.created()
	e.reconcile()
	if started := e.runner.created()[len(before):]; len(started) != 0 {
		e.t.Fatalf("started %v, want no Job", started)
	}
}

// reconcileNoApply reconciles once, failing the test if an apply Job
// started; a refresh or drift check may, and is finished successfully,
// so it does not hold the next pass.
func (e *holdEnv) reconcileNoApply() {
	e.t.Helper()
	before := e.runner.created()
	e.reconcile()
	for _, name := range e.runner.created()[len(before):] {
		if strings.Contains(name, "-apply-") {
			e.t.Fatalf("started apply Job %s, want none", name)
		}
		e.finish(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}, batchv1.JobComplete)
	}
}

// finish marks job finished with the given terminal condition type.
func (e *holdEnv) finish(job *batchv1.Job, condition batchv1.JobConditionType) {
	e.t.Helper()
	e.finished++
	stored := &batchv1.Job{}
	if err := e.c.Get(e.t.Context(), client.ObjectKeyFromObject(job), stored); err != nil {
		e.t.Fatal(err)
	}
	at := metav1.NewTime(t0.Add(-time.Hour + time.Duration(e.finished)*time.Second))
	stored.Status.Conditions = []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue, LastTransitionTime: at}}
	if err := e.c.Status().Update(e.t.Context(), stored); err != nil {
		e.t.Fatal(err)
	}
}

// succeed finishes job successfully; an apply's state then records the
// inputs hash it rendered, as the runner's backend leaves it.
func (e *holdEnv) succeed(job *batchv1.Job) {
	e.t.Helper()
	e.finish(job, batchv1.JobComplete)
	if jobs.OpOf(job) == jobs.OpApply {
		st := poolState(poolOutputs(`["aws:///us-east-1a/i-2"]`, `1`))
		st.InputsHash = job.Annotations[state.InputsHashAnnotation]
		e.sr.set(e.t, state.KindTerraformMachinePool, job.Labels[state.OwnerNameLabel], st)
	}
}

// block finishes job, an apply, as the runner's guard stops it before a
// destructive plan, which it reports.
func (e *holdEnv) block(job *batchv1.Job) {
	e.t.Helper()
	e.result(job, runner.Result{
		Version: runner.ResultVersion, Op: runner.OpApply, Steps: []runner.Step{{Name: "init"}, {Name: "plan", Exit: 2}, {Name: "show-json"}},
		Error: &runner.Error{Kind: runner.ErrorKindBlocked, Tail: blockedTail},
		Plan:  &runner.Plan{Hash: "p2:" + job.Name, Replace: 1, Resources: []string{"module.role.aws_autoscaling_group.this (replace)"}},
	})
	e.finish(job, batchv1.JobFailed)
}

// fail finishes job, an apply, as failing in its apply step.
func (e *holdEnv) fail(job *batchv1.Job) {
	e.t.Helper()
	e.failAt(job, runner.StepApply)
}

// failAt finishes job, an apply, as failing in step, after the steps
// before it in a guarded apply succeeded.
func (e *holdEnv) failAt(job *batchv1.Job, step string) {
	e.t.Helper()
	var steps []runner.Step
	for _, s := range []string{runner.StepInit, runner.StepPlan, runner.StepShowJSON, runner.StepApply} {
		if s == step {
			steps = append(steps, runner.Step{Name: s, Exit: 1})
			break
		}
		steps = append(steps, runner.Step{Name: s})
	}
	e.result(job, runner.Result{
		Version: runner.ResultVersion, Op: runner.OpApply, Steps: steps,
		Error: &runner.Error{Kind: runner.ErrorKindStep, Step: &step, Tail: "Error: quota exceeded"},
	})
	e.finish(job, batchv1.JobFailed)
}

// result records res as job's runner result, on its pod's source
// container.
func (e *holdEnv) result(job *batchv1.Job, res runner.Result) {
	st := corev1.ContainerStatus{Name: jobs.SourceContainer}
	st.State.Terminated = &corev1.ContainerStateTerminated{Message: string(runner.Encode(res))}
	p := corev1.Pod{}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{st}
	e.runner.mu.Lock()
	defer e.runner.mu.Unlock()
	e.runner.pods[job.Name] = []corev1.Pod{p}
}

// setExports makes the TerraformCluster's state export exports.
func (e *holdEnv) setExports(exports string) {
	e.t.Helper()
	e.sr.set(e.t, state.KindTerraformCluster, "c1", clusterState(exports, `[{"name":"az-1","control_plane":true,"attributes":{}}]`))
}

// rotate writes new bootstrap data, as the bootstrap provider rotates it,
// and returns its rendered (base64) form.
func (e *holdEnv) rotate(data string) string {
	e.t.Helper()
	b := &corev1.Secret{}
	if err := e.c.Get(e.t.Context(), client.ObjectKey{Namespace: ns, Name: "bootstrap-mp"}, b); err != nil {
		e.t.Fatal(err)
	}
	b.Data["value"] = []byte(data)
	if err := e.c.Update(e.t.Context(), b); err != nil {
		e.t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString([]byte(data))
}

// setVersion sets the MachinePool's Kubernetes version to v.
func (e *holdEnv) setVersion(v string) {
	e.t.Helper()
	mp := &clusterv1.MachinePool{}
	if err := e.c.Get(e.t.Context(), client.ObjectKeyFromObject(testMP()), mp); err != nil {
		e.t.Fatal(err)
	}
	mp.Spec.Template.Spec.Version = v
	if err := e.c.Update(e.t.Context(), mp); err != nil {
		e.t.Fatal(err)
	}
}

// setReplicas sets the MachinePool's spec.replicas to n.
func (e *holdEnv) setReplicas(n int32) {
	e.t.Helper()
	mp := &clusterv1.MachinePool{}
	if err := e.c.Get(e.t.Context(), client.ObjectKeyFromObject(testMP()), mp); err != nil {
		e.t.Fatal(err)
	}
	mp.Spec.Replicas = &n
	if err := e.c.Update(e.t.Context(), mp); err != nil {
		e.t.Fatal(err)
	}
}

// pool re-reads the TerraformMachinePool and returns it.
func (e *holdEnv) pool() *infrav1.TerraformMachinePool {
	e.t.Helper()
	p := &infrav1.TerraformMachinePool{}
	if err := e.c.Get(e.t.Context(), e.req.NamespacedName, p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// plans returns the pool's TerraformPlans.
func (e *holdEnv) plans() []infrav1.TerraformPlan {
	e.t.Helper()
	list := &infrav1.TerraformPlanList{}
	if err := e.c.List(e.t.Context(), list, client.InNamespace(ns)); err != nil {
		e.t.Fatal(err)
	}
	return list.Items
}

// livePlan returns the pool's live TerraformPlan made for the approval
// hash approval, nil when there is none.
func (e *holdEnv) livePlan(approval string) *infrav1.TerraformPlan {
	e.t.Helper()
	for _, p := range e.plans() {
		if p.Spec.InputsHash == approval && !infrav1.PlanPhase(p.Labels[infrav1.PlanPhaseLabel]).Terminal() {
			return &p
		}
	}
	return nil
}

// plan returns the TerraformPlan name.
func (e *holdEnv) plan(name string) *infrav1.TerraformPlan {
	e.t.Helper()
	p := &infrav1.TerraformPlan{}
	if err := e.c.Get(e.t.Context(), client.ObjectKey{Namespace: ns, Name: name}, p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// approve approves, as alice, the pool's live TerraformPlan of the change
// whose approval hash is approval, failing the test when there is none.
func (e *holdEnv) approve(approval string) {
	e.t.Helper()
	p := e.livePlan(approval)
	if p == nil {
		e.t.Fatalf("no live TerraformPlan of approval hash %s: %+v", approval, e.plans())
	}
	p.Spec.Approved, p.Spec.ApprovedBy = new(true), "alice"
	if err := e.c.Update(e.t.Context(), p); err != nil {
		e.t.Fatal(err)
	}
}

// durable reads the pool's durable inputs and returns them.
func (e *holdEnv) durable() *inputs.Durable {
	e.t.Helper()
	d, err := inputs.Read(e.t.Context(), e.c, ns, "mp", e.req.Name)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

// rendered returns the tfvars job runs with, from its per-run Secret.
func (e *holdEnv) rendered(job *batchv1.Job) contract.MachinePoolInputs {
	e.t.Helper()
	s := &corev1.Secret{}
	if err := e.c.Get(e.t.Context(), client.ObjectKey{Namespace: ns, Name: inputs.RunName(job.Name)}, s); err != nil {
		e.t.Fatal(err)
	}
	var in contract.MachinePoolInputs
	if err := json.Unmarshal(s.Data[inputs.TFVarsKey], &in); err != nil {
		e.t.Fatal(err)
	}
	return in
}

// args returns job's runner arguments: the source container runs the
// runner.
func (e *holdEnv) args(job *batchv1.Job) []string {
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.Name == jobs.SourceContainer {
			return c.Args
		}
	}
	e.t.Fatalf("Job %s has no source container", job.Name)
	return nil
}

// guarded reports whether job's runner guards against a destructive plan.
func (e *holdEnv) guarded(job *batchv1.Job) bool {
	return slices.Contains(e.args(job), "--guard-deletes")
}

// flag returns the value of job's runner flag name ("" when absent).
func (e *holdEnv) flag(job *batchv1.Job, name string) string {
	for _, a := range e.args(job) {
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	return ""
}

// applyCondition returns the pool's ApplyJobSucceeded condition.
func (e *holdEnv) applyCondition() metav1.Condition {
	e.t.Helper()
	c := conditions.Get(e.pool(), infrav1.ApplyJobSucceededCondition)
	if c == nil {
		e.t.Fatal("no ApplyJobSucceeded condition")
	}
	return *c
}

// heldHash returns the approval hash the held ApplyJobSucceeded
// condition's command names, failing the test unless the condition is
// the held one for the blocked Job named blocked.
func (e *holdEnv) heldHash(blocked string) string {
	e.t.Helper()
	c := e.applyCondition()
	if c.Status != metav1.ConditionFalse || c.Reason != infrav1.DestructivePlanBlockedReason ||
		!strings.HasPrefix(c.Message, "Job "+blocked+": "+blockedTail) || !strings.Contains(c.Message, "keeps applying with the exports of its last successful apply") {
		e.t.Fatalf("ApplyJobSucceeded = %+v, want the held change of Job %s", c, blocked)
	}
	return e.approveHash(c)
}

// approveHash returns the approval hash of the TerraformPlan the approve
// command in c, a DestructivePlanBlocked ApplyJobSucceeded condition,
// names, failing the test when c names none.
func (e *holdEnv) approveHash(c metav1.Condition) string {
	e.t.Helper()
	_, rest, ok := strings.Cut(c.Message, "kubectl patch terraformplan ")
	name, tail, ok2 := strings.Cut(rest, " ")
	if !ok || !ok2 || !strings.HasPrefix(tail, "-n "+ns+" --type merge") {
		e.t.Fatalf("ApplyJobSucceeded names no approve command: %s", c.Message)
	}
	p := &infrav1.TerraformPlan{}
	if err := e.c.Get(e.t.Context(), client.ObjectKey{Namespace: ns, Name: name}, p); err != nil {
		e.t.Fatal(err)
	}
	return p.Spec.InputsHash
}

// exportsHash returns hash.Exports of raw, failing t on error.
func exportsHash(t *testing.T, raw string) string {
	t.Helper()
	h, err := hash.Exports(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// sameExports reports whether got and want are the same exports, failing
// t when either is not JSON.
func sameExports(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	return len(got) > 0 && exportsHash(t, string(got)) == exportsHash(t, want)
}

// blockChange changes the cluster's exports to exports, runs the guarded
// apply that renders them and blocks it, and returns that Job and the
// approval hash it carries. t fails the test on any deviation.
func (e *holdEnv) blockChange(exports string) (*batchv1.Job, string) {
	e.t.Helper()
	e.setExports(exports)
	j := e.reconcileStarts(jobs.OpApply)
	approval := j.Annotations[shared.ApprovalHashAnnotation]
	if !e.guarded(j) || approval == "" || e.flag(j, "--inputs-hash") != approval || e.flag(j, "--allow-deletes-hash") != "" ||
		j.Annotations[shared.ClusterOutputsHashAnnotation] != exportsHash(e.t, exports) || !sameExports(e.t, e.rendered(j).ClusterOutputs, exports) {
		e.t.Fatalf("the apply of new exports is not guarded by its approval hash: args %v, annotations %v", e.args(j), j.Annotations)
	}
	e.block(j)
	return j, approval
}

// TestHoldExportsBlockedChange: an exports change whose plan is
// destructive blocks, and its plan becomes an ExportsChange TerraformPlan
// of its approval hash; the pool then keeps applying bootstrap rotations,
// unguarded, with the held exports and the new bootstrap data, while
// ApplyJobSucceeded stays DestructivePlanBlocked (one Warning event)
// naming that plan, which a rotation does not change. A version roll
// changes the approval hash: the change is guarded again, and its new
// block makes a new plan that supersedes the first. The approval of that
// plan applies the change, guarded and allowed, and the plan is Applied.
func TestHoldExportsBlockedChange(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	b1, approval := e.blockChange(exportsE1)

	// The block holds the exports: nothing re-applies the unchanged rest.
	e.reconcileIdle()
	if got := e.heldHash(b1.Name); got != approval {
		t.Errorf("held condition approves %s, want the blocked Job's %s", got, approval)
	}
	if d := e.durable(); d.Pending == nil || d.Pending.Job != b1.Name || d.Pending.ExportsHash != exportsHash(t, exportsE1) || !sameExports(t, d.AppliedClusterOutputs, exportsE0) {
		t.Fatalf("durable after the block: pending %+v, applied %s", d.Pending, d.AppliedClusterOutputs)
	}
	if n := e.rec.count(shared.EventDestructivePlanBlocked); n != 1 {
		t.Errorf("%d DestructivePlanBlocked events, want 1", n)
	}
	first := e.livePlan(approval)
	if first == nil || first.Spec.Reason != infrav1.PlanReasonExportsChange || first.Spec.TargetRef.Kind != infrav1.PlanTargetMachinePool ||
		first.Labels[infrav1.PlanReasonLabel] != string(infrav1.PlanReasonExportsChange) || e.pool().Status.PendingPlanRef.Name != first.Name {
		t.Fatalf("plan of the blocked change = %+v, pool's pendingPlanRef %+v", first, e.pool().Status.PendingPlanRef)
	}

	// A rotation applies, unguarded, with the held exports.
	data := e.rotate("#cloud-config\n# rotated\n")
	h1 := e.reconcileStarts(jobs.OpApply)
	if in := e.rendered(h1); e.guarded(h1) || h1.Annotations[shared.HeldClusterOutputsAnnotation] != "true" ||
		!sameExports(t, in.ClusterOutputs, exportsE0) || in.BootstrapData != data {
		t.Fatalf("rotation while held: guarded %v, annotations %v, exports %s", e.guarded(h1), h1.Annotations, in.ClusterOutputs)
	}
	e.succeed(h1)
	e.reconcileIdle()
	if got := e.heldHash(b1.Name); got != approval {
		t.Errorf("a rotation moved the approval hash: %s, want %s", got, approval)
	}
	if d := e.durable(); d.Pending == nil || !sameExports(t, d.AppliedClusterOutputs, exportsE0) {
		t.Errorf("a held apply's success changed the record: pending %+v, applied %s", d.Pending, d.AppliedClusterOutputs)
	}

	// A version roll changes the approval hash: the change is guarded
	// again, and the plan its block makes supersedes the first.
	e.setVersion("v1.37.0")
	h2 := e.reconcileStarts(jobs.OpApply)
	rolled := h2.Annotations[shared.ApprovalHashAnnotation]
	if in := e.rendered(h2); !e.guarded(h2) || rolled == "" || rolled == approval || !sameExports(t, in.ClusterOutputs, exportsE1) ||
		in.KubernetesVersion == nil || *in.KubernetesVersion != "v1.37.0" {
		t.Fatalf("version roll while held: guarded %v, annotations %v, exports %s", e.guarded(h2), h2.Annotations, in.ClusterOutputs)
	}
	e.block(h2)
	// The roll itself then applies with the held exports, as a rotation
	// does.
	h3 := e.reconcileStarts(jobs.OpApply)
	if in := e.rendered(h3); e.guarded(h3) || !sameExports(t, in.ClusterOutputs, exportsE0) || *in.KubernetesVersion != "v1.37.0" {
		t.Fatalf("version roll while held: guarded %v, exports %s", e.guarded(h3), in.ClusterOutputs)
	}
	if got := e.heldHash(h2.Name); got != rolled {
		t.Errorf("held condition after the roll approves %s, want %s", got, rolled)
	}
	if e.livePlan(approval) != nil || e.livePlan(rolled) == nil {
		t.Errorf("plans after the roll: %+v", e.plans())
	}
	if n := e.rec.count(shared.EventDestructivePlanBlocked); n != 2 {
		t.Errorf("%d DestructivePlanBlocked events, want one per blocked Job", n)
	}
	e.succeed(h3)
	e.reconcileIdle()

	// The approval applies the change, guarded and allowed.
	e.approve(rolled)
	a := e.reconcileStarts(jobs.OpApply)
	if in := e.rendered(a); !e.guarded(a) || e.flag(a, "--inputs-hash") != rolled || e.flag(a, "--allow-deletes-hash") != rolled ||
		a.Annotations[shared.HeldClusterOutputsAnnotation] != "" || a.Annotations[shared.PlanAnnotation] == "" || !sameExports(t, in.ClusterOutputs, exportsE1) {
		t.Fatalf("approved apply: args %v, annotations %v, exports %s", e.args(a), a.Annotations, in.ClusterOutputs)
	}
	e.succeed(a)
	e.reconcileIdle()
	if p := e.plan(a.Annotations[shared.PlanAnnotation]); p.Labels[infrav1.PlanPhaseLabel] != string(infrav1.PlanPhaseApplied) || e.pool().Status.PendingPlanRef.Name != "" {
		t.Errorf("the approved plan after its apply: %+v, pendingPlanRef %+v", p, e.pool().Status.PendingPlanRef)
	}
	if c := e.applyCondition(); c.Status != metav1.ConditionTrue || c.Reason != infrav1.ApplySucceededReason {
		t.Errorf("ApplyJobSucceeded after the approved apply = %+v", c)
	}
	if d := e.durable(); d.Pending != nil || !sameExports(t, d.AppliedClusterOutputs, exportsE1) {
		t.Errorf("durable after the approved apply: pending %+v, applied %s", d.Pending, d.AppliedClusterOutputs)
	}

	// The next rotation applies the new exports, unguarded.
	e.rotate("#cloud-config\n# rotated again\n")
	n := e.reconcileStarts(jobs.OpApply)
	if e.guarded(n) || !sameExports(t, e.rendered(n).ClusterOutputs, exportsE1) {
		t.Errorf("rotation after the approved change: guarded %v", e.guarded(n))
	}
}

// TestHoldExportsNonDestructiveChange: an exports change whose plan the
// guard passes applies at once, with no block, and becomes the applied
// exports.
func TestHoldExportsNonDestructiveChange(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	e.setExports(exportsE1)
	j := e.reconcileStarts(jobs.OpApply)
	if !e.guarded(j) {
		t.Fatal("the apply of new exports is not guarded")
	}
	e.succeed(j)
	e.reconcileIdle()
	if c := e.applyCondition(); c.Status != metav1.ConditionTrue {
		t.Errorf("ApplyJobSucceeded = %+v", c)
	}
	if d := e.durable(); d.Pending != nil || !sameExports(t, d.AppliedClusterOutputs, exportsE1) {
		t.Errorf("durable: pending %+v, applied %s", d.Pending, d.AppliedClusterOutputs)
	}
	if n := e.rec.count(shared.EventDestructivePlanBlocked); n != 0 {
		t.Errorf("%d DestructivePlanBlocked events, want 0", n)
	}
}

// TestHoldExportsChangeAgainAndRevert: exports that change again while a
// change is held are a new pending change, guarded anew with a new
// approval hash; exports that return to the applied ones leave nothing
// pending, and the next rotation applies them unguarded.
func TestHoldExportsChangeAgainAndRevert(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	b1, first := e.blockChange(exportsE1)
	e.reconcileIdle()

	b2, second := e.blockChange(exportsE2)
	if second == first {
		t.Error("a new exports change kept the approval hash")
	}
	e.reconcileIdle()
	if got := e.heldHash(b2.Name); got != second {
		t.Errorf("held condition approves %s, want %s", got, second)
	}
	if d := e.durable(); d.Pending == nil || d.Pending.Job != b2.Name || d.Pending.ExportsHash != exportsHash(t, exportsE2) {
		t.Errorf("pending %+v, want the change of Job %s (not %s)", d.Pending, b2.Name, b1.Name)
	}
	if n := e.rec.count(shared.EventDestructivePlanBlocked); n != 2 {
		t.Errorf("%d DestructivePlanBlocked events, want one per blocked Job", n)
	}

	e.setExports(exportsE0)
	e.reconcileIdle()
	e.rotate("#cloud-config\n# rotated\n")
	j := e.reconcileStarts(jobs.OpApply)
	if e.guarded(j) || j.Annotations[shared.HeldClusterOutputsAnnotation] != "" || !sameExports(t, e.rendered(j).ClusterOutputs, exportsE0) {
		t.Errorf("rotation after the revert: guarded %v, annotations %v", e.guarded(j), j.Annotations)
	}
	e.succeed(j)
	e.reconcileIdle()
	if c := e.applyCondition(); c.Status != metav1.ConditionTrue {
		t.Errorf("ApplyJobSucceeded after the revert = %+v", c)
	}
	if d := e.durable(); d.Pending != nil {
		t.Errorf("a successful apply of the applied exports kept the withdrawn change: %+v", d.Pending)
	}
}

// TestHoldExportsRevertThenReturn: exports that revert to the applied
// ones supersede the plan of the change they leave; a return to the
// change, with no apply between (the same bootstrap data), guards it
// again, unapproved, and its block makes a new plan: the pool then holds
// the change again and keeps applying a rotation with the held exports.
func TestHoldExportsRevertThenReturn(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	_, approval := e.blockChange(exportsE1)
	e.reconcileIdle()
	first := e.livePlan(approval)
	e.setExports(exportsE0)
	e.reconcileIdle()
	if p := e.plan(first.Name); p.Labels[infrav1.PlanPhaseLabel] != string(infrav1.PlanPhaseSuperseded) || e.pool().Status.PendingPlanRef.Name != "" {
		t.Fatalf("the withdrawn change's plan: %+v, pendingPlanRef %+v", p.Labels, e.pool().Status.PendingPlanRef)
	}
	if got := e.rec.count(shared.EventPlanSuperseded); got != 1 {
		t.Errorf("%d PlanSuperseded events, want 1", got)
	}
	e.setExports(exportsE1)
	r := e.reconcileStarts(jobs.OpApply)
	if !e.guarded(r) || e.flag(r, "--inputs-hash") != approval || e.flag(r, "--allow-deletes-hash") != "" || !sameExports(t, e.rendered(r).ClusterOutputs, exportsE1) {
		t.Fatalf("return to the change: args %v, annotations %v", e.args(r), r.Annotations)
	}
	e.block(r)
	e.reconcileIdle()
	if got := e.heldHash(r.Name); got != approval {
		t.Errorf("held condition after the return approves %s, want %s", got, approval)
	}
	if p := e.livePlan(approval); p == nil || p.Name == first.Name {
		t.Errorf("no new plan of the returned change: %+v", e.plans())
	}
	data := e.rotate("#cloud-config\n# rotated\n")
	h := e.reconcileStarts(jobs.OpApply)
	if in := e.rendered(h); e.guarded(h) || h.Annotations[shared.HeldClusterOutputsAnnotation] != "true" ||
		!sameExports(t, in.ClusterOutputs, exportsE0) || in.BootstrapData != data {
		t.Errorf("rotation after the return: guarded %v, annotations %v, exports %s", e.guarded(h), h.Annotations, in.ClusterOutputs)
	}
}

// TestHoldExportsRevertIgnoresApproval: an approved plan of a change that
// is withdrawn before its apply ran is superseded, its approval ignored
// (Approved False/ApprovalIgnored, a Warning), and a return to the change
// is guarded again without it; so is a change replaced by another.
func TestHoldExportsRevertIgnoresApproval(t *testing.T) {
	t.Parallel()
	for name, leave := range map[string]func(*holdEnv){
		"reverted": func(e *holdEnv) { e.setExports(exportsE0); e.reconcileIdle() },
		"replaced": func(e *holdEnv) { e.blockChange(exportsE2) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newHoldEnv(t)
			_, approval := e.blockChange(exportsE1)
			e.reconcileIdle()
			first := e.livePlan(approval)
			e.approve(approval)
			leave(e)
			p := e.plan(first.Name)
			if p.Labels[infrav1.PlanPhaseLabel] != string(infrav1.PlanPhaseSuperseded) ||
				conditions.GetReason(p, infrav1.PlanApprovedCondition) != infrav1.PlanApprovalIgnoredReason {
				t.Fatalf("the approved plan of the left change: labels %v, status %+v", p.Labels, p.Status)
			}
			warned := false
			for _, ev := range e.rec.events() {
				if ev.reason == shared.EventPlanSuperseded && ev.eventType == corev1.EventTypeWarning && strings.Contains(ev.note, "alice") {
					warned = true
				}
			}
			if !warned {
				t.Errorf("no PlanSuperseded Warning naming the approver: %v", e.rec.reasons)
			}
			e.setExports(exportsE1)
			r := e.reconcileStarts(jobs.OpApply)
			if !e.guarded(r) || e.flag(r, "--inputs-hash") != approval || e.flag(r, "--allow-deletes-hash") != "" || r.Annotations[shared.PlanAnnotation] != "" {
				t.Errorf("return to the left change: args %v, annotations %v", e.args(r), r.Annotations)
			}
		})
	}
}

// TestHoldExportsRevertCondition: right after the exports return to the
// applied ones, with no apply, ApplyJobSucceeded no longer reports the
// withdrawn change's block and approve command: the last successful
// apply stands. A return to the change guards it again, and its block is
// reported held.
func TestHoldExportsRevertCondition(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	b1, approval := e.blockChange(exportsE1)
	e.reconcileIdle()
	e.setExports(exportsE0)
	e.reconcileIdle()
	c := e.applyCondition()
	if c.Status != metav1.ConditionTrue || c.Reason != infrav1.ApplySucceededReason || !strings.Contains(c.Message, b1.Name) ||
		strings.Contains(c.Message, "kubectl") || strings.HasPrefix(c.Message, "Job ") {
		t.Errorf("ApplyJobSucceeded after the revert = %+v", c)
	}
	e.reconcileIdle()
	if again := e.applyCondition(); again.Message != c.Message {
		t.Errorf("the reverted condition changed on the next pass: %s", again.Message)
	}
	e.setExports(exportsE1)
	r := e.reconcileStarts(jobs.OpApply)
	e.block(r)
	e.reconcileIdle()
	if got := e.heldHash(r.Name); got != approval {
		t.Errorf("held condition after the return approves %s, want %s", got, approval)
	}
}

// TestHoldExportsRevertAfterFailedApply: when the apply before a
// withdrawn change's blocked Job failed, ApplyJobSucceeded reports that
// failure once the exports return to the applied ones, not the last
// successful apply standing: the current inputs never applied.
func TestHoldExportsRevertAfterFailedApply(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	e.setReplicas(3)
	a1 := e.reconcileStarts(jobs.OpApply)
	if e.guarded(a1) {
		t.Fatalf("a replicas edit is guarded: %v", e.args(a1))
	}
	e.fail(a1)
	e.setExports(exportsE1)
	b := e.reconcileStarts(jobs.OpApply)
	if !e.guarded(b) {
		t.Fatalf("the exports change is not guarded: %v", e.args(b))
	}
	e.block(b)
	e.setExports(exportsE0)
	e.reconcile()
	c := e.applyCondition()
	if c.Status != metav1.ConditionFalse || c.Reason != infrav1.ApplyFailedReason || !strings.HasPrefix(c.Message, "Job "+a1.Name) {
		t.Errorf("ApplyJobSucceeded after the revert = %+v, want the failure of Job %s", c, a1.Name)
	}
	e.reconcile()
	if again := e.applyCondition(); again.Reason != c.Reason || again.Message != c.Message {
		t.Errorf("ApplyJobSucceeded on the next pass = %+v, want %+v", again, c)
	}
}

// unsetReplicas clears the MachinePool's spec.replicas, as the fixture
// has it.
func (e *holdEnv) unsetReplicas() {
	e.t.Helper()
	mp := &clusterv1.MachinePool{}
	if err := e.c.Get(e.t.Context(), client.ObjectKeyFromObject(testMP()), mp); err != nil {
		e.t.Fatal(err)
	}
	mp.Spec.Replicas = nil
	if err := e.c.Update(e.t.Context(), mp); err != nil {
		e.t.Fatal(err)
	}
}

// TestHoldExportsRevertToStateAfterFailedApply: after an apply failed
// and a change of the cluster's exports since was blocked, inputs that
// return to the state's (the exports and the failed edit both reverted)
// are applied again: the blocked change undid nothing of the failure.
func TestHoldExportsRevertToStateAfterFailedApply(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	e.setReplicas(3)
	a1 := e.reconcileStarts(jobs.OpApply)
	e.fail(a1)
	e.setExports(exportsE1)
	b := e.reconcileStarts(jobs.OpApply)
	if !e.guarded(b) || b.Annotations[shared.AfterFailedApplyAnnotation] != "true" {
		t.Fatalf("the exports change after the failed apply: args %v, annotations %v", e.args(b), b.Annotations)
	}
	e.block(b)
	e.setExports(exportsE0)
	e.unsetReplicas()
	r := e.reconcileStarts(jobs.OpApply)
	if in := e.rendered(r); e.guarded(r) || in.Replicas == 3 || !sameExports(t, in.ClusterOutputs, exportsE0) {
		t.Errorf("retry of the state's inputs: args %v, replicas %v, exports %s", e.args(r), in.Replicas, in.ClusterOutputs)
	}
}

// deleteJob deletes job while it runs, as kubectl delete job does, and
// lets the run lease's grace for a missing holder pass.
func (e *holdEnv) deleteJob(job *batchv1.Job) {
	e.t.Helper()
	if err := e.c.Delete(e.t.Context(), job); err != nil {
		e.t.Fatal(err)
	}
	e.clock.SetTime(e.clock.Now().Add(2 * runlease.Grace))
}

// TestHoldExportsVanishedApply: an apply of a change of the cluster's
// exports deleted while it runs may have applied part of it, though no
// result tells: the change is recorded as partly applied, so the pool
// never falls back to the held exports unguarded. Exports that revert
// after an unapproved apply passed the guard are applied guarded; a held
// change whose approved apply was deleted is applied again, guarded under
// the same approval, which a rotation does not move: its plan stays
// approved. A deleted held apply rendered the applied exports and records
// nothing.
func TestHoldExportsVanishedApply(t *testing.T) {
	t.Parallel()
	// partial fails t unless the durable Secret records the change of
	// exports as partly applied by job.
	partial := func(e *holdEnv, job *batchv1.Job, exports string) {
		e.t.Helper()
		if d := e.durable(); d.Partial == nil || d.Partial.Job != job.Name || d.Partial.ExportsHash != exportsHash(e.t, exports) {
			e.t.Errorf("partial change after Job %s vanished: %+v", job.Name, d.Partial)
		}
	}
	t.Run("unapproved, exports reverted", func(t *testing.T) {
		t.Parallel()
		e := newHoldEnv(t)
		e.setExports(exportsE1)
		j := e.reconcileStarts(jobs.OpApply)
		e.deleteJob(j)
		e.setExports(exportsE0)
		r := e.reconcileStarts(jobs.OpApply)
		partial(e, j, exportsE1)
		if !e.guarded(r) || r.Annotations[shared.ApprovalHashAnnotation] == "" || !sameExports(t, e.rendered(r).ClusterOutputs, exportsE0) {
			t.Errorf("revert after the apply vanished: args %v, annotations %v", e.args(r), r.Annotations)
		}
		if d := e.durable(); d.InterruptedApply != j.Name || r.Annotations[shared.AfterInterruptedApplyAnnotation] != j.Name {
			t.Errorf("interrupted apply %q, revert annotations %v; want Job %s for both", d.InterruptedApply, r.Annotations, j.Name)
		}
		if c := e.applyCondition(); c.Reason != infrav1.ApplyFailedReason || c.Message != "Job "+j.Name+
			": disappeared while it ran and may have applied part of its change; an apply of the current inputs is due "+
			"(it is guarded, and a plan that deletes or replaces resources waits for approval)" {
			t.Errorf("ApplyJobSucceeded while the revert runs = %+v", c)
		}
		e.succeed(r)
		e.reconcileNoApply()
		if d := e.durable(); d.InterruptedApply != "" || d.Partial != nil {
			t.Errorf("after the revert succeeded: interrupted apply %q, partial %+v", d.InterruptedApply, d.Partial)
		}
	})
	t.Run("approved", func(t *testing.T) {
		t.Parallel()
		e := newHoldEnv(t)
		b1, _ := e.blockChange(exportsE1)
		e.reconcileIdle()
		approval := e.heldHash(b1.Name)
		e.approve(approval)
		a := e.reconcileStarts(jobs.OpApply)
		e.deleteJob(a)
		e.rotate("#cloud-config\n# rotated\n")
		r := e.reconcileStarts(jobs.OpApply)
		partial(e, a, exportsE1)
		c := e.applyCondition()
		if c.Reason != infrav1.ApplyFailedReason || !strings.HasPrefix(c.Message, "Job "+a.Name+": disappeared while it ran") ||
			!strings.HasSuffix(c.Message, "(it is guarded, and a plan that deletes or replaces resources waits for approval)") {
			t.Errorf("ApplyJobSucceeded after the approved apply vanished = %+v", c)
		}
		if !e.guarded(r) || e.flag(r, "--allow-deletes-hash") != approval || r.Annotations[shared.PlanAnnotation] != a.Annotations[shared.PlanAnnotation] ||
			!sameExports(t, e.rendered(r).ClusterOutputs, exportsE1) {
			t.Errorf("apply after the approved apply vanished: args %v, annotations %v", e.args(r), r.Annotations)
		}
	})
	t.Run("stuck, deleted by the controller", func(t *testing.T) {
		t.Parallel()
		e := newHoldEnv(t)
		e.setExports(exportsE1)
		j := e.reconcileStarts(jobs.OpApply)
		if err := e.c.Delete(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: inputs.RunName(j.Name)}}); err != nil {
			t.Fatal(err)
		}
		e.reconcileIdle()
		if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(j), &batchv1.Job{}); !apierrors.IsNotFound(err) {
			t.Fatalf("the stuck Job was not deleted: %v", err)
		}
		e.clock.SetTime(e.clock.Now().Add(2 * runlease.Grace))
		r := e.reconcileStarts(jobs.OpApply)
		if d := e.durable(); d.Partial != nil || d.InterruptedApply != "" || !e.guarded(r) {
			t.Errorf("after a stuck apply was deleted: partial %+v, interrupted apply %q, args %v", d.Partial, d.InterruptedApply, e.args(r))
		}
	})
	t.Run("held", func(t *testing.T) {
		t.Parallel()
		e := newHoldEnv(t)
		e.blockChange(exportsE1)
		e.reconcileIdle()
		e.rotate("#cloud-config\n# rotated\n")
		h := e.reconcileStarts(jobs.OpApply)
		e.deleteJob(h)
		e.reconcile()
		if d := e.durable(); d.Partial != nil || d.InterruptedApply != h.Name {
			t.Errorf("a vanished held apply: partial %+v, interrupted apply %q, want none and Job %s", d.Partial, d.InterruptedApply, h.Name)
		}
	})
}

// TestInterruptedApplyUnchangedExports: an apply of a version roll deleted
// while it runs, with the cluster's exports unchanged, may have applied
// part of the roll, though no result tells and the state's inputs hash
// is still the last successful apply's. Once the version reverts to the
// state's, the inputs equal it, yet the Job is recorded as interrupted
// and an apply is due: unguarded, as it renders no change of the
// exports, and no change is recorded as partly applied. Its success
// clears the record, and nothing is due after it.
func TestInterruptedApplyUnchangedExports(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	e.setVersion("v1.37.0")
	j := e.reconcileStarts(jobs.OpApply)
	e.deleteJob(j)
	e.setVersion("v1.36.2")
	r := e.reconcileStarts(jobs.OpApply)
	d := e.durable()
	if d.InterruptedApply != j.Name || d.Partial != nil {
		t.Fatalf("after Job %s vanished: interrupted apply %q, partial %+v", j.Name, d.InterruptedApply, d.Partial)
	}
	if e.guarded(r) || r.Annotations[shared.AfterInterruptedApplyAnnotation] != j.Name || r.Annotations[shared.AfterFailedApplyAnnotation] != "true" {
		t.Errorf("apply after Job %s vanished: args %v, annotations %v", j.Name, e.args(r), r.Annotations)
	}
	if c := e.applyCondition(); c.Status != metav1.ConditionFalse || c.Reason != infrav1.ApplyFailedReason || c.Message != "Job "+j.Name+
		": disappeared while it ran and may have applied part of its change; an apply of the current inputs is due "+
		"(it is not guarded: it renders no change of the cluster's exports)" {
		t.Errorf("ApplyJobSucceeded while the apply runs = %+v", c)
	}
	e.succeed(r)
	e.reconcileNoApply()
	if d := e.durable(); d.InterruptedApply != "" {
		t.Errorf("interrupted apply after Job %s succeeded = %q", r.Name, d.InterruptedApply)
	}
	if c := e.applyCondition(); c.Reason != infrav1.ApplySucceededReason || c.Message != "Job "+r.Name {
		t.Errorf("ApplyJobSucceeded after the apply succeeded = %+v", c)
	}
	e.reconcileNoApply()
}

// liveReader is the API reader of a pool whose cache lags: it reads
// through the client, except that the pool's status.activeJob is cleared,
// as the API server has it once the controller's patch cleared it.
type liveReader struct {
	client.Reader
}

// Get reads key into obj using ctx and opts through the embedded reader,
// clearing a TerraformMachinePool's status.activeJob, and returns any
// read error.
func (l liveReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := l.Reader.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if p, ok := obj.(*infrav1.TerraformMachinePool); ok {
		p.Status.ActiveJob = infrav1.ActiveJob{}
	}
	return nil
}

// TestHoldExportsStuckApplyStaleCache: a pass that reads the pool from a
// cache that still names a guarded apply the controller deleted as stuck
// (and cleared from status.activeJob since), while the Job itself is gone
// from the cache and the API server, does not record the change as
// partly applied: the API server's status no longer names the Job. Only
// a status the API server still has does.
func TestHoldExportsStuckApplyStaleCache(t *testing.T) {
	t.Parallel()
	for name, cleared := range map[string]bool{"live status cleared": true, "live status names the Job": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newHoldEnv(t)
			e.setExports(exportsE1)
			j := e.reconcileStarts(jobs.OpApply)
			active := e.pool().Status.ActiveJob
			if active.Name != j.Name {
				t.Fatalf("status.activeJob = %+v, want Job %s", active, j.Name)
			}
			if err := e.c.Delete(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: inputs.RunName(j.Name)}}); err != nil {
				t.Fatal(err)
			}
			e.reconcileIdle()
			if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(j), &batchv1.Job{}); !apierrors.IsNotFound(err) {
				t.Fatalf("the stuck Job was not deleted: %v", err)
			}
			// The client is the cache: it still has the status naming the
			// deleted Job.
			p := e.pool()
			p.Status.ActiveJob = active
			if err := e.c.Status().Update(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			if cleared {
				e.r.Deps.APIReader = liveReader{Reader: e.c}
			}
			e.clock.SetTime(e.clock.Now().Add(2 * runlease.Grace))
			e.reconcile()
			d := e.durable()
			switch {
			case cleared && (d.Partial != nil || d.InterruptedApply != ""):
				t.Errorf("a stale cache recorded the deleted stuck Job: partial %+v, interrupted apply %q", d.Partial, d.InterruptedApply)
			case !cleared && (d.Partial == nil || d.Partial.Job != j.Name || d.InterruptedApply != j.Name):
				t.Errorf("while the API server names Job %s: partial %+v, interrupted apply %q", j.Name, d.Partial, d.InterruptedApply)
			}
		})
	}
}

// gateBootstrap empties the bootstrap data Secret, so the pool's
// dependencies gate it and a pass builds no inputs, and returns a func
// that puts the data back.
func (e *holdEnv) gateBootstrap() func() {
	e.t.Helper()
	key := client.ObjectKey{Namespace: ns, Name: "bootstrap-mp"}
	b := &corev1.Secret{}
	if err := e.c.Get(e.t.Context(), key, b); err != nil {
		e.t.Fatal(err)
	}
	data := b.Data["value"]
	delete(b.Data, "value")
	if err := e.c.Update(e.t.Context(), b); err != nil {
		e.t.Fatal(err)
	}
	return func() {
		e.t.Helper()
		if err := e.c.Get(e.t.Context(), key, b); err != nil {
			e.t.Fatal(err)
		}
		if b.Data == nil {
			b.Data = map[string][]byte{}
		}
		b.Data["value"] = data
		if err := e.c.Update(e.t.Context(), b); err != nil {
			e.t.Fatal(err)
		}
	}
}

// TestHoldExportsGatePassAfterRevert: once the exports returned to the
// applied ones, a pass gated before it builds the inputs does not hold
// the withdrawn change again: ApplyJobSucceeded keeps reporting the last
// successful apply, with no flip and no repeated event once the gate
// lifts.
func TestHoldExportsGatePassAfterRevert(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	b1, _ := e.blockChange(exportsE1)
	e.reconcileIdle()
	e.rotate("#cloud-config\n# rotated\n")
	h := e.reconcileStarts(jobs.OpApply)
	e.succeed(h)
	e.reconcileIdle()
	e.heldHash(b1.Name)
	e.setExports(exportsE0)
	e.reconcileIdle()
	c := e.applyCondition()
	if c.Status != metav1.ConditionTrue || !strings.HasPrefix(c.Message, "Job "+h.Name) {
		t.Fatalf("ApplyJobSucceeded after the revert = %+v", c)
	}
	succeeded, blocked := e.rec.count(shared.EventJobSucceeded), e.rec.count(shared.EventDestructivePlanBlocked)

	ungate := e.gateBootstrap()
	e.reconcileIdle()
	if got := e.applyCondition(); got.Status != c.Status || got.Reason != c.Reason || got.Message != c.Message {
		t.Errorf("ApplyJobSucceeded on a gated pass = %+v, want %+v", got, c)
	}
	ungate()
	e.reconcileIdle()
	if got := e.applyCondition(); got.Status != c.Status || got.Reason != c.Reason || got.Message != c.Message {
		t.Errorf("ApplyJobSucceeded once the gate lifted = %+v, want %+v", got, c)
	}
	if s, b := e.rec.count(shared.EventJobSucceeded), e.rec.count(shared.EventDestructivePlanBlocked); s != succeeded || b != blocked {
		t.Errorf("events after the gated pass: %d JobSucceeded, %d DestructivePlanBlocked; want %d and %d", s, b, succeeded, blocked)
	}
}

// TestHoldExportsGatePassAfterHeldRetry: when a held apply failed, and
// its retry succeeds on a pass gated before it builds the inputs, the
// held change still waits: ApplyJobSucceeded returns to it, not to the
// retry's success, with no flip and no JobSucceeded event once the gate
// lifts.
func TestHoldExportsGatePassAfterHeldRetry(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	b1, approval := e.blockChange(exportsE1)
	e.reconcileIdle()
	e.rotate("#cloud-config\n# rotated\n")
	h := e.reconcileStarts(jobs.OpApply)
	e.fail(h)
	retry := e.reconcileStarts(jobs.OpApply)
	if c := e.applyCondition(); c.Reason != infrav1.ApplyFailedReason || !strings.HasPrefix(c.Message, "Job "+h.Name) {
		t.Fatalf("ApplyJobSucceeded after the held apply failed = %+v", c)
	}
	if retry.Annotations[shared.HeldClusterOutputsAnnotation] != "true" {
		t.Fatalf("the retry is not held: %v", retry.Annotations)
	}
	succeeded := e.rec.count(shared.EventJobSucceeded)

	ungate := e.gateBootstrap()
	e.succeed(retry)
	e.reconcileIdle()
	if got := e.heldHash(b1.Name); got != approval {
		t.Errorf("held condition on the gated pass approves %s, want %s", got, approval)
	}
	ungate()
	e.reconcileIdle()
	if got := e.heldHash(b1.Name); got != approval {
		t.Errorf("held condition once the gate lifted approves %s, want %s", got, approval)
	}
	if n := e.rec.count(shared.EventJobSucceeded); n != succeeded {
		t.Errorf("%d JobSucceeded events after the gated pass, want %d: %v", n, succeeded, e.rec.reasons)
	}
}

// TestHoldExportsDeletingCondition: a pool deleted while a change of the
// cluster's exports is held does not say it keeps applying: it says no
// apply runs, and names no approve command.
func TestHoldExportsDeletingCondition(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	b1, _ := e.blockChange(exportsE1)
	e.reconcileIdle()
	e.heldHash(b1.Name)
	if err := e.c.Delete(t.Context(), e.pool()); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	c := e.applyCondition()
	if c.Reason != infrav1.DestructivePlanBlockedReason || !strings.HasPrefix(c.Message, "Job "+b1.Name+": "+blockedTail) ||
		strings.Contains(c.Message, "keeps applying") || strings.Contains(c.Message, "kubectl annotate") || !strings.Contains(c.Message, "being deleted") {
		t.Errorf("ApplyJobSucceeded while deleting = %+v", c)
	}
}

// TestHoldExportsWarnsOnce: the blocked Job's Warning fires once, not
// again when ApplyJobSucceeded returns to the held change after a held
// apply failed and its retry succeeded.
func TestHoldExportsWarnsOnce(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	b1, _ := e.blockChange(exportsE1)
	e.reconcileIdle()
	e.rotate("#cloud-config\n# rotated\n")
	h := e.reconcileStarts(jobs.OpApply)
	e.fail(h)
	retry := e.reconcileStarts(jobs.OpApply)
	if c := e.applyCondition(); c.Reason != infrav1.ApplyFailedReason {
		t.Errorf("ApplyJobSucceeded after the held apply failed = %+v", c)
	}
	e.succeed(retry)
	e.reconcileIdle()
	e.heldHash(b1.Name)
	if n := e.rec.count(shared.EventDestructivePlanBlocked); n != 1 {
		t.Errorf("%d DestructivePlanBlocked events, want 1: %v", n, e.rec.reasons)
	}
}

// TestHoldExportsWarnsOnceBeforeBookkept: a pass that lists the blocked
// Job without the bookkept mark the pass before patched onto it (a cache
// that lags the patch) does not repeat the Warning: the condition names
// that Job already.
func TestHoldExportsWarnsOnceBeforeBookkept(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	b1, _ := e.blockChange(exportsE1)
	e.reconcileIdle()
	j := &batchv1.Job{}
	if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(b1), j); err != nil {
		t.Fatal(err)
	}
	if j.Annotations[shared.BookkeptAnnotation] != "true" {
		t.Fatalf("the blocked Job was not marked bookkept: %v", j.Annotations)
	}
	delete(j.Annotations, shared.BookkeptAnnotation)
	if err := e.c.Update(t.Context(), j); err != nil {
		t.Fatal(err)
	}
	e.reconcileIdle()
	e.heldHash(b1.Name)
	if n := e.rec.count(shared.EventDestructivePlanBlocked); n != 1 {
		t.Errorf("%d DestructivePlanBlocked events, want 1: %v", n, e.rec.reasons)
	}
}

// TestHoldExportsChecksRenderHeld: while a change is held, refresh and
// drift checks render the held exports, so the change does not read as
// drift.
func TestHoldExportsChecksRenderHeld(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	e.blockChange(exportsE1)
	e.reconcileIdle()

	p := e.pool()
	old := metav1.NewTime(t0.Add(-24 * time.Hour))
	p.Status.LastDriftCheck = &old
	if err := e.c.Status().Update(e.t.Context(), p); err != nil {
		t.Fatal(err)
	}
	d := e.reconcileStarts(jobs.OpDrift)
	if !sameExports(t, e.rendered(d).ClusterOutputs, exportsE0) {
		t.Errorf("the drift check renders %s, want the held exports", e.rendered(d).ClusterOutputs)
	}
	e.succeed(d)

	p = e.pool()
	p.Status.LastRefresh = &old
	if err := e.c.Status().Update(e.t.Context(), p); err != nil {
		t.Fatal(err)
	}
	r := e.reconcileStarts(jobs.OpRefresh)
	if !sameExports(t, e.rendered(r).ClusterOutputs, exportsE0) {
		t.Errorf("the refresh renders %s, want the held exports", e.rendered(r).ClusterOutputs)
	}
}

// TestHoldExportsFailedApplyNotApplied: a guarded apply of new exports
// that fails (not blocked) records nothing: the exports stay those of
// the last successful apply, nothing is held, and the retry renders the
// new exports, guarded again.
func TestHoldExportsFailedApplyNotApplied(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	e.setExports(exportsE1)
	j := e.reconcileStarts(jobs.OpApply)
	e.fail(j)
	retry := e.reconcileStarts(jobs.OpApply)
	if d := e.durable(); d.Pending != nil || !sameExports(t, d.AppliedClusterOutputs, exportsE0) {
		t.Errorf("durable after the failure: pending %+v, applied %s", d.Pending, d.AppliedClusterOutputs)
	}
	if !e.guarded(retry) || retry.Annotations[shared.HeldClusterOutputsAnnotation] != "" || !sameExports(t, e.rendered(retry).ClusterOutputs, exportsE1) {
		t.Errorf("retry: guarded %v, annotations %v", e.guarded(retry), retry.Annotations)
	}
}

// approveAndFail blocks a change of the exports to exports, approves it,
// and fails the approved apply at step. t fails the test on any
// deviation. It returns the failed Job and the approval hash.
func (e *holdEnv) approveAndFail(exports, step string) (*batchv1.Job, string) {
	e.t.Helper()
	e.blockChange(exports)
	e.reconcileIdle()
	approval := e.heldHash(e.durable().Pending.Job)
	e.approve(approval)
	a := e.reconcileStarts(jobs.OpApply)
	if !e.guarded(a) || e.flag(a, "--allow-deletes-hash") != approval || !sameExports(e.t, e.rendered(a).ClusterOutputs, exports) {
		e.t.Fatalf("approved apply: args %v", e.args(a))
	}
	e.failAt(a, step)
	return a, approval
}

// TestHoldExportsPartialApplyNotReverted: an approved apply of a blocked
// change that fails in its apply step may have applied part of it. A version roll or replicas edit then moves the approval hash off
// the approval, and the pool must not fall back to the held exports,
// whose apply would revert that part unguarded: it renders the change,
// guarded by the new approval hash. Once that apply is blocked, the pool
// waits for its approval, across rotations too, and ApplyJobSucceeded
// says why; the approval applies it and clears the record.
func TestHoldExportsPartialApplyNotReverted(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*holdEnv){
		"version roll":  func(e *holdEnv) { e.setVersion("v1.37.0") },
		"replicas edit": func(e *holdEnv) { e.setReplicas(3) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newHoldEnv(t)
			a, approval := e.approveAndFail(exportsE1, runner.StepApply)
			change(e)
			n := e.reconcileStarts(jobs.OpApply)
			rolled := n.Annotations[shared.ApprovalHashAnnotation]
			if in := e.rendered(n); !e.guarded(n) || n.Annotations[shared.HeldClusterOutputsAnnotation] != "" || !sameExports(t, in.ClusterOutputs, exportsE1) ||
				rolled == "" || rolled == approval || e.flag(n, "--inputs-hash") != rolled || e.flag(n, "--allow-deletes-hash") != "" {
				t.Fatalf("apply after the failed approved apply: args %v, annotations %v, exports %s", e.args(n), n.Annotations, in.ClusterOutputs)
			}
			if d := e.durable(); d.Partial == nil || d.Partial.Job != a.Name || d.Partial.ExportsHash != exportsHash(t, exportsE1) {
				t.Fatalf("partial change after the failed approved apply: %+v", d.Partial)
			}

			e.block(n)
			e.reconcileIdle()
			c := e.applyCondition()
			if c.Status != metav1.ConditionFalse || c.Reason != infrav1.DestructivePlanBlockedReason || !strings.HasPrefix(c.Message, "Job "+n.Name+": "+blockedTail) ||
				!strings.Contains(c.Message, "Job "+a.Name+", an earlier apply of a change of them, failed and may have applied part of it") ||
				strings.Contains(c.Message, "keeps applying") || e.approveHash(c) != rolled {
				t.Errorf("ApplyJobSucceeded = %+v, want the partly applied change waiting for %s", c, rolled)
			}

			// A rotation does not move the approval hash: the pool waits.
			e.rotate("#cloud-config\n# rotated\n")
			e.reconcileIdle()

			e.approve(rolled)
			f := e.reconcileStarts(jobs.OpApply)
			if !e.guarded(f) || e.flag(f, "--allow-deletes-hash") != rolled || !sameExports(t, e.rendered(f).ClusterOutputs, exportsE1) {
				t.Fatalf("approved apply: args %v", e.args(f))
			}
			e.succeed(f)
			e.reconcileIdle()
			if d := e.durable(); d.Partial != nil || d.Pending != nil || !sameExports(t, d.AppliedClusterOutputs, exportsE1) {
				t.Errorf("durable after the approved apply: partial %+v, pending %+v, applied %s", d.Partial, d.Pending, d.AppliedClusterOutputs)
			}
		})
	}
}

// TestHoldExportsPartialApplyRevertGuarded: exports that return to the
// applied ones after an approved apply failed part way are applied
// guarded, as the state may hold part of the change; once that apply
// succeeds the record is cleared and a rotation is unguarded again.
func TestHoldExportsPartialApplyRevertGuarded(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	e.approveAndFail(exportsE1, runner.StepApply)
	e.setExports(exportsE0)
	r := e.reconcileStarts(jobs.OpApply)
	if !e.guarded(r) || r.Annotations[shared.ApprovalHashAnnotation] == "" || r.Annotations[shared.HeldClusterOutputsAnnotation] != "" ||
		!sameExports(t, e.rendered(r).ClusterOutputs, exportsE0) {
		t.Fatalf("revert after a partial apply: args %v, annotations %v", e.args(r), r.Annotations)
	}
	e.succeed(r)
	e.reconcileIdle()
	if d := e.durable(); d.Partial != nil || d.Pending != nil || !sameExports(t, d.AppliedClusterOutputs, exportsE0) {
		t.Errorf("durable after the revert: partial %+v, pending %+v, applied %s", d.Partial, d.Pending, d.AppliedClusterOutputs)
	}
	e.rotate("#cloud-config\n# rotated\n")
	if j := e.reconcileStarts(jobs.OpApply); e.guarded(j) {
		t.Errorf("a rotation after the revert is guarded: %v", e.args(j))
	}
}

// TestHoldExportsPartialApplyRevertApproved: after an approved apply
// failed part way, exports that return to the applied ones render a
// guarded apply; when its plan is destructive it waits, and the approval
// of the hash ApplyJobSucceeded shows is not taken for one of a withdrawn
// change: it stays, the apply runs with it, and it is consumed once that
// apply succeeds. A blocked apply of the applied exports is no change of
// them, so nothing is recorded as pending for it; a pending record of
// them (as an earlier version wrote) withdraws nothing either.
func TestHoldExportsPartialApplyRevertApproved(t *testing.T) {
	t.Parallel()
	for name, recorded := range map[string]bool{"no pending record": false, "pending record of the applied exports": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newHoldEnv(t)
			e.approveAndFail(exportsE1, runner.StepApply)
			e.setExports(exportsE0)
			r := e.reconcileStarts(jobs.OpApply)
			if !e.guarded(r) || !sameExports(t, e.rendered(r).ClusterOutputs, exportsE0) {
				t.Fatalf("revert after a partial apply: args %v", e.args(r))
			}
			e.block(r)
			e.reconcileIdle()
			if d := e.durable(); d.Pending != nil && d.Pending.Job == r.Name {
				t.Errorf("the blocked apply of the applied exports was recorded as a pending change: %+v", d.Pending)
			}
			c := e.applyCondition()
			if c.Reason != infrav1.DestructivePlanBlockedReason || !strings.HasPrefix(c.Message, "Job "+r.Name+": ") {
				t.Fatalf("ApplyJobSucceeded = %+v, want the blocked revert", c)
			}
			approval := e.approveHash(c)
			if approval != r.Annotations[shared.ApprovalHashAnnotation] {
				t.Fatalf("the condition approves %s, the blocked Job carries %s", approval, r.Annotations[shared.ApprovalHashAnnotation])
			}
			if recorded {
				p := inputs.Pending{ExportsHash: exportsHash(t, exportsE0), ApprovalHash: approval, Job: r.Name}
				if err := inputs.SetPending(t.Context(), e.c, e.pool(), p); err != nil {
					t.Fatal(err)
				}
			}

			e.approve(approval)
			a := e.reconcileStarts(jobs.OpApply)
			if !e.guarded(a) || e.flag(a, "--allow-deletes-hash") != approval || !sameExports(t, e.rendered(a).ClusterOutputs, exportsE0) {
				t.Fatalf("approved revert: args %v", e.args(a))
			}
			e.reconcileIdle()
			name := a.Annotations[shared.PlanAnnotation]
			if p := e.plan(name); p.Labels[infrav1.PlanPhaseLabel] != string(infrav1.PlanPhaseApproved) {
				t.Fatalf("the plan while its apply runs: %+v", p.Labels)
			}
			e.succeed(a)
			e.reconcileIdle()
			if p := e.plan(name); p.Labels[infrav1.PlanPhaseLabel] != string(infrav1.PlanPhaseApplied) {
				t.Errorf("the plan after its apply succeeded: %+v", p.Labels)
			}
			if d := e.durable(); d.Partial != nil || d.Pending != nil || !sameExports(t, d.AppliedClusterOutputs, exportsE0) {
				t.Errorf("durable after the approved revert: partial %+v, pending %+v, applied %s", d.Partial, d.Pending, d.AppliedClusterOutputs)
			}
		})
	}
}

// dropRecord removes the record of the applied exports from the durable
// Secret, as inputs.WriteAttempt does when it no longer fits next to the
// rendered files: their hash stays.
func (e *holdEnv) dropRecord() {
	e.t.Helper()
	s := &corev1.Secret{}
	if err := e.c.Get(e.t.Context(), client.ObjectKey{Namespace: ns, Name: inputs.Name("mp", e.req.Name)}, s); err != nil {
		e.t.Fatal(err)
	}
	delete(s.Data, inputs.AppliedClusterOutputsKey)
	if err := e.c.Update(e.t.Context(), s); err != nil {
		e.t.Fatal(err)
	}
}

// TestHoldExportsUnrecordedPending: when the guarded apply of a change
// dropped the record of the applied exports (it did not fit next to the
// new files) and was blocked, the pool cannot hold, and "no record" must
// not read as nothing to guard: it waits for the approval like a
// cluster, across rotations, saying so, and the approval applies the
// change. Once the exports return to the old ones (their hash is still
// recorded) nothing needs to apply, and the next rotation applies them
// unguarded and records them again.
func TestHoldExportsUnrecordedPending(t *testing.T) {
	t.Parallel()
	// unrecordedBlock blocks a change to exportsE1 whose apply dropped
	// the record, and returns the blocked Job and its approval hash.
	unrecordedBlock := func(e *holdEnv) (*batchv1.Job, string) {
		e.setExports(exportsE1)
		j := e.reconcileStarts(jobs.OpApply)
		e.dropRecord()
		e.block(j)
		e.reconcileIdle()
		if d := e.durable(); d.Pending == nil || d.Pending.Job != j.Name || d.AppliedClusterOutputs != nil {
			e.t.Fatalf("durable after the block: pending %+v, applied %s", d.Pending, d.AppliedClusterOutputs)
		}
		return j, j.Annotations[shared.ApprovalHashAnnotation]
	}
	t.Run("approved", func(t *testing.T) {
		t.Parallel()
		e := newHoldEnv(t)
		b, approval := unrecordedBlock(e)
		c := e.applyCondition()
		if c.Reason != infrav1.DestructivePlanBlockedReason || !strings.HasPrefix(c.Message, "Job "+b.Name+": "+blockedTail) ||
			!strings.Contains(c.Message, "are not recorded") || strings.Contains(c.Message, "keeps applying") || e.approveHash(c) != approval {
			t.Errorf("ApplyJobSucceeded = %+v", c)
		}
		e.rotate("#cloud-config\n# rotated\n")
		e.reconcileIdle()
		if d := e.durable(); d.Pending == nil {
			t.Error("the pending change was dropped with the record")
		}

		e.approve(approval)
		a := e.reconcileStarts(jobs.OpApply)
		if !e.guarded(a) || e.flag(a, "--allow-deletes-hash") != approval || !sameExports(t, e.rendered(a).ClusterOutputs, exportsE1) {
			t.Fatalf("approved apply: args %v", e.args(a))
		}
		e.succeed(a)
		e.reconcileIdle()
		if d := e.durable(); d.Pending != nil || !sameExports(t, d.AppliedClusterOutputs, exportsE1) {
			t.Errorf("durable after the approved apply: pending %+v, applied %s", d.Pending, d.AppliedClusterOutputs)
		}
	})
	t.Run("reverted", func(t *testing.T) {
		t.Parallel()
		e := newHoldEnv(t)
		unrecordedBlock(e)
		e.setExports(exportsE0)
		e.reconcileIdle()
		if c := e.applyCondition(); strings.Contains(c.Message, "are not recorded") {
			t.Errorf("after the revert the pool still says it waits: %s", c.Message)
		}
		e.rotate("#cloud-config\n# rotated\n")
		r := e.reconcileStarts(jobs.OpApply)
		if e.guarded(r) || !sameExports(t, e.rendered(r).ClusterOutputs, exportsE0) {
			t.Fatalf("rotation after the revert without a record: args %v", e.args(r))
		}
		e.succeed(r)
		e.reconcileIdle()
		if d := e.durable(); d.Pending != nil || !sameExports(t, d.AppliedClusterOutputs, exportsE0) {
			t.Errorf("durable after the revert: pending %+v, applied %s", d.Pending, d.AppliedClusterOutputs)
		}
	})
}

// TestHoldExportsDroppedRecord: once the record of the applied exports
// was dropped for size, the pool still knows their hash. A later change
// of the exports is guarded, not taken for a first apply; it cannot be
// held, so a destructive plan waits for its approval like a cluster's,
// across rotations, saying why. Exports that are the applied ones stay
// unguarded, also next to a stale withdrawn pending change.
func TestHoldExportsDroppedRecord(t *testing.T) {
	t.Parallel()
	t.Run("change", func(t *testing.T) {
		t.Parallel()
		e := newHoldEnv(t)
		e.dropRecord()
		e.setExports(exportsE1)
		j := e.reconcileStarts(jobs.OpApply)
		approval := j.Annotations[shared.ApprovalHashAnnotation]
		if !e.guarded(j) || approval == "" || j.Annotations[shared.HeldClusterOutputsAnnotation] != "" || !sameExports(t, e.rendered(j).ClusterOutputs, exportsE1) {
			t.Fatalf("a change after the record was dropped: args %v, annotations %v", e.args(j), j.Annotations)
		}
		e.block(j)
		e.reconcileIdle()
		c := e.applyCondition()
		if c.Reason != infrav1.DestructivePlanBlockedReason || !strings.HasPrefix(c.Message, "Job "+j.Name+": ") ||
			!strings.Contains(c.Message, "are not recorded") || strings.Contains(c.Message, "keeps applying") || e.approveHash(c) != approval {
			t.Errorf("ApplyJobSucceeded = %+v", c)
		}
		e.rotate("#cloud-config\n# rotated\n")
		e.reconcileIdle()
		e.approve(approval)
		a := e.reconcileStarts(jobs.OpApply)
		if !e.guarded(a) || e.flag(a, "--allow-deletes-hash") != approval {
			t.Fatalf("approved apply: args %v", e.args(a))
		}
	})
	t.Run("stale pending, applied exports", func(t *testing.T) {
		t.Parallel()
		e := newHoldEnv(t)
		e.blockChange(exportsE1)
		e.reconcileIdle()
		e.setExports(exportsE0)
		e.reconcileIdle()
		e.dropRecord()
		e.setVersion("v1.37.0")
		r := e.reconcileStarts(jobs.OpApply)
		if e.guarded(r) || r.Annotations[shared.ApprovalHashAnnotation] != "" || !sameExports(t, e.rendered(r).ClusterOutputs, exportsE0) {
			t.Errorf("a version roll of the applied exports after the record was dropped: args %v, annotations %v", e.args(r), r.Annotations)
		}
	})
}

// TestHoldExportsUnheldConditionCause: the condition of a pool's blocked
// apply that waits for its approval says what the plan is for: a change
// of the cluster's exports, or, after a change was partly applied, the
// exports of the last successful apply (a rotation, a version roll or a
// revert); and why the pool cannot fall back to those exports only when
// it would have to.
func TestHoldExportsUnheldConditionCause(t *testing.T) {
	t.Parallel()
	const change, noChange = "The plan is for a change of the cluster's exports (captf_cluster_outputs)", "The plan is not for a change of the cluster's exports (captf_cluster_outputs)"
	for _, tt := range []struct {
		name          string
		setup         func(*holdEnv)
		want, notWant []string
	}{
		{
			name: "partial, exports reverted",
			setup: func(e *holdEnv) {
				e.approveAndFail(exportsE1, runner.StepApply)
				e.setExports(exportsE0)
			},
			want:    []string{noChange, "every apply of the pool is guarded until one succeeds"},
			notWant: []string{change, "cannot fall back"},
		},
		{
			name: "partial, version roll of the change",
			setup: func(e *holdEnv) {
				e.approveAndFail(exportsE1, runner.StepApply)
				e.setVersion("v1.37.0")
			},
			want:    []string{change, "cannot fall back to the exports of its last successful apply"},
			notWant: []string{noChange},
		},
		{
			name: "record dropped, change",
			setup: func(e *holdEnv) {
				e.dropRecord()
				e.setExports(exportsE1)
			},
			want:    []string{change, "are not recorded, only their hash"},
			notWant: []string{noChange},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newHoldEnv(t)
			tt.setup(e)
			j := e.reconcileStarts(jobs.OpApply)
			e.block(j)
			e.reconcileIdle()
			c := e.applyCondition()
			if c.Reason != infrav1.DestructivePlanBlockedReason || !strings.HasPrefix(c.Message, "Job "+j.Name+": ") {
				t.Fatalf("ApplyJobSucceeded = %+v", c)
			}
			for _, s := range tt.want {
				if !strings.Contains(c.Message, s) {
					t.Errorf("ApplyJobSucceeded lacks %q: %s", s, c.Message)
				}
			}
			for _, s := range tt.notWant {
				if strings.Contains(c.Message, s) {
					t.Errorf("ApplyJobSucceeded says %q: %s", s, c.Message)
				}
			}
		})
	}
}

// TestHoldExportsApprovedApplyFailedBeforeApplyStep: an approved apply
// that fails before its apply step changed nothing, so it records no
// partly applied change. A version roll then moves the approval hash off
// the approved plan's: the change is guarded again, without the approval,
// not held under an approval its plan was not made for.
func TestHoldExportsApprovedApplyFailedBeforeApplyStep(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	_, approval := e.approveAndFail(exportsE1, runner.StepInit)
	e.setVersion("v1.37.0")
	h := e.reconcileStarts(jobs.OpApply)
	if !e.guarded(h) || h.Annotations[shared.HeldClusterOutputsAnnotation] != "" || !sameExports(t, e.rendered(h).ClusterOutputs, exportsE1) ||
		e.flag(h, "--inputs-hash") == approval || e.flag(h, "--allow-deletes-hash") != "" {
		t.Errorf("version roll after an approved apply failed at init: args %v, annotations %v", e.args(h), h.Annotations)
	}
	if d := e.durable(); d.Partial != nil {
		t.Errorf("an apply that failed at init recorded a partial change: %+v", d.Partial)
	}
}
