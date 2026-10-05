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

package objects

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// API groups and versions.
const (
	// CAPIGroup is the Cluster API core group.
	CAPIGroup = "cluster.x-k8s.io"
	// CAPIVersion is the Cluster API version the builders target.
	CAPIVersion = "v1beta2"
	// CAPTFGroup is the group of the CAPTF kinds (an infrastructure
	// provider group by Cluster API convention).
	CAPTFGroup = "infrastructure.cluster.x-k8s.io"
	// CAPTFVersion is the CAPTF API version.
	CAPTFVersion = "v1alpha1"
	// ClusterNameLabel labels Machines and MachinePools with their Cluster.
	ClusterNameLabel = "cluster.x-k8s.io/cluster-name"
	// BootstrapKey is the data key of a bootstrap Secret CAPTF reads.
	BootstrapKey = "value"
)

// Kind names.
const (
	// KindTerraformCluster is the CAPTF cluster kind.
	KindTerraformCluster = "TerraformCluster"
	// KindTerraformMachine is the CAPTF machine kind.
	KindTerraformMachine = "TerraformMachine"
	// KindTerraformMachinePool is the CAPTF machine pool kind.
	KindTerraformMachinePool = "TerraformMachinePool"
	// KindTerraformClusterIdentity is the CAPTF cluster-scoped identity kind.
	KindTerraformClusterIdentity = "TerraformClusterIdentity"
)

// GVRs of the kinds the suites use. They carry a GVR suffix because the
// builders (Cluster, Machine, ...) own the bare kind names.
var (
	// ClusterGVR is the CAPI Cluster resource.
	ClusterGVR = schema.GroupVersionResource{Group: CAPIGroup, Version: CAPIVersion, Resource: "clusters"}
	// MachineGVR is the CAPI Machine resource.
	MachineGVR = schema.GroupVersionResource{Group: CAPIGroup, Version: CAPIVersion, Resource: "machines"}
	// MachinePoolGVR is the CAPI MachinePool resource.
	MachinePoolGVR = schema.GroupVersionResource{Group: CAPIGroup, Version: CAPIVersion, Resource: "machinepools"}
	// TerraformClusterGVR is the CAPTF TerraformCluster resource.
	TerraformClusterGVR = schema.GroupVersionResource{Group: CAPTFGroup, Version: CAPTFVersion, Resource: "terraformclusters"}
	// TerraformMachineGVR is the CAPTF TerraformMachine resource.
	TerraformMachineGVR = schema.GroupVersionResource{Group: CAPTFGroup, Version: CAPTFVersion, Resource: "terraformmachines"}
	// TerraformMachinePoolGVR is the CAPTF TerraformMachinePool resource.
	TerraformMachinePoolGVR = schema.GroupVersionResource{Group: CAPTFGroup, Version: CAPTFVersion, Resource: "terraformmachinepools"}
	// TerraformClusterIdentityGVR is the cluster-scoped CAPTF identity resource.
	TerraformClusterIdentityGVR = schema.GroupVersionResource{Group: CAPTFGroup, Version: CAPTFVersion, Resource: "terraformclusteridentities"}
	// JobGVR is the batch/v1 Job resource.
	JobGVR = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
)

// ClusterOpts tunes Cluster. Zero values fall back to the defaults noted.
type ClusterOpts struct {
	// PodsCIDR is the pod network CIDR; default 10.244.0.0/16.
	PodsCIDR string
	// ServicesCIDR is the service network CIDR; default 10.96.0.0/12.
	ServicesCIDR string
	// APIServerPort is clusterNetwork.apiServerPort; default 6443.
	APIServerPort int64
	// InfrastructureName is the TerraformCluster name; default the
	// Cluster's name.
	InfrastructureName string
}

// MachineOpts tunes Machine.
type MachineOpts struct {
	// InfrastructureName is the TerraformMachine name; default the
	// Machine's name.
	InfrastructureName string
	// BootstrapSecret is bootstrap.dataSecretName; default "<name>-bootstrap".
	BootstrapSecret string
	// FailureDomain is spec.failureDomain, left out when empty.
	FailureDomain string
	// Version is spec.version, left out when empty.
	Version string
}

// MachinePoolOpts tunes MachinePool.
type MachinePoolOpts struct {
	// InfrastructureName is the TerraformMachinePool name; default the
	// MachinePool's name.
	InfrastructureName string
	// BootstrapSecret is the template's bootstrap.dataSecretName; default
	// "<name>-bootstrap".
	BootstrapSecret string
	// Replicas is spec.replicas.
	Replicas int64
	// Version is the template's spec.version, left out when empty.
	Version string
}

// TerraformClusterOpts tunes TerraformCluster.
type TerraformClusterOpts struct {
	// Image is spec.source.image.
	Image string
	// Identity is the TerraformClusterIdentity name, set as identityRef and
	// defaults.identityRef; left out when empty.
	Identity string
	// DriftIntervalSeconds is spec.drift.intervalSeconds; left out when 0.
	DriftIntervalSeconds int64
	// DriftAction is spec.drift.action (Report or Remediate); left out when
	// empty.
	DriftAction string
	// Variables is spec.variables; left out when empty.
	Variables map[string]any
	// ActiveDeadlineSeconds is spec.jobs.activeDeadlineSeconds; left out
	// when 0.
	ActiveDeadlineSeconds int64
}

// TerraformMachineOpts tunes TerraformMachine.
type TerraformMachineOpts struct {
	// Image is spec.source.image.
	Image string
	// ActiveDeadlineSeconds is spec.jobs.activeDeadlineSeconds; left out
	// when 0.
	ActiveDeadlineSeconds int64
}

// TerraformMachinePoolOpts tunes TerraformMachinePool.
type TerraformMachinePoolOpts struct {
	// Image is spec.source.image.
	Image string
	// ActiveDeadlineSeconds is spec.jobs.activeDeadlineSeconds; left out
	// when 0.
	ActiveDeadlineSeconds int64
	// MembershipRefreshIntervalSeconds is
	// spec.membershipRefreshIntervalSeconds; left out when 0 (the CRD
	// minimum is 15).
	MembershipRefreshIntervalSeconds int64
}

// object returns an empty object of apiVersion and kind named name; ns is
// its namespace, empty for a cluster-scoped kind.
func object(apiVersion, kind, ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetName(name)
	if ns != "" {
		u.SetNamespace(ns)
	}
	return u
}

// capi returns an empty CAPI object of kind in ns named name.
func capi(kind, ns, name string) *unstructured.Unstructured {
	return object(CAPIGroup+"/"+CAPIVersion, kind, ns, name)
}

// captf returns an empty CAPTF object of kind in ns named name.
func captf(kind, ns, name string) *unstructured.Unstructured {
	return object(CAPTFGroup+"/"+CAPTFVersion, kind, ns, name)
}

// infraRef returns a v1beta2 infrastructureRef to the CAPTF kind named name.
func infraRef(kind, name string) map[string]any {
	return map[string]any{"apiGroup": CAPTFGroup, "kind": kind, "name": name}
}

// orDefault returns v, or def when v is empty.
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// jobsPolicy returns the spec.jobs map for deadline, nil when deadline is 0.
func jobsPolicy(deadline int64) map[string]any {
	if deadline == 0 {
		return nil
	}
	return map[string]any{"activeDeadlineSeconds": deadline}
}

// setSpec sets spec.<key> to v on u, ignoring a nil v.
func setSpec(u *unstructured.Unstructured, key string, v any) {
	if v == nil {
		return
	}
	spec, _ := u.Object["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
		u.Object["spec"] = spec
	}
	spec[key] = v
}

// Cluster returns a CAPI Cluster named name in ns whose infrastructureRef
// is the TerraformCluster named by o. It has no controlPlaneRef.
func Cluster(ns, name string, o ClusterOpts) *unstructured.Unstructured {
	u := capi("Cluster", ns, name)
	setSpec(u, "clusterNetwork", map[string]any{
		"pods":          map[string]any{"cidrBlocks": []any{orDefault(o.PodsCIDR, "10.244.0.0/16")}},
		"services":      map[string]any{"cidrBlocks": []any{orDefault(o.ServicesCIDR, "10.96.0.0/12")}},
		"apiServerPort": orInt(o.APIServerPort, 6443),
	})
	setSpec(u, "infrastructureRef", infraRef(KindTerraformCluster, orDefault(o.InfrastructureName, name)))
	return u
}

// orInt returns v, or def when v is 0.
func orInt(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}

// machineSpec returns the CAPI MachineSpec shared by Machine and the
// MachinePool template: clusterName (cluster), bootstrap (the Secret
// bootstrap) and infrastructureRef (infraKind and infraName); version is
// added when not empty.
func machineSpec(cluster, bootstrap, infraKind, infraName, version string) map[string]any {
	spec := map[string]any{
		"clusterName":       cluster,
		"bootstrap":         map[string]any{"dataSecretName": bootstrap},
		"infrastructureRef": infraRef(infraKind, infraName),
	}
	if version != "" {
		spec["version"] = version
	}
	return spec
}
