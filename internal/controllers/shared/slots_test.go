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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
)

// TestBelowLimit checks the headroom rule: background operations stop at
// 80% of a limit, the others at the limit, and 0 is no limit.
func TestBelowLimit(t *testing.T) {
	for _, tc := range []struct {
		name          string
		active, limit int
		op            jobs.Op
		want          bool
	}{
		{"no limit", 1000, 0, jobs.OpApply, true},
		{"apply below", 9, 10, jobs.OpApply, true},
		{"apply at", 10, 10, jobs.OpApply, false},
		{"destroy at", 10, 10, jobs.OpDestroy, false},
		{"drift below 80", 7, 10, jobs.OpDrift, true},
		{"drift at 80", 8, 10, jobs.OpDrift, false},
		{"refresh at 80", 8, 10, jobs.OpRefresh, false},
		{"limit 1 idle", 0, 1, jobs.OpDrift, true},
		{"limit 1 busy", 1, 1, jobs.OpDrift, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := belowLimit(tc.active, tc.limit, tc.op); got != tc.want {
				t.Errorf("belowLimit(%d, %d, %s) = %v, want %v", tc.active, tc.limit, tc.op, got, tc.want)
			}
		})
	}
}

// slotJob returns the Job called name in namespace ns, of cluster c, finished
// when done is true.
func slotJob(name, ns, c string, done bool) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{clusterv1.ClusterNameLabel: c}}}
	if done {
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	}
	return j
}

// TestTakeJobSlot checks that the global and the per-cluster limits both
// gate, that finished Jobs and other clusters' Jobs do not count, and that
// a spec limit overrides the flag.
func TestTakeJobSlot(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		slotJob("a1", testNS, "c1", false), slotJob("a2", testNS, "c1", false), slotJob("a4", testNS, "c1", false), slotJob("a5", testNS, "c1", false), slotJob("a3", testNS, "c1", true),
		slotJob("b1", "other", "c2", false),
	).Build()
	obj := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "m", UID: "u", Labels: map[string]string{clusterv1.ClusterNameLabel: "c1"}}}
	for _, tc := range []struct {
		name            string
		global, cluster int
		spec            int32
		op              jobs.Op
		wait            bool
	}{
		{"unlimited", 0, 0, 0, jobs.OpApply, false},
		{"cluster full", 0, 4, 0, jobs.OpApply, true},
		{"cluster room", 0, 5, 0, jobs.OpApply, false},
		{"spec overrides flag", 0, 20, 4, jobs.OpApply, true},
		{"spec raises flag", 0, 4, 5, jobs.OpApply, false},
		{"global full", 5, 0, 0, jobs.OpDestroy, true},
		{"global room", 6, 0, 0, jobs.OpDestroy, false},
		{"drift held at 80", 0, 5, 0, jobs.OpDrift, true},
		{"apply keeps headroom", 0, 5, 0, jobs.OpApply, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &reconciler{d: Deps{Client: c, MaxActiveJobs: tc.global, ClusterMaxActiveJobs: tc.cluster}, obj: obj, eff: EffectiveConfig{MaxActiveJobs: tc.spec}}
			w, err := r.takeJobSlot(t.Context(), tc.op)
			if err != nil {
				t.Fatal(err)
			}
			if got := w.reason == infrav1.WaitingForJobSlotReason; got != tc.wait {
				t.Fatalf("wait = %+v, want wait %v", w, tc.wait)
			}
			if tc.wait && (w.requeue < SlotRequeue || w.message == "") {
				t.Errorf("wait %+v: want a message and a requeue of at least %s", w, SlotRequeue)
			}
		})
	}
}
