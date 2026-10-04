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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// capiCluster returns a CAPI Cluster in namespace ns, named name, whose
// spec.infrastructureRef names the TerraformCluster infra.
func capiCluster(ns, name, infra string) *clusterv1.Cluster {
	return &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: clusterv1.ClusterSpec{InfrastructureRef: clusterv1.ContractVersionedObjectReference{
			APIGroup: infrav1.GroupVersion.Group, Kind: "TerraformCluster", Name: infra,
		}},
	}
}

// TestUsersThroughInfrastructureRef proves a machine or pool is found
// through its CAPI Cluster's infrastructureRef even when the
// TerraformCluster is unlabeled or a lower-named labeled one shadows it in
// the label index, and that a machine whose Cluster points elsewhere is
// not a user.
func TestUsersThroughInfrastructureRef(t *testing.T) {
	t.Parallel()
	s := newScheme(t)
	if err := clusterv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		objs []client.Object
		want int // machines and pools using the identity
	}{
		{name: "unlabeled TerraformCluster", objs: []client.Object{
			labeledCluster(tenant, "tc", "", "other", idName), capiCluster(tenant, "c1", "tc"), labeledMachine(tenant, "m", "c1", ""),
		}, want: 1},
		{name: "lower-named labeled TerraformCluster shadows", objs: []client.Object{
			labeledCluster(tenant, "a", "c1", "other", ""), labeledCluster(tenant, "tc", "c1", "other", idName),
			capiCluster(tenant, "c1", "tc"), labeledMachine(tenant, "m", "c1", ""),
		}, want: 1},
		{name: "pool through infrastructureRef", objs: []client.Object{
			labeledCluster(tenant, "tc", "", "other", idName), capiCluster(tenant, "c1", "tc"), labeledPool(tenant, "p", "c1", ""),
		}, want: 1},
		{name: "Cluster points at another TerraformCluster", objs: []client.Object{
			labeledCluster(tenant, "tc", "", "other", idName), labeledCluster(tenant, "tc2", "", "other", ""),
			capiCluster(tenant, "c1", "tc2"), labeledMachine(tenant, "m", "c1", ""),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := fake.NewClientBuilder().WithScheme(s).WithObjects(tt.objs...).Build()
			users, err := Users(t.Context(), r, idName)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, u := range users {
				if _, ok := u.(*infrav1.TerraformCluster); !ok {
					n++
				}
			}
			if n != tt.want {
				t.Errorf("machine/pool users = %d, want %d (%v)", n, tt.want, users)
			}
			first, err := FirstUser(t.Context(), r, idName)
			if err != nil || (first != nil) != (len(users) > 0) {
				t.Errorf("FirstUser = %v, %v; Users = %d", first, err, len(users))
			}
		})
	}
}
