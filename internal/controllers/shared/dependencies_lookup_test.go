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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestLookupClusterGone proves a machine whose Cluster is gone still finds
// the TerraformCluster labeled with its cluster name, for the deletion
// policy, identity and runner ServiceAccount it inherits, while the
// Cluster stays unknown; with two such TerraformClusters, or none, it
// finds nothing.
func TestLookupClusterGone(t *testing.T) {
	t.Parallel()
	tc := func(name string) *infrav1.TerraformCluster {
		return &infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{
			Namespace: testNS, Name: name, Labels: map[string]string{clusterv1.ClusterNameLabel: "c1"},
		}}
	}
	obj := metav1.ObjectMeta{Namespace: testNS, Name: "m1", Labels: map[string]string{clusterv1.ClusterNameLabel: "c1"}}
	for _, tt := range []struct {
		name     string
		clusters []*infrav1.TerraformCluster
		want     string
	}{
		{"one", []*infrav1.TerraformCluster{tc("tc1")}, "tc1"},
		{"ambiguous", []*infrav1.TerraformCluster{tc("tc1"), tc("tc2")}, ""},
		{"none", nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := fake.NewClientBuilder().WithScheme(testScheme(t))
			for _, c := range tt.clusters {
				b = b.WithObjects(c)
			}
			var owner OwnerInfo
			if err := LookupCluster(t.Context(), b.Build(), obj, &owner); err != nil {
				t.Fatal(err)
			}
			got := ""
			if owner.InfraCluster != nil {
				got = owner.InfraCluster.Name
			}
			if got != tt.want || owner.Cluster != nil {
				t.Errorf("InfraCluster %q, Cluster %v; want %q and none", got, owner.Cluster, tt.want)
			}
		})
	}
}
