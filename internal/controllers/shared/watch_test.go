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
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// names returns "namespace/name" for each reconcile.Request in reqs,
// sorted.
func names(reqs []reconcile.Request) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Namespace+"/"+r.Name)
	}
	slices.Sort(out)
	return out
}

// TestPredicates proves ClusterEndpointChanged, ManagedSecret, LabelsChanged
// and DeletesOnly each fire only on the event they name.
func TestPredicates(t *testing.T) {
	t.Parallel()
	c1 := &clusterv1.Cluster{}
	c2 := c1.DeepCopy()
	c2.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: "cp", Port: 6443}
	ep := ClusterEndpointChanged()
	if !ep.Update(event.UpdateEvent{ObjectOld: c1, ObjectNew: c2}) || ep.Update(event.UpdateEvent{ObjectOld: c2, ObjectNew: c2.DeepCopy()}) ||
		ep.Create(event.CreateEvent{Object: c2}) || ep.Delete(event.DeleteEvent{Object: c2}) || ep.Generic(event.GenericEvent{Object: c2}) {
		t.Error("ClusterEndpointChanged")
	}

	managed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{state.ManagedLabel: "true"}}}
	if !ManagedSecret().Create(event.CreateEvent{Object: managed}) || ManagedSecret().Create(event.CreateEvent{Object: &corev1.Secret{}}) {
		t.Error("ManagedSecret")
	}

	ns1 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"tier": "gold"}}}
	ns2 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"tier": "silver"}}}
	lc := LabelsChanged()
	if !lc.Update(event.UpdateEvent{ObjectOld: ns1, ObjectNew: ns2}) || lc.Update(event.UpdateEvent{ObjectOld: ns1, ObjectNew: ns1.DeepCopy()}) ||
		lc.Create(event.CreateEvent{Object: ns1}) || lc.Delete(event.DeleteEvent{Object: ns1}) || lc.Generic(event.GenericEvent{Object: ns1}) {
		t.Error("LabelsChanged")
	}

	do := DeletesOnly()
	if !do.Delete(event.DeleteEvent{Object: ns1}) || do.Create(event.CreateEvent{Object: ns1}) ||
		do.Update(event.UpdateEvent{ObjectOld: ns1, ObjectNew: ns2}) || do.Generic(event.GenericEvent{Object: ns1}) {
		t.Error("DeletesOnly")
	}
}

// TestInheritedPolicyChanged proves InheritedPolicyChanged passes only a
// TerraformCluster update that changes what its machines and pools
// inherit: spec.defaults, its identityRef, jobs, drift policy or
// deletionPolicy; not its
// source, a status write, or another kind.
func TestInheritedPolicyChanged(t *testing.T) {
	t.Parallel()
	p := InheritedPolicyChanged()
	old := &infrav1.TerraformCluster{Spec: infrav1.TerraformClusterSpec{
		WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: "id"}},
	}}
	for _, tt := range []struct {
		name string
		mut  func(*infrav1.TerraformCluster)
		want bool
	}{
		{"defaults.remediation", func(tc *infrav1.TerraformCluster) {
			tc.Spec.Defaults = &infrav1.TerraformClusterDefaults{Remediation: &infrav1.MachineRemediation{AnnotateMachine: new(true)}}
		}, true},
		{"defaults.membershipRefreshIntervalSeconds", func(tc *infrav1.TerraformCluster) {
			tc.Spec.Defaults = &infrav1.TerraformClusterDefaults{MembershipRefreshIntervalSeconds: 30}
		}, true},
		{"spec.drift.action", func(tc *infrav1.TerraformCluster) {
			tc.Spec.Drift = &infrav1.DriftPolicy{Action: infrav1.DriftActionRemediate}
		}, true},
		{"spec.identityRef", func(tc *infrav1.TerraformCluster) { tc.Spec.IdentityRef.Name = "other" }, true},
		{"spec.deletionPolicy", func(tc *infrav1.TerraformCluster) { tc.Spec.DeletionPolicy = infrav1.DeletionPolicyRetain }, true},
		{"defaults.deletionPolicy", func(tc *infrav1.TerraformCluster) {
			tc.Spec.Defaults = &infrav1.TerraformClusterDefaults{DeletionPolicy: infrav1.DeletionPolicyRetain}
		}, true},
		{"spec.jobs", func(tc *infrav1.TerraformCluster) { tc.Spec.Jobs = &infrav1.JobPolicy{ActiveDeadlineSeconds: 900} }, true},
		{"spec.source", func(tc *infrav1.TerraformCluster) { tc.Spec.Source.Image = "registry.example/c:2" }, false},
		{"status", func(tc *infrav1.TerraformCluster) { tc.Status.FailureDomains = []clusterv1.FailureDomain{{Name: "a"}} }, false},
	} {
		n := old.DeepCopy()
		tt.mut(n)
		if got := p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: n}); got != tt.want {
			t.Errorf("%s: passed = %v, want %v", tt.name, got, tt.want)
		}
	}
	if p.Create(event.CreateEvent{Object: old}) || p.Delete(event.DeleteEvent{Object: old}) || p.Generic(event.GenericEvent{Object: old}) {
		t.Error("InheritedPolicyChanged passes a create, delete or generic event")
	}
	if p.Update(event.UpdateEvent{ObjectOld: &infrav1.TerraformMachine{}, ObjectNew: &infrav1.TerraformMachine{}}) {
		t.Error("InheritedPolicyChanged passes another kind")
	}
}

// TestIndexers proves ClusterIdentityIndexer returns a cluster's own and
// default identity names (deduped), MachineIdentityIndexer returns nil for
// a machine without an identity, and each indexer returns nil for the other
// kind.
func TestIndexers(t *testing.T) {
	t.Parallel()
	tc := &infrav1.TerraformCluster{Spec: infrav1.TerraformClusterSpec{
		WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: "own"}},
		Defaults:      &infrav1.TerraformClusterDefaults{IdentityRef: infrav1.IdentityReference{Name: "machines"}},
	}}
	if got := ClusterIdentityIndexer(tc); !slices.Equal(got, []string{"own", "machines"}) {
		t.Errorf("cluster = %v", got)
	}
	same := &infrav1.TerraformCluster{Spec: infrav1.TerraformClusterSpec{
		WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: "a"}}, Defaults: &infrav1.TerraformClusterDefaults{IdentityRef: infrav1.IdentityReference{Name: "a"}},
	}}
	if got := ClusterIdentityIndexer(same); !slices.Equal(got, []string{"a"}) {
		t.Errorf("deduped = %v", got)
	}
	if got := MachineIdentityIndexer(&infrav1.TerraformMachine{}); got != nil {
		t.Errorf("machine without identity = %v", got)
	}
	// A Secret reference names no TerraformClusterIdentity, so it is not indexed.
	secret := infrav1.IdentityReference{Name: "s", Kind: infrav1.IdentityKindSecret}
	local := &infrav1.TerraformCluster{Spec: infrav1.TerraformClusterSpec{
		WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: secret}, Defaults: &infrav1.TerraformClusterDefaults{IdentityRef: secret},
	}}
	if got := ClusterIdentityIndexer(local); got != nil {
		t.Errorf("cluster with Secret refs = %v", got)
	}
	if got := MachineIdentityIndexer(&infrav1.TerraformMachine{Spec: infrav1.TerraformMachineSpec{WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: secret}}}); got != nil {
		t.Errorf("machine with a Secret ref = %v", got)
	}
	pool := &infrav1.TerraformMachinePool{Spec: infrav1.TerraformMachinePoolSpec{WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: "p"}}}}
	if got := PoolIdentityIndexer(pool); !slices.Equal(got, []string{"p"}) {
		t.Errorf("pool = %v", got)
	}
	if got := PoolIdentityIndexer(&infrav1.TerraformMachinePool{}); got != nil {
		t.Errorf("pool without identity = %v", got)
	}
	if ClusterIdentityIndexer(&infrav1.TerraformMachine{}) != nil || MachineIdentityIndexer(&infrav1.TerraformCluster{}) != nil || PoolIdentityIndexer(&infrav1.TerraformMachine{}) != nil {
		t.Error("wrong kind indexed")
	}
}

// watchClient returns a fake client, built using t, indexed by
// IdentityIndex and holding: cluster c1 with default identity "fleet";
// machine m-own with its own identity, m-inherit without, m-other in
// another cluster; pools p-own (own identity "fleet"), p-inherit, p-other
// (in c2) and p-admin (own identity "admin"); a TerraformCluster c2 with its own identity "fleet" and
// no defaults, which its machines therefore fall back to.
func watchClient(t *testing.T) client.Client {
	t.Helper()
	lbl := func(cluster string) map[string]string { return map[string]string{clusterv1.ClusterNameLabel: cluster} }
	objs := []client.Object{
		&infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "c1", Labels: lbl("c1")}, Spec: infrav1.TerraformClusterSpec{
			WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: "admin"}}, Defaults: &infrav1.TerraformClusterDefaults{IdentityRef: infrav1.IdentityReference{Name: "fleet"}},
		}},
		&infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "c2", Labels: lbl("c2")}, Spec: infrav1.TerraformClusterSpec{
			WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: "fleet"}},
		}},
		&infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "m-own", Labels: lbl("c1")},
			Spec: infrav1.TerraformMachineSpec{WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: "fleet"}}}},
		&infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "m-inherit", Labels: lbl("c1")}},
		&infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "m-other", Labels: lbl("c2")}},
		&infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: "elsewhere", Name: "m-far", Labels: lbl("c1")}},
		&infrav1.TerraformMachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "p-own", Labels: lbl("c1")},
			Spec: infrav1.TerraformMachinePoolSpec{WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: "fleet"}}}},
		&infrav1.TerraformMachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "p-inherit", Labels: lbl("c1")}},
		&infrav1.TerraformMachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "p-other", Labels: lbl("c2")}},
		&infrav1.TerraformMachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "p-admin", Labels: lbl("c1")},
			Spec: infrav1.TerraformMachinePoolSpec{WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: "admin"}}}},
	}
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithIndex(&infrav1.TerraformCluster{}, IdentityIndex, ClusterIdentityIndexer).
		WithIndex(&infrav1.TerraformMachine{}, IdentityIndex, MachineIdentityIndexer).
		WithIndex(&infrav1.TerraformMachinePool{}, IdentityIndex, PoolIdentityIndexer).Build()
}

// TestMappers proves IdentityToClusters, IdentityToMachines,
// NamespaceToObjects, MachineToClusters and ClusterStateSecretToMachines
// each map a watched object to the reconcile.Requests of the objects an
// identity, namespace, machine or state Secret should trigger, following
// the identityRef inheritance rules.
func TestMappers(t *testing.T) {
	t.Parallel()
	c := watchClient(t)
	ctx := t.Context()
	fleet := &infrav1.TerraformClusterIdentity{ObjectMeta: metav1.ObjectMeta{Name: "fleet"}}

	if got := names(IdentityToClusters(c)(ctx, fleet)); !slices.Equal(got, []string{"team-a/c1", "team-a/c2"}) {
		t.Errorf("IdentityToClusters = %v", got)
	}
	// m-other inherits c2's spec.identityRef: c2 has no defaults.identityRef.
	if got := names(IdentityToMachines(c)(ctx, fleet)); !slices.Equal(got, []string{"team-a/m-inherit", "team-a/m-other", "team-a/m-own"}) {
		t.Errorf("IdentityToMachines = %v", got)
	}
	// c1's own identity is not its machines' fallback: defaults.identityRef is.
	admin := &infrav1.TerraformClusterIdentity{ObjectMeta: metav1.ObjectMeta{Name: "admin"}}
	if got := names(IdentityToMachines(c)(ctx, admin)); len(got) != 0 {
		t.Errorf("IdentityToMachines(admin) = %v, want none", got)
	}
	if got := names(IdentityToPools(c)(ctx, fleet)); !slices.Equal(got, []string{"team-a/p-inherit", "team-a/p-other", "team-a/p-own"}) {
		t.Errorf("IdentityToPools = %v", got)
	}
	if got := names(IdentityToPools(c)(ctx, admin)); !slices.Equal(got, []string{"team-a/p-admin"}) {
		t.Errorf("IdentityToPools(admin) = %v", got)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS}}
	if got := names(NamespaceToObjects(c, func() client.ObjectList { return &infrav1.TerraformMachineList{} })(ctx, ns)); len(got) != 3 {
		t.Errorf("NamespaceToObjects = %v", got)
	}
	machine := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "m-own", Labels: map[string]string{clusterv1.ClusterNameLabel: "c1"}}}
	if got := names(MachineToClusters(c)(ctx, machine)); !slices.Equal(got, []string{"team-a/c1"}) {
		t.Errorf("MachineToClusters = %v", got)
	}
	if got := MachineToClusters(c)(ctx, &infrav1.TerraformMachine{}); got != nil {
		t.Errorf("unlabeled machine = %v", got)
	}

	c1 := &infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "c1", Labels: map[string]string{clusterv1.ClusterNameLabel: "c1"}}}
	toMachines := TerraformClusterToObjects(c, func() client.ObjectList { return &infrav1.TerraformMachineList{} })
	if got := names(toMachines(ctx, c1)); !slices.Equal(got, []string{"team-a/m-inherit", "team-a/m-own"}) {
		t.Errorf("TerraformClusterToObjects(machines) = %v", got)
	}
	toPools := TerraformClusterToObjects(c, func() client.ObjectList { return &infrav1.TerraformMachinePoolList{} })
	if got := names(toPools(ctx, c1)); !slices.Equal(got, []string{"team-a/p-admin", "team-a/p-inherit", "team-a/p-own"}) {
		t.Errorf("TerraformClusterToObjects(pools) = %v", got)
	}
	if got := toMachines(ctx, &infrav1.TerraformCluster{}); got != nil {
		t.Errorf("unlabeled TerraformCluster = %v", got)
	}

	suffix, err := state.Suffix(testNS, state.KindTerraformCluster, "c1")
	if err != nil {
		t.Fatal(err)
	}
	base := secretMeta(metav1.ObjectMeta{Namespace: testNS, Name: state.SecretName(suffix), Labels: map[string]string{
		state.OwnerKindLabel: state.KindTerraformCluster, clusterv1.ClusterNameLabel: "c1", state.BackendSuffixLabel: suffix,
	}})
	if got := names(ClusterStateSecretToMachines(c)(ctx, base)); !slices.Equal(got, []string{"team-a/m-inherit", "team-a/m-own"}) {
		t.Errorf("ClusterStateSecretToMachines = %v", got)
	}
	// Every pool of c1, provisioned or not: pools re-read the exports.
	if got := names(ClusterStateSecretToPools(c)(ctx, base)); !slices.Equal(got, []string{"team-a/p-admin", "team-a/p-inherit", "team-a/p-own"}) {
		t.Errorf("ClusterStateSecretToPools = %v", got)
	}
	pool := &infrav1.TerraformMachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "p-own", Labels: map[string]string{clusterv1.ClusterNameLabel: "c1"}}}
	if got := names(MachineToClusters(c)(ctx, pool)); !slices.Equal(got, []string{"team-a/c1"}) {
		t.Errorf("MachineToClusters(pool) = %v", got)
	}
	chunk := base.DeepCopy()
	chunk.Name += "-part-1"
	if got := ClusterStateSecretToMachines(c)(ctx, chunk); got != nil {
		t.Errorf("chunk Secret mapped: %v", got)
	}
	if got := ClusterStateSecretToPools(c)(ctx, chunk); got != nil {
		t.Errorf("chunk Secret mapped to pools: %v", got)
	}
	machineState := base.DeepCopy()
	machineState.Labels[state.OwnerKindLabel] = state.KindTerraformMachine
	if got := ClusterStateSecretToPools(c)(ctx, machineState); got != nil {
		t.Errorf("machine state Secret mapped to pools: %v", got)
	}
}

// TestSecretToOwner proves SecretToOwner maps a Secret to the
// reconcile.Requests of its matching-kind owner references (or its
// pre-adoption owner-kind/owner-name labels), ignores a foreign group and
// the wrong kind, and proves Merge dedupes the requests of two mappers over
// the same object.
func TestSecretToOwner(t *testing.T) {
	t.Parallel()
	ref := func(kind, name string) metav1.OwnerReference {
		return metav1.OwnerReference{APIVersion: infrav1.GroupVersion.String(), Kind: kind, Name: name}
	}
	mirror := secretMeta(metav1.ObjectMeta{Namespace: testNS, OwnerReferences: []metav1.OwnerReference{
		ref("TerraformMachine", "m1"), ref("TerraformMachine", "m2"), ref("TerraformCluster", "c1"),
		{APIVersion: "other.example/v1", Kind: "TerraformMachine", Name: "foreign"},
	}})
	if got := names(SecretToOwner("TerraformMachine")(t.Context(), mirror)); !slices.Equal(got, []string{"team-a/m1", "team-a/m2"}) {
		t.Errorf("mirror owners = %v", got)
	}
	preAdopt := secretMeta(metav1.ObjectMeta{Namespace: testNS, Labels: map[string]string{
		state.OwnerKindLabel: "TerraformMachine", state.OwnerNameLabel: "m3",
	}})
	if got := names(SecretToOwner("TerraformMachine")(t.Context(), preAdopt)); !slices.Equal(got, []string{"team-a/m3"}) {
		t.Errorf("pre-adoption state = %v", got)
	}
	if got := SecretToOwner("TerraformCluster")(t.Context(), preAdopt); got != nil {
		t.Errorf("wrong kind mapped: %v", got)
	}
	owner := SecretToOwner("TerraformMachine")
	both := Merge(owner, func(ctx context.Context, o client.Object) []reconcile.Request { return owner(ctx, o) })
	if got := both(t.Context(), mirror); len(got) != 2 {
		t.Errorf("Merge did not dedupe: %v", got)
	}
}

// TestEvents proves the reconciler emits EventJobFailed once per newly
// failed Job, EventDestroyed once a destroy Job is followed by cleanup but
// not when the finalizer drops with no state, and EventIdentityNotAllowed
// when the identity's allowed namespaces reject the object.
func TestEvents(t *testing.T) {
	t.Parallel()
	t.Run("JobFailed once per failed Job", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(withFinalizer, notPaused))...)
		e.runner.jobs = append(e.runner.jobs, job("a", jobs.OpApply, jobs.Failed, t0))
		reconcile := func() {
			t.Helper()
			k := e.kindFor(t, readyOwner)
			k.in = machineIn()
			if _, err := reconcileOnce(t, e, k); err != nil {
				t.Fatal(err)
			}
		}
		reconcile()
		reconcile() // nothing new: no second event
		if n := e.rec.count(EventJobFailed); n != 1 {
			t.Fatalf("after one failed Job, JobFailed emitted %d times", n)
		}
		// A second failed Job has the same reason but names another Job.
		e.runner.jobs = append(e.runner.jobs, job("b", jobs.OpApply, jobs.Failed, t0.Add(time.Second)))
		reconcile()
		reconcile()
		if n := e.rec.count(EventJobFailed); n != 2 {
			t.Errorf("after two failed Jobs, JobFailed emitted %d times", n)
		}
	})
	t.Run("destroy then cleanup emits Destroyed", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused))...)
		e.state.st = &state.State{InputsHash: "h1:x"}
		e.runner.jobs = append(e.runner.jobs, job("d", jobs.OpDestroy, jobs.Succeeded, t0))
		if _, err := reconcileOnce(t, e, e.kindFor(t, readyOwner)); err != nil {
			t.Fatal(err)
		}
		if e.rec.count(EventDestroyed) != 1 {
			t.Errorf("events = %v", e.rec.reasons)
		}
	})
	t.Run("deleting without state drops the finalizer without Destroyed", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused))...)
		if _, err := reconcileOnce(t, e, e.kindFor(t, readyOwner)); err != nil {
			t.Fatal(err)
		}
		if e.rec.count(EventDestroyed) != 0 {
			t.Errorf("events = %v", e.rec.reasons)
		}
	})
	t.Run("identity not allowed", func(t *testing.T) {
		t.Parallel()
		objs := world(machine(withFinalizer, notPaused))
		objs[1].(*infrav1.TerraformClusterIdentity).Spec.AllowedNamespaces = nil
		e := newEnv(t, objs...)
		k := e.kindFor(t, readyOwner)
		k.in = machineIn()
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		if e.rec.count(EventIdentityNotAllowed) != 1 {
			t.Errorf("events = %v", e.rec.reasons)
		}
	})
}
