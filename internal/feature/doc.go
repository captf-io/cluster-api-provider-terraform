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

// Package feature holds CAPTF's own feature gates, set on the manager
// through the --feature-gates flag in the same "Key1=value1,Key2=value2"
// form component-base uses for every other Kubernetes component. NewGates
// is the package's single entry point: it builds a component-base
// featuregate.MutableFeatureGate with every CAPTF gate registered at its
// default, ready for a flag set to bind and a caller to query.
//
// The set also carries component-base's logging gates (ContextualLogging,
// LoggingAlphaOptions, LoggingBetaOptions), which logsv1.ValidateAndApply
// reads, exactly as in kube-controller-manager.
//
// v1 defines no gates of its own; the set exists so an alpha switch, such as
// a future PatchNodeProviderID, has somewhere to register without changing
// the manager's flag wiring. Unlike CAPI's package-level MutableGates
// global, CAPTF never shares one gate set
// across callers: NewGates returns a fresh, independent instance each time,
// and the manager builds exactly one and passes it explicitly to whatever
// needs to read it. An unregistered gate name is rejected by the underlying
// flag parser, so a typo in --feature-gates fails fast at startup rather
// than being silently ignored.
package feature
