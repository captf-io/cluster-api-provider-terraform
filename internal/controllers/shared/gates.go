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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// GateInput is what the DependenciesReady gates of a machine or a pool
// read.
type GateInput struct {
	// OwnerGone is true when the owner Machine or MachinePool no longer
	// exists.
	OwnerGone bool
	// ClusterFound is true when the Cluster (cluster-name label) exists.
	ClusterFound bool
	// InfrastructureProvisioned is Cluster.status.initialization's field.
	InfrastructureProvisioned bool
	// InfraClusterFound is true when the Cluster's TerraformCluster exists.
	InfraClusterFound bool
	// OutputsReady is true when the outputs the kind reads from the
	// cluster's state are readable, or the TerraformCluster is externally
	// managed.
	OutputsReady bool
	// BootstrapReady is true when the bootstrap data Secret has a value.
	BootstrapReady bool
}

// GateText is the wording of the gates that differ between a machine and a
// pool.
type GateText struct {
	// OwnerGone is the message of the OwnerNotFound gate.
	OwnerGone string
	// OutputsWait is the message of the WaitingForClusterExports gate.
	OutputsWait string
	// BootstrapWait is the message of the WaitingForBootstrapData gate.
	BootstrapWait string
}

// CheckGates returns the first gate of in that is unmet, in the order
// owner, Cluster, cluster infrastructure, cluster outputs, bootstrap data,
// or nil. text supplies the messages that name the kind. The owner-kind
// gates ClusterNotTerraform and WaitingForOwner* are decided earlier, by
// the owner lookup.
func CheckGates(in GateInput, text GateText) *Gate {
	switch {
	case in.OwnerGone:
		return gate(metav1.ConditionFalse, infrav1.OwnerNotFoundReason, text.OwnerGone)
	case !in.ClusterFound:
		return gate(metav1.ConditionUnknown, infrav1.WaitingForOwnerReason, "Waiting for the Cluster named by the cluster.x-k8s.io/cluster-name label")
	case !in.InfrastructureProvisioned || !in.InfraClusterFound:
		return gate(metav1.ConditionUnknown, infrav1.WaitingForClusterInfrastructureReason, "Waiting for the cluster infrastructure to be provisioned")
	case !in.OutputsReady:
		return gate(metav1.ConditionUnknown, infrav1.WaitingForClusterExportsReason, text.OutputsWait)
	case !in.BootstrapReady:
		return gate(metav1.ConditionUnknown, infrav1.WaitingForBootstrapDataReason, text.BootstrapWait)
	}
	return nil
}

// gate returns a Gate with the given status, reason and msg.
func gate(status metav1.ConditionStatus, reason, msg string) *Gate {
	return &Gate{Status: status, Reason: reason, Message: msg}
}
