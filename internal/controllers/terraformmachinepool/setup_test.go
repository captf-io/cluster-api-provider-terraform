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

package terraformmachinepool

import (
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// requestNames returns reqs' request names, sorted.
func requestNames(reqs []reconcile.Request) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Name)
	}
	slices.Sort(out)
	return out
}

// TestClusterToPools: the pool controller's Cluster watch enqueues the
// cluster's TerraformMachinePools.
func TestClusterToPools(t *testing.T) {
	t.Parallel()
	lbl := func(cluster string) map[string]string { return map[string]string{clusterv1.ClusterNameLabel: cluster} }
	// ClusterToTypedObjectsMapper resolves the list kind's scope through the
	// RESTMapper; the fake client's default mapper is empty.
	rm := meta.NewDefaultRESTMapper([]schema.GroupVersion{infrav1.GroupVersion})
	rm.Add(infrav1.GroupVersion.WithKind("TerraformMachinePoolList"), meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithRESTMapper(rm).WithObjects(
		testTC(),
		&infrav1.TerraformMachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p1", Labels: lbl("c1")}},
		&infrav1.TerraformMachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p2", Labels: lbl("c1")}},
		&infrav1.TerraformMachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "other", Labels: lbl("c2")}},
	).Build()
	m, err := ClusterToPools(c, scheme(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := requestNames(m(t.Context(), testCluster())); !slices.Equal(got, []string{"p1", "p2"}) {
		t.Errorf("Cluster → %v, want the TerraformMachinePools p1 and p2", got)
	}
}

// TestSecretToPools proves SecretToPools maps a cluster base state Secret
// to every TerraformMachinePool of that cluster and an owned Secret to its
// owning pool.
func TestSecretToPools(t *testing.T) {
	t.Parallel()
	lbl := map[string]string{clusterv1.ClusterNameLabel: "c1"}
	provisioned := &infrav1.TerraformMachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p2", Labels: lbl}}
	provisioned.Status.Initialization.Provisioned = new(true)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		&infrav1.TerraformMachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p1", Labels: lbl}}, provisioned,
	).Build()
	suffix, err := state.Suffix(ns, state.KindTerraformCluster, "c1")
	if err != nil {
		t.Fatal(err)
	}
	clusterState := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: state.SecretName(suffix), Labels: map[string]string{
		state.OwnerKindLabel: state.KindTerraformCluster, clusterv1.ClusterNameLabel: "c1", state.BackendSuffixLabel: suffix,
	}}}
	own := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, OwnerReferences: []metav1.OwnerReference{{
		APIVersion: infrav1.GroupVersion.String(), Kind: "TerraformMachinePool", Name: "p9",
	}}}}
	mapper := SecretToPools(c)
	for obj, want := range map[client.Object][]string{clusterState: {"p1", "p2"}, own: {"p9"}} {
		if got := requestNames(mapper(t.Context(), obj)); !slices.Equal(got, want) {
			t.Errorf("%s → %v, want %v", obj.GetName(), got, want)
		}
	}
}

// TestAdapterContract pins what the pool adapter tells the shared core.
func TestAdapterContract(t *testing.T) {
	t.Parallel()
	tmp := testTMP()
	a := newAdapter(shared.Deps{}, tmp)
	if a.Kind() != "TerraformMachinePool" || a.Role() != contract.RoleMachinePool || a.Finalizer() != Finalizer ||
		!a.Mutable() || !a.RefreshAfterApply() || a.Object() != tmp {
		t.Error("adapter identity")
	}
	st := a.Status()
	st.StateSecretSuffix = "sfx"
	if tmp.Status.StateSecretSuffix != "sfx" || st.UnhealthySamples != nil || st.Plan != nil {
		t.Error("status pointers do not point into the object")
	}
}

// TestSpecAlwaysPool proves Spec never returns a nil PoolDrift, which is
// what marks a pool for shared.Resolve, copies an own drift policy, and
// passes the membership refresh interval through (0 left for Resolve's
// default).
func TestSpecAlwaysPool(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		mut        func(*infrav1.TerraformMachinePool)
		drift      infrav1.MachinePoolDriftPolicy
		membership time.Duration
	}{
		{"no drift, no membership interval", nil, infrav1.MachinePoolDriftPolicy{}, 0},
		{"own drift and membership interval", func(p *infrav1.TerraformMachinePool) {
			p.Spec.Drift = &infrav1.MachinePoolDriftPolicy{IntervalSeconds: 600, Action: infrav1.DriftActionRemediate}
			p.Spec.MembershipRefreshIntervalSeconds = 30
		}, infrav1.MachinePoolDriftPolicy{IntervalSeconds: 600, Action: infrav1.DriftActionRemediate}, 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tmp := testTMP()
			if tt.mut != nil {
				tt.mut(tmp)
			}
			s := newAdapter(shared.Deps{}, tmp).Spec()
			if s.PoolDrift == nil {
				t.Fatal("Spec returned a nil PoolDrift: the pool would resolve as a machine")
			}
			if *s.PoolDrift != tt.drift || s.MembershipRefreshInterval != tt.membership || !s.InheritsDefaults ||
				s.MachineDrift != nil || s.Remediation != nil || s.IdentityRef.Name != "aws" {
				t.Errorf("spec view = %+v", s)
			}
			if tmp.Spec.Drift != nil && s.PoolDrift == tmp.Spec.Drift {
				t.Error("PoolDrift aliases spec.drift")
			}
			// Resolve then treats it as a pool.
			if e := shared.Resolve(s, nil, 30*time.Minute, true); e.MembershipRefreshInterval == 0 {
				t.Errorf("resolved as a non-pool: %+v", e)
			}
		})
	}
}

// TestMembershipConverging proves MembershipConverging compares
// spec.providerIDList's length with status.replicas (nil counts as 0).
func TestMembershipConverging(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		ids      []string
		replicas *int32
		want     bool
	}{
		{"empty and unset", nil, nil, false},
		{"empty and 0", nil, new(int32(0)), false},
		{"scaling up", []string{"a"}, new(int32(2)), true},
		{"scaling down", []string{"a", "b"}, new(int32(1)), true},
		{"converged", []string{"a", "b"}, new(int32(2)), false},
		{"members, replicas unset", []string{"a"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tmp := testTMP(func(p *infrav1.TerraformMachinePool) {
				p.Spec.ProviderIDList = tt.ids
				p.Status.Replicas = tt.replicas
			})
			if got := newAdapter(shared.Deps{}, tmp).MembershipConverging(); got != tt.want {
				t.Errorf("MembershipConverging = %v, want %v", got, tt.want)
			}
		})
	}
}
