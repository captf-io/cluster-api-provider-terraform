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

package terraformmachine

import (
	"maps"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
)

// TestSyncRemediationStaleCache proves a stale cached Machine that lacks
// an annotation the server already carries yields no RemediationRequested
// event and no captf_remediation_requests_total increment.
func TestSyncRemediationStaleCache(t *testing.T) {
	t.Parallel()
	ann := map[string]string{clusterv1.RemediateMachineAnnotation: "", RequestedByAnnotation: "the instance is terminated"}
	server := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "m1", Annotations: maps.Clone(ann)}}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(server).Build()
	// The cache read predates the annotation: same RV lineage, older body.
	stale := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "m1", ResourceVersion: "1"}}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(server), server); err != nil {
		t.Fatal(err)
	}
	if server.ResourceVersion == stale.ResourceVersion {
		stale.ResourceVersion = "0"
	}
	rec := &recorder{}
	m, reg := metricsRecorder(t)
	tm := unhealthy(infrav1.InstanceTerminatedReason, 0)
	if err := SyncRemediation(t.Context(), shared.Deps{Client: c, Recorder: rec, Metrics: m}, stale, tm, nil); err != nil {
		t.Fatal(err)
	}
	checkRemediations(t, reg, 0, 0)
	if len(rec.reasons) != 0 {
		t.Errorf("events = %v, want none", rec.reasons)
	}
	if stale.Annotations[RequestedByAnnotation] == "" {
		t.Errorf("cached Machine was not refreshed: %v", stale.Annotations)
	}
}
