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

package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	apimachineryversion "k8s.io/apimachinery/pkg/version"
	cbmetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	featuremetrics "k8s.io/component-base/metrics/prometheus/feature"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// TestNewBridgeRegisters proves a Bridge's Register, MustRegister and
// Unregister reach its original registry, unchanged from registering on
// original directly.
func TestNewBridgeRegisters(t *testing.T) {
	t.Parallel()
	original := prometheus.NewRegistry()
	b := NewBridge(original, cbmetrics.NewKubeRegistry(), prometheus.NewRegistry())

	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "bridge_register_test_total", Help: "test"})
	if err := b.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if mfs, err := original.Gather(); err != nil || len(mfs) != 1 || mfs[0].GetName() != "bridge_register_test_total" {
		t.Errorf("original after Register: %v (%v)", mfs, err)
	}
	if !b.Unregister(c) {
		t.Error("Unregister reported false")
	}
	if mfs, err := original.Gather(); err != nil || len(mfs) != 0 {
		t.Errorf("original after Unregister: %v (%v)", mfs, err)
	}

	d := prometheus.NewCounter(prometheus.CounterOpts{Name: "bridge_mustregister_test_total", Help: "test"})
	b.MustRegister(d)
	if mfs, err := original.Gather(); err != nil || len(mfs) != 1 || mfs[0].GetName() != "bridge_mustregister_test_total" {
		t.Errorf("original after MustRegister: %v (%v)", mfs, err)
	}
}

// TestNewBridgeGatherMergesAndFilters proves Gather merges original,
// captf and legacy without error, and drops legacy's go_* and process_*
// families (both original and legacy would otherwise register their own
// Go and process collectors, and prometheus.Gatherers.Gather fails on the
// resulting duplicate samples).
func TestNewBridgeGatherMergesAndFilters(t *testing.T) {
	t.Parallel()
	original := prometheus.NewRegistry()
	original.MustRegister(
		prometheus.NewCounter(prometheus.CounterOpts{Name: "controller_runtime_test_probe_total", Help: "test"}),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	captfReg := cbmetrics.NewKubeRegistry()
	if err := Register(captfReg, apimachineryversion.Info{GitVersion: "v0.0.0-test", GitCommit: "deadbeef"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	legacy := prometheus.NewRegistry()
	legacy.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prometheus.NewCounter(prometheus.CounterOpts{Name: "legacy_kept_total", Help: "test"}),
	)

	b := NewBridge(original, captfReg, legacy)
	mfs, err := b.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	for _, want := range []string{"controller_runtime_test_probe_total", BuildInfoName, "legacy_kept_total"} {
		if !names[want] {
			t.Errorf("Gather: missing family %s (have %v)", want, names)
		}
	}
	goFamilies := 0
	for _, mf := range mfs {
		if mf.GetName() == "go_goroutines" {
			goFamilies++
		}
	}
	if goFamilies != 1 {
		t.Errorf("go_goroutines families = %d, want 1", goFamilies)
	}
}

// TestInstall proves Install replaces
// sigs.k8s.io/controller-runtime/pkg/metrics.Registry with a Bridge whose
// Register still reaches the value Install found there. It is not
// parallel: it mutates that package variable, restored on cleanup.
func TestInstall(t *testing.T) {
	original := ctrlmetrics.Registry
	t.Cleanup(func() { ctrlmetrics.Registry = original })

	captfReg := cbmetrics.NewKubeRegistry()
	Install(captfReg)
	if _, ok := ctrlmetrics.Registry.(*Bridge); !ok {
		t.Fatalf("ctrlmetrics.Registry = %T, want *Bridge", ctrlmetrics.Registry)
	}

	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "install_test_total", Help: "test"})
	if err := ctrlmetrics.Registry.Register(c); err != nil {
		t.Fatalf("Register through the installed Bridge: %v", err)
	}
	// original is the real, process-global controller-runtime registry:
	// unregister what this test added, so a repeat run of the binary
	// (-count=2) does not fail on a duplicate descriptor.
	t.Cleanup(func() { original.Unregister(c) })
	if mfs, err := original.Gather(); err != nil || len(mfs) != 1 || mfs[0].GetName() != "install_test_total" {
		t.Errorf("the pre-Install registry after Register: %v (%v)", mfs, err)
	}
}

// TestBridgeServesMergedMetrics is the cluster-free check that the bridge
// serves controller-runtime's own series, captf's series and component-
// base's legacyregistry (kubernetes_feature_enabled) together, exactly as
// controller-runtime's metrics server would build its handler
// (promhttp.HandlerFor(ctrlmetrics.Registry, ...)), without starting a
// manager. It is not parallel: it reads the shared legacyregistry.
func TestBridgeServesMergedMetrics(t *testing.T) {
	original := prometheus.NewRegistry()
	original.MustRegister(
		prometheus.NewCounter(prometheus.CounterOpts{Name: "controller_runtime_test_probe_total", Help: "a controller-runtime-style series"}),
		collectors.NewGoCollector(),
	)

	captfReg := cbmetrics.NewKubeRegistry()
	if err := Register(captfReg, apimachineryversion.Info{GitVersion: "v0.0.0-test", GitCommit: "deadbeef"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// kubernetes_feature_enabled is a GaugeVec with no samples until
	// something records one; importing the feature package and recording
	// one value is what featuregate.Set does in the real manager.
	featuremetrics.RecordFeatureInfo(t.Context(), "TestFeature", "ALPHA", true)

	bridge := NewBridge(original, captfReg, legacyregistry.DefaultGatherer)
	handler := promhttp.HandlerFor(bridge, promhttp.HandlerOpts{ErrorHandling: promhttp.HTTPErrorOnError})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	for _, want := range []string{
		"controller_runtime_test_probe_total",
		BuildInfoName,
		"kubernetes_feature_enabled",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if n := strings.Count(body, "# TYPE go_goroutines"); n != 1 {
		t.Errorf("# TYPE go_goroutines lines = %d, want 1:\n%s", n, body)
	}
}
