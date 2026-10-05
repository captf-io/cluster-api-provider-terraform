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

// Package identity resolves the TerraformClusterIdentity an object uses,
// enforces its allowedNamespaces, and mirrors its credential Secret into the
// object namespace.
//
// Identities and their Secrets may live outside the namespaces the manager
// caches, so everything that reads them takes a client.Reader that callers
// wire to mgr.GetAPIReader(). The identity has no finalizer: these
// functions only report, and the TerraformCluster and TerraformMachine
// controllers turn their results into the IdentityAllowed and
// CredentialsMirrored conditions. The identity's own status (Ready,
// status.namespaces) is set by internal/controllers/terraformclusteridentity,
// and its delete webhook refuses deletion while FirstUser finds a user or
// status.namespaces is non-empty. The user's credential Secret is never owned
// by the identity (EnsureSourceOwnerRef).
package identity
