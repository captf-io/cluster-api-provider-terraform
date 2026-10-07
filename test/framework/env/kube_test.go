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

package env

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/diag"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// The resources the fake dynamic client serves.
var (
	// crdGVR is the CustomResourceDefinition resource.
	crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	// tcGVR is the TerraformCluster resource.
	tcGVR = schema.GroupVersionResource{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Resource: "terraformclusters"}
)

// readyDeployment returns a rolled-out one-replica Deployment named name
// in namespace ns whose manager container runs image.
func readyDeployment(ns, name, image string) *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:  managerContainer,
				Image: image,
				Env:   []corev1.EnvVar{{Name: managerImageEnv, Value: image}, {Name: "POD_NAMESPACE", Value: "x"}},
			}}}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}},
		},
	}
}

// establishedCRD returns an Established CRD object called name.
func establishedCRD(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": name},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Established", "status": "True"},
		}},
	}}
}

// healthyClients returns fake clients of a ready environment: every
// provider namespace holds a ready Deployment, every CAPTF CRD is
// Established and the webhook denies the probe.
func healthyClients() wait.Clients {
	var objs []runtime.Object
	for _, ns := range wait.DefaultProviderNamespaces {
		objs = append(objs, readyDeployment(ns, "d", "img"))
	}
	objs[len(objs)-1] = readyDeployment(ManagerNamespace, ManagerDeployment, "localhost/captf/manager:old")
	var crds []runtime.Object
	for _, n := range CAPTFCRDs {
		crds = append(crds, establishedCRD(n))
	}
	lists := map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList"}
	for _, gvr := range diag.DefaultResources() {
		lists[gvr] = gvr.Resource + "List"
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, crds...)
	dyn.PrependReactor("create", "terraformclusters", func(clienttesting.Action) (bool, runtime.Object, error) {
		denied := apierrors.NewInvalid(schema.GroupKind{Group: tcGVR.Group, Kind: "TerraformCluster"}, "probe", nil)
		denied.ErrStatus.Message = `admission webhook "validation.terraformcluster.infrastructure.cluster.x-k8s.io" denied the request: not a valid image reference`
		return true, nil, denied
	})
	return wait.Clients{Kube: kubefake.NewSimpleClientset(objs...), Dynamic: dyn}
}

// TestCAPTFCRDsMatchConfig checks CAPTFCRDs against config/crd/bases.
func TestCAPTFCRDsMatchConfig(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("../../../config/crd/bases/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no CRDs under config/crd/bases: %v", err)
	}
	var want []string
	for _, f := range files {
		group, plural, ok := strings.Cut(strings.TrimSuffix(filepath.Base(f), ".yaml"), "_")
		if !ok {
			t.Fatalf("unexpected CRD file name %s", f)
		}
		want = append(want, plural+"."+group)
	}
	slices.Sort(want)
	if !slices.Equal(CAPTFCRDs, want) {
		t.Errorf("CAPTFCRDs = %v, want %v", CAPTFCRDs, want)
	}
}

// TestManagerNamesMatchConfig checks the manager names against config/.
func TestManagerNamesMatchConfig(t *testing.T) {
	t.Parallel()
	kust, err := os.ReadFile("../../../config/default/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := os.ReadFile("../../../config/manager/manager.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"namespace: " + ManagerNamespace, "namePrefix: captf-"} {
		if !strings.Contains(string(kust), s) {
			t.Errorf("config/default/kustomization.yaml lacks %q", s)
		}
	}
	for _, s := range []string{"name: " + strings.TrimPrefix(ManagerDeployment, "captf-"), "- name: " + managerContainer, "- name: " + managerImageEnv} {
		if !strings.Contains(string(mgr), s) {
			t.Errorf("config/manager/manager.yaml lacks %q", s)
		}
	}
}

// TestNewKubeErrors checks that a missing kubeconfig is refused.
func TestNewKubeErrors(t *testing.T) {
	t.Parallel()
	if _, err := NewKube("", nil); err == nil {
		t.Error("NewKube with an empty path succeeded")
	}
	if _, err := NewKube(filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Error("NewKube with a missing file succeeded")
	}
}

// TestKubeWaitProviders checks that a ready environment passes every wait.
func TestKubeWaitProviders(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	k := newKube(healthyClients(), out)
	if err := k.WaitProviders(context.Background()); err != nil {
		t.Fatalf("WaitProviders: %v\n%s", err, out)
	}
	for _, s := range []string{"deployments available: ready", "CRDs established: ready", "webhook serving: ready"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	rep, err := k.Report(context.Background())
	if err != nil || rep != "no non-ready pods" {
		t.Errorf("Report = %q, %v", rep, err)
	}
}

// TestKubeWaitProvidersCancelled checks that each wait stops on a done
// context.
func TestKubeWaitProvidersCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	empty := wait.Clients{Kube: kubefake.NewSimpleClientset(), Dynamic: healthyClients().Dynamic}
	if err := newKube(empty, nil).WaitProviders(ctx); err == nil {
		t.Error("WaitProviders on a cancelled context succeeded")
	}
	noCRDs := healthyClients()
	noCRDs.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		crdGVR: "CustomResourceDefinitionList",
		tcGVR:  "TerraformClusterList",
	})
	if err := newKube(noCRDs, nil).WaitProviders(ctx); err == nil || !strings.Contains(err.Error(), "CRDs established") {
		t.Errorf("WaitProviders without CRDs: %v", err)
	}
}

// TestKubeManagerImage checks reading and patching the manager image.
func TestKubeManagerImage(t *testing.T) {
	t.Parallel()
	c := healthyClients()
	k := newKube(c, nil)
	ctx := context.Background()
	img, err := k.ManagerImage(ctx)
	if err != nil || img != "localhost/captf/manager:old" {
		t.Fatalf("ManagerImage = %q, %v", img, err)
	}
	if err := k.SetManagerImage(ctx, "localhost/captf/manager:new"); err != nil {
		t.Fatal(err)
	}
	d, err := c.Kube.AppsV1().Deployments(ManagerNamespace).Get(ctx, ManagerDeployment, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Errorf("patched replicas = %v, want the unchanged 1", d.Spec.Replicas)
	}
	ctr := d.Spec.Template.Spec.Containers[0]
	wantEnv := []corev1.EnvVar{{Name: managerImageEnv, Value: "localhost/captf/manager:new"}, {Name: "POD_NAMESPACE", Value: "x"}}
	if ctr.Image != "localhost/captf/manager:new" || !slices.Equal(ctr.Env, wantEnv) {
		t.Errorf("patched container: image %s, env %v", ctr.Image, ctr.Env)
	}
}

// TestKubeManagerImageErrors checks a missing Deployment and container.
func TestKubeManagerImageErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	empty := newKube(wait.Clients{Kube: kubefake.NewSimpleClientset()}, nil)
	if _, err := empty.ManagerImage(ctx); err == nil {
		t.Error("ManagerImage without a Deployment succeeded")
	}
	if err := empty.SetManagerImage(ctx, "x:y"); err == nil {
		t.Error("SetManagerImage without a Deployment succeeded")
	}
	d := readyDeployment(ManagerNamespace, ManagerDeployment, "x")
	d.Spec.Template.Spec.Containers[0].Name = "other"
	noCtr := newKube(wait.Clients{Kube: kubefake.NewSimpleClientset(d)}, nil)
	if _, err := noCtr.ManagerImage(ctx); err == nil || !strings.Contains(err.Error(), "no \"manager\" container") {
		t.Errorf("ManagerImage without the container: %v", err)
	}
}

// TestRolledOut tables the rollout evaluation.
func TestRolledOut(t *testing.T) {
	t.Parallel()
	two := int32(2)
	for name, tc := range map[string]struct {
		mutate func(*appsv1.Deployment)
		want   bool
	}{
		"ready":       {func(*appsv1.Deployment) {}, true},
		"nil replica": {func(d *appsv1.Deployment) { d.Spec.Replicas = nil }, true},
		"unobserved":  {func(d *appsv1.Deployment) { d.Generation = 2 }, false},
		"updating":    {func(d *appsv1.Deployment) { d.Status.UpdatedReplicas = 0 }, false},
		"old pod":     {func(d *appsv1.Deployment) { d.Status.Replicas = 2 }, false},
		"unavailable": {func(d *appsv1.Deployment) { d.Status.UnavailableReplicas = 1 }, false},
		"scaled":      {func(d *appsv1.Deployment) { d.Spec.Replicas = &two }, false},
	} {
		d := readyDeployment("ns", "d", "x")
		tc.mutate(d)
		if got, status := rolledOut(d); got != tc.want {
			t.Errorf("%s: rolledOut = %v (%s), want %v", name, got, status, tc.want)
		}
	}
}

// TestKubeWaitManagerRollout checks a finished rollout and a cancelled
// wait.
func TestKubeWaitManagerRollout(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	if err := newKube(healthyClients(), out).WaitManagerRollout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rolled out after") {
		t.Errorf("output:\n%s", out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := readyDeployment(ManagerNamespace, ManagerDeployment, "x")
	d.Generation = 2
	for _, c := range []wait.Clients{{Kube: kubefake.NewSimpleClientset(d)}, {Kube: kubefake.NewSimpleClientset()}} {
		if err := newKube(c, nil).WaitManagerRollout(ctx); err == nil {
			t.Error("WaitManagerRollout on a cancelled context succeeded")
		}
	}
}

// TestKubeCollect checks that Collect writes a bundle and runs the
// node-log hook.
func TestKubeCollect(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "bundle")
	var hookDir string
	err := newKube(healthyClients(), nil).Collect(context.Background(), dir, func(d string) error {
		hookDir = d
		return os.MkdirAll(d, 0o750)
	})
	if err != nil {
		t.Fatal(err)
	}
	if hookDir != filepath.Join(dir, "node-logs") {
		t.Errorf("hook dir = %s", hookDir)
	}
	if _, err := os.Stat(filepath.Join(dir, "nodes.yaml")); err != nil {
		t.Errorf("nodes.yaml: %v", err)
	}
	if got := diagNamespaces(); !slices.Contains(got, "kube-system") || !slices.Contains(got, ManagerNamespace) {
		t.Errorf("diagNamespaces = %v", got)
	}
}
