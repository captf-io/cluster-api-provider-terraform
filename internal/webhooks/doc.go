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

// Package webhooks holds the validating admission webhooks for CAPTF's
// seven v1 kinds: TerraformCluster, TerraformClusterTemplate,
// TerraformMachine, TerraformMachineTemplate, TerraformMachinePool,
// TerraformMachinePoolTemplate and TerraformClusterIdentity. Each kind has
// its own file and its own type implementing
// controller-runtime's admission.Validator, registered on the manager by
// SetupWebhooks; setup.go also refuses to start if the manager's scheme
// lacks the CAPI core Machine type the TerraformMachine delete check needs.
// There are no mutating webhooks: no runtime, job or drift default is
// persisted on an object, since every default is resolved at reconcile
// instead. Every rejection is built with invalid into an
// apierrors.NewInvalid carrying field.Forbidden or field.Invalid causes,
// because KCP's in-place update dry-run treats only IsInvalid/IsForbidden
// responses as "fall back to a rollout" for an immutable-field change.
//
// common.go holds the validation shared across kinds: validateSource checks
// an image reference, validateJobPolicy checks a Job security context and
// lock/deadline relationship, validateVariables checks spec.variables and
// spec.variablesFrom against the contract package's per-role rules, and
// equalVariables compares two variables blobs by value so a
// re-serialization is never mistaken for a change to an immutable field.
// templates.go holds what the *Template kinds share: spec.template.spec is
// immutable except during a ClusterClass topology dry-run
// (skipImmutability), and spec.template.metadata must be valid labels and
// annotations. The TerraformClusterIdentity webhook additionally runs a
// SubjectAccessReview so an identity may only reference a Secret its
// author can read, and its delete check refuses to remove an identity a
// TerraformCluster or TerraformMachine still uses, or whose credentials are
// still mirrored into a namespace.
package webhooks
