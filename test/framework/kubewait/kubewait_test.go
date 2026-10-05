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

package kubewait

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// widgets is the GVR of the fake kind the tests poll.
var widgets = schema.GroupVersionResource{Group: "example.io", Version: "v1", Resource: "widgets"}

// fast returns Options with millisecond timing writing progress to out.
func fast(out *bytes.Buffer) Options {
	o := Options{Interval: time.Millisecond, Timeout: 60 * time.Millisecond, ReportEvery: time.Millisecond}
	if out != nil {
		o.Out = out
	}
	return o
}

// widget returns a widget object named name in ns with the given spec and
// status maps.
func widget(ns, name string, status map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.io/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"name": name, "namespace": ns},
	}}
	if status != nil {
		u.Object["status"] = status
	}
	return u
}

// fakeDyn returns a fake dynamic client holding objs.
func fakeDyn(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{widgets: "WidgetList"}, objs...)
}

// TestOptionsDefaults checks the documented defaults.
func TestOptionsDefaults(t *testing.T) {
	t.Parallel()
	o := Options{}.withDefaults()
	if o.Interval != 2*time.Second || o.Timeout != 3*time.Minute || o.ReportEvery != 30*time.Second || o.Out == nil {
		t.Errorf("defaults: %+v", o)
	}
}

// TestGet checks Get for a present and a missing object.
func TestGet(t *testing.T) {
	t.Parallel()
	dyn := fakeDyn(widget("ns", "a", nil))
	u, err := Get(context.Background(), dyn, widgets, "ns", "a")
	if err != nil || u.GetName() != "a" {
		t.Fatalf("Get = %v, %v", u, err)
	}
	_, err = Get(context.Background(), dyn, widgets, "ns", "missing")
	if !apierrors.IsNotFound(err) || !strings.HasPrefix(err.Error(), "kubewait:") {
		t.Errorf("err = %v, want wrapped NotFound", err)
	}
	// A cluster-scoped read takes the no-namespace path.
	if _, err := Get(context.Background(), dyn, widgets, "", "a"); err == nil {
		t.Errorf("cluster-scoped Get of a namespaced object succeeded")
	}
}

// TestUntil checks success on a later poll, progress lines and the returned
// object.
func TestUntil(t *testing.T) {
	t.Parallel()
	dyn := fakeDyn(widget("ns", "a", nil))
	calls := 0
	var out bytes.Buffer
	u, err := Until(context.Background(), dyn, widgets, "ns", "a", "ready", func(*unstructured.Unstructured) (bool, string) {
		calls++
		time.Sleep(2 * time.Millisecond) // let ReportEvery elapse
		return calls >= 3, "call count"
	}, fast(&out))
	if err != nil || u.GetName() != "a" {
		t.Fatalf("Until = %v, %v", u, err)
	}
	if !strings.Contains(out.String(), "call count") || !strings.Contains(out.String(), "done after") {
		t.Errorf("progress output: %q", out.String())
	}
}

// TestUntilTimeout checks that the timeout error names the wait and the last
// observation, for a missing object and for a rejecting predicate.
func TestUntilTimeout(t *testing.T) {
	t.Parallel()
	_, err := Until(context.Background(), fakeDyn(), widgets, "ns", "a", "ready",
		func(*unstructured.Unstructured) (bool, string) { return true, "" }, fast(nil))
	if err == nil || !strings.Contains(err.Error(), "widgets ns/a ready") || !strings.Contains(err.Error(), "not found") ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("missing object: %v", err)
	}
	_, err = Until(context.Background(), fakeDyn(widget("ns", "a", nil)), widgets, "ns", "a", "ready",
		func(*unstructured.Unstructured) (bool, string) { return false, "phase=Pending" }, fast(nil))
	if err == nil || !strings.Contains(err.Error(), "phase=Pending") {
		t.Errorf("rejecting predicate: %v", err)
	}
}

// TestUntilGetError checks that a read error is the observation.
func TestUntilGetError(t *testing.T) {
	t.Parallel()
	dyn := fakeDyn(widget("ns", "a", nil))
	dyn.PrependReactor("get", "widgets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver down")
	})
	_, err := Until(context.Background(), dyn, widgets, "ns", "a", "ready",
		func(*unstructured.Unstructured) (bool, string) { return true, "" }, fast(nil))
	if err == nil || !strings.Contains(err.Error(), "get failed: apiserver down") {
		t.Errorf("err = %v", err)
	}
}

// TestUntilContextCanceled checks that a canceled context ends the wait.
func TestUntilContextCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o := fast(nil)
	o.Timeout = time.Minute
	_, err := Until(ctx, fakeDyn(widget("ns", "a", nil)), widgets, "ns", "a", "ready",
		func(*unstructured.Unstructured) (bool, string) { return false, "x" }, o)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want Canceled", err)
	}
}

// conds returns a status map holding one condition of type typ with the
// given status and reason.
func conds(typ, status, reason string) map[string]any {
	return map[string]any{"conditions": []any{
		map[string]any{"type": "Other", "status": "False"},
		map[string]any{"type": typ, "status": status, "reason": reason, "message": "m"},
	}}
}

// TestConditionStatus checks status and reason matching.
func TestConditionStatus(t *testing.T) {
	t.Parallel()
	dyn := fakeDyn(widget("ns", "a", conds("Ready", "True", "Fine")))
	for _, reason := range []string{"", "Fine"} {
		if _, err := ConditionStatus(context.Background(), dyn, widgets, "ns", "a", "Ready", "True", reason, fast(nil)); err != nil {
			t.Errorf("reason %q: %v", reason, err)
		}
	}
	_, err := ConditionStatus(context.Background(), dyn, widgets, "ns", "a", "Ready", "True", "Other", fast(nil))
	if err == nil || !strings.Contains(err.Error(), `reason="Fine"`) || !strings.Contains(err.Error(), "reason Other") {
		t.Errorf("wrong reason: %v", err)
	}
	_, err = ConditionStatus(context.Background(), dyn, widgets, "ns", "a", "Ready", "False", "", fast(nil))
	if err == nil || !strings.Contains(err.Error(), "Ready=True") {
		t.Errorf("wrong status: %v", err)
	}
	_, err = ConditionStatus(context.Background(), dyn, widgets, "ns", "a", "Missing", "True", "", fast(nil))
	if err == nil || !strings.Contains(err.Error(), "condition Missing absent (2 conditions)") {
		t.Errorf("absent condition: %v", err)
	}
}

// TestFieldEquals checks equal, different, absent and unreadable fields.
func TestFieldEquals(t *testing.T) {
	t.Parallel()
	dyn := fakeDyn(widget("ns", "a", map[string]any{"replicas": int64(3), "ids": []any{"x", "y"}, "name": "n"}))
	ctx := context.Background()
	if _, err := FieldEquals(ctx, dyn, widgets, "ns", "a", []string{"status", "replicas"}, int64(3), fast(nil)); err != nil {
		t.Errorf("int: %v", err)
	}
	if _, err := FieldEquals(ctx, dyn, widgets, "ns", "a", []string{"status", "ids"}, []any{"x", "y"}, fast(nil)); err != nil {
		t.Errorf("list: %v", err)
	}
	_, err := FieldEquals(ctx, dyn, widgets, "ns", "a", []string{"status", "replicas"}, int64(2), fast(nil))
	if err == nil || !strings.Contains(err.Error(), "field = 3") || !strings.Contains(err.Error(), "status.replicas == 2") {
		t.Errorf("different: %v", err)
	}
	_, err = FieldEquals(ctx, dyn, widgets, "ns", "a", []string{"status", "nope"}, "x", fast(nil))
	if err == nil || !strings.Contains(err.Error(), "field absent") {
		t.Errorf("absent: %v", err)
	}
	_, err = FieldEquals(ctx, dyn, widgets, "ns", "a", []string{"status", "name", "deeper"}, "x", fast(nil))
	if err == nil || !strings.Contains(err.Error(), "field unreadable") {
		t.Errorf("unreadable: %v", err)
	}
}

// TestGone checks an absent object, a terminating one, a plain one, a read
// error and a deletion during the wait.
func TestGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if err := Gone(ctx, fakeDyn(), widgets, "ns", "a", fast(nil)); err != nil {
		t.Errorf("absent: %v", err)
	}
	err := Gone(ctx, fakeDyn(widget("ns", "a", nil)), widgets, "ns", "a", fast(nil))
	if err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Errorf("present: %v", err)
	}
	term := widget("ns", "b", nil)
	now := metav1.Now()
	term.SetDeletionTimestamp(&now)
	term.SetFinalizers([]string{"example.io/hold"})
	err = Gone(ctx, fakeDyn(term), widgets, "ns", "b", fast(nil))
	if err == nil || !strings.Contains(err.Error(), "terminating since") || !strings.Contains(err.Error(), "example.io/hold") {
		t.Errorf("terminating: %v", err)
	}
	broken := fakeDyn(widget("ns", "c", nil))
	broken.PrependReactor("get", "widgets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	if err := Gone(ctx, broken, widgets, "ns", "c", fast(nil)); err == nil || !strings.Contains(err.Error(), "get failed: boom") {
		t.Errorf("read error: %v", err)
	}

	dyn := fakeDyn(widget("ns", "d", nil))
	go func() {
		time.Sleep(5 * time.Millisecond)
		_ = dyn.Resource(widgets).Namespace("ns").Delete(ctx, "d", metav1.DeleteOptions{})
	}()
	o := fast(nil)
	o.Timeout = 5 * time.Second
	if err := Gone(ctx, dyn, widgets, "ns", "d", o); err != nil {
		t.Errorf("deleted during wait: %v", err)
	}
}
