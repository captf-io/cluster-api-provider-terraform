//go:build envtest

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

package envtest

import (
	"context"
	"slices"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/terraformmachine"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// otherFinalizer is the finalizer of another controller.
const otherFinalizer = "example.com/other"

// reconciler returns a TerraformMachine reconciler on te's API server whose
// client is wrapped by funcs, so a test can act between the reconcile's
// read and its write. The Jobs, state and Secret reads all go to the API
// server; nothing is faked but the event recorder. Test t fails when the
// client cannot be built.
func reconciler(t *testing.T, te *testEnv, funcs interceptor.Funcs) *terraformmachine.Reconciler {
	t.Helper()
	wc, err := client.NewWithWatch(te.cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	c := interceptor.NewClient(wc, funcs)
	return &terraformmachine.Reconciler{Deps: shared.Deps{
		Client:    c,
		APIReader: te.client,
		Scheme:    scheme,
		Recorder:  events.NewFakeRecorder(1000),
		Jobs:      jobs.NewRunner(c, te.client),
		State:     state.NewReader(c),
		Clock:     clock.RealClock{},
	}}
}

// getMachine returns the TerraformMachine key from te's API server. Test t
// fails when it cannot be read.
func getMachine(t *testing.T, te *testEnv, key client.ObjectKey) *infrav1.TerraformMachine {
	t.Helper()
	m := &infrav1.TerraformMachine{}
	if err := te.client.Get(context.Background(), key, m); err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return m
}

// TestDropFinalizerConflict proves the finalizer release of an externally
// managed, deleting object is conditional on the version it was read at:
// when the object changes between the read and the write the API server
// answers Conflict, the reconcile requeues after a second instead of
// failing, and nothing is dropped (not even by a patch that would have
// resurrected a finalizer another controller had just released). The next
// pass, from a fresh read, drops only its own finalizer.
func TestDropFinalizerConflict(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		// concurrent changes the stored object after the reconcile's read.
		concurrent func(m *infrav1.TerraformMachine)
		// kept is the finalizers the stored object holds after the conflict
		// and after the next pass.
		kept, final []string
	}{
		{
			name:       "another controller releases its finalizer",
			concurrent: func(m *infrav1.TerraformMachine) { controllerutil.RemoveFinalizer(m, otherFinalizer) },
			kept:       []string{terraformmachine.Finalizer},
			final:      nil,
		},
		{
			name:       "someone annotates the object",
			concurrent: func(m *infrav1.TerraformMachine) { m.Annotations["example.com/touched"] = "yes" },
			kept:       []string{terraformmachine.Finalizer, otherFinalizer},
			final:      []string{otherFinalizer},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			ns := newNamespace(t, hooked.client)
			m := machine(ns, "external")
			m.Annotations = map[string]string{clusterv1.ManagedByAnnotation: ""}
			m.Finalizers = []string{terraformmachine.Finalizer, otherFinalizer}
			if err := hooked.client.Create(ctx, m); err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := hooked.client.Delete(ctx, m); err != nil {
				t.Fatalf("delete: %v", err)
			}
			key := client.ObjectKeyFromObject(m)

			patched := false
			r := reconciler(t, hooked, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
					if !patched {
						patched = true
						cur := getMachine(t, hooked, key)
						before := cur.DeepCopy()
						tt.concurrent(cur)
						if err := hooked.client.Patch(ctx, cur, client.MergeFrom(before)); err != nil {
							t.Errorf("the concurrent patch: %v", err)
						}
					}
					return c.Patch(ctx, obj, p, opts...)
				},
			})
			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			if err != nil {
				t.Fatalf("reconcile returned %v, want a requeue", err)
			}
			if res.RequeueAfter != time.Second {
				t.Errorf("RequeueAfter = %v, want 1s", res.RequeueAfter)
			}
			if got := getMachine(t, hooked, key).Finalizers; !slices.Equal(got, tt.kept) {
				t.Errorf("finalizers after the conflict = %v, want %v", got, tt.kept)
			}

			if _, err := reconciler(t, hooked, interceptor.Funcs{}).Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("second reconcile: %v", err)
			}
			got := &infrav1.TerraformMachine{}
			err = hooked.client.Get(ctx, key, got)
			switch {
			case len(tt.final) == 0 && !apierrors.IsNotFound(err):
				t.Errorf("after the next pass: %v, finalizers %v, want the object gone", err, got.Finalizers)
			case len(tt.final) > 0 && (err != nil || !slices.Equal(got.Finalizers, tt.final)):
				t.Errorf("after the next pass: %v, finalizers %v, want %v", err, got.Finalizers, tt.final)
			}
		})
	}
}

// TestStatusPatchMergesDisjointWrites proves how the reconcile's status
// write behaves when the object changed after its read: the Cluster API
// patch helper sends a merge patch of the fields this reconcile changed,
// not a conditional write, so a concurrent metadata change is neither
// lost nor reported as a Conflict and both are persisted.
func TestStatusPatchMergesDisjointWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, hooked.client)
	m := machine(ns, "gate")
	if err := hooked.client.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	key := client.ObjectKeyFromObject(m)

	patched := false
	r := reconciler(t, hooked, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
			if !patched {
				patched = true
				cur := getMachine(t, hooked, key)
				before := cur.DeepCopy()
				cur.Labels = map[string]string{"example.com/concurrent": "yes"}
				if err := hooked.client.Patch(ctx, cur, client.MergeFrom(before)); err != nil {
					t.Errorf("the concurrent patch: %v", err)
				}
			}
			return c.SubResource(sub).Patch(ctx, obj, p, opts...)
		},
	})
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != shared.GateRequeue {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, shared.GateRequeue)
	}
	if !patched {
		t.Fatal("the reconcile wrote no status")
	}
	got := getMachine(t, hooked, key)
	if got.Labels["example.com/concurrent"] != "yes" {
		t.Errorf("labels = %v, want the concurrent label kept", got.Labels)
	}
	waitingForOwner(t, got)
}

// waitingForOwner fails t unless m carries DependenciesReady=Unknown with
// the WaitingForOwner reason.
func waitingForOwner(t *testing.T, m *infrav1.TerraformMachine) {
	t.Helper()
	for _, c := range m.Status.Conditions {
		if c.Type == infrav1.DependenciesReadyCondition {
			if c.Status != metav1.ConditionUnknown || c.Reason != infrav1.WaitingForOwnerReason {
				t.Errorf("DependenciesReady = %s/%s, want Unknown/%s", c.Status, c.Reason, infrav1.WaitingForOwnerReason)
			}
			return
		}
	}
	t.Errorf("conditions = %v, want DependenciesReady", m.Status.Conditions)
}

// TestPlanApprovalConflict proves two approvals of one TerraformPlan read
// at the same version do not both win: the second Update answers 409
// Conflict even though its content is acceptable to the webhook, and
// retrying from a fresh read finds the approval already there.
func TestPlanApprovalConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, hooked.client)
	alice := hooked.userClient(t, "alice")
	if err := alice.Create(ctx, plan(ns, "contended", nil, "")); err != nil {
		t.Fatalf("create: %v", err)
	}
	key := types.NamespacedName{Namespace: ns, Name: "contended"}
	first, second := &infrav1.TerraformPlan{}, &infrav1.TerraformPlan{}
	for _, p := range []*infrav1.TerraformPlan{first, second} {
		if err := alice.Get(ctx, key, p); err != nil {
			t.Fatalf("get: %v", err)
		}
		p.Spec.Approved, p.Spec.ApprovedBy = ptr.To(true), "alice"
	}
	if err := alice.Update(ctx, first); err != nil {
		t.Fatalf("first approval: %v", err)
	}
	err := alice.Update(ctx, second)
	if !apierrors.IsConflict(err) {
		t.Fatalf("second approval at the stale version: %v, want Conflict", err)
	}
	cur := &infrav1.TerraformPlan{}
	if err := alice.Get(ctx, key, cur); err != nil {
		t.Fatalf("get: %v", err)
	}
	if cur.Spec.Approved == nil || !*cur.Spec.Approved || cur.Spec.ApprovedBy != "alice" {
		t.Errorf("spec = %+v, want the first approval kept", cur.Spec)
	}
}

// TestReconcileWritesThroughTheAPIServer drives the TerraformMachine
// reconcile by hand against the API server with the webhooks installed:
// with no owner it records DependenciesReady and requeues; once a Machine
// owns it, it adds its finalizer; and a delete of an object that never
// applied (deletionPolicy Retain) lets it go. The Job, state and Secret reads are real; no
// controller or kubelet runs, so there is no Job to observe.
func TestReconcileWritesThroughTheAPIServer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, hooked.client)
	m := machine(ns, "flow")
	// Without a deletionPolicy, and with no TerraformCluster to inherit one
	// from, a delete waits (DeletionPolicyUnresolved).
	m.Spec.DeletionPolicy = infrav1.DeletionPolicyRetain
	if err := hooked.client.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	key := client.ObjectKeyFromObject(m)
	req := ctrl.Request{NamespacedName: key}
	r := reconciler(t, hooked, interceptor.Funcs{})

	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile without an owner: %v", err)
	}
	if res.RequeueAfter != shared.GateRequeue {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, shared.GateRequeue)
	}
	waitingForOwner(t, getMachine(t, hooked, key))

	owner := capiMachine(ns, "owner", m.Name)
	if err := hooked.client.Create(ctx, owner); err != nil {
		t.Fatalf("create the Machine: %v", err)
	}
	cur := getMachine(t, hooked, key)
	before := cur.DeepCopy()
	cur.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: owner.Name, UID: owner.UID,
	}}
	if err := hooked.client.Patch(ctx, cur, client.MergeFrom(before)); err != nil {
		t.Fatalf("set the ownerRef: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile with an owner: %v", err)
	}
	if got := getMachine(t, hooked, key); !slices.Contains(got.Finalizers, terraformmachine.Finalizer) {
		t.Fatalf("finalizers = %v, want %s", got.Finalizers, terraformmachine.Finalizer)
	}

	// The next pass runs the whole flow past the preamble; whatever stops it
	// (no Cluster here) is recorded as a condition, never an error.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile past the preamble: %v", err)
	}
	got := getMachine(t, hooked, key)
	if len(got.Status.Conditions) == 0 {
		t.Errorf("no conditions after the reconcile")
	}

	// Deleting the Machine, then the TerraformMachine: with Retain and
	// nothing applied, the next pass releases the finalizer.
	if err := hooked.client.Delete(ctx, owner); err != nil {
		t.Fatalf("delete the Machine: %v", err)
	}
	if err := hooked.client.Delete(ctx, got); err != nil {
		t.Fatalf("delete: %v", err)
	}
	eventuallyGone(t, func() (bool, error) {
		if _, err := r.Reconcile(ctx, req); err != nil {
			return false, err
		}
		err := hooked.client.Get(ctx, key, &infrav1.TerraformMachine{})
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	})
}

// eventuallyGone polls done until it reports true or fails t after
// eventually; done returns an error to fail the test at once.
func eventuallyGone(t *testing.T, done func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(eventually)
	for {
		ok, err := done()
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("not done after %v", eventually)
		}
		time.Sleep(tick)
	}
}
