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

// Package inputs owns the two Secrets a run depends on:
//
//   - captf-inputs-<kindshort>-<name>, owned by the Terraform* object: the
//     rendered root and tfvars, and the pinned image, image digest and
//     identity. It moves with the object and is the single record of what
//     was applied; immutable machines drift and destroy from it. Write
//     creates or updates it, Read returns its content as a Durable, PinDigest
//     records the resolved image digest, MarkApplied records that the
//     object ever applied (a missing state is then a lost one),
//     SetInterruptedApply and ClearInterruptedApply record and remove an
//     apply Job that is gone before it finished, and Delete removes the
//     Secret. A TerraformMachinePool's Secret also records the cluster
//     exports of its last successful apply and their hash
//     (RecordClusterOutputs, or SeedClusterOutputs, once, for a pool that
//     applied before the record existed), a change of those that waits
//     for approval (SetPending), and one that a failed apply may have
//     partly applied (SetPartial).
//   - captf-run-<job>, owned by the Job: a copy the Job mounts, so a running
//     Job never sees a rewrite. CreateRun writes it before the Job's pod
//     starts, and DeleteRun removes it once the controller sees the Job
//     finished.
//
// LastControlPlaneInitialized, LastControlPlaneEndpointNull and
// LastClusterOutputs read fields
// back out of a Durable's last-rendered tfvars, for state that Cluster
// status cannot carry across a clusterctl move. PoolInputsHash hashes a
// pool's rendered inputs back, to tell whether they are an apply Job's.
//
// Both Secrets carry bootstrap data in cleartext by design: never log
// their data.
package inputs
