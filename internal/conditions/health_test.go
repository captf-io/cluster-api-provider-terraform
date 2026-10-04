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

package conditions

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// TestSetInfrastructureHealthy exercises SetInfrastructureHealthy across
// health states and lifecycle phases.
func TestSetInfrastructureHealthy(t *testing.T) {
	t.Parallel()
	h := func(state contract.HealthState, healthy bool) *contract.Health {
		return &contract.Health{State: state, Healthy: healthy}
	}
	tests := []struct {
		name   string
		h      *contract.Health
		phase  HealthPhase
		status metav1.ConditionStatus
		reason string
	}{
		{"waiting, no apply yet", nil, HealthNotStarted, metav1.ConditionUnknown, "WaitingForProvisioning"},
		{"first apply started", nil, HealthApplyStarted, metav1.ConditionFalse, "Provisioning"},
		{"pending", h("pending", false), HealthObserved, metav1.ConditionFalse, "InstancePending"},
		{"running healthy", h("running", true), HealthObserved, metav1.ConditionTrue, "Healthy"},
		{"running unhealthy", h("running", false), HealthObserved, metav1.ConditionFalse, "InstanceUnhealthy"},
		{"degraded", h("degraded", false), HealthObserved, metav1.ConditionFalse, "InstanceDegraded"},
		{"stopped", h("stopped", false), HealthObserved, metav1.ConditionFalse, "InstanceStopped"},
		{"terminated", h("terminated", false), HealthObserved, metav1.ConditionFalse, "InstanceTerminated"},
		{"unknown", h("unknown", false), HealthObserved, metav1.ConditionUnknown, "HealthUnknown"},
		{"provider_id missing once", h(contract.HealthProviderIDMissing, false), HealthObserved, metav1.ConditionUnknown, "ProviderIDMissing"},
		{"null", nil, HealthObserved, metav1.ConditionUnknown, "HealthUnknown"},
		{"unrecognized state never reads as healthy", h("bogus", true), HealthObserved, metav1.ConditionUnknown, "HealthUnknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			obj := &infrav1.TerraformMachine{}
			SetInfrastructureHealthy(obj, tt.h, tt.phase)
			got := conditions.Get(obj, "InfrastructureHealthy")
			if got == nil || got.Status != tt.status || got.Reason != tt.reason || got.Message == "" {
				t.Errorf("InfrastructureHealthy = %+v, want %s/%s with a message", got, tt.status, tt.reason)
			}
		})
	}
}

// TestSetInfrastructureHealthyMessage proves SetInfrastructureHealthy
// builds the InfrastructureHealthy message from health.message and
// health.reasons, falling back to the reason when both are empty.
func TestSetInfrastructureHealthyMessage(t *testing.T) {
	t.Parallel()
	msg := "disk full"
	tests := []struct {
		h    *contract.Health
		want string
	}{
		{&contract.Health{State: "degraded", Message: &msg, Reasons: []string{"DiskPressure", "Slow"}}, "disk full; reasons: DiskPressure, Slow"},
		{&contract.Health{State: "degraded", Reasons: []string{"DiskPressure"}}, "reasons: DiskPressure"},
		{&contract.Health{State: "degraded", Message: new("")}, "InstanceDegraded"},
	}
	for _, tt := range tests {
		obj := &infrav1.TerraformMachine{}
		SetInfrastructureHealthy(obj, tt.h, HealthObserved)
		if got := conditions.Get(obj, "InfrastructureHealthy").Message; got != tt.want {
			t.Errorf("message = %q, want %q", got, tt.want)
		}
	}
}
