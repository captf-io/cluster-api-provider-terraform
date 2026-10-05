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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// ownerObj is a minimal metav1.Object fixture: a name, ownerReferences and
// labels, enough for MachineOwnerMismatch and its siblings, which read only
// those three accessors.
type ownerObj struct {
	metav1.Object
	name   string
	refs   []metav1.OwnerReference
	labels map[string]string
}

// GetName returns o's fixture name.
func (o ownerObj) GetName() string { return o.name }

// GetOwnerReferences returns o's fixture ownerReferences.
func (o ownerObj) GetOwnerReferences() []metav1.OwnerReference { return o.refs }

// GetLabels returns o's fixture labels.
func (o ownerObj) GetLabels() map[string]string { return o.labels }

// TestMachineBacksReference proves MachineBacksReference matches only a
// Machine whose spec.infrastructureRef names the TerraformMachine back by
// APIGroup, Kind and Name, and never a nil Machine.
func TestMachineBacksReference(t *testing.T) {
	t.Parallel()
	valid := func() *clusterv1.Machine {
		m := &clusterv1.Machine{}
		m.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{APIGroup: infrav1.GroupVersion.Group, Kind: "TerraformMachine", Name: "tm1"}
		return m
	}
	tests := []struct {
		name    string
		machine *clusterv1.Machine
		want    bool
	}{
		{"matches", valid(), true},
		{"nil Machine", nil, false},
		{"wrong Name", func() *clusterv1.Machine { m := valid(); m.Spec.InfrastructureRef.Name = "other"; return m }(), false},
		{"wrong Kind", func() *clusterv1.Machine { m := valid(); m.Spec.InfrastructureRef.Kind = "AWSMachine"; return m }(), false},
		{"wrong APIGroup", func() *clusterv1.Machine { m := valid(); m.Spec.InfrastructureRef.APIGroup = "other.io"; return m }(), false},
		{"unset infrastructureRef", &clusterv1.Machine{}, false},
	}
	for _, tt := range tests {
		if got := MachineBacksReference(tt.machine, "tm1"); got != tt.want {
			t.Errorf("%s: MachineBacksReference = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestMachinePoolBacksReference mirrors TestMachineBacksReference for
// MachinePool's spec.template.spec.infrastructureRef.
func TestMachinePoolBacksReference(t *testing.T) {
	t.Parallel()
	valid := func() *clusterv1.MachinePool {
		mp := &clusterv1.MachinePool{}
		mp.Spec.Template.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{APIGroup: infrav1.GroupVersion.Group, Kind: "TerraformMachinePool", Name: "tmp1"}
		return mp
	}
	if !MachinePoolBacksReference(valid(), "tmp1") {
		t.Error("matching MachinePool rejected")
	}
	if MachinePoolBacksReference(nil, "tmp1") {
		t.Error("nil MachinePool accepted")
	}
	wrong := valid()
	wrong.Spec.Template.Spec.InfrastructureRef.Name = "other"
	if MachinePoolBacksReference(wrong, "tmp1") {
		t.Error("wrong Name accepted")
	}
}

// TestClusterBacksReference mirrors TestMachineBacksReference for Cluster's
// spec.infrastructureRef.
func TestClusterBacksReference(t *testing.T) {
	t.Parallel()
	valid := func() *clusterv1.Cluster {
		c := &clusterv1.Cluster{}
		c.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{APIGroup: infrav1.GroupVersion.Group, Kind: "TerraformCluster", Name: "tc1"}
		return c
	}
	if !ClusterBacksReference(valid(), "tc1") {
		t.Error("matching Cluster rejected")
	}
	if ClusterBacksReference(nil, "tc1") {
		t.Error("nil Cluster accepted")
	}
	wrong := valid()
	wrong.Spec.InfrastructureRef.Kind = "AWSCluster"
	if ClusterBacksReference(wrong, "tc1") {
		t.Error("wrong Kind accepted")
	}
}

// TestMachineOwnerMismatch proves MachineOwnerMismatch requires the back
// reference, a matching ownerRef UID when one is carried, and an equal
// cluster-name label when one is set, but tolerates a UID-less ownerRef or
// an unset label.
func TestMachineOwnerMismatch(t *testing.T) {
	t.Parallel()
	machine := &clusterv1.Machine{}
	machine.UID = "m-uid"
	machine.Spec.ClusterName = "c1"
	machine.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{APIGroup: infrav1.GroupVersion.Group, Kind: "TerraformMachine", Name: "tm1"}

	tests := []struct {
		name  string
		refs  []metav1.OwnerReference
		label string
		want  bool
	}{
		{"UID matches, label matches", []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: "m1", UID: "m-uid"}}, "c1", true},
		{"no UID on the ownerRef: tolerated", []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: "m1"}}, "c1", true},
		{"no label yet: tolerated", []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: "m1", UID: "m-uid"}}, "", true},
		{"UID mismatch", []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: "m1", UID: "other-uid"}}, "c1", false},
		{"cluster-name mismatch", []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: "m1", UID: "m-uid"}}, "c2", false},
	}
	for _, tt := range tests {
		obj := ownerObjFor("tm1", tt.refs, tt.label)
		if got := MachineOwnerMismatch(machine, obj, tt.label) == ""; got != tt.want {
			t.Errorf("%s: MachineOwnerMismatch = %q, want valid %v", tt.name, MachineOwnerMismatch(machine, obj, tt.label), tt.want)
		}
	}
	if msg := MachineOwnerMismatch(machine, ownerObjFor("someone-elses-tm", nil, ""), ""); msg == "" {
		t.Error("a back-reference to a different name was accepted")
	}
}

// TestMachinePoolOwnerMismatch proves MachinePoolOwnerMismatch applies the
// same three checks as MachineOwnerMismatch to a MachinePool: the template's
// back reference, a matching ownerRef UID, and an equal cluster-name label.
func TestMachinePoolOwnerMismatch(t *testing.T) {
	t.Parallel()
	mp := &clusterv1.MachinePool{}
	mp.UID = "mp-uid"
	mp.Spec.ClusterName = "c1"
	mp.Spec.Template.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{APIGroup: infrav1.GroupVersion.Group, Kind: "TerraformMachinePool", Name: "tmp1"}
	ref := func(uid string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "MachinePool", Name: "mp1", UID: types.UID(uid)}}
	}

	tests := []struct {
		name  string
		obj   metav1.Object
		label string
		want  bool
	}{
		{"UID and label match", ownerObjFor("tmp1", ref("mp-uid"), "c1"), "c1", true},
		{"no UID, no label: tolerated", ownerObjFor("tmp1", ref(""), ""), "", true},
		{"UID mismatch", ownerObjFor("tmp1", ref("other"), "c1"), "c1", false},
		{"cluster-name mismatch", ownerObjFor("tmp1", ref("mp-uid"), "c2"), "c2", false},
		{"no back reference", ownerObjFor("someone-elses-tmp", ref("mp-uid"), "c1"), "c1", false},
	}
	for _, tt := range tests {
		if got := MachinePoolOwnerMismatch(mp, tt.obj, tt.label) == ""; got != tt.want {
			t.Errorf("%s: MachinePoolOwnerMismatch = %q, want valid %v", tt.name, MachinePoolOwnerMismatch(mp, tt.obj, tt.label), tt.want)
		}
	}
	if MachinePoolOwnerMismatch(nil, ownerObjFor("tmp1", nil, ""), "") == "" {
		t.Error("a nil MachinePool was accepted as the owner")
	}
}

// TestClusterOwnerMismatch proves ClusterOwnerMismatch requires the
// Cluster's back reference, a matching ownerRef UID when one is carried,
// and a cluster-name label, when set, that names the owner Cluster itself.
func TestClusterOwnerMismatch(t *testing.T) {
	t.Parallel()
	cluster := &clusterv1.Cluster{}
	cluster.Name, cluster.UID = "c1", "c-uid"
	cluster.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{APIGroup: infrav1.GroupVersion.Group, Kind: "TerraformCluster", Name: "tc1"}
	ref := func(uid string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster", Name: "c1", UID: types.UID(uid)}}
	}

	tests := []struct {
		name  string
		obj   metav1.Object
		label string
		want  bool
	}{
		{"UID and label match", ownerObjFor("tc1", ref("c-uid"), "c1"), "c1", true},
		{"no UID, no label: tolerated", ownerObjFor("tc1", ref(""), ""), "", true},
		{"UID mismatch", ownerObjFor("tc1", ref("other"), "c1"), "c1", false},
		{"label names another Cluster", ownerObjFor("tc1", ref("c-uid"), "c2"), "c2", false},
		{"no back reference", ownerObjFor("someone-elses-tc", ref("c-uid"), "c1"), "c1", false},
	}
	for _, tt := range tests {
		if got := ClusterOwnerMismatch(cluster, tt.obj, tt.label) == ""; got != tt.want {
			t.Errorf("%s: ClusterOwnerMismatch = %q, want valid %v", tt.name, ClusterOwnerMismatch(cluster, tt.obj, tt.label), tt.want)
		}
	}
	if ClusterOwnerMismatch(nil, ownerObjFor("tc1", nil, ""), "") == "" {
		t.Error("a nil Cluster was accepted as the owner")
	}
}

// TestFindOwnerRefSkipsMalformed proves findOwnerRef skips an ownerRef whose
// apiVersion does not parse and still finds a later matching one.
func TestFindOwnerRefSkipsMalformed(t *testing.T) {
	t.Parallel()
	refs := []metav1.OwnerReference{
		{APIVersion: "a/b/c", Kind: "Machine", Name: "bad"},
		{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: "good", UID: "u"},
	}
	if got := findOwnerRef(refs, clusterv1.GroupVersion.Group, "Machine"); got.Name != "good" {
		t.Errorf("findOwnerRef = %q, want good", got.Name)
	}
	if got := findOwnerRef(refs, clusterv1.GroupVersion.Group, "MachinePool"); got.UID != "" {
		t.Errorf("findOwnerRef with no match = %+v, want the zero value", got)
	}
}

// ownerObjFor returns a metav1.Object fixture named name, carrying refs and
// a cluster-name label of label (when set).
func ownerObjFor(name string, refs []metav1.OwnerReference, label string) metav1.Object {
	labels := map[string]string{}
	if label != "" {
		labels[clusterv1.ClusterNameLabel] = label
	}
	return ownerObj{name: name, refs: refs, labels: labels}
}
