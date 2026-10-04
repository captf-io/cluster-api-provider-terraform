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

package shared

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	testingclock "k8s.io/utils/clock/testing"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/outputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// testNS, testName, testFinal and testIdentity are the namespace, name,
// finalizer and identity name every fake object in this package uses.
const (
	testNS       = "team-a"
	testName     = "m1"
	testFinal    = "terraformmachine.infrastructure.cluster.x-k8s.io"
	testIdentity = "aws"
)

// testScheme returns a runtime.Scheme with the core, batch, coordination and
// RBAC groups this package reads and writes, plus the Cluster API and CAPTF
// types, registered, failing t on error. It registers only those groups,
// not all of client-go's: the fake client rebuilds a RESTMapper from the
// whole scheme on every write, which dominated this package's test time.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, batchv1.AddToScheme, coordinationv1.AddToScheme, rbacv1.AddToScheme,
		clusterv1.AddToScheme, infrav1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// fakeKind adapts a TerraformMachine; every behavior is a field.
type fakeKind struct {
	obj      *infrav1.TerraformMachine
	owner    OwnerInfo
	ownerErr error
	mutable  bool
	refresh  bool
	in       any
	gate     *Gate
	result   outputs.Result
	health   *contract.Health
	blocked  bool
	// blockedCondition makes DeletionBlocked set the DeletionBlocked
	// condition, as the TerraformCluster adapter does.
	blockedCondition bool
	// asCluster makes the adapter report the TerraformCluster kind (whose
	// applies are guarded against destructive plans) with drift policy
	// clusterDrift and no inherited defaults; the object stays a
	// TerraformMachine.
	asCluster    bool
	clusterDrift *infrav1.DriftPolicy
	// applyPolicy is the cluster kind's applyPolicy; plan, when set, is
	// its status.plan (env.plan, so it outlives the adapter).
	applyPolicy infrav1.ApplyPolicy
	plan        *infrav1.PlanPreview
}

// Object returns f's object.
func (f *fakeKind) Object() Object { return f.obj }

// Kind returns state.KindTerraformCluster when f.asCluster, otherwise
// state.KindTerraformMachine.
func (f *fakeKind) Kind() string {
	if f.asCluster {
		return state.KindTerraformCluster
	}
	return state.KindTerraformMachine
}

// Role always returns contract.RoleMachine.
func (f *fakeKind) Role() contract.Role { return contract.RoleMachine }

// Finalizer always returns testFinal.
func (f *fakeKind) Finalizer() string { return testFinal }

// Mutable returns f.mutable.
func (f *fakeKind) Mutable() bool { return f.mutable }

// RefreshAfterApply returns f.refresh.
func (f *fakeKind) RefreshAfterApply() bool { return f.refresh }

// Spec returns a SpecView built from f.obj's spec, with the cluster fields
// (Drift, ApplyPolicy, no InheritsDefaults) set when f.asCluster, and the
// machine fields (MachineDrift, Remediation, InheritsDefaults) otherwise.
func (f *fakeKind) Spec() SpecView {
	if f.asCluster {
		return SpecView{WorkspaceSpec: f.obj.Spec.WorkspaceSpec, Drift: f.clusterDrift, ApplyPolicy: f.applyPolicy}
	}
	return SpecView{
		WorkspaceSpec: f.obj.Spec.WorkspaceSpec, MachineDrift: f.obj.Spec.Drift,
		Remediation: f.obj.Spec.Remediation, InheritsDefaults: true,
	}
}

// Status returns pointers into f.obj's status fields, with Plan set to
// f.plan.
func (f *fakeKind) Status() CommonStatus {
	return CommonStatus{WorkspaceStatus: &f.obj.Status.WorkspaceStatus, UnhealthySamples: &f.obj.Status.UnhealthySamples, Plan: f.plan}
}

// Owner returns f.owner and f.ownerErr.
func (f *fakeKind) Owner(context.Context) (OwnerInfo, error) { return f.owner, f.ownerErr }

// BuildInputs returns f.in and f.gate, and a nil error.
func (f *fakeKind) BuildInputs(context.Context, OwnerInfo, *inputs.Durable) (any, *Gate, error) {
	return f.in, f.gate, nil
}

// ApplyOutputs returns f.result and f.health, and a nil error.
func (f *fakeKind) ApplyOutputs(context.Context, OwnerInfo, *state.State, *inputs.Durable) (outputs.Result, *contract.Health, error) {
	return f.result, f.health, nil
}

// DeletionBlocked returns f.blocked and a nil error, setting the
// DeletionBlocked condition from it when f.blockedCondition.
func (f *fakeKind) DeletionBlocked(context.Context, OwnerInfo) (bool, error) {
	if f.blockedCondition {
		c := metav1.Condition{Type: infrav1.DeletionBlockedCondition, Status: metav1.ConditionFalse, Reason: infrav1.NotBlockedReason}
		if f.blocked {
			c.Status, c.Reason, c.Message = metav1.ConditionTrue, infrav1.DependentsExistReason, "1 TerraformMachine(s) still exist"
		}
		conditions.Set(f.obj, c)
	}
	return f.blocked, nil
}

// fakeRunner is an in-memory jobs.Runner.
type fakeRunner struct {
	mu      sync.Mutex
	jobs    []batchv1.Job
	pods    map[string][]corev1.Pod
	created []string
	deleted []string
	// podLists names the Job of each Pods call.
	podLists []string
	uid      int
	// onCreate runs before a Job is stored, for tests asserting that
	// block-move is set before the Job exists.
	onCreate func(*batchv1.Job)
	// createErr, when set, makes Create fail (e.g. apierrors.NewAlreadyExists
	// to simulate a Job that already exists after a crashed retry) instead
	// of storing the Job.
	createErr error
}

// Create stores job (after running r.onCreate on it) and records its name
// in r.created, assigning it a fake UID, or returns r.createErr when set.
func (r *fakeRunner) Create(_ context.Context, _ client.Object, job *batchv1.Job) error {
	if r.onCreate != nil {
		r.onCreate(job)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.createErr != nil {
		return r.createErr
	}
	r.uid++
	job.UID = types.UID(fmt.Sprintf("job-uid-%d", r.uid))
	r.jobs = append(r.jobs, *job.DeepCopy())
	r.created = append(r.created, job.Name)
	return nil
}

// List returns a copy of every Job r holds, and a nil error.
func (r *fakeRunner) List(context.Context, client.Object, string) ([]batchv1.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]batchv1.Job, 0, len(r.jobs))
	for i := range r.jobs {
		out = append(out, *r.jobs[i].DeepCopy())
	}
	return out, nil
}

// Delete removes job from r's Jobs, records its name in r.deleted, and
// returns a nil error.
func (r *fakeRunner) Delete(_ context.Context, job *batchv1.Job) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs = slices.DeleteFunc(r.jobs, func(j batchv1.Job) bool { return j.Name == job.Name })
	r.deleted = append(r.deleted, job.Name)
	return nil
}

// Pods records job's name in r.podLists and returns a copy of r.pods[job.Name],
// with a nil error.
func (r *fakeRunner) Pods(_ context.Context, job *batchv1.Job) ([]corev1.Pod, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.podLists = append(r.podLists, job.Name)
	return slices.Clone(r.pods[job.Name]), nil
}

// fakeState serves st for every suffix, or the state in bySuffix for a
// suffix listed there (several objects in one env).
type fakeState struct {
	st       *state.State
	err      error
	bySuffix map[string]*state.State
}

// Read returns the state recorded for suffix in f.bySuffix (state.ErrNoState
// when that entry is nil), or otherwise f.st and f.err, defaulting to
// state.ErrNoState when both are unset.
func (f *fakeState) Read(_ context.Context, _, suffix string) (*state.State, error) {
	if st, ok := f.bySuffix[suffix]; ok {
		if st == nil {
			return nil, state.ErrNoState
		}
		return st, nil
	}
	if f.st == nil && f.err == nil {
		return nil, state.ErrNoState
	}
	return f.st, f.err
}

// machine returns a TerraformMachine with a Machine ownerRef, with each mut
// applied in order.
func machine(mut ...func(*infrav1.TerraformMachine)) *infrav1.TerraformMachine {
	m := &infrav1.TerraformMachine{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNS, Name: testName, UID: "m1-uid",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "c1"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: "m1", UID: "machine-uid",
			}},
		},
		Spec: infrav1.TerraformMachineSpec{
			WorkspaceSpec: infrav1.WorkspaceSpec{
				Source:      infrav1.Source{Image: "registry.example/mod:1.0"},
				IdentityRef: infrav1.IdentityReference{Name: testIdentity},
			},
		},
	}
	for _, f := range mut {
		f(m)
	}
	return m
}

// withFinalizer sets m's finalizers to just testFinal.
func withFinalizer(m *infrav1.TerraformMachine) { m.Finalizers = []string{testFinal} }

// deleting sets m's deletion timestamp to now, adding testFinal as its
// finalizer if it has none.
func deleting(m *infrav1.TerraformMachine) {
	now := metav1.Now()
	m.DeletionTimestamp = &now
	if len(m.Finalizers) == 0 {
		m.Finalizers = []string{testFinal}
	}
}

// cluster returns a Cluster named "c1", paused when paused is true.
func cluster(paused bool) *clusterv1.Cluster {
	c := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "c1"}}
	if paused {
		c.Spec.Paused = new(true)
	}
	return c
}

// env is one test's fake client, runner, state and recorder, plus the Deps
// built from them.
type env struct {
	c      client.WithWatch
	runner *fakeRunner
	state  *fakeState
	rec    *fakeRecorder
	d      Deps
	// plan is the cluster kind's status.plan (fakeKind.plan).
	plan infrav1.PlanPreview
}

// recorded is one event the fakeRecorder saw.
type recorded struct {
	eventType, reason, note string
	related                 runtime.Object
}

// fakeRecorder records events: their reasons in order, and in full.
type fakeRecorder struct {
	mu      sync.Mutex
	reasons []string
	events  []recorded
}

// Eventf records an event: reason in f.reasons, and eventType, reason,
// related and the note formatted from note and args in f.events.
func (f *fakeRecorder) Eventf(_, related runtime.Object, eventType, reason, _, note string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reasons = append(f.reasons, reason)
	f.events = append(f.events, recorded{eventType: eventType, reason: reason, note: fmt.Sprintf(note, args...), related: related})
}

// only returns the events of reason.
func (f *fakeRecorder) only(reason string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, e := range f.events {
		if e.reason == reason {
			out = append(out, e)
		}
	}
	return out
}

// count returns how many recorded events have reason.
func (f *fakeRecorder) count(reason string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reasons {
		if r == reason {
			n++
		}
	}
	return n
}

// newEnv returns a fresh env, built using t, holding objs.
func newEnv(t *testing.T, objs ...client.Object) *env {
	t.Helper()
	return newEnvWith(t, interceptor.Funcs{}, objs...)
}

// newEnvWith is newEnv with funcs as the fake client's interceptors (for
// injected failures), holding objs; it returns the built env, using t for
// setup and cleanup.
func newEnvWith(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *env {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&infrav1.TerraformMachine{}).WithInterceptorFuncs(funcs).Build()
	e := &env{c: c, runner: &fakeRunner{pods: map[string][]corev1.Pod{}}, state: &fakeState{}}
	e.rec = &fakeRecorder{}
	e.d = Deps{
		Client: c, APIReader: c, Scheme: s, Jobs: e.runner, State: e.state, Recorder: e.rec,
		Clock: testingclock.NewFakePassiveClock(t0), RunnerImage: "registry.example/captf:dev", DriftDefault: 30 * time.Minute,
	}
	return e
}

// get re-reads the machine from the fake client, failing t on any error
// but not-found; it returns nil when the object is gone.
func (e *env) get(t *testing.T) *infrav1.TerraformMachine {
	t.Helper()
	m := &infrav1.TerraformMachine{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: testName}, m); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil
		}
		t.Fatal(err)
	}
	return m
}

// kindFor loads the stored machine into a fakeKind, failing t on error, and
// returns it set to owner.
func (e *env) kindFor(t *testing.T, owner OwnerInfo) *fakeKind {
	t.Helper()
	return &fakeKind{obj: e.get(t), owner: owner}
}

// job returns a Job named name, of op, that finished (succeeded or failed
// per outcome) at finished.
func job(name string, op jobs.Op, outcome jobs.Outcome, finished time.Time) batchv1.Job {
	j := batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: testNS, Name: name, CreationTimestamp: metav1.NewTime(finished.Add(-time.Minute)),
		Labels: map[string]string{jobs.OpLabel: string(op)},
	}}
	switch outcome {
	case jobs.Succeeded:
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(finished)}}
	case jobs.Failed:
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(finished)}}
	}
	return j
}
