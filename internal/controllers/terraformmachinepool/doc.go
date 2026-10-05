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

// Package terraformmachinepool reconciles TerraformMachinePools: the
// machinepool-role adapter of internal/controllers/shared. A pool is
// mutable: a change of its inputs (replicas, failure domains, node labels,
// Kubernetes version, the bootstrap data, the cluster's exports and
// failure domains) re-applies it. The adapter looks the MachinePool and
// Cluster up, gates on cluster infrastructure, the cluster's outputs and
// the bootstrap data, renders base64 bootstrap data, and maps the group's
// provider IDs, replicas and instances after every apply or refresh, whose
// membership converges asynchronously (shared.MembershipObserver).
//
// Only an apply that renders a change of the cluster's exports is guarded
// against a plan that deletes or replaces resources
// (shared.ExportsGuard). While such a change waits for approval of its
// approval hash, the pool keeps applying everything else with the exports
// of its last successful apply. Once a guarded apply failed after its plan
// passed the guard, the state may hold part of a change, so until an
// apply succeeds every apply is guarded and none falls back to those
// exports: a destructive plan then waits for approval, as a cluster's
// does. The same holds while the exports of the last successful apply
// are unknown: a pool that applied before this version recorded them is
// seeded once with the exports it provably applied (its newest
// successful apply's, from durable inputs that still hash to the state's,
// or its current ones when no apply Job is retained and they hash to the
// state's), and otherwise guards its every apply until one succeeds and
// records them. An apply Job deleted while it ran keeps an apply due, as
// for every mutable kind.
package terraformmachinepool
