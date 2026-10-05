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

package terraformcluster

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2/ktesting"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// TestClusterPredicates: every Cluster change the cluster inputs hash
// passes; an unrelated update does not.
func TestClusterPredicates(t *testing.T) {
	t.Parallel()
	logger, _ := ktesting.NewTestContext(t)
	p := ClusterPredicates(scheme(t), logger)
	base := testCluster(func(c *clusterv1.Cluster) {
		c.Spec.Topology.ClassRef.Name = "cc"
		c.Spec.Topology.Version = "v1.36.1"
	})
	tests := []struct {
		name   string
		mutate func(*clusterv1.Cluster)
		want   bool
	}{
		{"endpoint set by a control-plane provider", func(c *clusterv1.Cluster) { c.Spec.ControlPlaneEndpoint = valid }, true},
		// CAPI's predicate watches the ControlPlaneInitialized condition, which
		// the Cluster controller sets with status.initialization's field.
		{"control plane initialized", func(c *clusterv1.Cluster) {
			c.Status.Conditions = []metav1.Condition{{Type: clusterv1.ClusterControlPlaneInitializedCondition, Status: metav1.ConditionTrue}}
		}, true},
		{"topology version", func(c *clusterv1.Cluster) { c.Spec.Topology.Version = "v1.36.2" }, true},
		{"paused", func(c *clusterv1.Cluster) { c.Spec.Paused = new(true) }, true},
		// cluster_network is a hashed input: the change must not wait for
		// the resync.
		{"cluster network", func(c *clusterv1.Cluster) {
			c.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{"10.244.0.0/16"}
		}, true},
		{"unrelated label", func(c *clusterv1.Cluster) { c.Labels = map[string]string{"x": "y"} }, false},
	}
	for _, tt := range tests {
		n := base.DeepCopy()
		tt.mutate(n)
		if got := p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: n}); got != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}
