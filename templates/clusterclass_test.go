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

package templates_test

import (
	"context"
	"os"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	bootstrapv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
	controlplanev1 "sigs.k8s.io/cluster-api/api/controlplane/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	"sigs.k8s.io/cluster-api/core/webhooks/admission"
	"sigs.k8s.io/cluster-api/exp/topology/desiredstate"
	"sigs.k8s.io/cluster-api/exp/topology/scope"
	"sigs.k8s.io/cluster-api/feature"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/webhooks"
)

// classObjects uses t to decode clusterclass-noop.yaml, which must carry
// no clusterctl variables and no namespaces (contracts/clusterctl.md). It
// returns the decoded objects.
func classObjects(t *testing.T) []runtime.Object {
	t.Helper()
	raw, err := os.ReadFile("clusterclass-noop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if varRef.Match(raw) {
		t.Fatal("clusterclass-noop.yaml uses ${VARIABLES}; a ClusterClass file must not")
	}
	objs := decode(t, raw)
	for _, o := range objs {
		if ns := o.(interface{ GetNamespace() string }).GetNamespace(); ns != "" {
			t.Errorf("%T has namespace %q", o, ns)
		}
	}
	return objs
}

// TestClusterClass: the class passes CAPI's own ClusterClass admission
// (template references, variable schemas, patch paths and variable use),
// every template it references is in the file, the Terraform templates pass
// our webhooks, and the control-plane defaults match cluster-template.yaml.
func TestClusterClass(t *testing.T) {
	// Not parallel: it enables the ClusterTopology feature gate.
	if err := feature.MutableGates.Set("ClusterTopology=true"); err != nil {
		t.Fatal(err)
	}
	objs := classObjects(t)
	if len(objs) != 6 {
		t.Fatalf("%d objects, want 6", len(objs))
	}
	cc := objectsOf[*clusterv1.ClusterClass](objs)[0]
	if _, err := (&admission.ClusterClass{}).ValidateCreate(context.Background(), cc); err != nil {
		t.Fatalf("CAPI ClusterClass webhook: %v", err)
	}
	// The webhook really checks patches: one naming an undefined variable
	// is rejected.
	bad := cc.DeepCopy()
	bad.Spec.Patches[1].Definitions[0].JSONPatches[0].ValueFrom.Variable = "undefinedVariable"
	if _, err := (&admission.ClusterClass{}).ValidateCreate(context.Background(), bad); err == nil {
		t.Error("CAPI ClusterClass webhook accepted a patch with an undefined variable")
	}
	refs := []clusterv1.ClusterClassTemplateReference{
		cc.Spec.Infrastructure.TemplateRef, cc.Spec.ControlPlane.TemplateRef,
		cc.Spec.ControlPlane.MachineInfrastructure.TemplateRef,
		cc.Spec.Workers.MachineDeployments[0].Bootstrap.TemplateRef,
		cc.Spec.Workers.MachineDeployments[0].Infrastructure.TemplateRef,
	}
	for _, ref := range refs {
		if !slicesContainsRef(objs, ref) {
			t.Errorf("templateRef %+v is not in the file", ref)
		}
	}
	for _, v := range cc.Spec.Variables {
		if v.Required == nil || !*v.Required {
			t.Errorf("variable %s is optional; images and identity have no safe default", v.Name)
		}
	}

	tct := objectsOf[*infrav1.TerraformClusterTemplate](objs)[0]
	if _, err := (&webhooks.TerraformClusterTemplate{}).ValidateCreate(context.Background(), tct); err != nil {
		t.Errorf("TerraformClusterTemplate webhook: %v", err)
	}
	for _, tmt := range objectsOf[*infrav1.TerraformMachineTemplate](objs) {
		w := &webhooks.TerraformMachineTemplate{}
		if _, err := w.ValidateCreate(context.Background(), tmt); err != nil {
			t.Errorf("%s webhook: %v", tmt.Name, err)
		}
	}
	kcpt := objectsOf[*controlplanev1.KubeadmControlPlaneTemplate](objs)[0].Spec.Template.Spec
	if kcpt.Remediation.MaxRetry == nil || *kcpt.Remediation.MaxRetry != 3 ||
		kcpt.Rollout.Strategy.RollingUpdate.MaxSurge == nil || kcpt.Rollout.Strategy.RollingUpdate.MaxSurge.IntValue() != 0 {
		t.Errorf("KubeadmControlPlaneTemplate remediation %+v rollout %+v", kcpt.Remediation, kcpt.Rollout)
	}
}

// TestClusterClassPatches runs CAPI's own topology desired-state generator,
// which applies the ClusterClass patches, for the Cluster in
// cluster-template-clusterclass.yaml, with the default and with overridden
// health-check timeouts. The TerraformCluster gets clusterImage and the
// identity as its own and as the machines' default; both machine templates
// get machineImage; the KubeadmControlPlane keeps the template's safeguards;
// the MachineHealthChecks CAPI derives carry the flavor's timeouts.
func TestClusterClassPatches(t *testing.T) {
	// Not parallel: the generator needs the process-global ClusterTopology
	// gate.
	if err := feature.MutableGates.Set("ClusterTopology=true"); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name                                     string
		vars                                     map[string]string
		worker, startup, control, controlStartup int32
	}{
		{"defaults", clusterVars, 1800, 1200, 3600, 1800},
		{"overrides", with(map[string]string{
			"TERRAFORM_UNHEALTHY_TIMEOUT":       "2400",
			"TERRAFORM_NODE_STARTUP_TIMEOUT":    "900",
			"TERRAFORM_CP_UNHEALTHY_TIMEOUT":    "7200",
			"TERRAFORM_CP_NODE_STARTUP_TIMEOUT": "2700",
		}), 2400, 900, 7200, 2700},
	} {
		desired := generateDesired(t, tt.vars)

		var tc infrav1.TerraformCluster
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(desired.InfrastructureCluster.Object, &tc); err != nil {
			t.Fatal(err)
		}
		if tc.Spec.Source.Image != clusterVars["TERRAFORM_CLUSTER_IMAGE"] || tc.Spec.IdentityRef.Name != "example" ||
			tc.Spec.Defaults == nil || tc.Spec.Defaults.IdentityRef.Name != "example" {
			t.Errorf("%s: patched TerraformCluster spec = %+v", tt.name, tc.Spec)
		}
		machineTemplates := []*unstructured.Unstructured{desired.ControlPlane.InfrastructureMachineTemplate}
		for _, md := range desired.MachineDeployments {
			machineTemplates = append(machineTemplates, md.InfrastructureMachineTemplate)
		}
		if len(machineTemplates) != 2 {
			t.Fatalf("%s: %d machine templates, want 2", tt.name, len(machineTemplates))
		}
		for _, u := range machineTemplates {
			var tmt infrav1.TerraformMachineTemplate
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &tmt); err != nil {
				t.Fatal(err)
			}
			if got := tmt.Spec.Template.Spec.Source.Image; got != clusterVars["TERRAFORM_MACHINE_IMAGE"] {
				t.Errorf("%s: machine template %s image %q, want machineImage", tt.name, tmt.Name, got)
			}
		}

		var kcp controlplanev1.KubeadmControlPlane
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(desired.ControlPlane.Object.Object, &kcp); err != nil {
			t.Fatal(err)
		}
		if kcp.Spec.Replicas == nil || *kcp.Spec.Replicas != 3 || kcp.Spec.Remediation.MaxRetry == nil || *kcp.Spec.Remediation.MaxRetry != 3 ||
			kcp.Spec.Rollout.Strategy.RollingUpdate.MaxSurge == nil || kcp.Spec.Rollout.Strategy.RollingUpdate.MaxSurge.IntValue() != 0 {
			t.Errorf("%s: desired KubeadmControlPlane replicas %v remediation %+v rollout %+v",
				tt.name, kcp.Spec.Replicas, kcp.Spec.Remediation, kcp.Spec.Rollout)
		}

		mhcs := []*clusterv1.MachineHealthCheck{desired.ControlPlane.MachineHealthCheck}
		for _, md := range desired.MachineDeployments {
			mhcs = append(mhcs, md.MachineHealthCheck)
		}
		if slices.Contains(mhcs, nil) {
			t.Fatalf("%s: the generator produced no MachineHealthCheck for some of %d objects", tt.name, len(mhcs))
		}
		checkMHCs(t, mhcs, tt.worker, tt.startup, tt.control, tt.controlStartup)
	}
}

// generateDesired uses t to render cluster-template-clusterclass.yaml
// with vars and runs CAPI's topology desired-state generator against
// clusterclass-noop. It returns the generated ClusterState.
func generateDesired(t *testing.T, vars map[string]string) *scope.ClusterState {
	t.Helper()
	objs := classObjects(t)
	cc := objectsOf[*clusterv1.ClusterClass](objs)[0]
	// The ClusterClass controller publishes the variable definitions in
	// status; the generator reads them from there.
	for _, v := range cc.Spec.Variables {
		cc.Status.Variables = append(cc.Status.Variables, clusterv1.ClusterClassStatusVariable{
			Name: v.Name,
			Definitions: []clusterv1.ClusterClassStatusVariableDefinition{{
				From: clusterv1.VariableDefinitionFromInline, Required: v.Required, Schema: v.Schema,
			}},
		})
	}

	rendered, err := render("cluster-template-clusterclass.yaml", vars)
	if err != nil {
		t.Fatal(err)
	}
	clusters := objectsOf[*clusterv1.Cluster](decode(t, rendered))
	if len(clusters) != 1 {
		t.Fatalf("%d objects in cluster-template-clusterclass.yaml, want one Cluster", len(clusters))
	}
	cluster := clusters[0]
	cluster.Namespace = "team-a"
	if cluster.Spec.Topology.ClassRef.Name != cc.Name {
		t.Fatalf("topology class %q, want %q", cluster.Spec.Topology.ClassRef.Name, cc.Name)
	}

	s := scope.New(cluster)
	s.Blueprint = &scope.ClusterBlueprint{
		Topology:                      cluster.Spec.Topology,
		ClusterClass:                  cc,
		InfrastructureClusterTemplate: toUnstructured(t, objectsOf[*infrav1.TerraformClusterTemplate](objs)[0]),
		ControlPlane: &scope.ControlPlaneBlueprint{
			Template:                      toUnstructured(t, objectsOf[*controlplanev1.KubeadmControlPlaneTemplate](objs)[0]),
			InfrastructureMachineTemplate: toUnstructured(t, templateNamed(t, objs, cc.Spec.ControlPlane.MachineInfrastructure.TemplateRef.Name)),
			HealthCheck:                   cc.Spec.ControlPlane.HealthCheck,
		},
		MachineDeployments: map[string]*scope.MachineDeploymentBlueprint{
			"default-worker": {
				BootstrapTemplate:             toUnstructured(t, objectsOf[*bootstrapv1.KubeadmConfigTemplate](objs)[0]),
				InfrastructureMachineTemplate: toUnstructured(t, templateNamed(t, objs, cc.Spec.Workers.MachineDeployments[0].Infrastructure.TemplateRef.Name)),
				HealthCheck:                   cc.Spec.Workers.MachineDeployments[0].HealthCheck,
			},
		},
	}
	// The generator reads each referenced kind's contract version from its
	// CRD's cluster.x-k8s.io/v1beta2 label.
	crdScheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(crdScheme); err != nil {
		t.Fatal(err)
	}
	var crds []client.Object
	for name, version := range map[string]string{
		"kubeadmcontrolplanes.controlplane.cluster.x-k8s.io":         "v1beta2",
		"kubeadmconfigtemplates.bootstrap.cluster.x-k8s.io":          "v1beta2",
		"terraformclusters.infrastructure.cluster.x-k8s.io":          "v1alpha1",
		"terraformmachinetemplates.infrastructure.cluster.x-k8s.io":  "v1alpha1",
		"terraformclustertemplates.infrastructure.cluster.x-k8s.io":  "v1alpha1",
		"kubeadmcontrolplanetemplates.controlplane.cluster.x-k8s.io": "v1beta2",
	} {
		crds = append(crds, &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: map[string]string{"cluster.x-k8s.io/v1beta2": version},
		}})
	}
	gen, err := desiredstate.NewGenerator(fake.NewClientBuilder().WithScheme(crdScheme).WithObjects(crds...).Build(),
		clustercache.NewFakeEmptyClusterCache(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := gen.Generate(context.Background(), s)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return desired
}

// templateNamed uses t to find the TerraformMachineTemplate named name
// among objs, failing t when none matches. It returns the found template.
func templateNamed(t *testing.T, objs []runtime.Object, name string) *infrav1.TerraformMachineTemplate {
	t.Helper()
	for _, tmt := range objectsOf[*infrav1.TerraformMachineTemplate](objs) {
		if tmt.Name == name {
			return tmt
		}
	}
	t.Fatalf("no TerraformMachineTemplate %q", name)
	return nil
}

// slicesContainsRef reports whether objs contains the object ref refers
// to, matched by group/version, kind and name.
func slicesContainsRef(objs []runtime.Object, ref clusterv1.ClusterClassTemplateReference) bool {
	for _, o := range objs {
		gvk := o.GetObjectKind().GroupVersionKind()
		if gvk.GroupVersion().String() == ref.APIVersion && gvk.Kind == ref.Kind &&
			o.(interface{ GetName() string }).GetName() == ref.Name {
			return true
		}
	}
	return false
}

// toUnstructured uses t to convert o to an *unstructured.Unstructured,
// failing t on error, and returns the converted object.
func toUnstructured(t *testing.T, o runtime.Object) *unstructured.Unstructured {
	t.Helper()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: m}
}
