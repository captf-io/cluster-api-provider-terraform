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
	"slices"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// runnerBinding returns the managed runner RoleBinding of namespace ns with
// ServiceAccount subjects named names.
func runnerBinding(names ...string) *rbacv1.RoleBinding {
	rb := newBinding(ns, nil)
	for _, n := range names {
		rb.Subjects = append(rb.Subjects, subject(ns, n))
	}
	return rb
}

// TestSweepKeepsServiceAccountOfLiveJob: an object switched from
// ServiceAccount A to B keeps A in the binding while a captf Job or a
// non-terminal pod still runs as A, and loses A once the Job has finished
// and its pods are terminal.
func TestSweepKeepsServiceAccountOfLiveJob(t *testing.T) {
	t.Parallel()
	managedLabels := map[string]string{state.ManagedLabel: "true"}
	// The object was switched to B; A is in the binding.
	tc := &infrav1.TerraformCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c"},
	}
	tc.Spec.Jobs = &infrav1.JobPolicy{ServiceAccountName: "b"}
	job := func(done batchv1.JobConditionType) *batchv1.Job {
		j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "j", Labels: managedLabels}}
		j.Spec.Template.Spec.ServiceAccountName = "a"
		if done != "" {
			j.Status.Conditions = []batchv1.JobCondition{{Type: done, Status: corev1.ConditionTrue}}
		}
		return j
	}
	pod := func(phase corev1.PodPhase) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p", Labels: managedLabels}}
		p.Spec.ServiceAccountName = "a"
		p.Status.Phase = phase
		return p
	}
	unlabeled := job("")
	unlabeled.Labels = nil
	for _, tt := range []struct {
		name     string
		objs     []client.Object
		wantKept bool
	}{
		{"running job", []client.Object{job("")}, true},
		{"finished job, running pod", []client.Object{job(batchv1.JobFailed), pod(corev1.PodRunning)}, true},
		{"finished job, pending pod", []client.Object{job(batchv1.JobComplete), pod(corev1.PodPending)}, true},
		{"finished job, terminal pods", []client.Object{job(batchv1.JobFailed), pod(corev1.PodFailed)}, false},
		{"finished job alone", []client.Object{job(batchv1.JobComplete)}, false},
		{"job not managed by captf", []client.Object{unlabeled}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			objs := []client.Object{tc.DeepCopy(), runnerBinding(ServiceAccount, "a", "b")}
			for _, o := range tt.objs {
				objs = append(objs, o.DeepCopyObject().(client.Object))
			}
			c := newClient(t, nil, objs...)
			if _, err := SweepNamespace(t.Context(), c, c, ns); err != nil {
				t.Fatal(err)
			}
			got := subjectNames(binding(t, c))
			want := []string{ServiceAccount, "b"}
			if tt.wantKept {
				want = []string{"a", ServiceAccount, "b"}
			}
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("subjects = %v, want %v", got, want)
			}
		})
	}
}

// TestSweepNamespaceRechecksBeforeDelete: an object created after the
// managed objects were listed keeps the namespace's RBAC and leases.
func TestSweepNamespaceRechecksBeforeDelete(t *testing.T) {
	t.Parallel()
	created := false
	c := newClient(t, &interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			err := c.List(ctx, l, opts...)
			if _, ok := l.(*coordinationv1.LeaseList); ok && err == nil && !created {
				created = true
				return c.Create(ctx, &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "late"}})
			}
			return err
		},
	}, managedIn("a")...)
	empty, err := SweepNamespace(t.Context(), c, c, "a")
	if err != nil || empty {
		t.Fatalf("SweepNamespace = %v, %v; want false, nil", empty, err)
	}
	if n := count(t, c, "a"); n != 3 {
		t.Errorf("%d objects left, want 3", n)
	}
}

// TestSweepNamespaceDeletePreconditions: every delete carries the listed UID
// and resourceVersion, and a Conflict or NotFound skips the object without
// an error.
func TestSweepNamespaceDeletePreconditions(t *testing.T) {
	t.Parallel()
	var deletes int
	c := newClient(t, &interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
			deletes++
			var do client.DeleteOptions
			do.ApplyOptions(opts)
			if p := do.Preconditions; p == nil || p.ResourceVersion == nil || *p.ResourceVersion != o.GetResourceVersion() || p.UID == nil {
				t.Errorf("delete %s has preconditions %+v", o.GetName(), p)
			}
			switch o.(type) {
			case *corev1.ServiceAccount:
				return apierrors.NewConflict(schema.GroupResource{Resource: "serviceaccounts"}, o.GetName(), errBoom)
			case *rbacv1.RoleBinding:
				return apierrors.NewNotFound(schema.GroupResource{Resource: "rolebindings"}, o.GetName())
			}
			return c.Delete(ctx, o, opts...)
		},
	}, managedIn("a")...)
	if _, err := SweepNamespace(t.Context(), c, c, "a"); err != nil {
		t.Fatalf("SweepNamespace = %v", err)
	}
	if deletes != 3 {
		t.Errorf("%d deletes, want 3", deletes)
	}
	// The ServiceAccount and RoleBinding were skipped; the Lease went.
	if n := count(t, c, "a"); n != 2 {
		t.Errorf("%d objects left, want 2", n)
	}
}
