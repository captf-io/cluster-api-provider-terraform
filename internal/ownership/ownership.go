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

package ownership

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// MachineBacksReference reports whether machine's spec.infrastructureRef
// names the TerraformMachine called name back: apiGroup
// infrav1.GroupVersion.Group, kind "TerraformMachine", and that name. A nil
// machine never backs a reference.
func MachineBacksReference(machine *clusterv1.Machine, name string) bool {
	if machine == nil {
		return false
	}
	ref := machine.Spec.InfrastructureRef
	return ref.APIGroup == infrav1.GroupVersion.Group && ref.Kind == state.KindTerraformMachine && ref.Name == name
}

// MachinePoolBacksReference reports whether mp's
// spec.template.spec.infrastructureRef names the TerraformMachinePool
// called name back: apiGroup infrav1.GroupVersion.Group, kind
// "TerraformMachinePool", and that name. A nil mp never backs a reference.
func MachinePoolBacksReference(mp *clusterv1.MachinePool, name string) bool {
	if mp == nil {
		return false
	}
	ref := mp.Spec.Template.Spec.InfrastructureRef
	return ref.APIGroup == infrav1.GroupVersion.Group && ref.Kind == state.KindTerraformMachinePool && ref.Name == name
}

// ClusterBacksReference reports whether cluster's spec.infrastructureRef
// names the TerraformCluster called name back: apiGroup
// infrav1.GroupVersion.Group, kind "TerraformCluster", and that name. A nil
// cluster never backs a reference.
func ClusterBacksReference(cluster *clusterv1.Cluster, name string) bool {
	if cluster == nil {
		return false
	}
	ref := cluster.Spec.InfrastructureRef
	return ref.APIGroup == infrav1.GroupVersion.Group && ref.Kind == state.KindTerraformCluster && ref.Name == name
}

// findOwnerRef returns the first of refs whose Kind and APIVersion group
// match kind and group, or the zero metav1.OwnerReference (UID "") when
// none matches; a malformed apiVersion is skipped rather than treated as a
// match.
func findOwnerRef(refs []metav1.OwnerReference, group, kind string) metav1.OwnerReference {
	for _, ref := range refs {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil || gv.Group != group || ref.Kind != kind {
			continue
		}
		return ref
	}
	return metav1.OwnerReference{}
}

// MachineOwnerMismatch checks whether machine is a genuine owner of obj (a
// TerraformMachine): MachineBacksReference, the Machine ownerRef's UID (when
// the ownerRef carries one) matches machine's, and, when clusterLabel is
// set (obj's cluster.x-k8s.io/cluster-name label, what LookupCluster
// resolves the Cluster from), machine.Spec.ClusterName equals it. It
// returns "" when machine is a valid owner, or a message naming the
// specific check that failed (never a UID value) otherwise; a nil machine
// is never valid.
func MachineOwnerMismatch(machine *clusterv1.Machine, obj metav1.Object, clusterLabel string) string {
	if !MachineBacksReference(machine, obj.GetName()) {
		return fmt.Sprintf("Machine %s does not reference this TerraformMachine as its infrastructure", ownerRefName(obj, clusterv1.GroupVersion.Group, "Machine"))
	}
	if ref := findOwnerRef(obj.GetOwnerReferences(), clusterv1.GroupVersion.Group, "Machine"); ref.UID != "" && ref.UID != machine.UID {
		return fmt.Sprintf("the ownerReference to Machine %s does not match its UID", machine.Name)
	}
	if clusterLabel != "" && machine.Spec.ClusterName != clusterLabel {
		return fmt.Sprintf("Machine %s is on a different Cluster than this TerraformMachine's cluster-name label names", machine.Name)
	}
	return ""
}

// MachinePoolOwnerMismatch checks whether mp is a genuine owner of obj (a
// TerraformMachinePool): MachinePoolBacksReference, the MachinePool
// ownerRef's UID (when the ownerRef carries one) matches mp's, and, when
// clusterLabel is set (obj's cluster.x-k8s.io/cluster-name label), mp's
// spec.clusterName equals it. It returns "" when mp is a valid owner, or a
// message naming the specific check that failed (never a UID value)
// otherwise; a nil mp is never valid.
func MachinePoolOwnerMismatch(mp *clusterv1.MachinePool, obj metav1.Object, clusterLabel string) string {
	if !MachinePoolBacksReference(mp, obj.GetName()) {
		return fmt.Sprintf("MachinePool %s does not reference this TerraformMachinePool as its infrastructure", ownerRefName(obj, clusterv1.GroupVersion.Group, "MachinePool"))
	}
	if ref := findOwnerRef(obj.GetOwnerReferences(), clusterv1.GroupVersion.Group, "MachinePool"); ref.UID != "" && ref.UID != mp.UID {
		return fmt.Sprintf("the ownerReference to MachinePool %s does not match its UID", mp.Name)
	}
	if clusterLabel != "" && mp.Spec.ClusterName != clusterLabel {
		return fmt.Sprintf("MachinePool %s is on a different Cluster than this TerraformMachinePool's cluster-name label names", mp.Name)
	}
	return ""
}

// ClusterOwnerMismatch checks whether cluster is a genuine owner of obj (a
// TerraformCluster): ClusterBacksReference, the Cluster ownerRef's UID
// (when the ownerRef carries one) matches cluster's, and, when
// clusterLabel is set (obj's own cluster.x-k8s.io/cluster-name label),
// cluster.Name equals it. It returns "" when cluster is a valid owner, or a
// message naming the specific check that failed (never a UID value)
// otherwise; a nil cluster is never valid.
func ClusterOwnerMismatch(cluster *clusterv1.Cluster, obj metav1.Object, clusterLabel string) string {
	if !ClusterBacksReference(cluster, obj.GetName()) {
		return fmt.Sprintf("Cluster %s does not reference this TerraformCluster as its infrastructure", ownerRefName(obj, clusterv1.GroupVersion.Group, "Cluster"))
	}
	if ref := findOwnerRef(obj.GetOwnerReferences(), clusterv1.GroupVersion.Group, "Cluster"); ref.UID != "" && ref.UID != cluster.UID {
		return fmt.Sprintf("the ownerReference to Cluster %s does not match its UID", cluster.Name)
	}
	if clusterLabel != "" && cluster.Name != clusterLabel {
		return fmt.Sprintf("this TerraformCluster's cluster-name label does not name its owner Cluster %s", cluster.Name)
	}
	return ""
}

// ownerRefName returns the Name of obj's ownerRef matching group and kind
// (for a message about a failed back-reference, before the owner object
// itself is trusted for anything but its name), or "" when none matches.
func ownerRefName(obj metav1.Object, group, kind string) string {
	return findOwnerRef(obj.GetOwnerReferences(), group, kind).Name
}
