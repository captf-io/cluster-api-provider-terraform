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

package shared

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
)

// capture returns a buffered klog test logger for t, at the given verbosity
// level, and the ktesting.Underlier that grants access to what it logged.
func capture(t *testing.T, verbosity int) (klog.Logger, ktesting.Underlier) {
	t.Helper()
	logger := ktesting.NewLogger(t, ktesting.NewConfig(ktesting.BufferLogs(true), ktesting.Verbosity(verbosity)))
	u, ok := logger.GetSink().(ktesting.Underlier)
	if !ok {
		t.Fatal("klog test logger sink is not a ktesting.Underlier")
	}
	return logger, u
}

// TestLogRendered: the trace line carries the root with bootstrap_data
// redacted; the debug line names variables, never values.
func TestLogRendered(t *testing.T) {
	t.Parallel()
	secret := "c2VjcmV0LWJvb3RzdHJhcA=="
	files := render.Files{MainTF: []byte(`{"module":{}}`), TFVars: []byte(`{"bootstrap_data":"` + secret + `","machine_name":"m1"}`)}
	for _, v := range []int{0, LogDebug, LogTrace} {
		logger, u := capture(t, v)
		logRendered(klog.NewContext(t.Context(), logger), "h1:abc", files)
		got := u.GetBuffer().String()
		if strings.Contains(got, secret) {
			t.Fatalf("v=%d leaked bootstrap data: %s", v, got)
		}
		if (v >= LogDebug) != strings.Contains(got, "bootstrap_data") {
			t.Errorf("v=%d: %s", v, got)
		}
		if (v >= LogTrace) != strings.Contains(got, "<redacted:len=") {
			t.Errorf("v=%d: %s", v, got)
		}
	}
}

// TestWithObjectLogger proves WithObjectLogger attaches Cluster, Machine and
// MachinePool key-value pairs to the context logger when the OwnerInfo
// carries them, and attaches none when the OwnerInfo is empty.
func TestWithObjectLogger(t *testing.T) {
	t.Parallel()
	logger, u := capture(t, 0)
	owner := OwnerInfo{
		Cluster:     &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c1"}},
		Machine:     &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "m1"}},
		MachinePool: &clusterv1.MachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p1"}},
	}
	klog.FromContext(WithObjectLogger(klog.NewContext(t.Context(), logger), owner)).Info("x")
	if got := u.GetBuffer().String(); !strings.Contains(got, `Cluster="ns/c1"`) || !strings.Contains(got, `Machine="ns/m1"`) ||
		!strings.Contains(got, `MachinePool="ns/p1"`) {
		t.Errorf("line = %s", got)
	}
	logger2, u2 := capture(t, 0)
	klog.FromContext(WithObjectLogger(klog.NewContext(t.Context(), logger2), OwnerInfo{})).Info("y")
	if got := u2.GetBuffer().String(); strings.Contains(got, "Cluster") {
		t.Errorf("no owner: %s", got)
	}
}
