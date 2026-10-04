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

package terraformmachine

import (
	"slices"
	"testing"

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

// TestClusterToMachines: the machine controller's Cluster watch enqueues
// the cluster's TerraformMachines, never its TerraformCluster (the
// verification finding against util.ClusterToInfrastructureMapFunc).
func TestClusterToMachines(t *testing.T) {
	t.Parallel()
	lbl := func(cluster string) map[string]string { return map[string]string{clusterv1.ClusterNameLabel: cluster} }
	// ClusterToTypedObjectsMapper resolves the list kind's scope through the
	// RESTMapper; the fake client's default mapper is empty.
	rm := meta.NewDefaultRESTMapper([]schema.GroupVersion{infrav1.GroupVersion})
	rm.Add(infrav1.GroupVersion.WithKind("TerraformMachineList"), meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithRESTMapper(rm).WithObjects(
		testTC(),
		&infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "m1", Labels: lbl("c1")}},
		&infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "m2", Labels: lbl("c1")}},
		&infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "other", Labels: lbl("c2")}},
	).Build()
	m, err := ClusterToMachines(c, scheme(t))
	if err != nil {
		t.Fatal(err)
	}
	got := requestNames(m(t.Context(), testCluster()))
	if !slices.Equal(got, []string{"m1", "m2"}) {
		t.Errorf("Cluster → %v, want the TerraformMachines m1 and m2", got)
	}
}

// TestAdapterContract pins what the machine adapter tells the shared core.
func TestAdapterContract(t *testing.T) {
	t.Parallel()
	tm := testTM()
	a := newAdapter(shared.Deps{}, tm)
	if a.Kind() != "TerraformMachine" || a.Role() != contract.RoleMachine || a.Finalizer() != Finalizer ||
		a.Mutable() || !a.RefreshAfterApply() || a.Object() != tm {
		t.Error("adapter identity")
	}
	if a.Spec().Source.Image != tm.Spec.Source.Image || a.Spec().IdentityRef.Name != "aws" {
		t.Errorf("spec view = %+v", a.Spec())
	}
	st := a.Status()
	st.StateSecretSuffix = "sfx"
	st.ActiveJob.Name = "j"
	if tm.Status.StateSecretSuffix != "sfx" || tm.Status.ActiveJob.Name != "j" {
		t.Error("status pointers do not point into the object")
	}
}

// TestSecretToMachines proves SecretToMachines maps a cluster base state
// Secret to that cluster's TerraformMachines and an owned Secret to its
// owning TerraformMachine.
func TestSecretToMachines(t *testing.T) {
	t.Parallel()
	lbl := map[string]string{clusterv1.ClusterNameLabel: "c1"}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		&infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "m1", Labels: lbl}},
	).Build()
	suffix, err := state.Suffix(ns, state.KindTerraformCluster, "c1")
	if err != nil {
		t.Fatal(err)
	}
	clusterState := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: state.SecretName(suffix), Labels: map[string]string{
		state.OwnerKindLabel: state.KindTerraformCluster, clusterv1.ClusterNameLabel: "c1", state.BackendSuffixLabel: suffix,
	}}}
	own := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, OwnerReferences: []metav1.OwnerReference{{
		APIVersion: infrav1.GroupVersion.String(), Kind: "TerraformMachine", Name: "m9",
	}}}}
	mapper := SecretToMachines(c)
	for obj, want := range map[client.Object][]string{clusterState: {"m1"}, own: {"m9"}} {
		if got := requestNames(mapper(t.Context(), obj)); !slices.Equal(got, want) {
			t.Errorf("%s → %v, want %v", obj.GetName(), got, want)
		}
	}
}
