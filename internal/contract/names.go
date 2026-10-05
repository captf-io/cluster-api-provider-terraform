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

// ReservedPrefix starts every variable name the controller owns; modules
// may not declare other captf_ variables.
const ReservedPrefix = "captf_"

// The fixed captf_tags keys, always all present.
const (
	TagCluster   = "captf.io/cluster"
	TagNamespace = "captf.io/namespace"
	TagKind      = "captf.io/kind"
	TagName      = "captf.io/name"
	TagManagedBy = "captf.io/managed-by"
	TagTemplate  = "captf.io/template"

	// ManagedByValue is the value of TagManagedBy.
	ManagedByValue = "captf"
)

// Tags returns captf_tags for an object named name of kind kind, in
// namespace namespace, belonging to cluster cluster. template is the
// object's cluster.x-k8s.io/cloned-from-name annotation, or "" when absent.
func Tags(cluster, namespace, kind, name, template string) map[string]string {
	return map[string]string{
		TagCluster:   cluster,
		TagNamespace: namespace,
		TagKind:      kind,
		TagName:      name,
		TagManagedBy: ManagedByValue,
		TagTemplate:  template,
	}
}

// TagKeys lists the fixed captf_tags keys; it returns them in the fixed
// order TagCluster, TagNamespace, TagKind, TagName, TagManagedBy,
// TagTemplate.
func TagKeys() []string {
	return []string{TagCluster, TagNamespace, TagKind, TagName, TagManagedBy, TagTemplate}
}

// commonInputNames are the captf_ input variables every module role
// declares, in the order InputNames prepends them.
var commonInputNames = []string{
	"captf_contract", "captf_cluster", "captf_object", "captf_tags",
}

// InputNames returns every input variable of role, common inputs first.
func InputNames(role Role) ([]string, error) {
	if err := role.Validate(); err != nil {
		return nil, err
	}
	names := append([]string{}, commonInputNames...)
	switch role {
	case RoleCluster:
		return append(names, "control_plane_endpoint", "kubernetes_version", "control_plane_initialized", "cluster_network"), nil
	case RoleMachine:
		return append(names, "captf_cluster_outputs", "machine_name", "bootstrap_data", "bootstrap_format", "failure_domain", "kubernetes_version", "control_plane"), nil
	default: // RoleMachinePool
		return append(names, "captf_cluster_outputs", "machinepool_name", "replicas", "bootstrap_data", "bootstrap_format", "failure_domains", "cluster_failure_domains", "kubernetes_version", "node_labels", "autoscaling"), nil
	}
}

// RequiredOutputs returns the outputs every module of role must declare.
func RequiredOutputs(role Role) ([]string, error) {
	if err := role.Validate(); err != nil {
		return nil, err
	}
	switch role {
	case RoleCluster:
		return []string{"control_plane_endpoint", "failure_domains", "exports", "health"}, nil
	case RoleMachine:
		return []string{"provider_id", "addresses", "failure_domain", "interruptible", "health"}, nil
	default: // RoleMachinePool
		return []string{"provider_id", "provider_id_list", "replicas", "instances", "health"}, nil
	}
}
