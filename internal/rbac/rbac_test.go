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
	"errors"
	"slices"
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
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runlease"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// ns is the tenant namespace every test in this package uses.
const ns = "team-a"

// errBoom is the sentinel error interceptor funcs return to inject a
// failure.
var errBoom = errors.New("boom")

// newClient builds a fake client seeded with objs, its scheme covering
// client-go and CAPTF's own types, with funcs as its interceptor when
// non-nil; test t fails on a scheme registration error. It returns the
// built client.
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

// binding reads back the runner RoleBinding of namespace ns through c,
// failing test t if it cannot be read. It returns the RoleBinding.
func binding(t *testing.T, c client.Client) *rbacv1.RoleBinding {
	t.Helper()
	rb := &rbacv1.RoleBinding{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: RoleBinding}, rb); err != nil {
		t.Fatalf("get rolebinding: %v", err)
	}
	return rb
}

// subjectNames returns rb's subjects' names, sorted.
func subjectNames(rb *rbacv1.RoleBinding) []string {
	var out []string
	for _, s := range rb.Subjects {
		out = append(out, s.Name)
	}
	slices.Sort(out)
	return out
}

// assertNoRoles fails test t if c holds any Role: this package never
// creates one.
func assertNoRoles(t *testing.T, c client.Client) {
	t.Helper()
	roles := &rbacv1.RoleList{}
	if err := c.List(t.Context(), roles); err != nil {
		t.Fatal(err)
	}
	if len(roles.Items) != 0 {
		t.Errorf("Roles created: %v", roles.Items)
	}
}

// sa returns a ServiceAccount in namespace ns, named name, carrying labels.
func sa(name string, labels map[string]string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}}
}

// TestEnsureRunnerDefault proves EnsureRunner, called with no override,
// creates and reuses the managed captf-runner ServiceAccount and a
// RoleBinding of it to ClusterRole captf-runner, and creates no Role.
func TestEnsureRunnerDefault(t *testing.T) {
	t.Parallel()
	c := newClient(t, nil)
	for range 2 {
		name, reason, err := EnsureRunner(t.Context(), c, ns, nil)
		if err != nil || name != ServiceAccount || reason != infrav1.RBACReadyReason {
			t.Fatalf("EnsureRunner = %q, %q, %v", name, reason, err)
		}
	}
	got := &corev1.ServiceAccount{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: ServiceAccount}, got); err != nil {
		t.Fatal(err)
	}
	if got.Labels[state.ManagedLabel] != "true" {
		t.Errorf("SA labels = %v", got.Labels)
	}
	rb := binding(t, c)
	want := rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "captf-runner"}
	if rb.RoleRef != want || rb.Labels[state.ManagedLabel] != "true" {
		t.Errorf("rolebinding = %+v %v", rb.RoleRef, rb.Labels)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0] != (rbacv1.Subject{Kind: "ServiceAccount", Namespace: ns, Name: ServiceAccount}) {
		t.Errorf("subjects = %+v", rb.Subjects)
	}
	assertNoRoles(t, c)
}

// TestEnsureRunnerUsesAdminServiceAccount proves EnsureRunner uses an
// admin-created captf-runner ServiceAccount as it is, without relabeling
// it.
func TestEnsureRunnerUsesAdminServiceAccount(t *testing.T) {
	t.Parallel()
	c := newClient(t, nil, sa(ServiceAccount, nil))
	if _, _, err := EnsureRunner(t.Context(), c, ns, nil); err != nil {
		t.Fatal(err)
	}
	got := &corev1.ServiceAccount{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: ServiceAccount}, got); err != nil {
		t.Fatal(err)
	}
	if len(got.Labels) != 0 {
		t.Errorf("admin SA relabeled: %v", got.Labels)
	}
}

// TestEnsureRunnerOverride proves EnsureRunner binds an opted-in override
// ServiceAccount, unions it with the default runner in the same binding,
// treats naming captf-runner explicitly as the default path, and reports
// ServiceAccountNotOptedIn for one without the opt-in label, one with the
// wrong label value, or one that does not exist.
func TestEnsureRunnerOverride(t *testing.T) {
	t.Parallel()
	optedIn := sa("deployer", map[string]string{RunnerLabel: "true"})
	c := newClient(t, nil, optedIn, sa("plain", nil), sa("wrong", map[string]string{RunnerLabel: "yes"}))

	name, reason, err := EnsureRunner(t.Context(), c, ns, new("deployer"))
	if err != nil || name != "deployer" || reason != infrav1.RBACReadyReason {
		t.Fatalf("opted in: %q, %q, %v", name, reason, err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: ServiceAccount}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Errorf("captf-runner SA created with an override: %v", err)
	}

	// The default runner joins the same binding: subjects are a union.
	if _, _, err := EnsureRunner(t.Context(), c, ns, new("")); err != nil {
		t.Fatal(err)
	}
	if got := subjectNames(binding(t, c)); !slices.Equal(got, []string{ServiceAccount, "deployer"}) {
		t.Errorf("subjects = %v", got)
	}

	// Naming captf-runner explicitly is the default path: no opt-in label
	// needed, and the default subject stays bound.
	name, reason, err = EnsureRunner(t.Context(), c, ns, new(ServiceAccount))
	if err != nil || name != ServiceAccount || reason != infrav1.RBACReadyReason {
		t.Errorf("explicit default: %q, %q, %v", name, reason, err)
	}
	if got := subjectNames(binding(t, c)); !slices.Equal(got, []string{ServiceAccount, "deployer"}) {
		t.Errorf("subjects after explicit default = %v", got)
	}

	for _, name := range []string{"plain", "wrong", "missing"} {
		got, reason, err := EnsureRunner(t.Context(), c, ns, new(name))
		if err != nil || got != "" || reason != infrav1.ServiceAccountNotOptedInReason {
			t.Errorf("%s: %q, %q, %v; want ServiceAccountNotOptedIn", name, got, reason, err)
		}
	}
	assertNoRoles(t, c)
}

// TestEnsureRunnerWithdrawnConsent: removing captf.io/runner=true removes the
// ServiceAccount from the binding, and the last subject takes the
// binding with it.
func TestEnsureRunnerWithdrawnConsent(t *testing.T) {
	t.Parallel()
	c := newClient(t, nil, sa("deployer", map[string]string{RunnerLabel: "true"}), sa("ops", map[string]string{RunnerLabel: "true"}))
	for _, n := range []string{"deployer", "ops"} {
		if _, _, err := EnsureRunner(t.Context(), c, ns, new(n)); err != nil {
			t.Fatal(err)
		}
	}
	withdraw := func(name string) {
		t.Helper()
		s := &corev1.ServiceAccount{}
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, s); err != nil {
			t.Fatal(err)
		}
		s.Labels = nil
		if err := c.Update(t.Context(), s); err != nil {
			t.Fatal(err)
		}
		if _, reason, err := EnsureRunner(t.Context(), c, ns, new(name)); err != nil || reason != infrav1.ServiceAccountNotOptedInReason {
			t.Fatalf("withdrawn %s: %q, %v", name, reason, err)
		}
	}
	withdraw("deployer")
	if got := subjectNames(binding(t, c)); !slices.Equal(got, []string{"ops"}) {
		t.Errorf("subjects = %v", got)
	}
	withdraw("ops")
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: RoleBinding}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("empty binding kept: %v", err)
	}
	// No binding at all: still just NotOptedIn.
	if _, reason, err := EnsureRunner(t.Context(), c, ns, new("ops")); err != nil || reason != infrav1.ServiceAccountNotOptedInReason {
		t.Errorf("no binding: %q, %v", reason, err)
	}
}

// TestEnsureRunnerBindingConflicts proves EnsureRunner leaves an unmanaged
// binding alone with ErrBindingConflict, re-creates a managed binding whose
// roleRef changed, and never carries a forged or foreign subject into the
// runner ClusterRole's binding.
func TestEnsureRunnerBindingConflicts(t *testing.T) {
	t.Parallel()
	t.Run("unmanaged binding is left alone", func(t *testing.T) {
		t.Parallel()
		user := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: RoleBinding},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "admin"},
		}
		c := newClient(t, nil, user)
		_, reason, err := EnsureRunner(t.Context(), c, ns, nil)
		if !errors.Is(err, ErrBindingConflict) || reason != infrav1.RBACFailedReason {
			t.Errorf("EnsureRunner = %q, %v; want RBACFailed, ErrBindingConflict", reason, err)
		}
		if binding(t, c).RoleRef.Name != "admin" {
			t.Error("user binding modified")
		}
		// Withdrawal never touches it either.
		if err := removeSubject(t.Context(), c, ns, "x"); err != nil {
			t.Error(err)
		}
	})
	t.Run("managed binding with another roleRef is re-created", func(t *testing.T) {
		t.Parallel()
		old := newBinding(ns, []rbacv1.Subject{subject(ns, "deployer")})
		old.RoleRef.Name = "old-role"
		c := newClient(t, nil, old, sa("deployer", map[string]string{RunnerLabel: "true"}))
		if _, _, err := EnsureRunner(t.Context(), c, ns, nil); err != nil {
			t.Fatal(err)
		}
		rb := binding(t, c)
		if rb.RoleRef != roleRef() || !slices.Equal(subjectNames(rb), []string{ServiceAccount, "deployer"}) {
			t.Errorf("re-created binding = %+v %v", rb.RoleRef, subjectNames(rb))
		}
	})
	// A tenant who may create RoleBindings pre-creates captf-runner with the
	// managed label, a harmless roleRef and their own subjects. The manager
	// must not carry those subjects into a binding of the runner ClusterRole.
	t.Run("forged managed binding does not escalate", func(t *testing.T) {
		t.Parallel()
		forged := newBinding(ns, []rbacv1.Subject{
			subject(ns, "attacker"),
			subject("other-ns", ServiceAccount),
			{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "mallory"},
			{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "system:authenticated"},
		})
		forged.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "harmless"}
		c := newClient(t, nil, forged, sa("attacker", nil))
		if _, _, err := EnsureRunner(t.Context(), c, ns, nil); err != nil {
			t.Fatal(err)
		}
		rb := binding(t, c)
		if rb.RoleRef != roleRef() || !slices.Equal(subjectNames(rb), []string{ServiceAccount}) || rb.Subjects[0].Namespace != ns {
			t.Errorf("binding = %+v %+v, want only %s/%s", rb.RoleRef, rb.Subjects, ns, ServiceAccount)
		}
	})
	// The same filter applies when the roleRef is already right: a subject
	// added out of band is dropped on the next reconcile.
	t.Run("foreign subject on a correct binding is dropped", func(t *testing.T) {
		t.Parallel()
		rb := newBinding(ns, []rbacv1.Subject{subject(ns, ServiceAccount), subject(ns, "attacker")})
		c := newClient(t, nil, rb, sa("attacker", nil))
		if _, _, err := EnsureRunner(t.Context(), c, ns, nil); err != nil {
			t.Fatal(err)
		}
		if got := subjectNames(binding(t, c)); !slices.Equal(got, []string{ServiceAccount}) {
			t.Errorf("subjects = %v, want only %s", got, ServiceAccount)
		}
	})
}

// TestEnsureRunnerErrors proves EnsureRunner reports RBACFailed and the
// underlying error whenever a get, create, update or delete it depends on
// fails, for every step of both the override and default paths.
func TestEnsureRunnerErrors(t *testing.T) {
	t.Parallel()
	failOn := func(kind string, verb string) *interceptor.Funcs {
		fail := func(o client.Object) bool {
			switch o.(type) {
			case *corev1.ServiceAccount:
				return kind == "sa"
			case *rbacv1.RoleBinding:
				return kind == "rb"
			}
			return false
		}
		return &interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				if verb == "get" && fail(o) {
					return errBoom
				}
				return c.Get(ctx, k, o, opts...)
			},
			Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
				if verb == "create" && fail(o) {
					return errBoom
				}
				return c.Create(ctx, o, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
				if verb == "update" && fail(o) {
					return errBoom
				}
				return c.Update(ctx, o, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
				if verb == "delete" && fail(o) {
					return errBoom
				}
				return c.Delete(ctx, o, opts...)
			},
		}
	}
	optedIn := sa("deployer", map[string]string{RunnerLabel: "true"})
	otherSubject := newBinding(ns, []rbacv1.Subject{subject(ns, "other")})
	staleRef := newBinding(ns, nil)
	staleRef.RoleRef.Name = "old"
	tests := []struct {
		name     string
		kind     string
		verb     string
		override *string
		objs     []client.Object
	}{
		{"override get", "sa", "get", new("deployer"), nil},
		{"override binding get", "rb", "get", new("deployer"), []client.Object{optedIn}},
		{"withdrawal get", "rb", "get", new("plain"), nil},
		{"withdrawal update", "rb", "update", new("plain"), []client.Object{newBinding(ns, []rbacv1.Subject{subject(ns, "plain"), subject(ns, "other")})}},
		{"withdrawal delete", "rb", "delete", new("plain"), []client.Object{newBinding(ns, []rbacv1.Subject{subject(ns, "plain")})}},
		{"sa create", "sa", "create", nil, nil},
		{"binding create", "rb", "create", nil, nil},
		{"binding update", "rb", "update", nil, []client.Object{otherSubject}},
		{"binding delete on roleRef change", "rb", "delete", nil, []client.Object{staleRef}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newClient(t, failOn(tt.kind, tt.verb), tt.objs...)
			_, reason, err := EnsureRunner(t.Context(), c, ns, tt.override)
			if !errors.Is(err, errBoom) || reason != infrav1.RBACFailedReason {
				t.Errorf("EnsureRunner = %q, %v; want RBACFailed, boom", reason, err)
			}
		})
	}
}

// managedIn returns the managed ServiceAccount, RoleBinding and Lease a
// sweep should recognize in namespace, all carrying captf.io/managed=true.
func managedIn(namespace string) []client.Object {
	l := map[string]string{state.ManagedLabel: "true"}
	return []client.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: ServiceAccount, Labels: l}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: RoleBinding, Labels: l}, RoleRef: roleRef()},
		&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "lock-tfstate-default-abc", Labels: l}},
	}
}

// count lists the ServiceAccounts, RoleBindings and Leases of namespace
// through c, failing test t on a list error. It returns their total count.
func count(t *testing.T, c client.Client, namespace string) int {
	t.Helper()
	n := 0
	sas, rbs, leases := &corev1.ServiceAccountList{}, &rbacv1.RoleBindingList{}, &coordinationv1.LeaseList{}
	for _, l := range []client.ObjectList{sas, rbs, leases} {
		if err := c.List(t.Context(), l, client.InNamespace(namespace)); err != nil {
			t.Fatal(err)
		}
	}
	n += len(sas.Items) + len(rbs.Items) + len(leases.Items)
	return n
}

// sweepFixture returns objects across namespaces moved, machines, clusters
// and pools: managed runner objects in each, an unmanaged Lease and
// ServiceAccount that must survive any sweep, a TerraformMachine of
// another manager instance, a TerraformCluster, a TerraformMachinePool,
// and a template alone that does not keep its namespace. It returns that
// fixed object list.
func sweepFixture() []client.Object {
	var objs []client.Object
	for _, n := range []string{"moved", "machines", "clusters", "pools"} {
		objs = append(objs, managedIn(n)...)
	}
	objs = append(objs,
		// Not ours: the leader-election Lease and an admin SA survive.
		&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "moved", Name: "leader"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "moved", Name: "deployer"}},
		// Objects of another manager instance (another watch-filter value)
		// still keep their namespace.
		&infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: "machines", Name: "m", Labels: map[string]string{"cluster.x-k8s.io/watch-filter": "other"}}},
		&infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "clusters", Name: "c"}},
		&infrav1.TerraformMachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: "pools", Name: "p"}},
		// A template alone does not.
		&infrav1.TerraformMachineTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: "moved", Name: "t"}},
	)
	return objs
}

// TestSweepPrunesSubjects: in a namespace that still holds objects, the
// runner binding keeps captf-runner and the ServiceAccounts objects run as
// (own jobs.serviceAccountName, else the cluster's defaults), and loses the
// rest: switching from A to B withdraws A's Secret permissions.
func TestSweepPrunesSubjects(t *testing.T) {
	t.Parallel()
	labels := func(cluster string) map[string]string {
		if cluster == "" {
			return nil
		}
		return map[string]string{"cluster.x-k8s.io/cluster-name": cluster}
	}
	cluster := func(name, cl, own, def string) *infrav1.TerraformCluster {
		tc := &infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels(cl)}}
		if own != "" {
			tc.Spec.Jobs = &infrav1.JobPolicy{ServiceAccountName: own}
		}
		if def != "" {
			tc.Spec.Defaults = &infrav1.TerraformClusterDefaults{Jobs: &infrav1.JobPolicy{ServiceAccountName: def}}
		}
		return tc
	}
	machine := func(name, cl, own string) *infrav1.TerraformMachine {
		m := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels(cl)}}
		if own != "" {
			m.Spec.Jobs = &infrav1.JobPolicy{ServiceAccountName: own}
		}
		return m
	}
	pool := func(name, cl, own string) *infrav1.TerraformMachinePool {
		p := &infrav1.TerraformMachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels(cl)}}
		if own != "" {
			p.Spec.Jobs = &infrav1.JobPolicy{ServiceAccountName: own}
		}
		return p
	}
	all := []string{ServiceAccount, "old", "cluster-sa", "machine-sa", "pool-sa", "defaults-sa", "other-defaults"}
	tests := []struct {
		name string
		objs []client.Object
		want []string // remaining subject names, sorted; nil = binding deleted
	}{
		{name: "only the default runner is used", objs: []client.Object{cluster("tc", "c1", "", "")},
			want: []string{ServiceAccount}},
		{name: "cluster and machine own names", objs: []client.Object{cluster("tc", "c1", "cluster-sa", ""), machine("m", "c1", "machine-sa")},
			want: []string{ServiceAccount, "cluster-sa", "machine-sa"}},
		{name: "machine inherits the cluster defaults", objs: []client.Object{cluster("tc", "c1", "", "defaults-sa"), machine("m", "c1", "")},
			want: []string{ServiceAccount, "defaults-sa"}},
		{name: "defaults unused when every machine sets its own", objs: []client.Object{cluster("tc", "c1", "", "defaults-sa"), machine("m", "c1", "machine-sa")},
			want: []string{ServiceAccount, "machine-sa"}},
		{name: "machine without a found cluster keeps every cluster default", objs: []client.Object{
			cluster("tc", "c1", "", "defaults-sa"), cluster("tc2", "c2", "", "other-defaults"), machine("m", "", ""),
		}, want: []string{ServiceAccount, "defaults-sa", "other-defaults"}},
		{name: "pool own name", objs: []client.Object{cluster("tc", "c1", "", "defaults-sa"), pool("p", "c1", "pool-sa")},
			want: []string{ServiceAccount, "pool-sa"}},
		{name: "pool inherits the cluster defaults", objs: []client.Object{cluster("tc", "c1", "", "defaults-sa"), pool("p", "c1", "")},
			want: []string{ServiceAccount, "defaults-sa"}},
		{name: "pool without a found cluster keeps every cluster default", objs: []client.Object{
			cluster("tc", "c1", "", "defaults-sa"), cluster("tc2", "c2", "", "other-defaults"), pool("p", "", ""),
		}, want: []string{ServiceAccount, "defaults-sa", "other-defaults"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var subjects []rbacv1.Subject
			for _, n := range all {
				subjects = append(subjects, subject(ns, n))
			}
			rb := newBinding(ns, append(subjects, rbacv1.Subject{Kind: rbacv1.UserKind, Name: "mallory"}))
			c := newClient(t, nil, append(tt.objs, rb)...)
			swept, err := SweepNamespace(t.Context(), c, c, ns)
			if err != nil || swept {
				t.Fatalf("SweepNamespace = %v, %v; want false, nil", swept, err)
			}
			if got := subjectNames(binding(t, c)); !slices.Equal(got, tt.want) {
				t.Errorf("subjects = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("unmanaged binding untouched", func(t *testing.T) {
		t.Parallel()
		rb := newBinding(ns, []rbacv1.Subject{subject(ns, "old")})
		rb.Labels = nil
		c := newClient(t, nil, cluster("tc", "c1", "", ""), rb)
		if _, err := SweepNamespace(t.Context(), c, c, ns); err != nil {
			t.Fatal(err)
		}
		if got := subjectNames(binding(t, c)); !slices.Equal(got, []string{"old"}) {
			t.Errorf("subjects = %v", got)
		}
	})
	t.Run("nothing left deletes the binding", func(t *testing.T) {
		t.Parallel()
		c := newClient(t, nil, cluster("tc", "c1", "", ""), newBinding(ns, []rbacv1.Subject{subject(ns, "old")}))
		if _, err := SweepNamespace(t.Context(), c, c, ns); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: RoleBinding}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
			t.Errorf("binding with no used subject survived: %v", err)
		}
	})
	t.Run("no binding", func(t *testing.T) {
		t.Parallel()
		c := newClient(t, nil, cluster("tc", "c1", "", ""))
		if _, err := SweepNamespace(t.Context(), c, c, ns); err != nil {
			t.Errorf("SweepNamespace without a binding: %v", err)
		}
	})
	t.Run("update conflict is an error", func(t *testing.T) {
		t.Parallel()
		c := newClient(t, &interceptor.Funcs{
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error { return errBoom },
		}, cluster("tc", "c1", "", ""), newBinding(ns, []rbacv1.Subject{subject(ns, ServiceAccount), subject(ns, "old")}))
		if _, err := SweepNamespace(t.Context(), c, c, ns); !errors.Is(err, errBoom) {
			t.Errorf("err = %v, want boom", err)
		}
	})
}

// TestSweep proves Sweep, checking every managed namespace, clears the
// runner objects of a namespace with no Terraform* object and leaves the
// others' objects, including unmanaged ones, in place.
func TestSweep(t *testing.T) {
	t.Parallel()
	c := newClient(t, nil, sweepFixture()...)
	if err := Sweep(t.Context(), c, c); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n := count(t, c, "moved"); n != 2 {
		t.Errorf("moved: %d objects left, want the 2 unmanaged ones", n)
	}
	for _, n := range []string{"machines", "clusters", "pools"} {
		if got := count(t, c, n); got != 3 {
			t.Errorf("%s: %d objects left, want 3", n, got)
		}
	}
}

// TestSweepRunLeases: the run and cluster write leases carry
// captf.io/managed=true, so a namespace left without Terraform* objects (a
// Job active during clusterctl move leaves its lease on the source) loses
// them; a namespace with objects keeps them.
func TestSweepRunLeases(t *testing.T) {
	t.Parallel()
	c := newClient(t, nil, &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: "machines", Name: "m"}})
	leases := map[string][]string{}
	for _, namespace := range []string{"moved", "machines"} {
		for _, s := range []runlease.Spec{
			{Name: runlease.RunName("0123456789abcdef-m"), Kind: runlease.KindRun, OwnerKind: state.KindTerraformMachine, OwnerName: "m"},
			{Name: runlease.ClusterName(namespace, "c1"), Kind: runlease.KindCluster, OwnerKind: state.KindTerraformCluster, OwnerName: "tc"},
		} {
			s.Namespace, s.Holder, s.Op, s.ClusterName = namespace, "job", jobs.OpApply, "c1"
			if _, err := runlease.Acquire(t.Context(), c, c, time.Now(), s); err != nil {
				t.Fatal(err)
			}
			leases[namespace] = append(leases[namespace], s.Name)
		}
	}
	if err := Sweep(t.Context(), c, c); err != nil {
		t.Fatal(err)
	}
	for namespace, want := range map[string]int{"moved": 0, "machines": 2} {
		list := &coordinationv1.LeaseList{}
		if err := c.List(t.Context(), list, client.InNamespace(namespace)); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != want {
			t.Errorf("%s: %d of the leases %v left, want %d", namespace, len(list.Items), leases[namespace], want)
		}
	}
}

// TestSweepNamespaces proves Sweep, given explicit namespaces, checks only
// those and leaves an unlisted namespace untouched, and that
// SweepNamespace reports swept=true for a namespace with no runner objects
// at all.
func TestSweepNamespaces(t *testing.T) {
	t.Parallel()
	c := newClient(t, nil, sweepFixture()...)
	if err := Sweep(t.Context(), c, c, "machines"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, c, "moved"); n != 5 {
		t.Errorf("unlisted namespace swept: %d left", n)
	}
	swept, err := SweepNamespace(t.Context(), c, c, "empty")
	if err != nil || !swept {
		t.Errorf("SweepNamespace(empty) = %v, %v", swept, err)
	}
}

// TestSweepUsesUncachedReader: the decision reads only the reader passed as
// uncached; the client is used only to delete.
func TestSweepUsesUncachedReader(t *testing.T) {
	t.Parallel()
	uncached := newClient(t, nil, sweepFixture()...)
	// The "cached" client lacks the TerraformMachine; if the sweep asked it,
	// namespace machines would be swept.
	var cachedObjs []client.Object
	for _, o := range sweepFixture() {
		if _, ok := o.(*infrav1.TerraformMachine); !ok {
			cachedObjs = append(cachedObjs, o)
		}
	}
	cached := newClient(t, &interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			t.Error("sweep listed through the cached client")
			return nil
		},
	}, cachedObjs...)
	if err := Sweep(t.Context(), uncached, cached); err != nil {
		t.Fatal(err)
	}
	if n := count(t, uncached, "machines"); n != 3 {
		t.Errorf("machines swept: %d left", n)
	}
}

// TestSweepErrors proves Sweep joins a delete failure in one namespace
// without skipping the others, and that Sweep and SweepNamespace surface a
// list failure from either the Terraform-object check or the managed-object
// listing.
func TestSweepErrors(t *testing.T) {
	t.Parallel()
	t.Run("delete failure does not stop other namespaces", func(t *testing.T) {
		t.Parallel()
		objs := append(managedIn("a"), managedIn("b")...)
		c := newClient(t, &interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
				if o.GetNamespace() == "a" {
					return errBoom
				}
				return c.Delete(ctx, o, opts...)
			},
		}, objs...)
		if err := Sweep(t.Context(), c, c); !errors.Is(err, errBoom) {
			t.Errorf("Sweep = %v, want boom", err)
		}
		if n := count(t, c, "b"); n != 0 {
			t.Errorf("b: %d left", n)
		}
	})
	listFail := func(match func(client.ObjectList) bool) client.WithWatch {
		return newClient(t, &interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
				if match(l) {
					return errBoom
				}
				return c.List(ctx, l, opts...)
			},
		}, managedIn("a")...)
	}
	isMachines := func(l client.ObjectList) bool { _, ok := l.(*infrav1.TerraformMachineList); return ok }
	isLeases := func(l client.ObjectList) bool { _, ok := l.(*coordinationv1.LeaseList); return ok }
	if err := Sweep(t.Context(), listFail(isMachines), newClient(t, nil)); !errors.Is(err, errBoom) {
		t.Errorf("terraform list failure: %v", err)
	}
	if err := Sweep(t.Context(), listFail(isLeases), newClient(t, nil)); !errors.Is(err, errBoom) {
		t.Errorf("managed list failure: %v", err)
	}
	if _, err := SweepNamespace(t.Context(), listFail(isLeases), newClient(t, nil), "a"); !errors.Is(err, errBoom) {
		t.Errorf("namespace list failure: %v", err)
	}
}
