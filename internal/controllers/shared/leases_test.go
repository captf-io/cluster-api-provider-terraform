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
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	cbmetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/testutil"
	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
	"github.com/captf-io/cluster-api-provider-terraform/internal/plankey"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runlease"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// clientRunner is a jobs.Runner over the fake client, as the real one is
// over the API server: Jobs are visible to Free's uncached reads, a second
// create of one name is AlreadyExists, and a stale runner (a cache that has
// not seen them) lists extra instead of what the client holds.
type clientRunner struct {
	c client.Client
	// stale, when set, is what List returns instead of the client's Jobs.
	stale     []batchv1.Job
	createErr error
	created   atomic.Int32
	// now, when set, is the API server's clock: Create stamps
	// creationTimestamp with it, as the API server does and the fake
	// client does not.
	now func() time.Time
}

// apiNow returns the time of e's clock, the one its API server stamps
// objects with.
func (e *env) apiNow() time.Time {
	return e.d.Clock.Now()
}

// Create stores job in r's client using ctx, stamped with r.now when set,
// or returns r.createErr when set. Without the stamp a just-created Job
// reads as created in year 1, older than StuckJobAge, so a concurrent pass
// could delete it as stuck before its per-run Secret exists.
func (r *clientRunner) Create(ctx context.Context, _ client.Object, job *batchv1.Job) error {
	if r.createErr != nil {
		return r.createErr
	}
	if r.now != nil {
		job.CreationTimestamp = metav1.NewTime(r.now())
	}
	if err := r.c.Create(ctx, job); err != nil {
		return err
	}
	r.created.Add(1)
	return nil
}

// List returns r.stale when set, otherwise the client's Jobs of kind owned
// by owner, using ctx.
func (r *clientRunner) List(ctx context.Context, owner client.Object, kind string) ([]batchv1.Job, error) {
	if r.stale != nil {
		out := make([]batchv1.Job, 0, len(r.stale))
		for i := range r.stale {
			out = append(out, *r.stale[i].DeepCopy())
		}
		return out, nil
	}
	list := &batchv1.JobList{}
	err := r.c.List(ctx, list, client.InNamespace(owner.GetNamespace()), client.MatchingLabels{
		state.OwnerKindLabel: kind, state.OwnerNameLabel: state.LabelValue(owner.GetName()),
	})
	return list.Items, err
}

// Delete removes job from r's client using ctx and returns any error other
// than not-found.
func (r *clientRunner) Delete(ctx context.Context, job *batchv1.Job) error {
	return client.IgnoreNotFound(r.c.Delete(ctx, job))
}

// Pods always returns nil, nil: clientRunner tests never read pods.
func (r *clientRunner) Pods(context.Context, *batchv1.Job) ([]corev1.Pod, error) { return nil, nil }

// named returns a machine-mutator that sets m's Name to name and derives a
// UID from it.
func named(name string) func(*infrav1.TerraformMachine) {
	return func(m *infrav1.TerraformMachine) { m.Name, m.UID = name, "uid-"+m.UID }
}

// leaseEnv returns, failing t on error, an env holding a namespace with the
// objects of cluster c1 (TerraformMachines m1 and m2 and a TerraformMachine
// "tc" that the adapter reports as the TerraformCluster, plus extra), Jobs
// backed by a clientRunner over the fake client, and ClusterOperationGate
// set to gate; funcs are the fake client's interceptors. It also returns
// that clientRunner.
func leaseEnv(t *testing.T, gate bool, funcs interceptor.Funcs, extra ...client.Object) (*env, *clientRunner) {
	t.Helper()
	objs := world(
		machine(withFinalizer, notPaused),
		machine(withFinalizer, notPaused, named("m2")),
		machine(withFinalizer, notPaused, named("tc")),
	)
	e := newEnvWith(t, funcs, append(objs, extra...)...)
	jr := &clientRunner{c: e.c, now: e.apiNow}
	e.d.Jobs = jr
	e.d.ClusterOperationGate = gate
	return e, jr
}

// kindNamed loads the stored object named name into a fakeKind and returns
// it, failing t on error; "tc" is reported as the TerraformCluster.
func (e *env) kindNamed(t *testing.T, name string) *fakeKind {
	t.Helper()
	m := &infrav1.TerraformMachine{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, m); err != nil {
		t.Fatal(err)
	}
	return &fakeKind{obj: m, owner: readyOwner, in: machineIn(), asCluster: name == "tc"}
}

// reconcileNamed reconciles name with d, failing t on error, and returns
// its requeue and the object afterwards.
func (e *env) reconcileNamed(t *testing.T, d Deps, name string) (time.Duration, *infrav1.TerraformMachine) {
	t.Helper()
	k := e.kindNamed(t, name)
	res, err := Reconcile(t.Context(), d, k)
	if err != nil {
		t.Fatalf("Reconcile %s: %v", name, err)
	}
	return res.RequeueAfter, e.kindNamed(t, name).obj
}

// startLeased starts the Job for req on k's object as a reconcile pass
// does: it takes the leases first (acquireLeases), then calls StartJob,
// then gives the run lease back as if the Job finished, so a later start
// of another Job of the object goes ahead. It fails t on error or a lease
// wait and returns the started Job.
func (e *env) startLeased(t *testing.T, k Kind, req JobRequest) *batchv1.Job {
	t.Helper()
	name := JobName(k, req)
	if w, err := acquireLeases(t.Context(), e.d, k, req, name); err != nil || w.reason != "" {
		t.Fatalf("take the leases for %s = %+v, %v", name, w, err)
	}
	created, err := StartJob(t.Context(), e.d, k, req)
	if err != nil {
		t.Fatalf("StartJob %s: %v", name, err)
	}
	if _, err := runlease.Release(t.Context(), e.c, e.c, k.Object().GetNamespace(), runlease.RunName(req.Suffix), created.Name); err != nil {
		t.Fatalf("release the run lease of %s: %v", created.Name, err)
	}
	return created
}

// jobsOf returns every Job in the fake client's namespace, failing t on
// error.
func (e *env) jobsOf(t *testing.T) []batchv1.Job {
	t.Helper()
	list := &batchv1.JobList{}
	if err := e.c.List(t.Context(), list, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// lease returns the Lease named name, failing t on any error but
// not-found; it returns nil when the Lease is gone.
func (e *env) lease(t *testing.T, name string) *coordinationv1.Lease {
	t.Helper()
	l := &coordinationv1.Lease{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, l); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil
		}
		t.Fatal(err)
	}
	return l
}

// finishJob gives the Job name a terminal condition of outcome, failing t
// on error.
func (e *env) finishJob(t *testing.T, name string, outcome jobs.Outcome) {
	t.Helper()
	j := &batchv1.Job{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, j); err != nil {
		t.Fatal(err)
	}
	typ := batchv1.JobComplete
	if outcome == jobs.Failed {
		typ = batchv1.JobFailed
	}
	j.Status.Conditions = []batchv1.JobCondition{{Type: typ, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(t0)}}
	if err := e.c.Status().Update(t.Context(), j); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(j), j); err != nil || jobs.OutcomeOf(j) != outcome {
		t.Fatalf("Job %s did not finish: %v", name, err)
	}
}

// runLeaseOf returns the run lease name of the object of kind and name,
// failing t on error.
func runLeaseOf(t *testing.T, kind, name string) string {
	t.Helper()
	suffix, err := state.Suffix(testNS, kind, name)
	if err != nil {
		t.Fatal(err)
	}
	return runlease.RunName(suffix)
}

// foreignLease returns a Lease named name, held by holder for op since at.
func foreignLease(name, holder string, op jobs.Op, at time.Time) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name, Annotations: map[string]string{
			runlease.OpAnnotation: string(op), runlease.AcquiredAnnotation: at.Format(time.RFC3339Nano),
		}},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
}

// jobObj returns a pointer to an apply Job named name that finished at t0
// with outcome.
func jobObj(name string, outcome jobs.Outcome) *batchv1.Job {
	j := job(name, jobs.OpApply, outcome, t0)
	return &j
}

// applyReason returns m's ApplyJobSucceeded condition's reason, or "" when
// the condition is unset.
func applyReason(m *infrav1.TerraformMachine) string {
	if c := conditions.Get(m, infrav1.ApplyJobSucceededCondition); c != nil {
		return c.Reason
	}
	return ""
}

// TestRunLeaseNamesTheJob: the run lease is taken before the Job and names
// it; StartJob creates exactly the Job JobName predicts.
func TestRunLeaseNamesTheJob(t *testing.T) {
	t.Parallel()
	e, _ := leaseEnv(t, true, interceptor.Funcs{})
	requeue, _ := e.reconcileNamed(t, e.d, testName)
	created := e.jobsOf(t)
	if requeue != ActiveJobRequeue || len(created) != 1 {
		t.Fatalf("requeue %s, Jobs %d", requeue, len(created))
	}
	l := e.lease(t, runLeaseOf(t, state.KindTerraformMachine, testName))
	if l == nil || runlease.HolderOf(l) != created[0].Name || l.Annotations[runlease.OpAnnotation] != "apply" ||
		l.Labels[runlease.KindLabel] != runlease.KindRun || l.Labels[state.ManagedLabel] != "true" {
		t.Errorf("run lease = %+v, Job %s", l, created[0].Name)
	}
}

// TestRunLeaseTakeover: a lease held by another Job is taken over only when
// that Job finished, or never appeared within the grace; otherwise no Job
// starts, the apply waits (once: one event, one count) at GateRequeue, and
// the wait costs no backoff: the Job starts as soon as the holder is done.
func TestRunLeaseTakeover(t *testing.T) {
	t.Parallel()
	const other = "captf-m-m1-apply-a7-000000"
	tests := []struct {
		name  string
		job   *batchv1.Job
		age   time.Duration
		takes bool
	}{
		{"holder failed", jobObj(other, jobs.Failed), time.Second, true},
		{"holder succeeded", jobObj(other, jobs.Succeeded), time.Second, true},
		{"holder absent past the grace", nil, runlease.Grace + time.Second, true},
		{"holder running", jobObj(other, jobs.Running), time.Hour, false},
		{"holder absent within the grace", nil, runlease.Grace / 2, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			name := runLeaseOf(t, state.KindTerraformMachine, testName)
			extra := []client.Object{foreignLease(name, other, jobs.OpApply, t0.Add(-tt.age))}
			if tt.job != nil {
				extra = append(extra, tt.job)
			}
			e, _ := leaseEnv(t, true, interceptor.Funcs{}, extra...)
			reg := cbmetrics.NewKubeRegistry()
			e.d.Metrics = metrics.New()
			if err := e.d.Metrics.Register(reg); err != nil {
				t.Fatal(err)
			}
			requeue, m := e.reconcileNamed(t, e.d, testName)
			var mine []string
			for _, j := range e.jobsOf(t) {
				if j.Name != other {
					mine = append(mine, j.Name)
				}
			}
			if tt.takes {
				if len(mine) != 1 || runlease.HolderOf(e.lease(t, name)) != mine[0] || requeue != ActiveJobRequeue {
					t.Errorf("Jobs %v, lease holder %s, requeue %s; want a takeover", mine, runlease.HolderOf(e.lease(t, name)), requeue)
				}
				return
			}
			if len(mine) != 0 || requeue != GateRequeue || applyReason(m) != infrav1.WaitingForRunLeaseReason ||
				runlease.HolderOf(e.lease(t, name)) != other {
				t.Fatalf("Jobs %v, requeue %s, ApplyJobSucceeded %s; want a wait", mine, requeue, applyReason(m))
			}
			if c := conditions.Get(m, infrav1.ApplyJobSucceededCondition); c.Status != metav1.ConditionUnknown {
				t.Errorf("waiting ApplyJobSucceeded is %s, want Unknown", c.Status)
			}
			// A second waiting reconcile emits and counts nothing more.
			e.reconcileNamed(t, e.d, testName)
			if n := e.rec.count(EventWaitingForRunLease); n != 1 {
				t.Errorf("%d WaitingForRunLease events, want 1", n)
			}
			var help string
			for _, s := range metrics.Specs() {
				if s.Name == metrics.LeaseWaitsName {
					help = s.Help
				}
			}
			want := "# HELP captf_lease_waits_total [ALPHA] " + help + "\n# TYPE captf_lease_waits_total counter\n" +
				`captf_lease_waits_total{kind="TerraformMachine",reason="run_lease"} 1` + "\n"
			if err := testutil.GatherAndCompare(reg, strings.NewReader(want), metrics.LeaseWaitsName); err != nil {
				t.Error(err)
			}
			// The holder finishes: the apply starts now, not after a backoff.
			if tt.job == nil {
				e.d.Clock = testingclock.NewFakePassiveClock(t0.Add(runlease.Grace))
			} else {
				e.finishJob(t, other, jobs.Failed)
			}
			requeue, m = e.reconcileNamed(t, e.d, testName)
			if requeue != ActiveJobRequeue || m.Status.ActiveJob.Name == "" || m.Status.ActiveJob.Attempt != 1 {
				t.Errorf("after the holder finished: requeue %s, activeJob %+v", requeue, m.Status.ActiveJob)
			}
			if n := e.rec.count(EventJobSucceeded) + e.rec.count(EventJobFailed); n != 0 {
				t.Errorf("leaving the wait emitted %d Job outcome events for no finished Job", n)
			}
		})
	}
}

// TestRunLeaseRace: two managers reconcile one object at once, the second
// from a stale cache that makes it pick another attempt (another Job name).
// Exactly one Job is created; the loser requeues without one.
func TestRunLeaseRace(t *testing.T) {
	t.Parallel()
	staleFailed := job("captf-m-m1-apply-a1-ffffff", jobs.OpApply, jobs.Failed, t0.Add(-time.Hour))
	staleFailed.Labels[jobs.AttemptLabel] = "1"
	t.Run("sequential", func(t *testing.T) {
		t.Parallel()
		e, _ := leaseEnv(t, false, interceptor.Funcs{})
		b := e.d
		b.Jobs = &clientRunner{c: e.c, stale: []batchv1.Job{staleFailed}, now: e.apiNow}
		if requeue, _ := e.reconcileNamed(t, e.d, testName); requeue != ActiveJobRequeue {
			t.Fatalf("first manager: requeue %s", requeue)
		}
		// Its object cache is as stale as its Job cache: status.activeJob
		// does not name the first manager's Job yet.
		k := e.kindNamed(t, testName)
		k.obj.Status.ActiveJob = infrav1.ActiveJob{}
		res, err := Reconcile(t.Context(), b, k)
		if err != nil {
			t.Fatal(err)
		}
		requeue, m := res.RequeueAfter, e.kindNamed(t, testName).obj
		// The live run lease names a Job the caches do not show: the pass
		// waits for them and starts nothing.
		if n := len(e.jobsOf(t)); n != 1 || requeue != LagRequeue {
			t.Errorf("second manager: %d Jobs, requeue %s, reason %s", n, requeue, applyReason(m))
		}
	})
	t.Run("between the create and the per-run Secret", func(t *testing.T) {
		t.Parallel()
		// The stale manager creates its Job; the other manager reconciles
		// before the stale one creates the Job's per-run Secret. The Job is
		// moments old, so it is not stuck: both report it running.
		var (
			e     *env
			once  sync.Once
			inner time.Duration
			err   error
		)
		e, _ = leaseEnv(t, false, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if s, ok := obj.(*corev1.Secret); ok && strings.HasPrefix(s.Name, inputs.RunName("")) {
					once.Do(func() {
						var res ctrl.Result
						res, err = Reconcile(ctx, e.d, e.kindNamed(t, testName))
						inner = res.RequeueAfter
					})
				}
				return c.Create(ctx, obj, opts...)
			},
		})
		b := e.d
		b.Jobs = &clientRunner{c: e.c, stale: []batchv1.Job{staleFailed}, now: e.apiNow}
		res, outerErr := Reconcile(t.Context(), b, e.kindNamed(t, testName))
		if err != nil || outerErr != nil {
			t.Fatalf("Reconcile errors: %v, %v", err, outerErr)
		}
		if n := len(e.jobsOf(t)); n != 1 || inner != ActiveJobRequeue || res.RequeueAfter != ActiveJobRequeue {
			t.Errorf("%d Jobs, requeues %s (fresh cache) and %s (stale); want 1 Job, both %s", n, inner, res.RequeueAfter, ActiveJobRequeue)
		}
	})
	t.Run("concurrent", func(t *testing.T) {
		t.Parallel()
		for range 5 {
			e, _ := leaseEnv(t, false, interceptor.Funcs{})
			b := e.d
			b.Jobs = &clientRunner{c: e.c, stale: []batchv1.Job{staleFailed}, now: e.apiNow}
			ka, kb := e.kindNamed(t, testName), e.kindNamed(t, testName)
			var wg sync.WaitGroup
			results := make([]time.Duration, 2)
			errs := make([]error, 2)
			for i, run := range []struct {
				d Deps
				k *fakeKind
			}{{e.d, ka}, {b, kb}} {
				wg.Go(func() {
					res, err := Reconcile(t.Context(), run.d, run.k)
					results[i], errs[i] = res.RequeueAfter, err
				})
			}
			wg.Wait()
			if n := len(e.jobsOf(t)); n != 1 {
				t.Fatalf("%d Jobs after a race, want 1 (results %v, errors %v)", n, results, errs)
			}
			for i := range results {
				if errs[i] == nil && results[i] != ActiveJobRequeue && results[i] != GateRequeue && results[i] != LagRequeue {
					t.Errorf("manager %d requeued after %s", i, results[i])
				}
			}
		}
	})
}

// TestRunLeaseCreateFailed: a pass that took the leases and whose Job create
// was rejected gives them back, so the next pass, whatever Job name it
// picks, starts at once; a Job that exists (an AlreadyExists adoption
// counts) or may exist (an ambiguous create error) keeps its leases.
func TestRunLeaseCreateFailed(t *testing.T) {
	t.Parallel()
	runName := runLeaseOf(t, state.KindTerraformMachine, testName)
	t.Run("create fails", func(t *testing.T) {
		t.Parallel()
		e, jr := leaseEnv(t, true, interceptor.Funcs{})
		jr.createErr = apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, "rejected", errors.New("quota exceeded"))
		if _, err := Reconcile(t.Context(), e.d, e.kindNamed(t, testName)); err == nil {
			t.Fatal("the Job create failed but Reconcile succeeded")
		}
		if l := e.lease(t, runName); l != nil {
			t.Fatalf("the run lease of a Job never created is held by %s", runlease.HolderOf(l))
		}
		jr.createErr = nil
		e.d.Clock = testingclock.NewFakePassiveClock(t0.Add(10 * time.Second))
		requeue, _ := e.reconcileNamed(t, e.d, testName)
		created := e.jobsOf(t)
		if requeue != ActiveJobRequeue || len(created) != 1 || runlease.HolderOf(e.lease(t, runName)) != created[0].Name {
			t.Errorf("requeue %s, Jobs %v, lease %+v", requeue, created, e.lease(t, runName))
		}
	})
	t.Run("another name takes it at once", func(t *testing.T) {
		t.Parallel()
		e, jr := leaseEnv(t, true, interceptor.Funcs{})
		jr.createErr = apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, "rejected", errors.New("quota exceeded"))
		if _, err := Reconcile(t.Context(), e.d, e.kindNamed(t, testName)); err == nil {
			t.Fatal("the Job create failed but Reconcile succeeded")
		}
		res, err := runlease.Acquire(t.Context(), e.c, e.c, t0, runlease.Spec{
			Namespace: testNS, Name: runName, Kind: runlease.KindRun, Holder: "another-name", Op: jobs.OpApply,
			OwnerKind: state.KindTerraformMachine, OwnerName: testName, ClusterName: "c1",
		})
		if err != nil || !res.Acquired || res.Previous != "" {
			t.Errorf("Acquire after the failed pass = %+v, %v; want a fresh acquire", res, err)
		}
	})
	t.Run("an existing Job keeps the leases", func(t *testing.T) {
		t.Parallel()
		e, jr := leaseEnv(t, true, interceptor.Funcs{})
		jr.createErr = apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"}, "adopted")
		if _, err := Reconcile(t.Context(), e.d, e.kindNamed(t, testName)); err == nil {
			t.Fatal("the adopted Job is missing but Reconcile succeeded")
		}
		if e.lease(t, runName) == nil {
			t.Error("the run lease of an existing Job was released")
		}
	})
	t.Run("an ambiguous create error keeps the leases", func(t *testing.T) {
		t.Parallel()
		e, jr := leaseEnv(t, true, interceptor.Funcs{})
		jr.createErr = errors.New("connection reset")
		if _, err := Reconcile(t.Context(), e.d, e.kindNamed(t, testName)); err == nil {
			t.Fatal("the Job create failed but Reconcile succeeded")
		}
		if e.lease(t, runName) == nil {
			t.Error("the run lease was released although the create may have gone through")
		}
	})
}

// TestRunLeaseCreateForbiddenWithLaggingCache: a pass whose Job cache has
// not seen the Job an earlier pass created rebuilds its name and calls Create
// again; a quota answers Forbidden before AlreadyExists, and the leases of
// the running Job stay. With no such Job they are released.
func TestRunLeaseCreateForbiddenWithLaggingCache(t *testing.T) {
	t.Parallel()
	quota := apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, "rejected", errors.New("quota exceeded"))
	runName := runLeaseOf(t, state.KindTerraformMachine, testName)
	t.Run("the Job exists", func(t *testing.T) {
		t.Parallel()
		e, jr := leaseEnv(t, true, interceptor.Funcs{})
		k := e.kindNamed(t, testName)
		suffix, err := state.Suffix(testNS, state.KindTerraformMachine, testName)
		if err != nil {
			t.Fatal(err)
		}
		req := JobRequest{
			Op: jobs.OpApply, Files: renderMachine(t), InputsHash: "h1:x", Attempt: 1, Suffix: suffix, ClusterName: "c1",
			Source: infrav1.Source{Image: "registry.example/mod:1.0"}, Identity: testIdentity,
		}
		// An earlier pass took the leases and created the Job; this pass
		// rebuilds its name from a cache that has not shown it.
		holder := JobName(k, req)
		existing := jobObj(holder, jobs.Running)
		if err := e.c.Create(t.Context(), existing); err != nil {
			t.Fatal(err)
		}
		if res, err := runlease.Acquire(t.Context(), e.c, e.c, t0, runlease.Spec{
			Namespace: testNS, Name: runName, Kind: runlease.KindRun, Holder: holder, Op: jobs.OpApply,
			OwnerKind: state.KindTerraformMachine, OwnerName: testName, ClusterName: "c1",
		}); err != nil || !res.Acquired {
			t.Fatalf("Acquire = %+v, %v", res, err)
		}
		jr.createErr = quota
		if _, err := StartJob(t.Context(), e.d, k, req); err == nil {
			t.Fatal("the Job create was forbidden but StartJob succeeded")
		}
		if l := e.lease(t, runName); l == nil || runlease.HolderOf(l) != holder {
			t.Errorf("run lease %+v after the forbidden create, want it held by the running Job %s", l, holder)
		}
	})
	t.Run("no Job exists", func(t *testing.T) {
		t.Parallel()
		e, jr := leaseEnv(t, true, interceptor.Funcs{})
		jr.createErr = quota
		if _, err := Reconcile(t.Context(), e.d, e.kindNamed(t, testName)); err == nil {
			t.Fatal("the Job create was forbidden but Reconcile succeeded")
		}
		if l := e.lease(t, runName); l != nil {
			t.Errorf("the run lease of a Job that does not exist is held by %s", runlease.HolderOf(l))
		}
	})
}

// TestClusterGateErrorReleasesBothLeases: a TerraformCluster whose list of
// machine operations fails gives back both the run lease and the cluster
// write lease.
func TestClusterGateErrorReleasesBothLeases(t *testing.T) {
	t.Parallel()
	listErr := errors.New("list leases failed")
	funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*coordinationv1.LeaseList); ok {
			return listErr
		}
		return c.List(ctx, list, opts...)
	}}
	runName := runLeaseOf(t, state.KindTerraformMachine, "tc")
	clusterLease := runlease.ClusterName(testNS, "c1")
	e, _ := leaseEnv(t, true, funcs)
	if _, err := Reconcile(t.Context(), e.d, e.kindNamed(t, "tc")); !errors.Is(err, listErr) {
		t.Fatalf("Reconcile error = %v, want the list error", err)
	}
	if e.lease(t, runName) != nil || e.lease(t, clusterLease) != nil {
		t.Errorf("leases kept after the gate failed: run %v, cluster %v", e.lease(t, runName), e.lease(t, clusterLease))
	}
}

// TestRunLeaseReleasedOnce: the lease goes when bookkeeping counts its
// finished Job, once; a later reconcile, with that Job bookkept, never
// deletes the lease the next Job took.
func TestRunLeaseReleasedOnce(t *testing.T) {
	t.Parallel()
	var deletes atomic.Int32
	e, _ := leaseEnv(t, true, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*coordinationv1.Lease); ok {
			deletes.Add(1)
		}
		return c.Delete(ctx, obj, opts...)
	}})
	name := runLeaseOf(t, state.KindTerraformMachine, testName)
	e.reconcileNamed(t, e.d, testName)
	first := e.jobsOf(t)[0].Name
	e.finishJob(t, first, jobs.Failed)
	e.d.Clock = testingclock.NewFakePassiveClock(t0.Add(10 * time.Minute)) // past the retry backoff
	e.reconcileNamed(t, e.d, testName)
	if n := deletes.Load(); n != 1 {
		t.Fatalf("%d lease deletes after the Job was counted, want 1", n)
	}
	// The retry took a fresh lease.
	var second string
	for _, j := range e.jobsOf(t) {
		if j.Name != first {
			second = j.Name
		}
	}
	if second == "" || runlease.HolderOf(e.lease(t, name)) != second {
		t.Fatalf("retry %q, lease %+v", second, e.lease(t, name))
	}
	if j := &(batchv1.Job{}); e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: first}, j) != nil || j.Annotations[BookkeptAnnotation] != "true" {
		t.Fatalf("Job %s is not bookkept", first)
	}
	e.reconcileNamed(t, e.d, testName)
	if n := deletes.Load(); n != 1 || runlease.HolderOf(e.lease(t, name)) != second {
		t.Errorf("a reconcile with the Job bookkept deleted leases (%d deletes), holder %s", n, runlease.HolderOf(e.lease(t, name)))
	}
}

// TestCleanupDeletesLeases: Cleanup removes the object's run and cluster
// leases, and nobody else's.
func TestCleanupDeletesLeases(t *testing.T) {
	t.Parallel()
	own := runLeaseOf(t, state.KindTerraformCluster, "tc")
	other := runLeaseOf(t, state.KindTerraformMachine, "m2")
	e, _ := leaseEnv(t, true, interceptor.Funcs{})
	for _, s := range []runlease.Spec{
		{Namespace: testNS, Name: own, Kind: runlease.KindRun, Holder: "j1", Op: jobs.OpDestroy, OwnerKind: state.KindTerraformCluster, OwnerName: "tc", ClusterName: "c1"},
		{Namespace: testNS, Name: runlease.ClusterName(testNS, "c1"), Kind: runlease.KindCluster, Holder: "j1", Op: jobs.OpDestroy, OwnerKind: state.KindTerraformCluster, OwnerName: "tc", ClusterName: "c1"},
		{Namespace: testNS, Name: other, Kind: runlease.KindRun, Holder: "j2", Op: jobs.OpApply, OwnerKind: state.KindTerraformMachine, OwnerName: "m2", ClusterName: "c1"},
	} {
		if _, err := runlease.Acquire(t.Context(), e.c, e.c, t0, s); err != nil {
			t.Fatal(err)
		}
	}
	k := e.kindNamed(t, "tc")
	if err := Cleanup(t.Context(), e.d, k, "unused", ""); err != nil {
		t.Fatal(err)
	}
	if e.lease(t, own) != nil || e.lease(t, runlease.ClusterName(testNS, "c1")) != nil || e.lease(t, other) == nil {
		t.Errorf("after Cleanup: own %v, cluster %v, other machine's %v",
			e.lease(t, own) != nil, e.lease(t, runlease.ClusterName(testNS, "c1")) != nil, e.lease(t, other) != nil)
	}
}

// TestCleanupDeletesPlanKey: Cleanup removes the object's plan key Secret
// and no other object's, and a missing key is not an error.
func TestCleanupDeletesPlanKey(t *testing.T) {
	t.Parallel()
	e, _ := leaseEnv(t, false, interceptor.Funcs{})
	own := e.kindNamed(t, testName)
	other := e.kindNamed(t, "m2")
	for _, k := range []*fakeKind{own, other} {
		if _, err := plankey.Ensure(t.Context(), e.c, k.obj); err != nil {
			t.Fatal(err)
		}
	}
	keyOf := func(name string) error {
		return e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: plankey.Name("m", name)}, &corev1.Secret{})
	}
	for range 2 {
		if err := Cleanup(t.Context(), e.d, own, "unused", ""); err != nil {
			t.Fatal(err)
		}
		if !apierrors.IsNotFound(keyOf(testName)) || keyOf("m2") != nil {
			t.Fatalf("after Cleanup: own key err %v, other machine's key err %v", keyOf(testName), keyOf("m2"))
		}
	}
}

// TestClusterGateMachineFirst: a machine apply runs; the cluster's apply
// waits for it, holding the cluster write lease, which keeps a second
// machine from starting; once the machine's Job finished, the cluster's
// starts.
func TestClusterGateMachineFirst(t *testing.T) {
	t.Parallel()
	e, _ := leaseEnv(t, true, interceptor.Funcs{})
	if requeue, _ := e.reconcileNamed(t, e.d, testName); requeue != ActiveJobRequeue {
		t.Fatalf("m1: requeue %s", requeue)
	}
	machineJob := e.jobsOf(t)[0].Name

	requeue, tc := e.reconcileNamed(t, e.d, "tc")
	if requeue != GateRequeue || applyReason(tc) != infrav1.WaitingForMachineOperationsReason || len(e.jobsOf(t)) != 1 {
		t.Fatalf("cluster: requeue %s, reason %s, %d Jobs", requeue, applyReason(tc), len(e.jobsOf(t)))
	}
	clusterLease := e.lease(t, runlease.ClusterName(testNS, "c1"))
	if clusterLease == nil || e.lease(t, runLeaseOf(t, state.KindTerraformCluster, "tc")) == nil {
		t.Fatal("the waiting cluster gave up its leases")
	}
	if n := e.rec.count(EventWaitingForMachineOperations); n != 1 {
		t.Errorf("%d WaitingForMachineOperations events", n)
	}

	requeue, m2 := e.reconcileNamed(t, e.d, "m2")
	if requeue != GateRequeue || applyReason(m2) != infrav1.WaitingForClusterOperationReason || len(e.jobsOf(t)) != 1 {
		t.Errorf("m2: requeue %s, reason %s, %d Jobs; want it blocked by the waiting cluster", requeue, applyReason(m2), len(e.jobsOf(t)))
	}
	if e.lease(t, runLeaseOf(t, state.KindTerraformMachine, "m2")) != nil {
		t.Error("m2 kept its run lease while it waits for the cluster")
	}

	e.finishJob(t, machineJob, jobs.Succeeded)
	requeue, tc = e.reconcileNamed(t, e.d, "tc")
	if requeue != ActiveJobRequeue || tc.Status.ActiveJob.Name == "" ||
		runlease.HolderOf(e.lease(t, runlease.ClusterName(testNS, "c1"))) != tc.Status.ActiveJob.Name {
		t.Errorf("cluster after the machine finished: requeue %s, activeJob %+v", requeue, tc.Status.ActiveJob)
	}
}

// TestClusterGateClusterFirst: a machine that finds the cluster's apply
// running gives its run lease back and waits; its refresh and drift are
// not gated; with --cluster-operation-gate=false nothing is.
func TestClusterGateClusterFirst(t *testing.T) {
	t.Parallel()
	start := func(t *testing.T, gate bool) *env {
		t.Helper()
		e, _ := leaseEnv(t, gate, interceptor.Funcs{})
		if requeue, _ := e.reconcileNamed(t, e.d, "tc"); requeue != ActiveJobRequeue {
			t.Fatalf("cluster: requeue %s", requeue)
		}
		return e
	}
	t.Run("apply waits", func(t *testing.T) {
		t.Parallel()
		e := start(t, true)
		requeue, m := e.reconcileNamed(t, e.d, testName)
		if requeue != GateRequeue || applyReason(m) != infrav1.WaitingForClusterOperationReason || len(e.jobsOf(t)) != 1 {
			t.Errorf("m1: requeue %s, reason %s, %d Jobs", requeue, applyReason(m), len(e.jobsOf(t)))
		}
		if e.lease(t, runLeaseOf(t, state.KindTerraformMachine, testName)) != nil {
			t.Error("the waiting machine kept its run lease, blocking the cluster")
		}
		if n := e.rec.count(EventWaitingForClusterOperation); n != 1 {
			t.Errorf("%d WaitingForClusterOperation events", n)
		}
	})
	t.Run("gate off", func(t *testing.T) {
		t.Parallel()
		e := start(t, false)
		if e.lease(t, runlease.ClusterName(testNS, "c1")) != nil {
			t.Error("the cluster took the write lease with the gate off")
		}
		// Even a write lease left behind by a gated manager is ignored.
		if _, err := runlease.Acquire(t.Context(), e.c, e.c, t0, runlease.Spec{
			Namespace: testNS, Name: runlease.ClusterName(testNS, "c1"), Kind: runlease.KindCluster, Holder: e.jobsOf(t)[0].Name,
			Op: jobs.OpApply, OwnerKind: state.KindTerraformCluster, OwnerName: "tc", ClusterName: "c1",
		}); err != nil {
			t.Fatal(err)
		}
		if requeue, _ := e.reconcileNamed(t, e.d, testName); requeue != ActiveJobRequeue || len(e.jobsOf(t)) != 2 {
			t.Errorf("m1 with the gate off: requeue %s, %d Jobs", requeue, len(e.jobsOf(t)))
		}
	})
	t.Run("drift is not gated", func(t *testing.T) {
		t.Parallel()
		e := start(t, true)
		m := e.kindNamed(t, testName).obj
		m.Status.Initialization.Provisioned = new(true)
		if err := e.c.Status().Update(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		if err := inputs.Write(t.Context(), e.c, m, renderMachine(t), inputs.Meta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
			t.Fatal(err)
		}
		e.state.st = &state.State{InputsHash: "h1:x"}
		requeue, _ := e.reconcileNamed(t, e.d, testName)
		var op jobs.Op
		for _, j := range e.jobsOf(t) {
			if j.Labels[state.OwnerNameLabel] == testName {
				op = jobs.OpOf(&j)
			}
		}
		if requeue != ActiveJobRequeue || (op != jobs.OpDrift && op != jobs.OpRefresh) {
			t.Errorf("m1 check during the cluster apply: requeue %s, op %q", requeue, op)
		}
	})
}

// TestDriftWaitsForRunLease: a drift or refresh whose run lease another
// live Job holds waits on DriftJobSucceeded, not ApplyJobSucceeded.
func TestDriftWaitsForRunLease(t *testing.T) {
	t.Parallel()
	const other = "captf-m-m1-drift-a9-000000"
	name := runLeaseOf(t, state.KindTerraformMachine, testName)
	e, _ := leaseEnv(t, true, interceptor.Funcs{}, foreignLease(name, other, jobs.OpDrift, t0), jobObj(other, jobs.Running))
	m := e.kindNamed(t, testName).obj
	m.Status.Initialization.Provisioned = new(true)
	if err := e.c.Status().Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if err := inputs.Write(t.Context(), e.c, m, renderMachine(t), inputs.Meta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
		t.Fatal(err)
	}
	e.state.st = &state.State{InputsHash: "h1:x"}
	requeue, m := e.reconcileNamed(t, e.d, testName)
	c := conditions.Get(m, infrav1.DriftJobSucceededCondition)
	if requeue != GateRequeue || c == nil || c.Reason != infrav1.WaitingForRunLeaseReason || c.Status != metav1.ConditionUnknown {
		t.Errorf("requeue %s, DriftJobSucceeded %+v", requeue, c)
	}
	if applyReason(m) == infrav1.WaitingForRunLeaseReason {
		t.Error("a waiting drift set ApplyJobSucceeded")
	}
}
