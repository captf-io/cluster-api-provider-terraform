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

// Package health holds reusable health checks for a management cluster,
// for the e2e foundation suite and for any later test that needs to know
// the cluster is sound.
//
// The checks are APIServer (readyz and livez), Nodes, Pods (a list of
// Problems), Workloads (Deployments, DaemonSets, StatefulSets), Stable
// (no restarts, no pod churn and no pod going not-ready over a window),
// PodProxyGet, LeaseHeld and LeaseRenewing, ScanLogs (fatal log lines,
// with allowlists) and WarningEvents.
//
// Each check splits a pure evaluator from the client call that feeds it,
// so the rules are table-tested without a cluster. Every check reports all
// the problems it finds, one per line, never only the first. Namespace
// lists passed to a check mean "these namespaces"; an empty list means all
// namespaces. All checks use wait.Clients, so they run against the
// client-go fakes in unit tests. Errors are prefixed "health:" and wrap
// their cause.
package health
