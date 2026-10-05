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

package wait

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// fast returns the Options every test uses: tight polling and a short
// timeout, writing progress to out when it is non-nil.
func fast(out *bytes.Buffer) Options {
	o := Options{Interval: time.Millisecond, ReportEvery: time.Millisecond, Timeout: 200 * time.Millisecond}
	if out != nil {
		o.Out = out
	}
	return o
}

// safeBuffer is a bytes.Buffer safe for the poll goroutine and the test.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Write appends p under the lock. It returns len(p) and a nil error.
func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// String returns the buffered text.
func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// dyn returns a fake dynamic client that lists CRDs and TerraformClusters,
// seeded with objs.
func dyn(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		crdGVR:              "CustomResourceDefinitionList",
		terraformClusterGVR: "TerraformClusterList",
	}, objs...)
}

// deployment returns a fully ready Deployment of one replica, named name in
// namespace ns.
func deployment(ns, name string) *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}},
		},
	}
}

// crd returns a CRD object called name with the given established status
// ("" for no condition).
func crd(name, established string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": name},
	}}
	if established != "" {
		u.Object["status"] = map[string]any{"conditions": []any{
			map[string]any{"type": "NamesAccepted", "status": "True"},
			map[string]any{"type": "Established", "status": established},
		}}
	}
	return u
}

// TestDeploymentReady tables the Deployment rollout evaluation.
func TestDeploymentReady(t *testing.T) {
	t.Parallel()
	two := int32(2)
	tests := []struct {
		name   string
		mutate func(d *appsv1.Deployment)
		want   bool
		reason string
	}{
		{"ready", func(*appsv1.Deployment) {}, true, ""},
		{"nil replicas defaults to one", func(d *appsv1.Deployment) { d.Spec.Replicas = nil }, true, ""},
		{"generation unobserved", func(d *appsv1.Deployment) { d.Generation = 2 }, false, "generation"},
		{"not available", func(d *appsv1.Deployment) { d.Status.Conditions[0].Status = corev1.ConditionFalse }, false, "not Available"},
		{"no condition", func(d *appsv1.Deployment) { d.Status.Conditions = nil }, false, "not Available"},
		{"not all updated", func(d *appsv1.Deployment) { d.Spec.Replicas = &two }, false, "1/2 replicas updated"},
		{"old replicas", func(d *appsv1.Deployment) { d.Status.Replicas = 2 }, false, "old replicas"},
		{"not all available", func(d *appsv1.Deployment) { d.Status.AvailableReplicas = 0 }, false, "0/1 replicas available"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := deployment("ns", "d")
			tt.mutate(d)
			got, why := deploymentReady(d)
			if got != tt.want || !strings.Contains(why, tt.reason) {
				t.Fatalf("deploymentReady = %v, %q; want %v containing %q", got, why, tt.want, tt.reason)
			}
		})
	}
}

// TestCRDEstablished tables the CRD condition evaluation.
func TestCRDEstablished(t *testing.T) {
	t.Parallel()
	bad := crd("x", "")
	bad.Object["status"] = map[string]any{"conditions": []any{"junk"}}
	tests := []struct {
		name string
		in   *unstructured.Unstructured
		want bool
		why  string
	}{
		{"established", crd("x", "True"), true, ""},
		{"false", crd("x", "False"), false, "Established=False"},
		{"no status", crd("x", ""), false, "no Established"},
		{"malformed condition", bad, false, "no Established"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, why := crdEstablished(tt.in)
			if got != tt.want || !strings.Contains(why, tt.why) {
				t.Fatalf("crdEstablished = %v, %q; want %v containing %q", got, why, tt.want, tt.why)
			}
		})
	}
}

// TestClassifyProbe tables which probe errors prove the webhook serves.
func TestClassifyProbe(t *testing.T) {
	t.Parallel()
	gk := schema.GroupKind{Group: "infrastructure.cluster.x-k8s.io", Kind: "TerraformCluster"}
	webhookDenied := apierrors.NewInvalid(gk, "p", nil)
	webhookDenied.ErrStatus.Message = `admission webhook "` + webhookName + `" denied the request: spec.source.image: Invalid value`
	validationOnly := apierrors.NewInvalid(gk, "p", nil)
	validationOnly.ErrStatus.Message = `TerraformCluster "p" is invalid: spec.source.image: Invalid value: "X": not a valid image reference: bad`
	schemaOnly := apierrors.NewInvalid(gk, "p", nil)
	schemaOnly.ErrStatus.Message = `TerraformCluster "p" is invalid: spec.source: Required value`
	unreachable := apierrors.NewInternalError(errors.New(`failed calling webhook "` + webhookName + `": dial tcp: connect: connection refused`))
	unreachableInvalid := apierrors.NewInvalid(gk, "p", nil)
	unreachableInvalid.ErrStatus.Message = `failed calling webhook "` + webhookName + `" ` + webhookValidation
	tests := []struct {
		name string
		err  error
		want bool
		why  string
	}{
		{"accepted", nil, false, "accepted"},
		{"webhook denial", webhookDenied, true, ""},
		{"webhook validation message", validationOnly, true, ""},
		{"schema rejection", schemaOnly, false, "not by the CAPTF webhook"},
		{"unreachable", unreachable, false, "unreachable"},
		{"unreachable wins over text", unreachableInvalid, false, "unreachable"},
		{"crd missing", apierrors.NewNotFound(schema.GroupResource{Resource: "terraformclusters"}, "p"), false, "probe failed"},
		{"plain error", errors.New("connection refused"), false, "probe failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, why := classifyProbe(tt.err)
			if got != tt.want || !strings.Contains(why, tt.why) {
				t.Fatalf("classifyProbe = %v, %q; want %v containing %q", got, why, tt.want, tt.why)
			}
		})
	}
}

// TestOptionsDefaults checks zero fields are defaulted and others kept.
func TestOptionsDefaults(t *testing.T) {
	t.Parallel()
	o := Options{}.withDefaults()
	if o.Interval != 5*time.Second || o.ReportEvery != 30*time.Second || o.Timeout != 5*time.Minute || o.Out == nil {
		t.Fatalf("defaults = %+v", o)
	}
	keep := Options{Interval: time.Second, ReportEvery: time.Minute, Timeout: time.Hour}.withDefaults()
	if keep.Interval != time.Second || keep.ReportEvery != time.Minute || keep.Timeout != time.Hour {
		t.Fatalf("explicit values replaced: %+v", keep)
	}
}

// TestDeploymentsAvailable covers the Deployment wait over a fake clientset.
func TestDeploymentsAvailable(t *testing.T) {
	t.Parallel()
	t.Run("ready", func(t *testing.T) {
		t.Parallel()
		c := Clients{Kube: kubefake.NewSimpleClientset(deployment("a", "x"), deployment("b", "y"))}
		var out bytes.Buffer
		if err := DeploymentsAvailable(context.Background(), c, []string{"a", "b"}, fast(&out)); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "ready after") {
			t.Fatalf("output = %q", out.String())
		}
	})
	t.Run("empty namespace is not ready", func(t *testing.T) {
		t.Parallel()
		c := Clients{Kube: kubefake.NewSimpleClientset(deployment("a", "x"))}
		err := DeploymentsAvailable(context.Background(), c, []string{"a", "empty"}, fast(nil))
		if err == nil || !strings.Contains(err.Error(), "empty: no deployments yet") || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unready deployment reports pods", func(t *testing.T) {
		t.Parallel()
		d := deployment("a", "x")
		d.Status.AvailableReplicas = 0
		d.Status.Conditions[0].Status = corev1.ConditionFalse
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "x-1"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "m"}}},
			Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
				Name: "m", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
			}}},
		}
		c := Clients{Kube: kubefake.NewSimpleClientset(d, pod)}
		var out safeBuffer
		o := fast(nil)
		o.Out = &out
		err := DeploymentsAvailable(context.Background(), c, []string{"a"}, o)
		if err == nil || !strings.Contains(err.Error(), "a/x: not Available") {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(out.String(), "ImagePullBackOff") {
			t.Fatalf("progress lacks the pod reason: %q", out.String())
		}
	})
	t.Run("list error is a status, not a crash", func(t *testing.T) {
		t.Parallel()
		kube := kubefake.NewSimpleClientset()
		kube.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("boom")
		})
		kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("pods boom")
		})
		var out safeBuffer
		o := fast(nil)
		o.Out = &out
		err := DeploymentsAvailable(context.Background(), Clients{Kube: kube}, []string{"a"}, o)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(out.String(), "pod report unavailable") {
			t.Fatalf("output = %q", out.String())
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c := Clients{Kube: kubefake.NewSimpleClientset()}
		err := DeploymentsAvailable(ctx, c, []string{"a"}, fast(nil))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("becomes ready while waiting", func(t *testing.T) {
		t.Parallel()
		kube := kubefake.NewSimpleClientset()
		c := Clients{Kube: kube}
		go func() {
			time.Sleep(20 * time.Millisecond)
			_, _ = kube.AppsV1().Deployments("a").Create(context.Background(), deployment("a", "x"), metav1.CreateOptions{})
		}()
		o := fast(nil)
		o.Timeout = 5 * time.Second
		if err := DeploymentsAvailable(context.Background(), c, []string{"a"}, o); err != nil {
			t.Fatal(err)
		}
	})
}

// TestCRDsEstablished covers the CRD wait over a fake dynamic client.
func TestCRDsEstablished(t *testing.T) {
	t.Parallel()
	t.Run("established", func(t *testing.T) {
		t.Parallel()
		c := Clients{Dynamic: dyn(crd("a.x", "True"), crd("b.x", "True"))}
		if err := CRDsEstablished(context.Background(), c, []string{"a.x", "b.x"}, fast(nil)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("missing and not established", func(t *testing.T) {
		t.Parallel()
		c := Clients{Dynamic: dyn(crd("a.x", "False"))}
		err := CRDsEstablished(context.Background(), c, []string{"a.x", "gone.x"}, fast(nil))
		if err == nil || !strings.Contains(err.Error(), "a.x: Established=False") || !strings.Contains(err.Error(), "gone.x: not found") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("get error", func(t *testing.T) {
		t.Parallel()
		d := dyn()
		d.PrependReactor("get", "customresourcedefinitions", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("api down")
		})
		err := CRDsEstablished(context.Background(), Clients{Dynamic: d}, []string{"a.x"}, fast(nil))
		if err == nil || !strings.Contains(err.Error(), "api down") {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestWebhookServing covers the webhook probe wait over a fake dynamic
// client.
func TestWebhookServing(t *testing.T) {
	t.Parallel()
	gk := schema.GroupKind{Group: "infrastructure.cluster.x-k8s.io", Kind: "TerraformCluster"}
	denial := apierrors.NewInvalid(gk, "p", nil)
	denial.ErrStatus.Message = `admission webhook "` + webhookName + `" denied the request: not a valid image reference`

	t.Run("ready once the webhook rejects", func(t *testing.T) {
		t.Parallel()
		d := dyn()
		calls := 0
		d.PrependReactor("create", "terraformclusters", func(a k8stesting.Action) (bool, runtime.Object, error) {
			calls++
			create := a.(k8stesting.CreateActionImpl)
			if len(create.CreateOptions.DryRun) != 1 || create.CreateOptions.DryRun[0] != metav1.DryRunAll {
				t.Errorf("create is not a dry run: %+v", create.CreateOptions)
			}
			u := create.GetObject().(*unstructured.Unstructured)
			img, _, _ := unstructured.NestedString(u.Object, "spec", "source", "image")
			if u.GetNamespace() != "probe-ns" || strings.TrimSpace(img) == "" {
				t.Errorf("probe object malformed: %v", u.Object)
			}
			if calls < 3 {
				return true, nil, apierrors.NewInternalError(errors.New(`failed calling webhook: connection refused`))
			}
			return true, nil, denial
		})
		o := fast(nil)
		o.Timeout = 5 * time.Second
		if err := WebhookServing(context.Background(), Clients{Dynamic: d}, "probe-ns", o); err != nil {
			t.Fatal(err)
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3", calls)
		}
	})
	t.Run("accepted object never counts", func(t *testing.T) {
		t.Parallel()
		d := dyn()
		d.PrependReactor("create", "terraformclusters", func(a k8stesting.Action) (bool, runtime.Object, error) {
			return true, a.(k8stesting.CreateActionImpl).Object, nil
		})
		err := WebhookServing(context.Background(), Clients{Dynamic: d}, "default", fast(nil))
		if err == nil || !strings.Contains(err.Error(), "accepted") {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestProbeClusterIsInvalidForTheWebhookOnly checks the probe's shape.
func TestProbeClusterIsInvalidForTheWebhookOnly(t *testing.T) {
	t.Parallel()
	u := probeCluster("ns")
	if u.GetAPIVersion() != "infrastructure.cluster.x-k8s.io/v1alpha1" || u.GetKind() != "TerraformCluster" || u.GetNamespace() != "ns" {
		t.Fatalf("probe = %v", u.Object)
	}
	name, _, _ := unstructured.NestedString(u.Object, "spec", "identityRef", "name")
	img, _, _ := unstructured.NestedString(u.Object, "spec", "source", "image")
	if name == "" || img == "" || strings.ToLower(img) == img {
		t.Fatalf("identity %q and image %q must be set, the image with uppercase (an invalid reference)", name, img)
	}
}

// TestReport covers the non-ready pod summary.
func TestReport(t *testing.T) {
	t.Parallel()
	pod := func(name string, phase corev1.PodPhase, mutate func(p *corev1.Pod)) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
			Status:     corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{{Name: "main", Ready: phase == corev1.PodRunning}}},
		}
		if mutate != nil {
			mutate(p)
		}
		return p
	}
	t.Run("all ready", func(t *testing.T) {
		t.Parallel()
		kube := kubefake.NewSimpleClientset(pod("ok", corev1.PodRunning, nil), pod("job", corev1.PodSucceeded, nil))
		got, err := Report(context.Background(), Clients{Kube: kube}, []string{"ns"})
		if err != nil || got != "no non-ready pods" {
			t.Fatalf("Report = %q, %v", got, err)
		}
	})
	t.Run("reasons and restarts", func(t *testing.T) {
		t.Parallel()
		kube := kubefake.NewSimpleClientset(
			pod("crash", corev1.PodRunning, func(p *corev1.Pod) {
				p.Status.ContainerStatuses[0].Ready = false
				p.Status.ContainerStatuses[0].RestartCount = 4
				p.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
				p.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "OOMKilled"}
			}),
			pod("oom", corev1.PodRunning, func(p *corev1.Pod) {
				p.Status.ContainerStatuses[0].Ready = false
				p.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "OOMKilled"}
			}),
			pod("failed", corev1.PodRunning, func(p *corev1.Pod) {
				p.Status.ContainerStatuses[0].Ready = false
				p.Status.ContainerStatuses[0].State.Terminated = &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}
			}),
			pod("evicted", corev1.PodFailed, func(p *corev1.Pod) {
				p.Status.ContainerStatuses = nil
				p.Status.Reason = "Evicted"
			}),
			pod("init", corev1.PodPending, func(p *corev1.Pod) {
				p.Status.ContainerStatuses = nil
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name: "setup", RestartCount: 1, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
				}}
			}),
		)
		got, err := Report(context.Background(), Clients{Kube: kube}, []string{"ns"})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"ns/crash Running ready 0/1 restarts 4 (main waiting: CrashLoopBackOff)",
			"ns/oom Running ready 0/1 restarts 0 (main last terminated: OOMKilled)",
			"ns/failed Running ready 0/1 restarts 0 (main terminated: Error)",
			"ns/evicted Failed ready 0/1 restarts 0 (Evicted)",
			"ns/init Pending ready 0/1 restarts 1 (setup waiting: PodInitializing)",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("report lacks %q:\n%s", want, got)
			}
		}
	})
	t.Run("list error", func(t *testing.T) {
		t.Parallel()
		kube := kubefake.NewSimpleClientset()
		kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("boom")
		})
		_, err := Report(context.Background(), Clients{Kube: kube}, []string{"ns"})
		if err == nil || !strings.Contains(err.Error(), "wait: list pods in ns") {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestClientsFromKubeconfig covers explicit-path loading and its refusals.
func TestClientsFromKubeconfig(t *testing.T) {
	t.Parallel()
	t.Run("empty path refused", func(t *testing.T) {
		t.Parallel()
		if _, err := ClientsFromKubeconfig(""); err == nil || !strings.Contains(err.Error(), "wait:") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		if _, err := ClientsFromKubeconfig(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("valid file", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "kubeconfig")
		cfg := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: x
  context: {cluster: c, user: u}
current-context: x
users:
- name: u
  user: {token: stand-in}
`
		if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := ClientsFromKubeconfig(path)
		if err != nil || c.Kube == nil || c.Dynamic == nil {
			t.Fatalf("ClientsFromKubeconfig = %+v, %v", c, err)
		}
	})
}
