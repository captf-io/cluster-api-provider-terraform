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

// Package shared is the kind-agnostic core of the Job-running reconcilers:
// the preamble (externally-managed → owner → finalizer → pause), Job
// bookkeeping, the operation decision, Job start and post-destroy cleanup.
// The cluster and machine reconcilers supply a Kind adapter: owner lookup,
// input building and output mapping.
//
// Order matters and is the point of this package: block-move is written
// before a Job exists, cleared by a paused reconcile once no Job runs,
// state is read only when no Job is active, provisioned is derived from
// state and latched, and a state Secret without an inputs hash re-applies
// even on an immutable machine. Once no Job is active, an unpaused pass
// also re-owns the object's Secrets whose owner references are missing or
// name an earlier UID (repairOwners), as a management-cluster restore
// leaves them.
package shared
