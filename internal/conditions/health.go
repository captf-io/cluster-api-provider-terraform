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
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// HealthPhase is where the object is in its lifecycle when
// InfrastructureHealthy is set.
type HealthPhase int

const (
	// HealthNotStarted is waiting on dependencies, with no apply yet.
	HealthNotStarted HealthPhase = iota
	// HealthApplyStarted is after the first apply started, with no health
	// output yet. False from here is what starts MHC's clock.
	HealthApplyStarted
	// HealthObserved is when the health output was read from state. A null
	// provider_id after provisioning is observed as a terminated
	// health by the machine adapter.
	HealthObserved
)

// SetInfrastructureHealthy sets InfrastructureHealthy on obj from h and
// phase, the object's stage in its lifecycle. h is used only for
// HealthObserved, where nil (a null output) and an unrecognized state are
// Unknown/HealthUnknown. The message is the module's health.message and
// reasons, else the reason.
func SetInfrastructureHealthy(obj conditions.Setter, h *contract.Health, phase HealthPhase) {
	status, reason := healthStatus(h, phase)
	msg := reason
	if phase == HealthObserved && h != nil {
		if m := healthMessage(h); m != "" {
			msg = m
		}
	}
	conditions.Set(obj, metav1.Condition{
		Type:    infrav1.InfrastructureHealthyCondition,
		Status:  status,
		Reason:  reason,
		Message: msg,
	})
}

// healthStatus derives the InfrastructureHealthy status and reason for
// phase, consulting h's state only once phase is HealthObserved; it returns
// that status and reason.
func healthStatus(h *contract.Health, phase HealthPhase) (metav1.ConditionStatus, string) {
	switch phase {
	case HealthNotStarted:
		return metav1.ConditionUnknown, infrav1.WaitingForProvisioningReason
	case HealthApplyStarted:
		return metav1.ConditionFalse, infrav1.ProvisioningReason
	}
	if h == nil {
		return metav1.ConditionUnknown, infrav1.HealthUnknownReason
	}
	switch h.State {
	case contract.HealthPending:
		return metav1.ConditionFalse, infrav1.InstancePendingReason
	case contract.HealthRunning:
		if h.Healthy {
			return metav1.ConditionTrue, infrav1.HealthyReason
		}
		return metav1.ConditionFalse, infrav1.InstanceUnhealthyReason
	case contract.HealthDegraded:
		return metav1.ConditionFalse, infrav1.InstanceDegradedReason
	case contract.HealthStopped:
		return metav1.ConditionFalse, infrav1.InstanceStoppedReason
	case contract.HealthTerminated:
		return metav1.ConditionFalse, infrav1.InstanceTerminatedReason
	case contract.HealthProviderIDMissing:
		return metav1.ConditionUnknown, infrav1.ProviderIDMissingReason
	}
	return metav1.ConditionUnknown, infrav1.HealthUnknownReason
}

// healthMessage joins h's health.message and health.reasons into the
// condition message; it returns that joined string, empty if h has
// neither.
func healthMessage(h *contract.Health) string {
	var parts []string
	if h.Message != nil && *h.Message != "" {
		parts = append(parts, *h.Message)
	}
	if len(h.Reasons) > 0 {
		parts = append(parts, "reasons: "+strings.Join(h.Reasons, ", "))
	}
	return strings.Join(parts, "; ")
}
