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

package sweep

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// newClient builds a fake client seeded with objs, scheme-aware for the core,
// RBAC, coordination and infrav1 types the sweep touches, failing t if the
// scheme cannot be built. funcs, if non-nil, intercepts client calls (for
// example to inject a failure); it returns the built client.
func newClient(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) client.WithWatch {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := infrav1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return b.Build()
}

// orphans returns the three managed objects (a ServiceAccount, a
// RoleBinding and a Lease) a sweep of namespace ns should delete when no
// TerraformCluster remains there.
func orphans(ns string) []client.Object {
	meta := metav1.ObjectMeta{Namespace: ns, Name: "captf-runner", Labels: map[string]string{state.ManagedLabel: "true"}}
	return []client.Object{
		&corev1.ServiceAccount{ObjectMeta: meta},
		&rbacv1.RoleBinding{ObjectMeta: meta},
		&coordinationv1.Lease{ObjectMeta: meta},
	}
}

// remaining counts how many of orphans(ns) still exist in c, using t to
// fail the test on any error other than not-found; it returns that count.
func remaining(t *testing.T, c client.Client, ns string) int {
	t.Helper()
	n := 0
	for _, o := range orphans(ns) {
		err := c.Get(t.Context(), client.ObjectKeyFromObject(o), o)
		switch {
		case err == nil:
			n++
		case !apierrors.IsNotFound(err):
			t.Fatal(err)
		}
	}
	return n
}

// start runs r.Start in a goroutine until t ends, canceling it on cleanup.
// It returns the cancel func and a channel that receives Start's return
// value.
func start(t *testing.T, r *Runnable) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	t.Cleanup(cancel)
	return cancel, done
}

// waitFor polls f until it returns true or 5 seconds pass, failing t with a
// message naming what if the deadline is reached first.
func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestStartSweepsOrphansOnly proves Start deletes the managed orphans in a
// namespace with no TerraformCluster while leaving those in a namespace
// that still has one.
func TestStartSweepsOrphansOnly(t *testing.T) {
	t.Parallel()
	objs := append(orphans("moved"), orphans("live")...)
	objs = append(objs, &infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "live", Name: "tc"}})
	c := newClient(t, nil, objs...)
	cancel, done := start(t, &Runnable{Reader: c, Client: c, Interval: time.Hour})
	// The first sweep runs at start, not after the first interval.
	waitFor(t, "the orphans to go", func() bool { return remaining(t, c, "moved") == 0 })
	if n := remaining(t, c, "live"); n != 3 {
		t.Errorf("%d of 3 objects left in a namespace with a TerraformCluster", n)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start = %v", err)
	}
}

// TestStartScopedToNamespace proves a Runnable configured with a Namespace
// sweeps orphans only in that namespace, leaving another namespace's
// orphans untouched.
func TestStartScopedToNamespace(t *testing.T) {
	t.Parallel()
	c := newClient(t, nil, append(orphans("mine"), orphans("other")...)...)
	start(t, &Runnable{Reader: c, Client: c, Interval: time.Hour, Namespace: "mine"})
	waitFor(t, "the scoped sweep", func() bool { return remaining(t, c, "mine") == 0 })
	if n := remaining(t, c, "other"); n != 3 {
		t.Errorf("a namespace-scoped sweep deleted %d objects elsewhere", 3-n)
	}
}

// TestStartSurvivesErrors: a failed sweep is retried at the next tick and
// never ends Start, so the manager keeps running.
func TestStartSurvivesErrors(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := newClient(t, &interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			calls.Add(1)
			return errors.New("forbidden")
		},
	})
	cancel, done := start(t, &Runnable{Reader: c, Client: c, Interval: time.Millisecond})
	waitFor(t, "a retry", func() bool { return calls.Load() >= 3 })
	select {
	case err := <-done:
		t.Fatalf("Start returned early: %v", err)
	default:
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start = %v", err)
	}
}

// TestNeedLeaderElection proves NeedLeaderElection reports true.
func TestNeedLeaderElection(t *testing.T) {
	t.Parallel()
	if !(&Runnable{}).NeedLeaderElection() {
		t.Error("the sweep must run on the leader only")
	}
}
