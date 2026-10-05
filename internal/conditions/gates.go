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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// SetDependenciesReady sets the DependenciesReady gating condition on obj to
// status with the given reason, which carries the per-gate reasons Ready
// cannot express. msg is the condition's message; an empty msg defaults to
// reason.
func SetDependenciesReady(obj conditions.Setter, status metav1.ConditionStatus, reason, msg string) {
	if msg == "" {
		msg = reason
	}
	conditions.Set(obj, metav1.Condition{
		Type:    infrav1.DependenciesReadyCondition,
		Status:  status,
		Reason:  reason,
		Message: msg,
	})
}

// SetInitial sets, when missing, the conditions every object carries from
// its first visit, so IgnoreTypesIfMissing in SetReady is only a safety
// net: Paused=False/NotPaused, Deleting=False/NotDeleting,
// DriftDetected=Unknown/DriftNotChecked (no check has
// completed, so nothing is known about drift) and, for the cluster,
// DeletionBlocked=False/NotBlocked. A condition already present is never
// overwritten. Paused is owned by CAPI's paused.EnsurePausedCondition in the
// reconcilers; this only covers an object not seen by it yet. obj is the
// object to set the conditions on; kind names its kind (see KindCluster and
// the other Kind constants), which decides whether DeletionBlocked applies.
func SetInitial(obj conditions.Setter, kind string) {
	initial := []metav1.Condition{
		{Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason},
		{Type: clusterv1.DeletingCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotDeletingReason},
		{
			Type: infrav1.DriftDetectedCondition, Status: metav1.ConditionUnknown, Reason: infrav1.DriftNotCheckedReason,
			Message: "No drift check has completed yet",
		},
	}
	if kind == KindCluster {
		initial = append(initial, metav1.Condition{Type: infrav1.DeletionBlockedCondition, Status: metav1.ConditionFalse, Reason: infrav1.NotBlockedReason})
	}
	for _, c := range initial {
		if !conditions.Has(obj, c.Type) {
			conditions.Set(obj, c)
		}
	}
}
