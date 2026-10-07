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

// Package terraformclusteridentity reconciles the status of
// TerraformClusterIdentities. It only reports: Ready says whether the
// credentials Secret exists, and status.namespaces lists the namespaces
// holding a mirror of it. The identity has no finalizer, and mirrors are
// still written and revoked by the TerraformCluster and TerraformMachine
// controllers (internal/identity). The delete webhook reads
// status.namespaces to refuse deleting an identity that is still mirrored.
//
// Reconciler.Reconcile reads the source Secret through an uncached API
// reader (the Secret is unlabeled and usually outside the manager's cache
// scope), sets Ready to SecretFound or SecretNotFound accordingly, and
// requeues periodically (Reconciler.RequeueAfter, defaulting to
// DefaultRequeueAfter) so a Secret created or deleted out of band is
// noticed without a watch. Reconciler.SetupWithManager also watches the
// metadata of credential mirror Secrets and maps a changed mirror back to
// its identity with MirrorToIdentity, so status.namespaces stays current
// as mirrors come and go. The mirrors and the objects using the identity
// are read from the manager's cache (Reconciler.Cache), never listed live.
package terraformclusteridentity
