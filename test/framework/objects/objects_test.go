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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// field returns the value at path in u, failing t when it is absent.
func field(t *testing.T, u *unstructured.Unstructured, path ...string) any {
	t.Helper()
	v, found, err := unstructured.NestedFieldNoCopy(u.Object, path...)
	if err != nil || !found {
		t.Fatalf("%s %s: field %v missing (err %v)", u.GetKind(), u.GetName(), path, err)
	}
	return v
}

// absent fails t when u has a value at path.
func absent(t *testing.T, u *unstructured.Unstructured, path ...string) {
	t.Helper()
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, path...); found {
		t.Errorf("%s %s: field %v should be absent", u.GetKind(), u.GetName(), path)
	}
}

// want fails t unless the value at path in u deep-equals expected.
func want(t *testing.T, u *unstructured.Unstructured, expected any, path ...string) {
	t.Helper()
	if got := field(t, u, path...); !reflect.DeepEqual(got, expected) {
		t.Errorf("%s %v = %#v, want %#v", u.GetKind(), path, got, expected)
	}
}

// TestCluster checks the CAPI Cluster shape: v1beta2, apiGroup reference,
// no controlPlaneRef.
func TestCluster(t *testing.T) {
	t.Parallel()
	u := Cluster("ns", "c1", ClusterOpts{})
	if u.GetAPIVersion() != "cluster.x-k8s.io/v1beta2" || u.GetKind() != "Cluster" || u.GetNamespace() != "ns" {
		t.Fatalf("type meta: %s %s ns=%s", u.GetAPIVersion(), u.GetKind(), u.GetNamespace())
	}
	want(t, u, []any{"10.244.0.0/16"}, "spec", "clusterNetwork", "pods", "cidrBlocks")
	want(t, u, []any{"10.96.0.0/12"}, "spec", "clusterNetwork", "services", "cidrBlocks")
	want(t, u, int64(6443), "spec", "clusterNetwork", "apiServerPort")
	want(t, u, map[string]any{"apiGroup": "infrastructure.cluster.x-k8s.io", "kind": "TerraformCluster", "name": "c1"}, "spec", "infrastructureRef")
	absent(t, u, "spec", "controlPlaneRef")
	_ = u.DeepCopy() // every value must be deep-copyable JSON
}

// TestClusterOpts checks the Cluster overrides.
func TestClusterOpts(t *testing.T) {
	t.Parallel()
	u := Cluster("ns", "c1", ClusterOpts{PodsCIDR: "10.1.0.0/16", ServicesCIDR: "10.2.0.0/16", APIServerPort: 8443, InfrastructureName: "tc"})
	want(t, u, []any{"10.1.0.0/16"}, "spec", "clusterNetwork", "pods", "cidrBlocks")
	want(t, u, []any{"10.2.0.0/16"}, "spec", "clusterNetwork", "services", "cidrBlocks")
	want(t, u, int64(8443), "spec", "clusterNetwork", "apiServerPort")
	want(t, u, "tc", "spec", "infrastructureRef", "name")
}

// TestMachine checks the Machine shape and its options.
func TestMachine(t *testing.T) {
	t.Parallel()
	u := Machine("ns", "m1", "c1", MachineOpts{FailureDomain: "fd1", Version: "v1.33.0"})
	if u.GetKind() != "Machine" || u.GetLabels()[ClusterNameLabel] != "c1" {
		t.Fatalf("kind/labels: %s %v", u.GetKind(), u.GetLabels())
	}
	want(t, u, "c1", "spec", "clusterName")
	want(t, u, "m1-bootstrap", "spec", "bootstrap", "dataSecretName")
	want(t, u, map[string]any{"apiGroup": "infrastructure.cluster.x-k8s.io", "kind": "TerraformMachine", "name": "m1"}, "spec", "infrastructureRef")
	want(t, u, "fd1", "spec", "failureDomain")
	want(t, u, "v1.33.0", "spec", "version")

	bare := Machine("ns", "m2", "c1", MachineOpts{BootstrapSecret: "boot", InfrastructureName: "tm2"})
	absent(t, bare, "spec", "failureDomain")
	absent(t, bare, "spec", "version")
	want(t, bare, "boot", "spec", "bootstrap", "dataSecretName")
	want(t, bare, "tm2", "spec", "infrastructureRef", "name")
}

// TestMachinePool checks the MachinePool shape.
func TestMachinePool(t *testing.T) {
	t.Parallel()
	u := MachinePool("ns", "p1", "c1", MachinePoolOpts{Replicas: 3})
	if u.GetAPIVersion() != "cluster.x-k8s.io/v1beta2" || u.GetKind() != "MachinePool" || u.GetLabels()[ClusterNameLabel] != "c1" {
		t.Fatalf("meta: %s %s %v", u.GetAPIVersion(), u.GetKind(), u.GetLabels())
	}
	want(t, u, "c1", "spec", "clusterName")
	want(t, u, int64(3), "spec", "replicas")
	want(t, u, "p1-bootstrap", "spec", "template", "spec", "bootstrap", "dataSecretName")
	want(t, u, "c1", "spec", "template", "spec", "clusterName")
	want(t, u, map[string]any{"apiGroup": "infrastructure.cluster.x-k8s.io", "kind": "TerraformMachinePool", "name": "p1"}, "spec", "template", "spec", "infrastructureRef")
	absent(t, u, "spec", "template", "spec", "version")
	_ = u.DeepCopy()

	v := MachinePool("ns", "p2", "c1", MachinePoolOpts{Version: "v1.33.0", BootstrapSecret: "b", InfrastructureName: "tp"})
	want(t, v, "v1.33.0", "spec", "template", "spec", "version")
	want(t, v, "b", "spec", "template", "spec", "bootstrap", "dataSecretName")
	want(t, v, "tp", "spec", "template", "spec", "infrastructureRef", "name")
}

// TestIdentity checks the cluster-scoped identity.
func TestIdentity(t *testing.T) {
	t.Parallel()
	u := TerraformClusterIdentity("id", "captf-system", "creds", []string{"a", "b"})
	if u.GetNamespace() != "" || u.GetKind() != "TerraformClusterIdentity" || u.GetAPIVersion() != "infrastructure.cluster.x-k8s.io/v1alpha1" {
		t.Fatalf("meta: %s %s ns=%q", u.GetAPIVersion(), u.GetKind(), u.GetNamespace())
	}
	want(t, u, map[string]any{"name": "creds", "namespace": "captf-system"}, "spec", "secretRef")
	want(t, u, []any{"a", "b"}, "spec", "allowedNamespaces", "list")
	_ = u.DeepCopy()
}

// TestTerraformCluster checks the TerraformCluster fields and defaults.
func TestTerraformCluster(t *testing.T) {
	t.Parallel()
	u := TerraformCluster("ns", "tc", TerraformClusterOpts{
		Image: "img", Identity: "id", DriftIntervalSeconds: 60, DriftAction: "Report",
		Variables: map[string]any{"x": "y"}, ActiveDeadlineSeconds: 600,
	})
	want(t, u, "img", "spec", "source", "image")
	want(t, u, "id", "spec", "identityRef", "name")
	want(t, u, "id", "spec", "defaults", "identityRef", "name")
	want(t, u, int64(60), "spec", "drift", "intervalSeconds")
	want(t, u, "Report", "spec", "drift", "action")
	want(t, u, map[string]any{"x": "y"}, "spec", "variables")
	want(t, u, int64(600), "spec", "jobs", "activeDeadlineSeconds")
	_ = u.DeepCopy()

	bare := TerraformCluster("ns", "tc", TerraformClusterOpts{Image: "img"})
	for _, p := range []string{"identityRef", "defaults", "drift", "variables", "jobs"} {
		absent(t, bare, "spec", p)
	}
}

// TestTerraformMachine checks the machine and pool builders.
func TestTerraformMachine(t *testing.T) {
	t.Parallel()
	m := TerraformMachine("ns", "tm", TerraformMachineOpts{Image: "img", ActiveDeadlineSeconds: 600})
	want(t, m, "img", "spec", "source", "image")
	want(t, m, int64(600), "spec", "jobs", "activeDeadlineSeconds")
	absent(t, m, "spec", "identityRef")
	bare := TerraformMachine("ns", "tm", TerraformMachineOpts{Image: "img"})
	absent(t, bare, "spec", "jobs")

	p := TerraformMachinePool("ns", "tp", TerraformMachinePoolOpts{Image: "img", ActiveDeadlineSeconds: 600, MembershipRefreshIntervalSeconds: 60})
	want(t, p, "img", "spec", "source", "image")
	want(t, p, int64(600), "spec", "jobs", "activeDeadlineSeconds")
	want(t, p, int64(60), "spec", "membershipRefreshIntervalSeconds")
	absent(t, p, "spec", "identityRef")
	bare2 := TerraformMachinePool("ns", "tp", TerraformMachinePoolOpts{Image: "img"})
	absent(t, bare2, "spec", "jobs")
	absent(t, bare2, "spec", "membershipRefreshIntervalSeconds")
}

// TestSecrets checks the typed Secrets.
func TestSecrets(t *testing.T) {
	t.Parallel()
	id := IdentitySecret("captf-system", "creds")
	if id.Namespace != "captf-system" || id.Name != "creds" || len(id.Data) == 0 {
		t.Errorf("identity secret: %+v", id)
	}
	b := BootstrapSecret("ns", "boot", "#cloud-config\n")
	if string(b.Data["value"]) != "#cloud-config\n" || b.Namespace != "ns" || b.Name != "boot" {
		t.Errorf("bootstrap secret: %+v", b)
	}
}

// TestGVRs checks the GVR table.
func TestGVRs(t *testing.T) {
	t.Parallel()
	for gvr, res := range map[string]string{
		ClusterGVR.String():                  "clusters",
		MachineGVR.String():                  "machines",
		MachinePoolGVR.String():              "machinepools",
		TerraformClusterGVR.String():         "terraformclusters",
		TerraformMachineGVR.String():         "terraformmachines",
		TerraformMachinePoolGVR.String():     "terraformmachinepools",
		TerraformClusterIdentityGVR.String(): "terraformclusteridentities",
		JobGVR.String():                      "jobs",
	} {
		if gvr == "" || res == "" {
			t.Errorf("empty GVR or resource")
		}
	}
	if ClusterGVR.Version != "v1beta2" || TerraformClusterGVR.Version != "v1alpha1" || JobGVR.Group != "batch" {
		t.Errorf("unexpected versions: %v %v %v", ClusterGVR, TerraformClusterGVR, JobGVR)
	}
}

// crdSchema returns the openAPIV3Schema of the CRD file name under
// config/crd/bases, or skips t when the repository layout is unavailable.
func crdSchema(t *testing.T, name string) map[string]any {
	t.Helper()
	path := filepath.Join("..", "..", "..", "config", "crd", "bases", name)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Skipf("CRD %s not present: %v", path, err)
	}
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parse CRD: %v", err)
	}
	spec, _ := crd["spec"].(map[string]any)
	versions, _ := spec["versions"].([]any)
	if len(versions) == 0 {
		t.Fatalf("CRD %s has no versions", name)
	}
	v, _ := versions[0].(map[string]any)
	schema, _ := v["schema"].(map[string]any)
	root, _ := schema["openAPIV3Schema"].(map[string]any)
	if root == nil {
		t.Fatalf("CRD %s has no openAPIV3Schema", name)
	}
	return root
}

// checkKeys fails t for every key of value (a decoded JSON object) that the
// schema node does not declare, recursing into nested objects. A node with
// x-kubernetes-preserve-unknown-fields, or without properties, accepts any
// keys. path locates value for the error message.
func checkKeys(t *testing.T, schema map[string]any, value map[string]any, path string) {
	t.Helper()
	if preserve, _ := schema["x-kubernetes-preserve-unknown-fields"].(bool); preserve {
		return
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		return
	}
	for k, v := range value {
		sub, ok := props[k].(map[string]any)
		if !ok {
			t.Errorf("%s.%s is not in the CRD schema", path, k)
			continue
		}
		if obj, ok := v.(map[string]any); ok {
			checkKeys(t, sub, obj, path+"."+k)
		}
	}
}

// TestSpecKeysExistInCRDs checks that every spec key each CAPTF builder sets
// exists in its CRD schema, so a renamed API field fails here, not live.
func TestSpecKeysExistInCRDs(t *testing.T) {
	t.Parallel()
	cases := map[string]*unstructured.Unstructured{
		"infrastructure.cluster.x-k8s.io_terraformclusters.yaml": TerraformCluster("ns", "tc", TerraformClusterOpts{
			Image: "img", Identity: "id", DriftIntervalSeconds: 60, DriftAction: "Report",
			Variables: map[string]any{"x": "y"}, ActiveDeadlineSeconds: 600,
		}),
		"infrastructure.cluster.x-k8s.io_terraformmachines.yaml": TerraformMachine("ns", "tm", TerraformMachineOpts{Image: "img", ActiveDeadlineSeconds: 600}),
		"infrastructure.cluster.x-k8s.io_terraformmachinepools.yaml": TerraformMachinePool("ns", "tp", TerraformMachinePoolOpts{
			Image: "img", ActiveDeadlineSeconds: 600, MembershipRefreshIntervalSeconds: 60,
		}),
		"infrastructure.cluster.x-k8s.io_terraformclusteridentities.yaml": TerraformClusterIdentity("id", "captf-system", "creds", []string{"ns"}),
	}
	for file, obj := range cases {
		t.Run(obj.GetKind(), func(t *testing.T) {
			t.Parallel()
			root := crdSchema(t, file)
			props, _ := root["properties"].(map[string]any)
			specSchema, _ := props["spec"].(map[string]any)
			spec, _ := obj.Object["spec"].(map[string]any)
			if specSchema == nil || spec == nil {
				t.Fatalf("no spec schema or spec for %s", file)
			}
			checkKeys(t, specSchema, spec, "spec")
		})
	}
}
