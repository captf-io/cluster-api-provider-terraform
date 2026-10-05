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

// Package outputs turns the root outputs read from state into the typed
// contract outputs, and validates every value against the CAPI CRD markers
// before anything reaches status. A status patch carrying one invalid value
// would fail entirely, conditions included.
//
// Absent means null. The generated root re-exports every required output, a
// module lacking one fails `validate` before it can apply, and neither
// Terraform nor OpenTofu persists an output whose value is null. So after an
// apply, an output missing from state is a null output. Only health, which
// is never nullable, can be missing.
//
// DecodeMachine and DecodeCluster are the entry points: each reads a role's
// outputs from a decoded state.State and returns the typed
// contract.MachineOutputs/ClusterOutputs alongside a Result that records
// every problem (Missing, Pending, Violations) and reduces to the
// OutputsValid condition's status, reason and message. The Validate*
// functions in validate.go check individual values against the CRD's own
// markers so a bad output is caught before status is patched. addresses.go
// sorts and deduplicates decoded addresses into canonical order, and
// capi.go converts the decoded and validated outputs into the Cluster API
// types (clusterv1.MachineAddress, clusterv1.APIEndpoint, clusterv1.
// FailureDomain) the controllers write to spec and status.
//
// The functions are pure; the package imports internal/state only for the
// State type.
package outputs
