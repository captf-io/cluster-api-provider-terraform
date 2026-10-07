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
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
)

// TestTakeJobSlotMessageStable proves the WaitingForJobSlot message does
// not change with the number of running Jobs: every waiter would otherwise
// rewrite its status each time a Job starts or ends. It still names the
// limit, the scope and the operation.
func TestTakeJobSlotMessageStable(t *testing.T) {
	obj := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "m", UID: "u", Labels: map[string]string{clusterv1.ClusterNameLabel: "c1"}}}
	for _, tc := range []struct {
		name            string
		global, cluster int
		want            []string
	}{
		{"global", 2, 0, []string{"limit of 2 Jobs running in all", "the apply starts"}},
		{"cluster", 0, 2, []string{"limit of 2 Jobs running for cluster c1", "the apply starts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var msgs []string
			for _, active := range []int{2, 3, 5} {
				var objs []client.Object
				for i := range active {
					objs = append(objs, slotJob(fmt.Sprintf("j%d", i), testNS, "c1", false))
				}
				c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
				r := &reconciler{d: Deps{Client: c, MaxActiveJobs: tc.global, ClusterMaxActiveJobs: tc.cluster}, obj: obj}
				w, err := r.takeJobSlot(t.Context(), jobs.OpApply)
				if err != nil {
					t.Fatal(err)
				}
				if w.reason != infrav1.WaitingForJobSlotReason {
					t.Fatalf("%d active: wait = %+v, want WaitingForJobSlot", active, w)
				}
				msgs = append(msgs, w.message)
			}
			for _, m := range msgs[1:] {
				if m != msgs[0] {
					t.Errorf("message changes with the active count: %q vs %q", msgs[0], m)
				}
			}
			for _, s := range tc.want {
				if !strings.Contains(msgs[0], s) {
					t.Errorf("message %q does not contain %q", msgs[0], s)
				}
			}
		})
	}
}
