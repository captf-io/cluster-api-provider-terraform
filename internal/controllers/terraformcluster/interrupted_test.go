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

package terraformcluster

import (
	"cmp"
	"context"
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
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runlease"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// t0 is the fake clock's start: the cluster's last refresh and drift
// check, so neither is due.
var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// jobStore is a jobs.Runner that keeps the Jobs in the fake client, so
// the marks bookkeeping patches onto them stick, and a deleted Job is
// gone from the list and the API server alike.
type jobStore struct {
	c    client.Client
	mu   sync.Mutex
	pods map[string][]corev1.Pod
	// names are the names of the Jobs created, in order.
	names []string
	// onDelete, when set, runs at the start of every Delete.
	onDelete func()
}

var _ jobs.Runner = &jobStore{}

// created returns the names of the Jobs created so far, in order.
func (r *jobStore) created() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.names)
}

// Create stores job in the client with a fake UID and a creation time an
// hour or two before t0, using ctx, and returns any create error.
func (r *jobStore) Create(ctx context.Context, _ client.Object, job *batchv1.Job) error {
	r.mu.Lock()
	r.names = append(r.names, job.Name)
	job.UID = types.UID(fmt.Sprintf("job-uid-%03d", len(r.names)))
	job.CreationTimestamp = metav1.NewTime(t0.Add(-2*time.Hour + time.Duration(len(r.names))*time.Second))
	r.mu.Unlock()
	return r.c.Create(ctx, job)
}

// List returns every Job in ns, read through the client using ctx, or the
// list error.
func (r *jobStore) List(ctx context.Context, _ client.Object, _ string) ([]batchv1.Job, error) {
	var l batchv1.JobList
	if err := r.c.List(ctx, &l, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// Delete removes job from the client using ctx, ignoring not-found, and
// returns any other error.
func (r *jobStore) Delete(ctx context.Context, job *batchv1.Job) error {
	if r.onDelete != nil {
		r.onDelete()
	}
	return client.IgnoreNotFound(r.c.Delete(ctx, job))
}

// Pods returns the pods recorded for job and a nil error.
func (r *jobStore) Pods(_ context.Context, job *batchv1.Job) ([]corev1.Pod, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.pods[job.Name]), nil
}

// stateStore serves the cluster's state, set by the test.
type stateStore struct {
	mu sync.Mutex
	st *state.State
}

// Read returns the state set last, or state.ErrNoState before any.
func (s *stateStore) Read(context.Context, string, string) (*state.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == nil {
		return nil, state.ErrNoState
	}
	return s.st, nil
}

// clusterEnv drives one TerraformCluster through the shared reconcile
// with a fake client, state and runner.
type clusterEnv struct {
	t      *testing.T
	c      client.Client
	st     *stateStore
	runner *jobStore
	clock  *testingclock.FakePassiveClock
	r      *Reconciler
	req    ctrl.Request
	// finished counts finished Jobs, which finish a second apart, an hour
	// before finishBase.
	finished int
	// finishBase is the time Jobs finish an hour before: t0 unless set.
	finishBase time.Time
}

// newClusterEnv returns a TerraformCluster after its first apply: started,
// succeeded and bookkept, so the state records the inputs hash of the
// Cluster's version v1.36.2. t fails the test on any error.
func newClusterEnv(t *testing.T) *clusterEnv {
	t.Helper()
	tc := testTC(owned, func(tc *infrav1.TerraformCluster) {
		tc.Finalizers = []string{Finalizer}
		tc.Status.Conditions = []metav1.Condition{{
			Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason, LastTransitionTime: metav1.NewTime(t0),
		}}
		now := metav1.NewTime(t0)
		tc.Status.LastRefresh, tc.Status.LastDriftCheck = &now, &now
	})
	s := scheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&infrav1.TerraformClusterIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: "aws", UID: "id-uid"},
			Spec: infrav1.TerraformClusterIdentitySpec{
				SecretRef:         infrav1.SecretReference{Name: "aws-creds", Namespace: "captf-system"},
				AllowedNamespaces: &infrav1.AllowedNamespaces{Selector: &metav1.LabelSelector{}},
			},
		},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "captf-system", Name: "aws-creds"}, Data: map[string][]byte{"KEY": []byte("v")}},
		testCluster(func(c *clusterv1.Cluster) { c.Spec.Topology.Version = "v1.36.2" }),
		tc,
	).WithStatusSubresource(&infrav1.TerraformCluster{}, &infrav1.TerraformPlan{}).
		WithIndex(&infrav1.TerraformPlan{}, shared.PlanTargetIndex, shared.PlanTargetIndexer).Build()
	e := &clusterEnv{
		t: t, c: c, st: &stateStore{}, runner: &jobStore{c: c, pods: map[string][]corev1.Pod{}},
		clock: testingclock.NewFakePassiveClock(t0), req: ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tc)},
	}
	e.r = &Reconciler{Deps: shared.Deps{
		Client: c, APIReader: c, Scheme: s, Jobs: e.runner, State: e.st, Recorder: &recorder{},
		Clock: e.clock, RunnerImage: "registry.example/captf:dev", DriftDefault: 30 * time.Minute,
	}}
	first := e.reconcileStarts()
	e.succeed(first)
	e.reconcileNoApply()
	if c := e.applyCondition(); c.Reason != infrav1.ApplySucceededReason {
		t.Fatalf("after the first apply: ApplyJobSucceeded = %+v", c)
	}
	return e
}

// reconcile runs one reconcile of the cluster, failing the test on error.
func (e *clusterEnv) reconcile() {
	e.t.Helper()
	if _, err := e.r.Reconcile(e.t.Context(), e.req); err != nil {
		e.t.Fatal(err)
	}
}

// reconcileStarts reconciles once and returns the one apply Job it
// started, failing the test unless exactly that Job started.
func (e *clusterEnv) reconcileStarts() *batchv1.Job {
	e.t.Helper()
	before := e.runner.created()
	e.reconcile()
	started := e.runner.created()[len(before):]
	if len(started) != 1 || !strings.Contains(started[0], "-apply-") {
		e.t.Fatalf("started %v, want one apply Job", started)
	}
	j := &batchv1.Job{}
	if err := e.c.Get(e.t.Context(), client.ObjectKey{Namespace: ns, Name: started[0]}, j); err != nil {
		e.t.Fatal(err)
	}
	return j
}

// reconcileNoApply reconciles once, failing the test if an apply Job
// started; a refresh or drift check may.
func (e *clusterEnv) reconcileNoApply() {
	e.t.Helper()
	before := e.runner.created()
	e.reconcile()
	for _, name := range e.runner.created()[len(before):] {
		if strings.Contains(name, "-apply-") {
			e.t.Fatalf("started apply Job %s, want none", name)
		}
	}
}

// finish marks job finished with the given terminal condition type.
func (e *clusterEnv) finish(job *batchv1.Job, condition batchv1.JobConditionType) {
	e.t.Helper()
	e.finished++
	stored := &batchv1.Job{}
	if err := e.c.Get(e.t.Context(), client.ObjectKeyFromObject(job), stored); err != nil {
		e.t.Fatal(err)
	}
	at := metav1.NewTime(cmp.Or(e.finishBase, t0).Add(-time.Hour + time.Duration(e.finished)*time.Second))
	stored.Status.Conditions = []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue, LastTransitionTime: at}}
	if err := e.c.Status().Update(e.t.Context(), stored); err != nil {
		e.t.Fatal(err)
	}
}

// succeed finishes job, an apply, successfully: the state then records
// the inputs hash it rendered, as the runner's backend leaves it.
func (e *clusterEnv) succeed(job *batchv1.Job) {
	e.t.Helper()
	e.finish(job, batchv1.JobComplete)
	st := clusterState(false, map[string]string{
		"control_plane_endpoint": `{"host":"lb.example","port":6443}`,
		"failure_domains":        `[{"name":"az-1"}]`,
		"health":                 healthy,
	})
	st.InputsHash = job.Annotations[state.InputsHashAnnotation]
	e.st.mu.Lock()
	e.st.st = st
	e.st.mu.Unlock()
}

// blockedPlanHash is the hash of the plan a blocked apply reports (block).
const blockedPlanHash = "p2:blocked"

// block finishes job, an apply, as the runner's guard stops it before a
// destructive plan, which it reports.
func (e *clusterEnv) block(job *batchv1.Job) {
	e.t.Helper()
	res := runner.Result{
		Version: runner.ResultVersion, Op: runner.OpApply, Steps: []runner.Step{{Name: "init"}, {Name: "plan", Exit: 2}, {Name: "show-json"}},
		Error: &runner.Error{Kind: runner.ErrorKindBlocked, Tail: "the plan deletes or replaces 1 resource(s): module.role.aws_lb.this (delete)"},
		Plan:  &runner.Plan{Hash: blockedPlanHash, Delete: 1, Resources: []string{"module.role.aws_lb.this (delete)"}},
	}
	cs := corev1.ContainerStatus{Name: jobs.SourceContainer}
	cs.State.Terminated = &corev1.ContainerStateTerminated{Message: string(runner.Encode(res))}
	p := corev1.Pod{}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
	e.runner.mu.Lock()
	e.runner.pods[job.Name] = []corev1.Pod{p}
	e.runner.mu.Unlock()
	e.finish(job, batchv1.JobFailed)
}

// deleteJob deletes job while it runs, as kubectl delete job does, and
// lets the run lease's grace for a missing holder pass.
func (e *clusterEnv) deleteJob(job *batchv1.Job) {
	e.t.Helper()
	if err := e.c.Delete(e.t.Context(), job); err != nil {
		e.t.Fatal(err)
	}
	e.clock.SetTime(e.clock.Now().Add(2 * runlease.Grace))
}

// setVersion sets the Cluster's topology version to v, an input of the
// cluster's apply.
func (e *clusterEnv) setVersion(v string) {
	e.t.Helper()
	c := &clusterv1.Cluster{}
	if err := e.c.Get(e.t.Context(), client.ObjectKey{Namespace: ns, Name: "c1"}, c); err != nil {
		e.t.Fatal(err)
	}
	c.Spec.Topology.Version = v
	if err := e.c.Update(e.t.Context(), c); err != nil {
		e.t.Fatal(err)
	}
}

// cluster re-reads the TerraformCluster and returns it.
func (e *clusterEnv) cluster() *infrav1.TerraformCluster {
	e.t.Helper()
	tc := &infrav1.TerraformCluster{}
	if err := e.c.Get(e.t.Context(), e.req.NamespacedName, tc); err != nil {
		e.t.Fatal(err)
	}
	return tc
}

// approve approves the cluster's live TerraformPlan (status.pendingPlanRef)
// as alice and returns it.
func (e *clusterEnv) approve() *infrav1.TerraformPlan {
	e.t.Helper()
	tp := &infrav1.TerraformPlan{}
	if err := e.c.Get(e.t.Context(), client.ObjectKey{Namespace: ns, Name: e.cluster().Status.PendingPlanRef.Name}, tp); err != nil {
		e.t.Fatal(err)
	}
	tp.Spec.Approved, tp.Spec.ApprovedBy = new(true), "alice"
	if err := e.c.Update(e.t.Context(), tp); err != nil {
		e.t.Fatal(err)
	}
	return tp
}

// interrupted returns the apply Job the durable Secret records as gone
// before it finished ("" when none).
func (e *clusterEnv) interrupted() string {
	e.t.Helper()
	d, err := inputs.Read(e.t.Context(), e.c, ns, "c", e.req.Name)
	if err != nil {
		e.t.Fatal(err)
	}
	return d.InterruptedApply
}

// applyCondition returns the cluster's ApplyJobSucceeded condition.
func (e *clusterEnv) applyCondition() metav1.Condition {
	e.t.Helper()
	c := conditions.Get(e.cluster(), infrav1.ApplyJobSucceededCondition)
	if c == nil {
		e.t.Fatal("no ApplyJobSucceeded condition")
	}
	return *c
}

// args returns job's runner arguments.
func args(job *batchv1.Job) []string {
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.Name == jobs.SourceContainer {
			return c.Args
		}
	}
	return nil
}

// TestInterruptedApply: an apply of a version roll deleted while it runs
// may have applied part of the roll, though no result tells and the
// state's inputs hash is still the last successful apply's. Once the
// version reverts to the state's, the inputs equal it, yet the Job is
// recorded as interrupted and an apply of them is due, guarded, and
// ApplyJobSucceeded says why. Its plan, which deletes what the deleted
// Job created, is blocked and waits for its approval, which applies it;
// that success clears the record, and nothing is due after it.
func TestInterruptedApply(t *testing.T) {
	t.Parallel()
	e := newClusterEnv(t)
	h0 := e.applyJob(t).Annotations[state.InputsHashAnnotation]
	e.setVersion("v1.37.0")
	j := e.reconcileStarts()
	e.deleteJob(j)
	e.setVersion("v1.36.2")
	r := e.reconcileStarts()
	if got := e.interrupted(); got != j.Name {
		t.Fatalf("interrupted apply after Job %s vanished = %q", j.Name, got)
	}
	if r.Annotations[state.InputsHashAnnotation] != h0 || !slices.Contains(args(r), "--guard-deletes") ||
		r.Annotations[shared.AfterInterruptedApplyAnnotation] != j.Name || r.Annotations[shared.AfterFailedApplyAnnotation] != "true" {
		t.Errorf("apply after Job %s vanished: args %v, annotations %v; want the state's inputs %s, guarded", j.Name, args(r), r.Annotations, h0)
	}
	if c := e.applyCondition(); c.Status != metav1.ConditionFalse || c.Reason != infrav1.ApplyFailedReason || c.Message != "Job "+j.Name+
		": disappeared while it ran and may have applied part of its change; an apply of the current inputs is due "+
		"(it is guarded, and a plan that deletes or replaces resources waits for approval)" {
		t.Errorf("ApplyJobSucceeded while the apply runs = %+v", c)
	}

	e.block(r)
	e.reconcileNoApply()
	e.reconcileNoApply()
	if c := e.applyCondition(); c.Reason != infrav1.DestructivePlanBlockedReason || !strings.HasPrefix(c.Message, "Job "+r.Name+":") ||
		!strings.Contains(c.Message, "kubectl patch terraformplan "+e.cluster().Status.PendingPlanRef.Name+" ") {
		t.Errorf("ApplyJobSucceeded after the apply was blocked = %+v", c)
	}
	if got := e.interrupted(); got != j.Name {
		t.Errorf("interrupted apply after a blocked apply = %q, want %s", got, j.Name)
	}

	tp := e.approve()
	if tp.Spec.InputsHash != h0 || tp.Spec.Reason != infrav1.PlanReasonDestructive {
		t.Errorf("TerraformPlan = %+v, want the destructive plan of %s", tp.Spec, h0)
	}
	a := e.reconcileStarts()
	if !slices.Contains(args(a), "--expect-plan="+blockedPlanHash) || a.Annotations[shared.AfterInterruptedApplyAnnotation] != j.Name {
		t.Errorf("approved apply: args %v, annotations %v", args(a), a.Annotations)
	}
	e.succeed(a)
	e.reconcileNoApply()
	if got := e.interrupted(); got != "" {
		t.Errorf("interrupted apply after Job %s succeeded = %q", a.Name, got)
	}
	if c := e.applyCondition(); c.Reason != infrav1.ApplySucceededReason || c.Message != "Job "+a.Name {
		t.Errorf("ApplyJobSucceeded after the approved apply = %+v", c)
	}
	if p := e.cluster().Status.PendingPlanRef; p.Name != "" {
		t.Errorf("status.pendingPlanRef = %+v after the plan was applied", p)
	}
	e.reconcileNoApply()
}

// applyJob returns the newest apply Job, failing t when there is none.
func (e *clusterEnv) applyJob(t *testing.T) *batchv1.Job {
	t.Helper()
	names := e.runner.created()
	for i := len(names) - 1; i >= 0; i-- {
		if strings.Contains(names[i], "-apply-") {
			j := &batchv1.Job{}
			if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: names[i]}, j); err != nil {
				t.Fatal(err)
			}
			return j
		}
	}
	t.Fatal("no apply Job")
	return nil
}

// staleStatus is the API reader of a cluster whose cache lags: it reads
// through the client, except that the cluster's status.activeJob is
// cleared, as the API server has it once a patch cleared it.
type staleStatus struct {
	client.Reader
}

// Get reads key into obj using ctx and opts through the embedded reader,
// clearing a TerraformCluster's status.activeJob, and returns any read
// error.
func (s staleStatus) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := s.Reader.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if tc, ok := obj.(*infrav1.TerraformCluster); ok {
		tc.Status.ActiveJob = infrav1.ActiveJob{}
	}
	return nil
}

// TestInterruptedApplyNotRecorded: no apply Job is recorded as
// interrupted when the API server's status.activeJob no longer names the
// gone Job (a cache that lags a pass that cleared it), nor when the
// controller deleted a stuck Job, which never started, itself; with the
// inputs back at the state's, nothing is then due.
func TestInterruptedApplyNotRecorded(t *testing.T) {
	t.Parallel()
	t.Run("stale cache", func(t *testing.T) {
		t.Parallel()
		e := newClusterEnv(t)
		e.setVersion("v1.37.0")
		j := e.reconcileStarts()
		e.deleteJob(j)
		e.setVersion("v1.36.2")
		e.r.Deps.APIReader = staleStatus{Reader: e.c}
		e.reconcileNoApply()
		if got := e.interrupted(); got != "" {
			t.Errorf("interrupted apply recorded from a stale status: %q", got)
		}
	})
	t.Run("stuck, deleted by the controller", func(t *testing.T) {
		t.Parallel()
		e := newClusterEnv(t)
		e.setVersion("v1.37.0")
		j := e.reconcileStarts()
		if err := e.c.Delete(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: inputs.RunName(j.Name)}}); err != nil {
			t.Fatal(err)
		}
		e.reconcileNoApply()
		if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(j), &batchv1.Job{}); !apierrors.IsNotFound(err) {
			t.Fatalf("the stuck Job was not deleted: %v", err)
		}
		e.setVersion("v1.36.2")
		e.clock.SetTime(e.clock.Now().Add(2 * runlease.Grace))
		e.reconcileNoApply()
		if got := e.interrupted(); got != "" {
			t.Errorf("interrupted apply recorded for a stuck Job the controller deleted: %q", got)
		}
	})
}

// planFinished finishes job, a plan Job, successfully with a plan of
// hash, as the runner's termination message reports it.
func (e *clusterEnv) planFinished(job *batchv1.Job, hash string) {
	e.t.Helper()
	e.resultFinished(job, runner.Result{
		Version: runner.ResultVersion, Op: runner.OpPlan, Steps: []runner.Step{{Name: "init"}, {Name: "plan", Exit: 2}, {Name: "show-json"}},
		Plan: &runner.Plan{Hash: hash, Update: 1, Resources: []string{"module.role.aws_lb.this (update)"}},
	})
}

// driftFinished finishes job, a drift Job, successfully with no drift.
func (e *clusterEnv) driftFinished(job *batchv1.Job) {
	e.t.Helper()
	e.resultFinished(job, runner.Result{
		Version: runner.ResultVersion, Op: runner.OpDrift, Steps: []runner.Step{{Name: "init"}, {Name: "plan"}}, Drift: &runner.Drift{},
	})
}

// resultFinished finishes job successfully, its termination message the
// encoding of res.
func (e *clusterEnv) resultFinished(job *batchv1.Job, res runner.Result) {
	e.t.Helper()
	cs := corev1.ContainerStatus{Name: jobs.SourceContainer}
	cs.State.Terminated = &corev1.ContainerStateTerminated{Message: string(runner.Encode(res))}
	p := corev1.Pod{}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
	e.runner.mu.Lock()
	e.runner.pods[job.Name] = []corev1.Pod{p}
	e.runner.mu.Unlock()
	e.finish(job, batchv1.JobComplete)
}

// reconcileStartsOp reconciles once and returns the one Job it started,
// failing the test unless exactly one Job of op started.
func (e *clusterEnv) reconcileStartsOp(op jobs.Op) *batchv1.Job {
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
		e.t.Fatalf("started Job %s of %s, want %s", j.Name, jobs.OpOf(j), op)
	}
	return j
}

// TestInterruptedApplyManual: under applyPolicy Manual, a due apply after
// a vanished one plans first. While the plan waits for approval the
// condition holds the plan's wait, also while drift and refresh checks
// run, and the Warning for the vanished Job is emitted once; the
// approved apply carries the marker's Job, and its success clears the
// marker.
func TestInterruptedApplyManual(t *testing.T) {
	t.Parallel()
	e := newClusterEnv(t)
	e.setVersion("v1.37.0")
	j := e.reconcileStarts()
	e.deleteJob(j)
	e.setVersion("v1.36.2")
	tc := e.cluster()
	tc.Spec.ApplyPolicy = infrav1.ApplyPolicyManual
	if err := e.c.Update(t.Context(), tc); err != nil {
		t.Fatal(err)
	}

	plan := e.reconcileStartsOp(jobs.OpPlan)
	if got := e.interrupted(); got != j.Name {
		t.Fatalf("interrupted apply after Job %s vanished = %q", j.Name, got)
	}
	planHash := runner.PlanHash([]string{"module.role.aws_lb.this|update"})
	e.planFinished(plan, planHash)
	rec, ok := e.r.Deps.Recorder.(*recorder)
	if !ok {
		t.Fatalf("recorder is %T", e.r.Deps.Recorder)
	}
	e.reconcileNoApply()
	if c := e.applyCondition(); c.Reason != infrav1.PlanAwaitingApprovalReason {
		t.Fatalf("ApplyJobSucceeded while the plan waits = %+v", c)
	}
	// A drift check that runs meanwhile keeps the plan's wait: the
	// interrupted condition never replaces it, on the pass that starts the
	// check or while it runs.
	interruptedSeen := 0
	for range 3 {
		e.clock.SetTime(e.clock.Now().Add(time.Hour))
		e.finishBase = e.clock.Now().Add(59 * time.Minute)
		for passes := 0; ; passes++ {
			if passes > 5 {
				t.Fatal("the plan's wait did not settle")
			}
			before := len(e.runner.created())
			e.reconcileNoApply()
			started := e.runner.created()[before:]
			if len(started) == 0 {
				break
			}
			if c := e.applyCondition(); c.Reason == infrav1.ApplyFailedReason && strings.HasPrefix(c.Message, "Job "+j.Name+": disappeared") {
				interruptedSeen++
			}
			for _, name := range started {
				s := &batchv1.Job{}
				if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, s); err != nil {
					t.Fatal(err)
				}
				if jobs.OpOf(s) == jobs.OpPlan {
					e.planFinished(s, planHash)
				} else {
					e.driftFinished(s)
				}
			}
		}
		if c := e.applyCondition(); c.Reason != infrav1.PlanAwaitingApprovalReason {
			t.Fatalf("ApplyJobSucceeded once the checks settled = %+v", c)
		}
	}
	if interruptedSeen != 0 {
		t.Error("the interrupted condition replaced the plan's wait")
	}
	failed := 0
	rec.mu.Lock()
	for _, reason := range rec.reasons {
		if reason == shared.EventJobFailed {
			failed++
		}
	}
	rec.mu.Unlock()
	if failed != 1 {
		t.Errorf("%d JobFailed Warnings while the plan waits for approval, want 1", failed)
	}

	tp := e.approve()
	a := e.reconcileStartsOp(jobs.OpApply)
	if a.Annotations[shared.AfterInterruptedApplyAnnotation] != j.Name || !slices.Contains(args(a), "--expect-plan="+planHash) ||
		a.Annotations[shared.PlanAnnotation] != tp.Name {
		t.Errorf("approved apply: args %v, annotations %v", args(a), a.Annotations)
	}
	e.succeed(a)
	e.reconcileNoApply()
	if got := e.interrupted(); got != "" {
		t.Errorf("interrupted apply after Job %s succeeded = %q", a.Name, got)
	}
	if c := e.applyCondition(); c.Reason != infrav1.ApplySucceededReason {
		t.Errorf("ApplyJobSucceeded after the approved apply = %+v", c)
	}
}
