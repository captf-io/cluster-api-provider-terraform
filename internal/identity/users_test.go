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

package identity

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// labeledCluster returns a TerraformCluster in namespace ns, named name,
// with cluster.x-k8s.io/cluster-name label clusterName (omitted when
// empty), spec.identityRef.name own and, when def is non-empty,
// spec.defaults.identityRef.name def.
func labeledCluster(ns, name, clusterName, own, def string) *infrav1.TerraformCluster {
	tc := &infrav1.TerraformCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       infrav1.TerraformClusterSpec{WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: own}}},
	}
	if clusterName != "" {
		tc.Labels = map[string]string{clusterv1.ClusterNameLabel: clusterName}
	}
	if def != "" {
		tc.Spec.Defaults = &infrav1.TerraformClusterDefaults{IdentityRef: infrav1.IdentityReference{Name: def}}
	}
	return tc
}

// labeledMachine returns a TerraformMachine in namespace ns, named name,
// with cluster.x-k8s.io/cluster-name label clusterName (omitted when empty)
// and spec.identityRef.name own.
func labeledMachine(ns, name, clusterName, own string) *infrav1.TerraformMachine {
	m := &infrav1.TerraformMachine{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       infrav1.TerraformMachineSpec{WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: own}}},
	}
	if clusterName != "" {
		m.Labels = map[string]string{clusterv1.ClusterNameLabel: clusterName}
	}
	return m
}

// labeledPool returns a TerraformMachinePool in namespace ns, named name,
// with cluster.x-k8s.io/cluster-name label clusterName (omitted when empty)
// and spec.identityRef.name own.
func labeledPool(ns, name, clusterName, own string) *infrav1.TerraformMachinePool {
	p := &infrav1.TerraformMachinePool{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       infrav1.TerraformMachinePoolSpec{WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: own}}},
	}
	if clusterName != "" {
		p.Labels = map[string]string{clusterv1.ClusterNameLabel: clusterName}
	}
	return p
}

// TestClusterIndex proves ClusterIndex.For finds the lowest-named cluster
// sharing a machine's cluster-name label in its own namespace, and returns
// nil for an unlabeled machine or an unknown cluster name.
func TestClusterIndex(t *testing.T) {
	t.Parallel()
	b := labeledCluster(tenant, "b", "c1", "x", "")
	a := labeledCluster(tenant, "a", "c1", "y", "")
	other := labeledCluster("other", "a", "c1", "z", "")
	ix := IndexClusters([]infrav1.TerraformCluster{*b, *a, *other, *labeledCluster(tenant, "nolabel", "", "w", "")})
	if got := ix.For(labeledMachine(tenant, "m", "c1", "")); got == nil || got.Name != "a" || got.Namespace != tenant {
		t.Errorf("For = %v, want %s/a (lowest name, same namespace)", got, tenant)
	}
	if got := ix.For(labeledMachine(tenant, "m", "", "")); got != nil {
		t.Errorf("For unlabeled machine = %v, want nil", got)
	}
	if got := ix.For(labeledMachine(tenant, "m", "c2", "")); got != nil {
		t.Errorf("For unknown cluster = %v, want nil", got)
	}
	if got := ix.For(labeledPool(tenant, "p", "c1", "")); got == nil || got.Name != "a" {
		t.Errorf("For pool = %v, want %s/a", got, tenant)
	}
}

// TestFirstUser proves FirstUser finds a cluster, machine or pool using the
// identity directly, a machine using it only through its cluster's
// defaults or identityRef fallback, that an object's own identityRef beats
// the fallback, that a same-named cluster in another namespace does not
// count, and that a list failure surfaces as an error.
func TestFirstUser(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		objs []client.Object
		want string // kind/namespace/name, "" for none
	}{
		{name: "unused", objs: []client.Object{labeledCluster(tenant, "tc", "c1", "other", "")}},
		{name: "cluster identityRef", objs: []client.Object{labeledCluster(tenant, "tc", "c1", idName, "")},
			want: "TerraformCluster/" + tenant + "/tc"},
		{name: "machine identityRef", objs: []client.Object{labeledMachine(tenant, "m", "", idName)},
			want: "TerraformMachine/" + tenant + "/m"},
		{name: "machine via cluster defaults", objs: []client.Object{
			labeledCluster(tenant, "tc", "c1", "other", idName), labeledMachine(tenant, "m", "c1", ""),
		}, want: "TerraformMachine/" + tenant + "/m"},
		{name: "machine via cluster identityRef fallback", objs: []client.Object{
			labeledCluster(tenant, "tc", "c1", idName, ""), labeledMachine(tenant, "m", "c1", ""),
		}, want: "TerraformCluster/" + tenant + "/tc"},
		{name: "own identityRef beats the fallback", objs: []client.Object{
			labeledCluster(tenant, "tc", "c1", "other", idName), labeledMachine(tenant, "m", "c1", "mine"),
		}},
		{name: "cluster of another namespace is not the machine's", objs: []client.Object{
			labeledCluster("other", "tc", "c1", "other", idName), labeledMachine(tenant, "m", "c1", ""),
		}},
		{name: "pool identityRef", objs: []client.Object{labeledPool(tenant, "p", "", idName)},
			want: "TerraformMachinePool/" + tenant + "/p"},
		{name: "pool via cluster defaults", objs: []client.Object{
			labeledCluster(tenant, "tc", "c1", "other", idName), labeledPool(tenant, "p", "c1", ""),
		}, want: "TerraformMachinePool/" + tenant + "/p"},
		{name: "pool's own identityRef beats the fallback", objs: []client.Object{
			labeledCluster(tenant, "tc", "c1", "other", idName), labeledPool(tenant, "p", "c1", "mine"),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(tt.objs...).Build()
			got, err := FirstUser(t.Context(), r, idName)
			if err != nil {
				t.Fatal(err)
			}
			desc := ""
			switch o := got.(type) {
			case *infrav1.TerraformCluster:
				desc = "TerraformCluster/" + o.Namespace + "/" + o.Name
			case *infrav1.TerraformMachine:
				desc = "TerraformMachine/" + o.Namespace + "/" + o.Name
			case *infrav1.TerraformMachinePool:
				desc = "TerraformMachinePool/" + o.Namespace + "/" + o.Name
			}
			if desc != tt.want {
				t.Errorf("FirstUser = %q, want %q", desc, tt.want)
			}
		})
	}
	boom := errors.New("boom")
	failing := fake.NewClientBuilder().WithScheme(newScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return boom },
	}).Build()
	if _, err := FirstUser(t.Context(), failing, idName); !errors.Is(err, boom) {
		t.Errorf("list error = %v, want boom", err)
	}
}
