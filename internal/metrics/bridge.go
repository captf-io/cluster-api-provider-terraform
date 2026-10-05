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

package metrics

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	cbmetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Bridge is a sigs.k8s.io/controller-runtime/pkg/metrics.RegistererGatherer
// that keeps registration going to controller-runtime's own registry
// (Register, MustRegister, Unregister: unchanged behavior for controller-
// runtime, client-go and workqueue instrumentation) but Gathers the union
// of that registry, CAPTF's own component-base metrics.KubeRegistry and
// component-base's legacyregistry, since controller-runtime's server
// builds its /metrics handler from the ctrlmetrics.Registry package
// variable alone (pkg/metrics/server/server.go). Build one with NewBridge
// and install it with Install, once, before ctrl.NewManager: the handler
// is built the first time the metrics server starts, so the replacement
// must land before then.
type Bridge struct {
	original prometheus.Registerer
	gatherer prometheus.Gatherer
}

var _ ctrlmetrics.RegistererGatherer = &Bridge{}

// NewBridge returns a Bridge that registers on original (controller-
// runtime's own registry, normally the current value of
// sigs.k8s.io/controller-runtime/pkg/metrics.Registry) and Gathers the
// union of original, captf (CAPTF's own metrics.KubeRegistry) and legacy
// (component-base's legacyregistry gatherer, normally
// legacyregistry.DefaultGatherer). legacy's go_* and process_* families
// are dropped before the merge: both controller-runtime's registry
// (sigs.k8s.io/controller-runtime/pkg/internal/controller, registered as
// soon as pkg/controller is imported, which every CAPTF controller does)
// and component-base's legacyregistry register a Go and a process
// collector, and prometheus.Gatherers.Gather fails on the duplicate
// samples an unfiltered merge would produce; controller-runtime's copy is
// kept since it is the one already served today.
func NewBridge(original ctrlmetrics.RegistererGatherer, captf cbmetrics.KubeRegistry, legacy prometheus.Gatherer) *Bridge {
	return &Bridge{
		original: original,
		gatherer: prometheus.Gatherers{original, captf, filteredGatherer{gatherer: legacy, drop: dropGoProcess}},
	}
}

// Install replaces sigs.k8s.io/controller-runtime/pkg/metrics.Registry
// with a Bridge wrapping its current value and captf, so that controller-
// runtime's metrics server, built from that package variable, serves
// controller-runtime's own series, captf's series (captf) and component-
// base's legacyregistry (where kubernetes_feature_enabled and any other
// component-base series live). Call it once, before ctrl.NewManager.
func Install(captf cbmetrics.KubeRegistry) {
	ctrlmetrics.Registry = NewBridge(ctrlmetrics.Registry, captf, legacyregistry.DefaultGatherer)
}

// Register registers c on b's original controller-runtime registry, and
// returns any registration error.
func (b *Bridge) Register(c prometheus.Collector) error {
	return b.original.Register(c)
}

// MustRegister registers every collector in cs on b's original controller-
// runtime registry, and panics on the first registration error.
func (b *Bridge) MustRegister(cs ...prometheus.Collector) {
	b.original.MustRegister(cs...)
}

// Unregister unregisters c from b's original controller-runtime registry,
// and reports whether c was registered.
func (b *Bridge) Unregister(c prometheus.Collector) bool {
	return b.original.Unregister(c)
}

// Gather gathers and returns the merged metric families of b's original
// controller-runtime registry, CAPTF's own registry and component-base's
// filtered legacyregistry, and any error encountered.
func (b *Bridge) Gather() ([]*dto.MetricFamily, error) {
	return b.gatherer.Gather()
}

// dropGoProcess reports whether name is a go_ or process_ metric family:
// controller-runtime's own registry already serves those (see NewBridge),
// so filteredGatherer drops them from component-base's legacyregistry.
func dropGoProcess(name string) bool {
	return strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_")
}

// filteredGatherer wraps gatherer, dropping every metric family whose name
// drop reports true for.
type filteredGatherer struct {
	gatherer prometheus.Gatherer
	drop     func(name string) bool
}

// Gather gathers f's wrapped gatherer and returns its families with every
// one f.drop reports true for removed, and any error the wrapped Gather
// returned.
func (f filteredGatherer) Gather() ([]*dto.MetricFamily, error) {
	mfs, err := f.gatherer.Gather()
	kept := make([]*dto.MetricFamily, 0, len(mfs))
	for _, mf := range mfs {
		if f.drop(mf.GetName()) {
			continue
		}
		kept = append(kept, mf)
	}
	return kept, err
}
