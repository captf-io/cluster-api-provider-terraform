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

package terraformcluster

import (
	"slices"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// ClusterInputs builds the cluster role's inputs for tc, owned by cluster.
// lastCPInitialized is the value last rendered into the durable Secret:
// control_plane_initialized is latched from it because Cluster status does
// not survive clusterctl move. endpoint is the provenance-filtered
// control_plane_endpoint input (EndpointInput). Every field is
// spec-derived and hashed. It returns the built inputs.
func ClusterInputs(cluster *clusterv1.Cluster, tc *infrav1.TerraformCluster, lastCPInitialized bool, endpoint *contract.Endpoint) contract.ClusterInputs {
	cpInit := cluster.Status.Initialization.ControlPlaneInitialized
	in := contract.ClusterInputs{
		CommonInputs: contract.NewCommonInputs(cluster.Name, cluster.Namespace, state.KindTerraformCluster, tc.Name, tc.Namespace,
			tc.Annotations[clusterv1.TemplateClonedFromNameAnnotation]),
		ControlPlaneEndpoint:    endpoint,
		ControlPlaneInitialized: lastCPInitialized || (cpInit != nil && *cpInit),
		ClusterNetwork:          clusterNetwork(cluster.Spec.ClusterNetwork),
	}
	if v := cluster.Spec.Topology.Version; v != "" {
		in.KubernetesVersion = &v
	}
	return in
}

// clusterNetwork maps Cluster.spec.clusterNetwork n to the contract type:
// an unset network is null; an unset CIDR list renders as [] and an unset
// scalar as null. It returns the mapped network, or nil for an unset n.
func clusterNetwork(n clusterv1.ClusterNetwork) *contract.ClusterNetwork {
	if n.APIServerPort == 0 && n.ServiceDomain == "" && len(n.Pods.CIDRBlocks) == 0 && len(n.Services.CIDRBlocks) == 0 {
		return nil
	}
	out := &contract.ClusterNetwork{
		Pods:     slices.Clone(n.Pods.CIDRBlocks),
		Services: slices.Clone(n.Services.CIDRBlocks),
	}
	if n.ServiceDomain != "" {
		out.ServiceDomain = &n.ServiceDomain
	}
	if n.APIServerPort != 0 {
		out.APIServerPort = &n.APIServerPort
	}
	return out
}
