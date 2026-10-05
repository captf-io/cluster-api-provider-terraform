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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
)

// TestReconcileWithdrawsWhenPaused proves a paused TerraformMachine that
// reads Healthy takes back the remediation annotation it set on its
// Machine, and leaves one someone else set.
func TestReconcileWithdrawsWhenPaused(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		annotations map[string]string
		wantKept    bool
	}{
		{"ours: withdrawn", map[string]string{clusterv1.RemediateMachineAnnotation: "", RequestedByAnnotation: "the instance is terminated"}, false},
		{"someone else's: kept", map[string]string{clusterv1.RemediateMachineAnnotation: ""}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cluster := testCluster(func(c *clusterv1.Cluster) { c.Spec.Paused = new(true) })
			machine := testMachine(func(m *clusterv1.Machine) { m.Annotations = maps.Clone(tt.annotations) })
			tm := testTM(func(tm *infrav1.TerraformMachine) {
				conditions.Set(tm, metav1.Condition{Type: infrav1.InfrastructureHealthyCondition, Status: metav1.ConditionTrue, Reason: infrav1.HealthyReason})
			})
			c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cluster, machine, tm).WithStatusSubresource(tm).Build()
			m, _ := metricsRecorder(t)
			r := &Reconciler{Deps: shared.Deps{Client: c, APIReader: c, Clock: clock.RealClock{}, DriftDefault: time.Hour, Recorder: &recorder{}, Metrics: m}}
			if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tm)}); err != nil {
				t.Fatal(err)
			}
			got := &clusterv1.Machine{}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(machine), got); err != nil {
				t.Fatal(err)
			}
			if _, has := got.Annotations[clusterv1.RemediateMachineAnnotation]; has != tt.wantKept {
				t.Errorf("annotations %v, kept wanted %v", got.Annotations, tt.wantKept)
			}
		})
	}
}

// TestWithdrawRemediationNeverSets proves the withdraw-only half leaves a
// Machine without the annotation alone while the instance is terminated.
func TestWithdrawRemediationNeverSets(t *testing.T) {
	t.Parallel()
	machine := testMachine()
	tm := remediating(testTM(func(tm *infrav1.TerraformMachine) {
		tm.Status.Initialization.Provisioned = new(true)
		conditions.Set(tm, metav1.Condition{Type: infrav1.InfrastructureHealthyCondition, Status: metav1.ConditionFalse, Reason: infrav1.InstanceTerminatedReason})
	}))
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(machine.DeepCopy()).Build()
	if err := WithdrawRemediation(t.Context(), shared.Deps{Client: c}, machine, tm); err != nil {
		t.Fatal(err)
	}
	got := &clusterv1.Machine{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(machine), got); err != nil {
		t.Fatal(err)
	}
	if _, has := got.Annotations[clusterv1.RemediateMachineAnnotation]; has {
		t.Errorf("a terminated instance got annotations %v from the withdraw-only path", got.Annotations)
	}
}
