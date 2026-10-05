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

package contract

// InputSpec is how the generated root declares one contract input, and so
// what a module's matching variable must accept.
type InputSpec struct {
	// Type is the Terraform type expression. "any" leaves the precise type
	// to the module.
	Type string
	// Nullable inputs may be null; the generated root gives them a null
	// default.
	Nullable bool
	// Sensitive inputs are declared sensitive.
	Sensitive bool
}

// Type expressions shared by several inputs.
const (
	typeEndpoint       = "object({host=string, port=number})"
	typeClusterNetwork = "object({pods=list(string), services=list(string), service_domain=string, api_server_port=number})"
	typeAutoscaling    = "object({enabled=bool, min=number, max=number})"
)

// InputSpecs returns the declaration of every input of role, common ones
// included: the renderer declares the generated root's variables from it
// and tfcapi-lint checks a module's variables against it, so the two never
// disagree. captf_cluster, captf_object and captf_cluster_outputs are
// "any": the module declares their precise type.
func InputSpecs(role Role) (map[string]InputSpec, error) {
	if err := role.Validate(); err != nil {
		return nil, err
	}
	specs := map[string]InputSpec{
		"captf_contract": {Type: "string"},
		"captf_cluster":  {Type: "any"},
		"captf_object":   {Type: "any"},
		"captf_tags":     {Type: "map(string)"},
	}
	switch role {
	case RoleCluster:
		specs["control_plane_endpoint"] = InputSpec{Type: typeEndpoint, Nullable: true}
		specs["kubernetes_version"] = InputSpec{Type: "string", Nullable: true}
		specs["control_plane_initialized"] = InputSpec{Type: "bool"}
		specs["cluster_network"] = InputSpec{Type: typeClusterNetwork, Nullable: true}
	case RoleMachine:
		specs["captf_cluster_outputs"] = InputSpec{Type: "any"}
		specs["machine_name"] = InputSpec{Type: "string"}
		specs["bootstrap_data"] = InputSpec{Type: "string", Sensitive: true}
		specs["bootstrap_format"] = InputSpec{Type: "string"}
		specs["failure_domain"] = InputSpec{Type: "string", Nullable: true}
		specs["kubernetes_version"] = InputSpec{Type: "string", Nullable: true}
		specs["control_plane"] = InputSpec{Type: "bool"}
	case RoleMachinePool:
		specs["captf_cluster_outputs"] = InputSpec{Type: "any"}
		specs["machinepool_name"] = InputSpec{Type: "string"}
		specs["replicas"] = InputSpec{Type: "number"}
		specs["bootstrap_data"] = InputSpec{Type: "string", Sensitive: true}
		specs["bootstrap_format"] = InputSpec{Type: "string"}
		specs["failure_domains"] = InputSpec{Type: "list(string)"}
		specs["cluster_failure_domains"] = InputSpec{Type: "list(string)"}
		specs["kubernetes_version"] = InputSpec{Type: "string", Nullable: true}
		specs["node_labels"] = InputSpec{Type: "map(string)"}
		specs["autoscaling"] = InputSpec{Type: typeAutoscaling}
	}
	return specs, nil
}
