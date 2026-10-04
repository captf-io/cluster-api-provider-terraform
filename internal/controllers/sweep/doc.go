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

// Package sweep runs the orphan sweep as a leader-elected
// manager.Runnable: at manager start and every --sync-period it deletes
// the captf.io/managed=true ServiceAccounts, RoleBindings and Leases of
// namespaces that hold no TerraformCluster, no TerraformMachine and no
// TerraformMachinePool, which is what clusterctl move leaves on the
// source. The decision reads through the API reader with no
// --watch-filter, so another manager instance's objects keep a
// namespace; see rbac.Sweep.
//
// Leader hand-over needs no code here: every reconcile derives the active
// Job and the attempt from the Job list, never from status.activeJob.
package sweep
