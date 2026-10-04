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

package runlease

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// ns is the namespace of every fixture lease and object in this file.
const ns = "team-a"

var (
	// t0 is the fixed "now" most test cases build their leases and Jobs
	// around.
	t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	// errBoom is the sentinel a failing interceptor func returns.
	errBoom = errors.New("boom")
	// gr identifies the Lease resource for apierrors.NewConflict and
	// apierrors.NewAlreadyExists.
	gr = schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}
)

// newClient returns a fake controller-runtime client seeded with objs, with
// funcs intercepting its calls; t fails the test on a build error.
func newClient(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

// spec returns a fixture Spec for a run lease named name, held by holder,
// doing op, of TerraformMachine m1 in Cluster c1.
func spec(name, holder string, op jobs.Op) Spec {
	return Spec{
		Namespace: ns, Name: name, Kind: KindRun, Holder: holder, Op: op,
		OwnerKind: state.KindTerraformMachine, OwnerName: "m1", ClusterName: "c1",
	}
}

// held returns a lease of s taken at at.
func held(s Spec, at time.Time) *coordinationv1.Lease {
	l := &coordinationv1.Lease{}
	s.write(l, at)
	return l
}

// jobIn returns a fixture Job named name whose conditions give it outcome.
func jobIn(name string, outcome jobs.Outcome) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	switch outcome {
	case jobs.Succeeded:
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	case jobs.Failed:
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	}
	return j
}

// get reads the Lease name in ns through c using t's context, fails t on
// any error but not found, and returns the Lease or nil.
func get(t *testing.T, c client.Client, name string) *coordinationv1.Lease {
	t.Helper()
	l := &coordinationv1.Lease{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, l); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatal(err)
	}
	return l
}

// TestNames: both names fit a Lease name (and a label value) for the
// longest object names, are deterministic, and never collide with the
// backend's lock Lease.
func TestNames(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 253)
	suffix, err := state.Suffix(long[:63], state.KindTerraformMachine, long)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{RunName(suffix), ClusterName(long[:63], long)} {
		if len(name) > 63 {
			t.Errorf("%s is %d characters, want at most 63", name, len(name))
		}
	}
	if RunName(suffix) == state.LeaseName(suffix) || !strings.HasPrefix(RunName(suffix), "captf-run-") {
		t.Errorf("RunName = %s, lock Lease %s", RunName(suffix), state.LeaseName(suffix))
	}
	if n := ClusterName("a", "b"); !strings.HasPrefix(n, "captf-cluster-") || len(n) != 30 || n == ClusterName("b", "a") {
		t.Error("ClusterName is not a function of namespace and name")
	}
	if !Mutating(jobs.OpApply) || !Mutating(jobs.OpDestroy) || Mutating(jobs.OpRefresh) || Mutating(jobs.OpDrift) {
		t.Error("Mutating")
	}
}

// TestAcquireCreates checks that Acquire on a lease that does not exist
// yet creates it with the right labels, annotations and backstop.
func TestAcquireCreates(t *testing.T) {
	t.Parallel()
	c := newClient(t, interceptor.Funcs{})
	s := spec("captf-run-x", "job-1", jobs.OpApply)
	s.Deadline = 20 * time.Minute
	res, err := Acquire(t.Context(), c, c, t0, s)
	if err != nil || !res.Acquired || res.Previous != "" {
		t.Fatalf("Acquire = %+v, %v", res, err)
	}
	l := get(t, c, s.Name)
	want := map[string]string{
		KindLabel: KindRun, state.ManagedLabel: "true", state.OwnerKindLabel: state.KindTerraformMachine,
		state.OwnerNameLabel: "m1", clusterv1.ClusterNameLabel: "c1",
	}
	for k, v := range want {
		if l.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, l.Labels[k], v)
		}
	}
	if HolderOf(l) != "job-1" || l.Annotations[OpAnnotation] != "apply" || !AcquiredAt(l).Equal(t0) ||
		*l.Spec.LeaseDurationSeconds != int32((20*time.Minute+backstopExtra)/time.Second) {
		t.Errorf("lease = %+v", l)
	}
	if _, ok := l.Labels["clusterctl.cluster.x-k8s.io/move"]; ok {
		t.Error("a run lease must not be moved by clusterctl")
	}
}

// TestAcquireExisting: the FREE rule. A lease is taken over only when its
// holder Job finished, or is absent past the grace, or the backstop passed;
// the caller's own Job name is idempotent.
func TestAcquireExisting(t *testing.T) {
	t.Parallel()
	const name = "captf-run-x"
	other := spec(name, "job-other", jobs.OpApply)
	tests := []struct {
		name     string
		lease    *coordinationv1.Lease
		job      *batchv1.Job
		now      time.Time
		acquired bool
		previous string
	}{
		{"holder running", held(other, t0), jobIn("job-other", jobs.Running), t0.Add(time.Hour), false, ""},
		{"holder failed", held(other, t0), jobIn("job-other", jobs.Failed), t0, true, "job-other"},
		{"holder succeeded", held(other, t0), jobIn("job-other", jobs.Succeeded), t0, true, "job-other"},
		{"holder absent within the grace", held(other, t0), nil, t0.Add(Grace), false, ""},
		{"holder absent after the grace", held(other, t0), nil, t0.Add(Grace + time.Second), true, "job-other"},
		{"our own Job name", held(spec(name, "job-mine", jobs.OpApply), t0), jobIn("job-mine", jobs.Running), t0.Add(time.Hour), true, ""},
		{"backstop passed, holder still running", held(other, t0), jobIn("job-other", jobs.Running),
			t0.Add(time.Duration(jobs.DefaultActiveDeadlineSeconds)*time.Second + backstopExtra + time.Second), true, "job-other"},
		{"no holder", &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}, nil, t0, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			objs := []client.Object{tt.lease}
			if tt.job != nil {
				objs = append(objs, tt.job)
			}
			c := newClient(t, interceptor.Funcs{}, objs...)
			res, err := Acquire(t.Context(), c, c, tt.now, spec(name, "job-mine", jobs.OpDestroy))
			if err != nil {
				t.Fatal(err)
			}
			if res.Acquired != tt.acquired || res.Previous != tt.previous {
				t.Errorf("Acquire = %+v, want acquired %v from %q", res, tt.acquired, tt.previous)
			}
			l := get(t, c, name)
			switch {
			case tt.acquired && (HolderOf(l) != "job-mine" || !AcquiredAt(l).Equal(tt.now) || l.Annotations[OpAnnotation] != "destroy"):
				t.Errorf("acquired lease = %+v", l)
			case !tt.acquired && (HolderOf(l) != "job-other" || res.Holder != "job-other"):
				t.Errorf("held lease changed hands: %+v (result holder %q)", l, res.Holder)
			}
		})
	}
}

// TestAcquireRaces: a writer that loses (AlreadyExists then gone, or a
// Conflict on the takeover) does not hold the lease and does not retry.
func TestAcquireRaces(t *testing.T) {
	t.Parallel()
	const name = "captf-run-x"
	finished := func() *batchv1.Job { return jobIn("job-other", jobs.Failed) }
	t.Run("conflict on takeover", func(t *testing.T) {
		t.Parallel()
		updates := 0
		c := newClient(t, interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			updates++
			return apierrors.NewConflict(gr, name, errBoom)
		}}, held(spec(name, "job-other", jobs.OpApply), t0), finished())
		res, err := Acquire(t.Context(), c, c, t0, spec(name, "job-mine", jobs.OpApply))
		if err != nil || res.Acquired || updates != 1 {
			t.Errorf("Acquire = %+v, %v after %d updates; want lost after one", res, err, updates)
		}
	})
	t.Run("released between create and read", func(t *testing.T) {
		t.Parallel()
		c := newClient(t, interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return apierrors.NewAlreadyExists(gr, name)
		}})
		res, err := Acquire(t.Context(), c, c, t0, spec(name, "job-mine", jobs.OpApply))
		if err != nil || res.Acquired {
			t.Errorf("Acquire = %+v, %v", res, err)
		}
	})
	fail := func(verb string) interceptor.Funcs {
		f := interceptor.Funcs{}
		switch verb {
		case "create":
			f.Create = func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return errBoom }
		case "update":
			f.Update = func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error { return errBoom }
		case "get lease", "get job":
			f.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isJob := obj.(*batchv1.Job); isJob == (verb == "get job") {
					return errBoom
				}
				return c.Get(ctx, key, obj, opts...)
			}
		}
		return f
	}
	for _, verb := range []string{"create", "update", "get lease", "get job"} {
		t.Run(verb+" fails", func(t *testing.T) {
			t.Parallel()
			c := newClient(t, fail(verb), held(spec(name, "job-other", jobs.OpApply), t0), finished())
			if _, err := Acquire(t.Context(), c, c, t0, spec(name, "job-mine", jobs.OpApply)); !errors.Is(err, errBoom) {
				t.Errorf("Acquire error = %v, want boom", err)
			}
		})
	}
}

// TestAcquiredAtFallbacks checks AcquiredAt's fallback order (creation
// timestamp, then spec.acquireTime) and backstop's default duration.
func TestAcquiredAtFallbacks(t *testing.T) {
	t.Parallel()
	at := metav1.NewMicroTime(t0)
	l := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(t0.Add(-time.Hour))}}
	if !AcquiredAt(l).Equal(t0.Add(-time.Hour)) {
		t.Errorf("creation fallback = %s", AcquiredAt(l))
	}
	l.Spec.AcquireTime = &at
	if !AcquiredAt(l).Equal(t0) {
		t.Errorf("acquireTime fallback = %s", AcquiredAt(l))
	}
	if backstop(l) != time.Duration(jobs.DefaultActiveDeadlineSeconds)*time.Second+backstopExtra {
		t.Errorf("default backstop = %s", backstop(l))
	}
}

// TestLive checks Live's holder and live result for an absent lease, a
// fresh one whose Job has not appeared yet, one whose Job never appears
// past the grace, and a read failure.
func TestLive(t *testing.T) {
	t.Parallel()
	const name = "captf-cluster-x"
	c := newClient(t, interceptor.Funcs{}, held(spec(name, "job-c", jobs.OpApply), t0))
	if holder, live, err := Live(t.Context(), c, ns, "absent", t0); err != nil || live || holder != "" {
		t.Errorf("absent lease: %q %v %v", holder, live, err)
	}
	if holder, live, err := Live(t.Context(), c, ns, name, t0); err != nil || !live || holder != "job-c" {
		t.Errorf("fresh lease without its Job yet: %q %v %v, want live", holder, live, err)
	}
	if _, live, err := Live(t.Context(), c, ns, name, t0.Add(2*Grace)); err != nil || live {
		t.Errorf("lease whose Job never appeared: live %v %v", live, err)
	}
	failing := newClient(t, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return errBoom
	}})
	if _, _, err := Live(t.Context(), failing, ns, name, t0); !errors.Is(err, errBoom) {
		t.Errorf("Live error = %v", err)
	}
}

// TestLiveMachineOps: only live applies and destroys of the Cluster's
// machines count; refresh and drift, the cluster's own run lease, another
// Cluster's machines and the backend's lock Lease never do.
func TestLiveMachineOps(t *testing.T) {
	t.Parallel()
	machine := func(name, holder string, op jobs.Op) *coordinationv1.Lease {
		s := spec(name, holder, op)
		s.OwnerName = name
		return held(s, t0)
	}
	clusterRun := spec("captf-run-c", "job-c", jobs.OpApply)
	clusterRun.OwnerKind = state.KindTerraformCluster
	otherCluster := spec("captf-run-o", "job-o", jobs.OpApply)
	otherCluster.ClusterName = "c2"
	lock := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: "lock-tfstate-default-abc-m",
		Labels: state.BackendLabels(state.KindTerraformMachine, "m9", "c1"),
	}}
	holder := "m9@captf-m-m9-apply-a1-abcdef-xyz12"
	lock.Spec.HolderIdentity = &holder
	c := newClient(t, interceptor.Funcs{},
		machine("captf-run-1", "job-1", jobs.OpApply),
		machine("captf-run-2", "job-2", jobs.OpDestroy),
		machine("captf-run-3", "job-3", jobs.OpRefresh),
		machine("captf-run-4", "job-4", jobs.OpDrift),
		machine("captf-run-5", "job-5", jobs.OpApply), jobIn("job-5", jobs.Succeeded),
		held(clusterRun, t0), held(otherCluster, t0), lock,
	)
	got, err := LiveMachineOps(t.Context(), c, ns, "c1", t0)
	if err != nil || !slices.Equal(got, []string{"job-1", "job-2"}) {
		t.Errorf("LiveMachineOps = %v, %v; want job-1, job-2", got, err)
	}
	failing := newClient(t, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return errBoom
	}})
	if _, err := LiveMachineOps(t.Context(), failing, ns, "c1", t0); !errors.Is(err, errBoom) {
		t.Errorf("list error = %v", err)
	}
	jobFails := newClient(t, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return errBoom
	}}, machine("captf-run-1", "job-1", jobs.OpApply))
	if _, err := LiveMachineOps(t.Context(), jobFails, ns, "c1", t0); !errors.Is(err, errBoom) {
		t.Errorf("holder read error = %v", err)
	}
}

// TestRelease checks that Release deletes a lease its holder still holds
// with UID and resourceVersion preconditions, leaves a lease another
// holder took, and reports false rather than erroring when it is already
// gone or a conflict shows it was taken over between read and delete.
func TestRelease(t *testing.T) {
	t.Parallel()
	const name = "captf-run-x"
	t.Run("holder releases, with preconditions", func(t *testing.T) {
		t.Parallel()
		var pre *metav1.Preconditions
		c := newClient(t, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			do := &client.DeleteOptions{}
			do.ApplyOptions(opts)
			pre = do.Preconditions
			return c.Delete(ctx, obj, opts...)
		}}, held(spec(name, "job-1", jobs.OpApply), t0))
		ok, err := Release(t.Context(), c, c, ns, name, "job-1")
		if err != nil || !ok || get(t, c, name) != nil {
			t.Fatalf("Release = %v, %v", ok, err)
		}
		if pre == nil || pre.ResourceVersion == nil || *pre.ResourceVersion == "" || pre.UID == nil {
			t.Errorf("preconditions = %+v, want UID and resourceVersion", pre)
		}
	})
	t.Run("another holder keeps it", func(t *testing.T) {
		t.Parallel()
		c := newClient(t, interceptor.Funcs{}, held(spec(name, "job-2", jobs.OpApply), t0))
		if ok, err := Release(t.Context(), c, c, ns, name, "job-1"); err != nil || ok || get(t, c, name) == nil {
			t.Errorf("Release = %v, %v; want the lease kept", ok, err)
		}
	})
	t.Run("gone", func(t *testing.T) {
		t.Parallel()
		c := newClient(t, interceptor.Funcs{})
		if ok, err := Release(t.Context(), c, c, ns, name, "job-1"); err != nil || ok {
			t.Errorf("Release = %v, %v", ok, err)
		}
	})
	t.Run("taken over between read and delete", func(t *testing.T) {
		t.Parallel()
		c := newClient(t, interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return apierrors.NewConflict(gr, name, errBoom)
		}}, held(spec(name, "job-1", jobs.OpApply), t0))
		if ok, err := Release(t.Context(), c, c, ns, name, "job-1"); err != nil || ok {
			t.Errorf("Release = %v, %v", ok, err)
		}
	})
	for _, verb := range []string{"get", "delete"} {
		t.Run(verb+" fails", func(t *testing.T) {
			t.Parallel()
			f := interceptor.Funcs{}
			if verb == "get" {
				f.Get = func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errBoom
				}
			} else {
				f.Delete = func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return errBoom }
			}
			c := newClient(t, f, held(spec(name, "job-1", jobs.OpApply), t0))
			if _, err := Release(t.Context(), c, c, ns, name, "job-1"); !errors.Is(err, errBoom) {
				t.Errorf("Release error = %v", err)
			}
		})
	}
}

// TestDeleteOwned: the object's run and cluster leases go; the backend's
// lock Lease, which carries the same owner labels, and other objects'
// leases stay.
func TestDeleteOwned(t *testing.T) {
	t.Parallel()
	run := spec("captf-run-c", "job-1", jobs.OpApply)
	run.OwnerKind, run.OwnerName = state.KindTerraformCluster, "tc"
	cl := run
	cl.Name, cl.Kind = ClusterName(ns, "c1"), KindCluster
	otherRun := spec("captf-run-m", "job-2", jobs.OpApply)
	lock := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: "lock-tfstate-default-abc-c", Labels: state.BackendLabels(state.KindTerraformCluster, "tc", "c1"),
	}}
	c := newClient(t, interceptor.Funcs{}, held(run, t0), held(cl, t0), held(otherRun, t0), lock)
	if err := DeleteOwned(t.Context(), c, c, ns, state.KindTerraformCluster, "tc"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{run.Name: false, cl.Name: false, otherRun.Name: true, lock.Name: true} {
		if got := get(t, c, name) != nil; got != want {
			t.Errorf("lease %s exists = %v, want %v", name, got, want)
		}
	}
	for _, f := range []interceptor.Funcs{
		{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return errBoom }},
		{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return errBoom }},
	} {
		c := newClient(t, f, held(run, t0))
		if err := DeleteOwned(t.Context(), c, c, ns, state.KindTerraformCluster, "tc"); !errors.Is(err, errBoom) {
			t.Errorf("DeleteOwned error = %v", err)
		}
	}
}
