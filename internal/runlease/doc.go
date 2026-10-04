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

// Package runlease holds the coordination.k8s.io/v1 Leases that keep Jobs
// apart:
//
//   - the run lease captf-run-<state suffix> of each Terraform* object: at
//     most one Job of the object runs, whatever the Job cache says (a stale
//     cache, a leader-election handover, two manager instances with
//     overlapping --watch-filter);
//   - the cluster write lease captf-cluster-<hash> of each Cluster: a
//     TerraformCluster apply or destroy and a machine's apply or destroy of
//     the same Cluster never run at once.
//
// The manager holds a lease before it creates the Job it names
// (spec.holderIdentity is the deterministic Job name). A lease is free when
// that Job has finished, or does not exist and the lease was taken more
// than Grace ago, or when it is older than its backstop
// (spec.leaseDurationSeconds). Every write is a Create or an Update with the
// read resourceVersion, and every read goes to the API server, so two
// managers racing for one lease cannot both win.
//
// The Leases carry captf.io/managed=true and captf.io/lease=run|cluster.
// The backend's own state lock (lock-tfstate-default-<suffix>, package
// locks) carries the same owner labels but no captf.io/lease label, and a
// different name: nothing here reads or writes it.
package runlease
