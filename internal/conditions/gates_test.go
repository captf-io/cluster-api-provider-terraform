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

package conditions

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestSetInitial proves SetInitial sets Paused, Deleting and DriftDetected
// on both a machine and a cluster, and sets DeletionBlocked on the cluster
// only, never on the machine.
func TestSetInitial(t *testing.T) {
	t.Parallel()
	want := map[string]metav1.Condition{
		"Paused":   {Status: metav1.ConditionFalse, Reason: "NotPaused"},
		"Deleting": {Status: metav1.ConditionFalse, Reason: "NotDeleting"},
		// Nothing is known about drift until the first check completes.
		"DriftDetected": {Status: metav1.ConditionUnknown, Reason: "DriftNotChecked"},
	}
	machine := &infrav1.TerraformMachine{}
	SetInitial(machine, KindMachine)
	cluster := &infrav1.TerraformCluster{}
	SetInitial(cluster, KindCluster)
	for typ, w := range want {
		for _, obj := range []conditions.Setter{machine, cluster} {
			if c := conditions.Get(obj, typ); c == nil || c.Status != w.Status || c.Reason != w.Reason {
				t.Errorf("%s = %+v, want %s/%s", typ, c, w.Status, w.Reason)
			}
		}
	}
	if c := conditions.Get(cluster, "DeletionBlocked"); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "NotBlocked" {
		t.Errorf("cluster DeletionBlocked = %+v", c)
	}
	if conditions.Has(machine, "DeletionBlocked") {
		t.Error("machine got DeletionBlocked")
	}
}

// TestSetInitialKeepsExisting proves SetInitial never overwrites a
// condition an object already carries.
func TestSetInitialKeepsExisting(t *testing.T) {
	t.Parallel()
	obj := &infrav1.TerraformMachine{}
	conditions.Set(obj, metav1.Condition{Type: "Paused", Status: metav1.ConditionTrue, Reason: "Paused"})
	conditions.Set(obj, metav1.Condition{Type: "Deleting", Status: metav1.ConditionTrue, Reason: "Deleting"})
	SetInitial(obj, KindMachine)
	for _, typ := range []string{"Paused", "Deleting"} {
		if c := conditions.Get(obj, typ); c.Status != metav1.ConditionTrue {
			t.Errorf("%s overwritten: %+v", typ, c)
		}
	}
}

// TestSetDependenciesReady proves SetDependenciesReady sets the
// DependenciesReady condition, defaulting an empty message to the reason
// and otherwise keeping the given message.
func TestSetDependenciesReady(t *testing.T) {
	t.Parallel()
	obj := &infrav1.TerraformMachine{}
	SetDependenciesReady(obj, metav1.ConditionUnknown, infrav1.WaitingForBootstrapDataReason, "")
	c := conditions.Get(obj, "DependenciesReady")
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != "WaitingForBootstrapData" || c.Message != "WaitingForBootstrapData" {
		t.Errorf("DependenciesReady = %+v", c)
	}
	SetDependenciesReady(obj, metav1.ConditionTrue, infrav1.DependenciesReadyReason, "all set")
	if c := conditions.Get(obj, "DependenciesReady"); c.Status != metav1.ConditionTrue || c.Message != "all set" {
		t.Errorf("DependenciesReady = %+v", c)
	}
}
