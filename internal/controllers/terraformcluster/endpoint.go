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
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/outputs"
)

// Endpoint provenance.
const (
	// EndpointSourceAnnotation records who owns the control-plane endpoint.
	// It is written at most once and never changed.
	EndpointSourceAnnotation = "captf.io/endpoint-source"
	// EndpointSourceUser: an endpoint existed before the first apply.
	EndpointSourceUser = "user"
	// EndpointSourceModule: the module created the endpoint.
	EndpointSourceModule = "module"
)

// EndpointInput decides the control_plane_endpoint input.
//
//   - With source module the input is null forever, even once CAPI copies
//     the module's endpoint to the Cluster: the module is never told about
//     its own load balancer, and the copy-back never changes the hash.
//   - Otherwise the input is Cluster.spec.controlPlaneEndpoint when valid,
//     else a valid TerraformCluster.spec.controlPlaneEndpoint, else null. So
//     an endpoint a hosted control-plane provider sets later reaches the
//     module, and re-applies it once.
//   - Before the first apply (firstApply), a valid endpoint makes the
//     source user; setSource is then returned for the caller to record.
//
// clusterEP is the owning Cluster's spec.controlPlaneEndpoint and tcEP is
// the TerraformCluster's own spec.controlPlaneEndpoint.
func EndpointInput(source string, firstApply bool, clusterEP clusterv1.APIEndpoint, tcEP *clusterv1.APIEndpoint) (input *contract.Endpoint, setSource string) {
	if source == EndpointSourceModule {
		return nil, ""
	}
	var ep *clusterv1.APIEndpoint
	switch {
	case clusterEP.IsValid():
		ep = &clusterEP
	case tcEP != nil && tcEP.IsValid():
		ep = tcEP
	}
	if ep == nil {
		return nil, ""
	}
	if source == "" && firstApply {
		setSource = EndpointSourceUser
	}
	return &contract.Endpoint{Host: ep.Host, Port: ep.Port}, setSource
}

// ModuleEndpoint decides whether an apply's endpoint output is written to
// spec.controlPlaneEndpoint: only when no source is recorded yet,
// lastRenderedNull (the apply rendered a null input, so the module owned
// the endpoint), output has both host and port, and spec holds no valid
// endpoint. It then also records the source module. The write happens
// once. It returns the endpoint to write, or nil to leave spec unchanged.
func ModuleEndpoint(source string, lastRenderedNull bool, output *contract.Endpoint, spec *clusterv1.APIEndpoint) *clusterv1.APIEndpoint {
	if source != "" || !lastRenderedNull || output == nil || output.Host == "" || output.Port == 0 {
		return nil
	}
	if spec != nil && spec.IsValid() {
		return nil
	}
	return outputs.APIEndpoint(output)
}
