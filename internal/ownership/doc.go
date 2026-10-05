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

// Package ownership decides whether a Cluster API object is the genuine
// owner of a TerraformCluster, TerraformMachine or TerraformMachinePool. An
// ownerReference by name alone is not proof of ownership: anyone who can
// create a TerraformMachine or TerraformMachinePool can forge one naming any
// existing Machine or MachinePool in the namespace, and the controllers act
// on the owner they resolve (a remediation annotation on a Machine, replicas
// on a MachinePool). An owner counts only when its own infrastructureRef
// names the object back, the ownerReference's UID (when set) matches, and
// its cluster agrees with the object's cluster-name label. The workspace
// controllers' owner lookups and the TerraformMachine delete webhook both
// use these functions, so the two always agree on what is owned.
//
// It also keeps the owner references of the Secrets CAPTF creates for an
// object (state chunks, state backups, durable inputs, plan key, and the
// object's entry in a shared credential mirror) pointing at the object's
// current UID: EnsureRef and RepairSecret replace a reference that names
// the object with an earlier UID, as a management-cluster restore leaves
// behind, and add a missing one, as a restore with stripped references or
// a state chunk written without one leaves behind. A Secret is claimed
// only when its owner-kind and owner-name labels name the object.
//
// The package is a leaf: it imports only the API types and internal/state's
// kind names and labels, never a controller.
package ownership
