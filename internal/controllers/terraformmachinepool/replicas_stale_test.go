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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
)

// TestSyncReplicasStaleCache proves a stale cached MachinePool that
// already carries the server's write-back (the server is ahead) produces
// no second ReplicasWrittenBack event, and the cached object converges.
func TestSyncReplicasStaleCache(t *testing.T) {
	t.Parallel()
	server := testMP(autoscaled, managedBy(ReplicasManagedByValue), specReplicas(5))
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(server).Build()
	stale := &clusterv1.MachinePool{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(server), stale); err != nil {
		t.Fatal(err)
	}
	stale.Spec.Replicas = new(int32(2)) // what the cache still shows
	// Someone else bumps the server after the cache read.
	live := stale.DeepCopy()
	live.Spec.Replicas = new(int32(5))
	if err := c.Update(t.Context(), live); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	if err := SyncReplicas(t.Context(), shared.Deps{Client: c, Recorder: rec}, stale, testTMP(observed(5))); err != nil {
		t.Fatal(err)
	}
	if n := rec.count(shared.EventReplicasWrittenBack); n != 0 {
		t.Errorf("ReplicasWrittenBack emitted %d times for a change the server already had", n)
	}
	if *stale.Spec.Replicas != 5 {
		t.Errorf("cached replicas = %d, want the server's 5", *stale.Spec.Replicas)
	}
}
