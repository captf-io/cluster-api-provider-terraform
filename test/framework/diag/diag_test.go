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

package diag

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

var (
	// clusterGVR is a resource with namespaced and cluster-scoped items.
	clusterGVR = schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "v1beta2", Resource: "clusters"}
	// secretGVR is the resource Collect must never list.
	secretGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
	// nodeGVR is a spare resource registered with the fake list kinds.
	nodeGVR = schema.GroupVersionResource{Group: "example.io", Version: "v1", Resource: "things"}
)

// obj returns an unstructured object of apiVersion and kind, named name in
// namespace ns (cluster-scoped when empty), with the top-level fields of
// extra added.
func obj(apiVersion, kind, ns, name string, extra map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
	}}
	if ns != "" {
		u.SetNamespace(ns)
	}
	for k, v := range extra {
		u.Object[k] = v
	}
	return u
}

// dyn returns a fake dynamic client for the test GVRs seeded with objs.
func dyn(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		clusterGVR: "ClusterList",
		secretGVR:  "SecretList",
		nodeGVR:    "ThingList",
	}, objs...)
}

// read returns the contents of the file at the slash path rel under dir,
// failing t when it is missing.
func read(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// walk returns every file under dir, slash-separated and relative, failing
// t on a walk error.
func walk(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, filepath.ToSlash(rel))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// pod returns a pod called name in namespace ns with an init container, a
// main container and a sidecar, of which main has restarted.
func pod(ns, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "init"}},
			Containers:     []corev1.Container{{Name: "main"}, {Name: "side"}},
		},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "init"}},
			ContainerStatuses:     []corev1.ContainerStatus{{Name: "main", RestartCount: 2}, {Name: "side"}},
		},
	}
}

// TestCollect checks the layout of a clean run and that no Secret reaches
// disk.
func TestCollect(t *testing.T) {
	t.Parallel()
	kube := kubefake.NewSimpleClientset(
		pod("captf-system", "mgr-1"),
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "captf-system", Name: "ev"}, Reason: "Pulled"},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "captf-system", Name: "creds"}, Data: map[string][]byte{"token": []byte("hunter2")}},
	)
	d := dyn(
		obj("cluster.x-k8s.io/v1beta2", "Cluster", "work", "c1", map[string]any{"spec": map[string]any{"paused": false}}),
		obj("cluster.x-k8s.io/v1beta2", "Cluster", "", "global", nil),
		obj("v1", "Secret", "work", "leak", map[string]any{"data": map[string]any{"token": "aHVudGVyMg=="}}),
	)
	var secretListed bool
	d.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		secretListed = true
		return false, nil, nil
	})
	var hookDir string
	dir := filepath.Join(t.TempDir(), "art")
	err := Collect(context.Background(), wait.Clients{Kube: kube, Dynamic: d}, dir, Options{
		Namespaces: []string{"captf-system"},
		Resources:  []schema.GroupVersionResource{clusterGVR, secretGVR},
		NodeLogs:   func(p string) error { hookDir = p; return os.MkdirAll(p, 0o750) },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"pods/captf-system/mgr-1/init.log",
		"pods/captf-system/mgr-1/main.log",
		"pods/captf-system/mgr-1/main.previous.log",
		"pods/captf-system/mgr-1/side.log",
		"events/captf-system.yaml",
		"nodes.yaml",
		"objects/clusters/work/c1.yaml",
		"objects/clusters/_cluster/global.yaml",
	} {
		read(t, dir, rel)
	}
	if got := read(t, dir, "events/captf-system.yaml"); !strings.Contains(got, "Pulled") {
		t.Errorf("events yaml = %q", got)
	}
	if got := read(t, dir, "nodes.yaml"); !strings.Contains(got, "node-1") {
		t.Errorf("nodes yaml = %q", got)
	}
	if got := read(t, dir, "objects/clusters/work/c1.yaml"); !strings.Contains(got, "paused") {
		t.Errorf("object yaml = %q", got)
	}
	if hookDir != filepath.Join(dir, "node-logs") {
		t.Errorf("hook dir = %q", hookDir)
	}
	if _, err := os.Stat(filepath.Join(dir, "errors.txt")); err == nil {
		t.Errorf("errors.txt written on a clean run: %s", read(t, dir, "errors.txt"))
	}
	if _, err := os.Stat(filepath.Join(dir, "pods", "captf-system", "mgr-1", "init.previous.log")); err == nil {
		t.Error("previous log written for a container that never restarted")
	}

	// No secret may reach disk: not listed as a resource, not its data, not a
	// Secret-kinded item of another resource.
	if secretListed {
		t.Error("the secrets resource was listed")
	}
	for _, f := range walk(t, dir) {
		if strings.Contains(f, "secret") || strings.Contains(f, "leak") || strings.Contains(f, "creds") {
			t.Errorf("secret artifact written: %s", f)
		}
		body := read(t, dir, f)
		if strings.Contains(body, "hunter2") || strings.Contains(body, "aHVudGVyMg==") {
			t.Errorf("secret data in %s", f)
		}
	}
}

// TestCollectBestEffort checks failures are recorded and collection goes on.
func TestCollectBestEffort(t *testing.T) {
	t.Parallel()
	kube := kubefake.NewSimpleClientset(pod("ns", "p"), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}})
	kube.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("events down")
	})
	d := dyn()
	d.PrependReactor("list", "clusters", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("no such resource")
	})
	dir := t.TempDir()
	err := Collect(context.Background(), wait.Clients{Kube: kube, Dynamic: d}, dir, Options{
		Namespaces: []string{"ns", "other"},
		Resources:  []schema.GroupVersionResource{clusterGVR},
		NodeLogs:   func(string) error { return errors.New("hook failed") },
	})
	if err != nil {
		t.Fatalf("Collect failed although pods were written: %v", err)
	}
	read(t, dir, "pods/ns/p/main.log")
	errs := read(t, dir, "errors.txt")
	for _, want := range []string{"list events in ns: events down", "list events in other", "list clusters.cluster.x-k8s.io: no such resource", "node logs: hook failed"} {
		if !strings.Contains(errs, want) {
			t.Errorf("errors.txt lacks %q:\n%s", want, errs)
		}
	}
}

// TestCollectNothingWritten checks Collect fails when nothing is written.
func TestCollectNothingWritten(t *testing.T) {
	t.Parallel()
	kube := kubefake.NewSimpleClientset()
	for _, res := range []string{"pods", "events", "nodes"} {
		kube.PrependReactor("list", res, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("down")
		})
	}
	dir := t.TempDir()
	err := Collect(context.Background(), wait.Clients{Kube: kube, Dynamic: dyn()}, dir, Options{Namespaces: []string{"ns"}})
	if err == nil || !strings.Contains(err.Error(), "diag:") || !strings.Contains(err.Error(), "nothing could be written") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(read(t, dir, "errors.txt"), "list pods in ns") {
		t.Error("failures not recorded")
	}
}

// TestCollectBadDir checks an uncreatable directory is an error.
func TestCollectBadDir(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := Collect(context.Background(), wait.Clients{Kube: kubefake.NewSimpleClientset(), Dynamic: dyn()}, filepath.Join(file, "sub"), Options{})
	if err == nil || !strings.Contains(err.Error(), "diag: create") {
		t.Fatalf("err = %v", err)
	}
}

// TestCollectUnwritableTargets checks write failures are recorded, not
// returned.
func TestCollectUnwritableTargets(t *testing.T) {
	t.Parallel()
	// A file squatting on every directory Collect needs makes each write fail,
	// which must be recorded, not returned, while other items still land.
	dir := t.TempDir()
	for _, name := range []string{"events", "objects"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d := dyn(obj("cluster.x-k8s.io/v1beta2", "Cluster", "work", "c1", nil))
	err := Collect(context.Background(), wait.Clients{Kube: kubefake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}}), Dynamic: d}, dir, Options{
		Namespaces: []string{"ns"},
		Resources:  []schema.GroupVersionResource{clusterGVR},
	})
	if err != nil {
		t.Fatal(err)
	}
	read(t, dir, "nodes.yaml")
	errs := read(t, dir, "errors.txt")
	for _, want := range []string{"events/ns.yaml", "objects/clusters/work/c1.yaml"} {
		if !strings.Contains(errs, want) {
			t.Errorf("errors.txt lacks %q:\n%s", want, errs)
		}
	}
}

// TestDefaultResources checks the default set and that it omits Secrets.
func TestDefaultResources(t *testing.T) {
	t.Parallel()
	got := map[string]bool{}
	for _, g := range DefaultResources() {
		got[g.Resource] = true
		if g.Resource == "secrets" {
			t.Error("DefaultResources lists secrets")
		}
	}
	for _, want := range []string{"clusters", "machines", "machinedeployments", "kubeadmcontrolplanes", "terraformclusters", "terraformmachines", "terraformmachinepools", "jobs"} {
		if !got[want] {
			t.Errorf("DefaultResources lacks %s", want)
		}
	}
}

// TestSafe checks path-element sanitizing.
func TestSafe(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"plain":    "plain",
		"a/b":      "a_b",
		`a\b`:      "a_b",
		"":         "_",
		".":        "_.",
		"..":       "_..",
		"../../x":  ".._.._x",
		"with.dot": "with.dot",
	}
	for in, want := range tests {
		if got := safe(in); got != want {
			t.Errorf("safe(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIsSecret checks Secret detection by resource and by kind.
func TestIsSecret(t *testing.T) {
	t.Parallel()
	tests := []struct {
		resource, kind string
		want           bool
	}{
		{"secrets", "", true},
		{"Secrets", "", true},
		{"", "Secret", true},
		{"", "SecretList", true},
		{"clusters", "Cluster", false},
		{"", "", false},
	}
	for _, tt := range tests {
		if got := isSecret(tt.resource, tt.kind); got != tt.want {
			t.Errorf("isSecret(%q, %q) = %v", tt.resource, tt.kind, got)
		}
	}
}
