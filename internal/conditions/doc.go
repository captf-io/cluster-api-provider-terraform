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

// Package conditions is the condition model as code: which conditions feed
// Ready per kind and phase, how Ready is summarized, the gating and
// first-visit helpers, and the health output → InfrastructureHealthy
// mapping. Reconcilers call these helpers and never hand-roll polarity or
// reasons.
//
// The condition types and reasons themselves live in
// api/v1alpha1/conditions_consts.go. Ready is the only condition Cluster API
// mirrors (into InfrastructureReady), and MachineHealthCheck reads it, so
// after provisioning it reflects only the infrastructure's health and
// deletion.
package conditions
