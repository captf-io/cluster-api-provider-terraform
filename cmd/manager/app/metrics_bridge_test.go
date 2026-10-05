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

package app

import (
	"testing"

	apimachineryversion "k8s.io/apimachinery/pkg/version"
	cbmetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
)

// TestMetricsBridgeGathersCleanInThisBinary proves the metrics.Bridge
// filter (dropping legacyregistry's go_* and process_* families) is
// sufficient for what the manager binary actually links, not just for a
// synthetic fixture: this package (cmd/manager/app) imports options,
// which imports featuregate and every controller package server.go wires
// up, so its init functions populate sigs.k8s.io/controller-runtime/pkg/
// metrics.Registry and component-base's legacyregistry exactly as the
// running manager would before metrics.Install ever runs. If a future
// dependency starts registering, say, rest_client_* or workqueue_* on
// both registries, this is where that would show up as a Gather error.
func TestMetricsBridgeGathersCleanInThisBinary(t *testing.T) {
	t.Parallel()
	captf := cbmetrics.NewKubeRegistry()
	if err := metrics.Register(captf, apimachineryversion.Info{GitVersion: "v0.0.0-test", GitCommit: "deadbeef"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	b := metrics.NewBridge(ctrlmetrics.Registry, captf, legacyregistry.DefaultGatherer)
	if _, err := b.Gather(); err != nil {
		t.Errorf("Gather: %v (a duplicate go_*/process_*/other family between controller-runtime's registry and legacyregistry: widen the drop filter)", err)
	}
}
