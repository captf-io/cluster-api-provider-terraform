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

import (
	"encoding/json"
)

// Cluster is captf_cluster: the owning CAPI Cluster.
type Cluster struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// Object is captf_object: the Terraform* object being reconciled.
type Object struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// CommonInputs are the inputs every role receives, except
// captf_cluster_outputs, which only machine roles receive.
// Labels, annotations, uid and generation are deliberately absent: object
// metadata is never rendered, so a move or a metadata edit never re-applies.
type CommonInputs struct {
	// Contract is captf_contract, always Version.
	Contract string `json:"captf_contract"`
	// Cluster is captf_cluster.
	Cluster Cluster `json:"captf_cluster"`
	// Object is captf_object.
	Object Object `json:"captf_object"`
	// Tags is captf_tags; see Tags for the fixed keys.
	Tags map[string]string `json:"captf_tags"`
	// Variables are the user variables (spec.variables, variablesFrom).
	// They are not marshaled with the contract inputs: render adds them to
	// the tfvars and the module call, and hash covers them separately.
	Variables Variables `json:"-"`
}

// NewCommonInputs returns the CommonInputs of the object of kind kind named
// name in namespace, owned by the CAPI Cluster named cluster in
// clusterNamespace: Contract is Version, and Tags is built by Tags with
// template, the object's cluster.x-k8s.io/cloned-from-name annotation or ""
// when absent. Variables is left empty for the caller to fill.
func NewCommonInputs(cluster, clusterNamespace, kind, name, namespace, template string) CommonInputs {
	return CommonInputs{
		Contract: Version,
		Cluster:  Cluster{Name: cluster, Namespace: clusterNamespace},
		Object:   Object{Kind: kind, Name: name, Namespace: namespace},
		Tags:     Tags(cluster, namespace, kind, name, template),
	}
}

// Endpoint is an API endpoint: control_plane_endpoint.
type Endpoint struct {
	Host string `json:"host"`
	Port int32  `json:"port"`
}

// ClusterNetwork is cluster_network. Every attribute is always present; an
// unset CIDR list renders as [], an unset scalar as null.
type ClusterNetwork struct {
	Pods          []string `json:"pods"`
	Services      []string `json:"services"`
	ServiceDomain *string  `json:"service_domain"`
	APIServerPort *int32   `json:"api_server_port"`
}

// MarshalJSON renders nil CIDR lists as [], as the schema requires; it
// returns the encoded JSON, or an error from the underlying json.Marshal.
func (n ClusterNetwork) MarshalJSON() ([]byte, error) {
	type plain ClusterNetwork
	p := plain(n)
	if p.Pods == nil {
		p.Pods = []string{}
	}
	if p.Services == nil {
		p.Services = []string{}
	}
	return json.Marshal(p)
}

// ClusterInputs are the cluster role's inputs.
type ClusterInputs struct {
	CommonInputs
	// ControlPlaneEndpoint is an endpoint the module does not own, or nil.
	ControlPlaneEndpoint *Endpoint `json:"control_plane_endpoint"`
	// KubernetesVersion is Cluster.spec.topology.version, or nil.
	KubernetesVersion *string `json:"kubernetes_version"`
	// ControlPlaneInitialized is the latched
	// Cluster.status.initialization.controlPlaneInitialized.
	ControlPlaneInitialized bool `json:"control_plane_initialized"`
	// ClusterNetwork is Cluster.spec.clusterNetwork, or nil.
	ClusterNetwork *ClusterNetwork `json:"cluster_network"`
}

// MachineInputs are the machine role's inputs.
type MachineInputs struct {
	CommonInputs
	// ClusterOutputs is captf_cluster_outputs: the cluster role's exports
	// output, verbatim; {} when the TerraformCluster is externally managed.
	// The cluster role has no such input: its root declares no variable
	// and its tfvars carry no key.
	ClusterOutputs json.RawMessage `json:"captf_cluster_outputs"`
	// MachineName is the owning CAPI Machine's name.
	MachineName string `json:"machine_name"`
	// BootstrapData is the base64 (standard, padded) bootstrap Secret value.
	// It is sensitive: never log it.
	BootstrapData string `json:"bootstrap_data"`
	// BootstrapFormat is cloud-config or ignition.
	BootstrapFormat string `json:"bootstrap_format"`
	// FailureDomain is Machine.spec.failureDomain, or nil.
	FailureDomain *string `json:"failure_domain"`
	// KubernetesVersion is Machine.spec.version, or nil.
	KubernetesVersion *string `json:"kubernetes_version"`
	// ControlPlane is true for control-plane Machines.
	ControlPlane bool `json:"control_plane"`
}

// Autoscaling is the autoscaling input: the pool's only autoscaling
// switch, parsed from the MachinePool's autoscaler min/max-size
// annotations (machinepool.md). Enabled is false with Min and Max both 0
// when the annotations are absent, incomplete or invalid.
type Autoscaling struct {
	Enabled bool  `json:"enabled"`
	Min     int32 `json:"min"`
	Max     int32 `json:"max"`
}

// MachinePoolInputs are the machinepool role's inputs.
type MachinePoolInputs struct {
	CommonInputs
	// ClusterOutputs is captf_cluster_outputs: the cluster role's exports
	// output, verbatim; {} when the TerraformCluster is externally managed.
	ClusterOutputs json.RawMessage `json:"captf_cluster_outputs"`
	// MachinePoolName is the owning CAPI MachinePool's name.
	MachinePoolName string `json:"machinepool_name"`
	// Replicas is the group's desired capacity: MachinePool.spec.replicas
	// with autoscaling disabled, or the observed replicas output on every
	// apply after the first once autoscaling is enabled.
	Replicas int32 `json:"replicas"`
	// BootstrapData is the base64 (standard, padded) bootstrap Secret
	// value. It is sensitive: never log it. Rotates roughly every 7.5
	// minutes for MachinePools; see machinepool.md "Bootstrap rotation".
	BootstrapData string `json:"bootstrap_data"`
	// BootstrapFormat is cloud-config or ignition.
	BootstrapFormat string `json:"bootstrap_format"`
	// FailureDomains is MachinePool.spec.failureDomains; may be empty.
	FailureDomains []string `json:"failure_domains"`
	// ClusterFailureDomains are the cluster's own failure-domain names,
	// read from cluster state at render time; may be empty.
	ClusterFailureDomains []string `json:"cluster_failure_domains"`
	// KubernetesVersion is MachinePool.spec.template.spec.version, or nil.
	KubernetesVersion *string `json:"kubernetes_version"`
	// NodeLabels is MachinePool.spec.template.metadata.labels verbatim;
	// {} when absent.
	NodeLabels map[string]string `json:"node_labels"`
	// Autoscaling is the pool's autoscaling switch.
	Autoscaling Autoscaling `json:"autoscaling"`
}

// MarshalJSON renders nil FailureDomains/ClusterFailureDomains as [] and a
// nil NodeLabels as {}, as the schema requires; it returns the encoded
// JSON, or an error from the underlying json.Marshal.
func (in MachinePoolInputs) MarshalJSON() ([]byte, error) {
	type plain MachinePoolInputs
	p := plain(in)
	if p.FailureDomains == nil {
		p.FailureDomains = []string{}
	}
	if p.ClusterFailureDomains == nil {
		p.ClusterFailureDomains = []string{}
	}
	if p.NodeLabels == nil {
		p.NodeLabels = map[string]string{}
	}
	return json.Marshal(p)
}

// HashViewer is implemented by an inputs kind whose hashed view differs
// from what is rendered to the module: hash.Inputs hashes HashView()
// instead of the inputs themselves when a kind implements it.
type HashViewer interface {
	// HashView returns the value to hash in place of the receiver.
	HashView() any
}

// HashView returns in unchanged, except that with Autoscaling.Enabled it
// zeroes Replicas: the observed desired count then comes from the module's
// own refresh, not a user edit, so it must not trigger a re-apply
// (machinepool.md "autoscaling" input; "Write-back"). Value receiver, so
// both MachinePoolInputs and *MachinePoolInputs satisfy HashViewer.
func (in MachinePoolInputs) HashView() any {
	if in.Autoscaling.Enabled {
		in.Replicas = 0
		return in
	}
	return in
}

// ApprovalViewer is implemented by an inputs kind whose destructive-plan
// approval (an ExportsChange TerraformPlan) covers less than its inputs: hash.Approval hashes ApprovalView() in place of the inputs
// themselves, so a change outside the view keeps an approval valid.
type ApprovalViewer interface {
	// ApprovalView returns the inputs an approval covers.
	ApprovalView() any
}

// ApprovalView returns in with BootstrapData cleared: the bootstrap
// provider rotates it roughly every 7.5 minutes (machinepool.md "Bootstrap
// rotation"), and an approval must outlive the rotations between the block
// and the operator's annotation. Every other input, the cluster's exports
// included, stays covered; hash.Inputs still applies HashView to the
// result. Value receiver, so both MachinePoolInputs and *MachinePoolInputs
// satisfy ApprovalViewer.
func (in MachinePoolInputs) ApprovalView() any {
	in.BootstrapData = ""
	return in
}
