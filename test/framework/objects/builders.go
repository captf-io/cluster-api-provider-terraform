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

package objects

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Machine returns a CAPI Machine named name in ns, labeled and named for
// cluster, whose infrastructureRef is a TerraformMachine and whose
// bootstrap.dataSecretName is a pre-made Secret, so no bootstrap provider
// is needed. o adds the optional failure domain and version.
func Machine(ns, name, cluster string, o MachineOpts) *unstructured.Unstructured {
	u := capi("Machine", ns, name)
	u.SetLabels(map[string]string{ClusterNameLabel: cluster})
	spec := machineSpec(cluster, orDefault(o.BootstrapSecret, name+"-bootstrap"),
		KindTerraformMachine, orDefault(o.InfrastructureName, name), o.Version)
	if o.FailureDomain != "" {
		spec["failureDomain"] = o.FailureDomain
	}
	u.Object["spec"] = spec
	return u
}

// MachinePool returns a CAPI MachinePool (cluster.x-k8s.io/v1beta2) named
// name in ns, labeled and named for cluster, with o.Replicas replicas and a
// template whose infrastructureRef is a TerraformMachinePool and whose
// bootstrap.dataSecretName is a pre-made Secret.
func MachinePool(ns, name, cluster string, o MachinePoolOpts) *unstructured.Unstructured {
	u := capi("MachinePool", ns, name)
	u.SetLabels(map[string]string{ClusterNameLabel: cluster})
	u.Object["spec"] = map[string]any{
		"clusterName": cluster,
		"replicas":    o.Replicas,
		"template": map[string]any{
			"spec": machineSpec(cluster, orDefault(o.BootstrapSecret, name+"-bootstrap"),
				KindTerraformMachinePool, orDefault(o.InfrastructureName, name), o.Version),
		},
	}
	return u
}

// TerraformClusterIdentity returns a cluster-scoped TerraformClusterIdentity
// named name whose secretRef is the Secret secretName in secretNS and which
// admits allowedNamespaces (an empty list admits nothing).
func TerraformClusterIdentity(name, secretNS, secretName string, allowedNamespaces []string) *unstructured.Unstructured {
	u := captf(KindTerraformClusterIdentity, "", name)
	list := make([]any, 0, len(allowedNamespaces))
	for _, ns := range allowedNamespaces {
		list = append(list, ns)
	}
	u.Object["spec"] = map[string]any{
		"secretRef":         map[string]any{"name": secretName, "namespace": secretNS},
		"allowedNamespaces": map[string]any{"list": list},
	}
	return u
}

// TerraformCluster returns a TerraformCluster named name in ns running
// o.Image. It sets identityRef and defaults.identityRef to o.Identity (which
// the cluster's machines and pools inherit), spec.drift from the drift
// options, spec.variables and spec.jobs.activeDeadlineSeconds when given.
func TerraformCluster(ns, name string, o TerraformClusterOpts) *unstructured.Unstructured {
	u := captf(KindTerraformCluster, ns, name)
	setSpec(u, "source", map[string]any{"image": o.Image})
	if o.Identity != "" {
		ref := map[string]any{"name": o.Identity}
		setSpec(u, "identityRef", ref)
		setSpec(u, "defaults", map[string]any{"identityRef": map[string]any{"name": o.Identity}})
	}
	drift := map[string]any{}
	if o.DriftIntervalSeconds != 0 {
		drift["intervalSeconds"] = o.DriftIntervalSeconds
	}
	if o.DriftAction != "" {
		drift["action"] = o.DriftAction
	}
	if len(drift) > 0 {
		setSpec(u, "drift", drift)
	}
	if len(o.Variables) > 0 {
		setSpec(u, "variables", o.Variables)
	}
	if jobs := jobsPolicy(o.ActiveDeadlineSeconds); jobs != nil {
		setSpec(u, "jobs", jobs)
	}
	return u
}

// TerraformMachine returns a TerraformMachine named name in ns running
// o.Image. It sets no identityRef: the machine inherits the cluster's
// defaults.identityRef (or its identityRef).
func TerraformMachine(ns, name string, o TerraformMachineOpts) *unstructured.Unstructured {
	u := captf(KindTerraformMachine, ns, name)
	setSpec(u, "source", map[string]any{"image": o.Image})
	if jobs := jobsPolicy(o.ActiveDeadlineSeconds); jobs != nil {
		setSpec(u, "jobs", jobs)
	}
	return u
}

// TerraformMachinePool returns a TerraformMachinePool named name in ns
// running o.Image, inheriting its identity like TerraformMachine. o adds the
// job deadline and the membership refresh interval when non-zero.
func TerraformMachinePool(ns, name string, o TerraformMachinePoolOpts) *unstructured.Unstructured {
	u := captf(KindTerraformMachinePool, ns, name)
	setSpec(u, "source", map[string]any{"image": o.Image})
	if jobs := jobsPolicy(o.ActiveDeadlineSeconds); jobs != nil {
		setSpec(u, "jobs", jobs)
	}
	if o.MembershipRefreshIntervalSeconds != 0 {
		setSpec(u, "membershipRefreshIntervalSeconds", o.MembershipRefreshIntervalSeconds)
	}
	return u
}

// IdentitySecret returns the credential Secret a TerraformClusterIdentity
// points at, named name in ns, holding one dummy value: the noop modules
// need no credentials, but CAPTF requires the Secret to exist.
func IdentitySecret(ns, name string) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"NOOP_CREDENTIAL": []byte("noop")},
	}
}

// BootstrapSecret returns the bootstrap data Secret named name in ns, whose
// key "value" holds value. CAPTF passes its base64 to the module as
// bootstrap_data.
func BootstrapSecret(ns, name, value string) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{BootstrapKey: []byte(value)},
	}
}
