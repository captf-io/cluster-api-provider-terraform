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

// Package metrics defines CAPTF's series with k8s.io/component-base/metrics,
// the way Kubernetes components declare
// their own, all at StabilityLevel ALPHA, and registers them on a
// component-base metrics.KubeRegistry the manager builds for itself
// (metrics.NewKubeRegistry), not on controller-runtime's own registry.
// Bridge (built by NewBridge, installed by Install) replaces
// sigs.k8s.io/controller-runtime/pkg/metrics.Registry with a wrapper that
// keeps Register/MustRegister/Unregister going to controller-runtime's
// original registry, so controller-runtime, client-go and workqueue
// instrumentation are unaffected, but Gathers the union of that registry,
// CAPTF's own KubeRegistry and component-base's legacyregistry (where
// kubernetes_feature_enabled and any other component-base series live),
// with legacyregistry's go_* and process_* families dropped, since
// controller-runtime's registry already serves those and gathering both
// unfiltered fails on duplicate samples. cmd/manager calls Install once,
// before ctrl.NewManager, so the manager's diagnostics endpoint serves
// every source through controller-runtime's usual handler.
//
// Specs is the single table of every captf_* series (name, Prometheus type,
// labels and help text); every collector is built from its entry, so
// a series and its help text cannot drift apart. Recorder, built with
// New and registered with Recorder.Register, holds the actual collectors
// and exposes one method per series (JobFinished, Decision, SetObject,
// SetState, and so on) that reconcilers call as they work; every method is
// a no-op on a nil *Recorder, so code under test needs none. ActiveJobs is
// a custom metrics.StableCollector counting running Jobs from the
// manager's Job cache at scrape time, since a per-object reconcile cannot
// keep an aggregate gauge right and counting up and down would drift
// across restarts.
//
// Cardinality rule: labels are bounded enums (kind, op, result, reason,
// step, action, error_kind). Only per-object gauges (PerObject) carry
// namespace and name: a handful per object, each deleted with its object
// (DeleteObject).
//
// Nothing registers itself: cmd/manager builds a Recorder, registers it and
// hands it to the reconcilers.
package metrics
