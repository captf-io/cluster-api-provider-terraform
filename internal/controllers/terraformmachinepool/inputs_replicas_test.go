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


package terraformmachinepool

import (
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestPoolReplicas proves the rendered replicas for each owner case:
// no autoscaling and a foreign owner render spec.replicas, CAPTF's own or
// no owner annotation render the observed value clamped into min/max.
func TestPoolReplicas(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		mp   *clusterv1.MachinePool
		tmp  *infrav1.TerraformMachinePool
		want int32
	}{
		{"autoscaling off: spec", testMP(specReplicas(4)), testTMP(observed(7)), 4},
		{"no annotation: observed", testMP(autoscaled, specReplicas(2)), testTMP(observed(7)), 7},
		{"observed clamped to max", testMP(autoscaled, specReplicas(2)), testTMP(observed(50)), 10},
		{"no observation: spec", testMP(autoscaled, specReplicas(3)), testTMP(), 3},
		{"captf owner: observed", testMP(autoscaled, managedBy(ReplicasManagedByValue), specReplicas(2)), testTMP(observed(5)), 5},
		{"foreign owner: spec", testMP(autoscaled, managedBy("other"), specReplicas(10)), testTMP(observed(3)), 10},
		{"foreign owner, spec unset: 1", testMP(autoscaled, managedBy("other")), testTMP(observed(3)), 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			autoscaling, _, _ := ParseAutoscaling(tt.mp)
			if got := poolReplicas(tt.mp, tt.tmp, autoscaling); got != tt.want {
				t.Errorf("poolReplicas = %d, want %d", got, tt.want)
			}
		})
	}
}
