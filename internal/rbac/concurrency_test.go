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

package rbac

import (
	"context"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// TestEnsureServiceAccountGetsBeforeCreate proves an existing ServiceAccount
// costs a read and no Create, which would always answer 409 through
// admission.
func TestEnsureServiceAccountGetsBeforeCreate(t *testing.T) {
	t.Parallel()
	var creates atomic.Int32
	c := newClient(t, &interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*corev1.ServiceAccount); ok {
			creates.Add(1)
		}
		return cl.Create(ctx, obj, opts...)
	}})
	for range 3 {
		if _, reason, err := EnsureRunner(t.Context(), c, ns, nil); err != nil || reason != infrav1.RBACReadyReason {
			t.Fatalf("EnsureRunner = %q, %v", reason, err)
		}
	}
	if n := creates.Load(); n != 1 {
		t.Errorf("ServiceAccount Creates = %d, want 1", n)
	}
}

// TestEnsureBindingRetries proves the RoleBinding create that loses a race
// (already exists) and the update that conflicts are retried against a
// fresh read, not reported as failures.
func TestEnsureBindingRetries(t *testing.T) {
	t.Parallel()
	t.Run("create loses the race", func(t *testing.T) {
		t.Parallel()
		var lost atomic.Bool
		c := newClient(t, &interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if rb, ok := obj.(*rbacv1.RoleBinding); ok && lost.CompareAndSwap(false, true) {
				// Another object's reconcile created it first, with its own subject.
				other := newBinding(ns, []rbacv1.Subject{subject(ns, ServiceAccount)})
				other.Subjects = nil
				if err := cl.Create(ctx, other, opts...); err != nil {
					return err
				}
				return apierrors.NewAlreadyExists(schema.GroupResource{Group: rbacv1.GroupName, Resource: "rolebindings"}, rb.Name)
			}
			return cl.Create(ctx, obj, opts...)
		}})
		if _, reason, err := EnsureRunner(t.Context(), c, ns, nil); err != nil || reason != infrav1.RBACReadyReason {
			t.Fatalf("EnsureRunner = %q, %v", reason, err)
		}
		if got := subjectNames(binding(t, c)); len(got) != 1 || got[0] != ServiceAccount {
			t.Errorf("subjects = %v", got)
		}
	})
	t.Run("update conflicts once", func(t *testing.T) {
		t.Parallel()
		var updates atomic.Int32
		seed := newBinding(ns, nil)
		c := newClient(t, &interceptor.Funcs{Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*rbacv1.RoleBinding); ok && updates.Add(1) == 1 {
				return apierrors.NewConflict(schema.GroupResource{Group: rbacv1.GroupName, Resource: "rolebindings"}, obj.GetName(), nil)
			}
			return cl.Update(ctx, obj, opts...)
		}}, seed)
		if _, reason, err := EnsureRunner(t.Context(), c, ns, nil); err != nil || reason != infrav1.RBACReadyReason {
			t.Fatalf("EnsureRunner = %q, %v", reason, err)
		}
		if got := subjectNames(binding(t, c)); len(got) != 1 {
			t.Errorf("subjects = %v", got)
		}
		if n := updates.Load(); n != 2 {
			t.Errorf("Updates = %d, want 2", n)
		}
	})
}

// TestRemoveSubjectRetriesConflict proves a conflicting update while a
// subject is withdrawn is retried from a fresh read.
func TestRemoveSubjectRetriesConflict(t *testing.T) {
	t.Parallel()
	var updates atomic.Int32
	seed := newBinding(ns, []rbacv1.Subject{subject(ns, "a"), subject(ns, "b")})
	seed.Labels = map[string]string{state.ManagedLabel: "true"}
	c := newClient(t, &interceptor.Funcs{Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if updates.Add(1) == 1 {
			return apierrors.NewConflict(schema.GroupResource{Group: rbacv1.GroupName, Resource: "rolebindings"}, obj.GetName(), nil)
		}
		return cl.Update(ctx, obj, opts...)
	}}, seed)
	if err := removeSubject(t.Context(), c, ns, "a"); err != nil {
		t.Fatal(err)
	}
	if got := subjectNames(binding(t, c)); len(got) != 1 || got[0] != "b" {
		t.Errorf("subjects = %v", got)
	}
}
