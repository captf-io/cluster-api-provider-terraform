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

package terraformmachinepool

import (
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
)

// GateInput is what the DependenciesReady gates read.
type GateInput struct {
	// MachinePoolGone is true when the owner MachinePool no longer exists.
	MachinePoolGone bool
	// ClusterFound is true when the Cluster (cluster-name label) exists.
	ClusterFound bool
	// InfrastructureProvisioned is Cluster.status.initialization's field.
	InfrastructureProvisioned bool
	// InfraClusterFound is true when the Cluster's TerraformCluster exists.
	InfraClusterFound bool
	// ClusterOutputsReady is true when the cluster's exports and
	// failure_domains outputs are readable, or the TerraformCluster is
	// externally managed.
	ClusterOutputsReady bool
	// BootstrapReady is true when the bootstrap data Secret has a value.
	BootstrapReady bool
}

// CheckGates returns the first gate of in that is unmet, or nil, through
// shared.CheckGates with MachinePool's wording.
// ClusterNotTerraform and WaitingForOwnerMachinePool are decided earlier,
// by the owner lookup.
func CheckGates(in GateInput) *shared.Gate {
	return shared.CheckGates(shared.GateInput{
		OwnerGone: in.MachinePoolGone, ClusterFound: in.ClusterFound, InfrastructureProvisioned: in.InfrastructureProvisioned,
		InfraClusterFound: in.InfraClusterFound, OutputsReady: in.ClusterOutputsReady, BootstrapReady: in.BootstrapReady,
	}, machinepoolGateText)
}

// machinepoolGateText is the wording of the gates that name a MachinePool.
var machinepoolGateText = shared.GateText{
	OwnerGone:     "The owner MachinePool is gone",
	OutputsWait:   "Waiting for the TerraformCluster's exports and failure_domains outputs to be readable",
	BootstrapWait: "Waiting for the MachinePool's bootstrap data Secret",
}
