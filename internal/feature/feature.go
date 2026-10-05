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

package feature

import (
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/component-base/featuregate"
	logsv1 "k8s.io/component-base/logs/api/v1"
)

// NewGates returns a fresh mutable gate set with the component-base logging
// gates (ContextualLogging, LoggingAlphaOptions, LoggingBetaOptions)
// registered at their defaults, as every Kubernetes component registers
// them, so logsv1.ValidateAndApply can read them. CAPTF defines no gates of
// its own in v1.
func NewGates() featuregate.MutableFeatureGate {
	g := featuregate.NewFeatureGate()
	// Registering into a new, empty set fails only on a duplicate name, a
	// programming error; Kubernetes components treat it the same way.
	utilruntime.Must(logsv1.AddFeatureGates(g))
	return g
}
