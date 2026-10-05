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
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
)

// GateInput is what the DependenciesReady gates read.
type GateInput struct {
	// MachineGone is true when the owner Machine no longer exists.
	MachineGone bool
	// ClusterFound is true when the Cluster (cluster-name label) exists.
	ClusterFound bool
	// InfrastructureProvisioned is Cluster.status.initialization's field.
	InfrastructureProvisioned bool
	// InfraClusterFound is true when the Cluster's TerraformCluster exists.
	InfraClusterFound bool
	// ExportsReady is true when the cluster's exports output is readable,
	// or the TerraformCluster is externally managed.
	ExportsReady bool
	// BootstrapReady is true when the bootstrap data Secret has a value.
	BootstrapReady bool
}

// CheckGates returns the first gate of in that is unmet, or nil, through
// shared.CheckGates with Machine's wording.
// ClusterNotTerraform and WaitingForOwnerMachine are decided earlier, by
// the owner lookup.
func CheckGates(in GateInput) *shared.Gate {
	return shared.CheckGates(shared.GateInput{
		OwnerGone: in.MachineGone, ClusterFound: in.ClusterFound, InfrastructureProvisioned: in.InfrastructureProvisioned,
		InfraClusterFound: in.InfraClusterFound, OutputsReady: in.ExportsReady, BootstrapReady: in.BootstrapReady,
	}, machineGateText)
}

// machineGateText is the wording of the gates that name a Machine.
var machineGateText = shared.GateText{
	OwnerGone:     "The owner Machine is gone",
	OutputsWait:   "Waiting for the TerraformCluster's exports output to be readable",
	BootstrapWait: "Waiting for the Machine's bootstrap data Secret",
}
