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

// Package inputs owns the Secrets a run depends on:
//
//   - captf-inputs-<kindshort>-<name>, the durable Secret, owned by the
//     Terraform* object: the attempt record, the files of the newest apply
//     Job created and what it runs with (image, identity, inputs hash,
//     Job), and the object's metadata hub. WriteAttempt writes the record
//     once the Job exists, Read returns it with the rest as a Durable,
//     MarkApplied records that the object ever applied (a missing state is
//     then a lost one), SetMayHaveApplied and ClearMayHaveApplied mark the
//     record's Job as one that may have changed resources,
//     SetInterruptedApply and ClearInterruptedApply record and remove an
//     apply Job whose outcome is unconfirmed, and Delete removes it with
//     the applied Secret. A TerraformMachinePool's Secret also records the
//     cluster exports of its last successful apply and their hash
//     (RecordClusterOutputs, or SeedClusterOutputs, once, for a pool that
//     applied before the record existed), a change of those that waits
//     for approval (SetPending), and one that a failed apply may have
//     partly applied (SetPartial).
//   - captf-applied-<kindshort>-<name>, the applied Secret, owned and
//     labeled like the durable one: the applied record, the files of the
//     newest apply Job that succeeded, with the image digest its pod ran
//     (Promote). It moves with the object, and destroy, and an immutable
//     machine's drift checks, render from it, not from an attempt that
//     failed or never ran. It is a Secret of its own because two copies of
//     the files may not fit one.
//   - captf-run-<job>, owned by the Job: a copy the Job mounts, so a running
//     Job never sees a rewrite. CreateRun writes it before the Job's pod
//     starts, ReadRun reads it back for the promotion of a successful
//     apply, and DeleteRun removes it once the controller has done so.
//
// LastControlPlaneInitialized, LastControlPlaneEndpointNull and
// LastClusterOutputs read fields
// back out of a Record's rendered tfvars, for state that Cluster
// status cannot carry across a clusterctl move. PoolInputsHash hashes a
// pool's rendered inputs back, to tell whether they are an apply Job's.
//
// All three carry bootstrap data in cleartext by design: never log their
// data.
package inputs
