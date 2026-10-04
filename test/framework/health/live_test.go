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

package health

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// restClients starts an httptest server running h and returns clients
// built by a real kubernetes.NewForConfig against it. The server closes
// with t.
func restClients(t *testing.T, h http.Handler) wait.Clients {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	kube, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return wait.Clients{Kube: kube}
}

// TestAPIServer checks readyz and livez through a real REST client.
func TestAPIServer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var gotVerbose bool
	var mu sync.Mutex
	ok := restClients(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" && r.URL.Query().Has("verbose") {
			mu.Lock()
			gotVerbose = true
			mu.Unlock()
		}
		_, _ = w.Write([]byte("ok"))
	}))
	if err := APIServer(ctx, ok); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !gotVerbose {
		t.Error("readyz was not asked with ?verbose")
	}

	bad := restClients(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "[-]etcd failed", http.StatusInternalServerError)
	}))
	mustContain(t, APIServer(ctx, bad), "/readyz?verbose", "/livez")

	half := restClients(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/livez" {
			http.Error(w, "dead", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	err := APIServer(ctx, half)
	mustContain(t, err, "/livez")
	if strings.Contains(err.Error(), "/readyz") {
		t.Errorf("readyz passed but is reported: %v", err)
	}
}

// TestPodProxyGet checks the proxy path and body through a real REST
// client.
func TestPodProxyGet(t *testing.T) {
	t.Parallel()
	var path string
	var mu sync.Mutex
	c := restClients(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		path = r.URL.Path
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/missing") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	body, err := PodProxyGet(context.Background(), c, "captf-system", "mgr-1", 9440, "/healthz")
	if err != nil || string(body) != "ok" {
		t.Fatalf("body %q err %v", body, err)
	}
	mu.Lock()
	want := "/api/v1/namespaces/captf-system/pods/http:mgr-1:9440/proxy/healthz"
	if path != want {
		t.Errorf("path %q, want %q", path, want)
	}
	mu.Unlock()
	_, err = PodProxyGet(context.Background(), c, "captf-system", "mgr-1", 9440, "/missing")
	mustContain(t, err, "proxy get captf-system/mgr-1:9440/missing")
}

// TestScanLogsREST runs ScanLogs end to end against an httptest server:
// pod list, then current and previous log streams.
func TestScanLogsREST(t *testing.T) {
	t.Parallel()
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", Labels: map[string]string{"app": "x"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "c", RestartCount: 1}}},
	}
	list := corev1.PodList{TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"}, Items: []corev1.Pod{p}}
	var sel string
	var mu sync.Mutex
	c := restClients(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/log"):
			if r.URL.Query().Get("previous") == "true" {
				_, _ = w.Write([]byte("panic: before restart\n"))
				return
			}
			_, _ = w.Write([]byte("I0101 00:00:00 fine\nE0101 00:00:00 allowed noise\n"))
		default:
			mu.Lock()
			sel = r.URL.Query().Get("labelSelector")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(list)
		}
	}))
	r := Rules{Fatal: DefaultFatal(), Allow: []*regexp.Regexp{regexp.MustCompile(`allowed noise`)}}
	err := ScanLogs(context.Background(), c, "ns", "app=x", r)
	mustContain(t, err, "container c (previous): panic: before restart")
	if strings.Contains(err.Error(), "allowed noise") {
		t.Errorf("allowlisted line reported: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if sel != "app=x" {
		t.Errorf("labelSelector %q", sel)
	}

	broken := restClients(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	mustContain(t, ScanLogs(context.Background(), broken, "ns", "", r), "list pods")
}

// stablePod builds a one-container pod named name with the UID suffix uid,
// the given restarts, and readiness ready, for the Stable tests. It
// returns the pod.
func stablePod(name, uid string, restarts int32, ready bool) *corev1.Pod {
	p := pod(name, corev1.PodRunning, ready, corev1.ContainerStatus{Name: "c", Ready: ready, RestartCount: restarts})
	p.UID = types.UID("uid-" + uid)
	return p
}

// TestStable covers the clean window, every violation kind, progress
// output, additions and cancellation.
func TestStable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const interval = 5 * time.Millisecond

	t.Run("clean", func(t *testing.T) {
		t.Parallel()
		c := wait.Clients{Kube: fake.NewSimpleClientset(stablePod("a", "1", 0, true))}
		var out bytes.Buffer
		if err := stable(ctx, c, []string{"ns"}, 30*time.Millisecond, interval, &out, 10*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		s := out.String()
		if !strings.Contains(s, "baseline 1 pods") || !strings.Contains(s, "elapsed") || !strings.Contains(s, ": stable") {
			t.Errorf("output: %q", s)
		}
	})

	// mutate runs fn on the fake after 10ms, while Stable watches 2s.
	run := func(t *testing.T, base *corev1.Pod, fn func(k *fake.Clientset)) error {
		t.Helper()
		k := fake.NewSimpleClientset(base)
		go func() {
			time.Sleep(10 * time.Millisecond)
			fn(k)
		}()
		return Stable(ctx, wait.Clients{Kube: k}, []string{"ns"}, 2*time.Second, interval, &bytes.Buffer{})
	}
	pods := func(k *fake.Clientset) typedcorev1.PodInterface {
		return k.CoreV1().Pods("ns")
	}

	t.Run("restart", func(t *testing.T) {
		t.Parallel()
		err := run(t, stablePod("a", "1", 0, true), func(k *fake.Clientset) {
			_, _ = pods(k).Update(ctx, stablePod("a", "1", 1, true), metav1.UpdateOptions{})
		})
		mustContain(t, err, "not stable", "pod ns/a: container c restarted (restarts 0 -> 1)")
	})
	t.Run("recreated", func(t *testing.T) {
		t.Parallel()
		err := run(t, stablePod("a", "1", 0, true), func(k *fake.Clientset) {
			_, _ = pods(k).Update(ctx, stablePod("a", "2", 0, true), metav1.UpdateOptions{})
		})
		mustContain(t, err, "pod ns/a: recreated")
	})
	t.Run("deleted", func(t *testing.T) {
		t.Parallel()
		err := run(t, stablePod("a", "1", 0, true), func(k *fake.Clientset) { _ = pods(k).Delete(ctx, "a", metav1.DeleteOptions{}) })
		mustContain(t, err, "pod ns/a: deleted")
	})
	t.Run("not ready", func(t *testing.T) {
		t.Parallel()
		err := run(t, stablePod("a", "1", 0, true), func(k *fake.Clientset) {
			_, _ = pods(k).Update(ctx, stablePod("a", "1", 0, false), metav1.UpdateOptions{})
		})
		mustContain(t, err, "pod ns/a: went not ready")
	})
	t.Run("added", func(t *testing.T) {
		t.Parallel()
		err := run(t, stablePod("a", "1", 0, true), func(k *fake.Clientset) {
			_, _ = pods(k).Create(ctx, stablePod("b", "9", 0, true), metav1.CreateOptions{})
		})
		mustContain(t, err, "pod ns/b: added")
	})
	t.Run("completed job pod added is fine", func(t *testing.T) {
		t.Parallel()
		done := pod("job-1", corev1.PodSucceeded, false)
		k := fake.NewSimpleClientset(stablePod("a", "1", 0, true))
		go func() {
			time.Sleep(10 * time.Millisecond)
			_, _ = k.CoreV1().Pods("ns").Create(ctx, done, metav1.CreateOptions{})
		}()
		if err := Stable(ctx, wait.Clients{Kube: k}, []string{"ns"}, 80*time.Millisecond, interval, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		t.Parallel()
		cctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		c := wait.Clients{Kube: fake.NewSimpleClientset(stablePod("a", "1", 0, true))}
		mustContain(t, Stable(cctx, c, nil, time.Hour, interval, &bytes.Buffer{}), "deadline exceeded")
	})
	t.Run("bad interval", func(t *testing.T) {
		t.Parallel()
		mustContain(t, Stable(ctx, fakeClients(), nil, time.Second, 0, &bytes.Buffer{}), "interval must be positive")
	})
}

// TestDiffSnapshots checks the pure snapshot comparison, including a
// Succeeded baseline pod that vanishes (allowed).
func TestDiffSnapshots(t *testing.T) {
	t.Parallel()
	done := pod("done", corev1.PodSucceeded, false)
	done.UID = "u-done"
	run := stablePod("run", "1", 0, true)
	base := takeSnapshot([]corev1.Pod{*done, *run})
	v, a := diffSnapshots(base, takeSnapshot([]corev1.Pod{*run}))
	if len(v) != 0 || len(a) != 0 {
		t.Fatalf("Succeeded pod removal must be fine: %v %v", v, a)
	}
	owned := pod("job", corev1.PodRunning, true)
	owned.OwnerReferences = []metav1.OwnerReference{{Kind: "Job"}}
	if s := takeSnapshot([]corev1.Pod{*owned}); !s["ns/job"].job {
		t.Error("job owner not recorded")
	}
}
