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

// Package controllers is the parent of CAPTF's reconcilers: one
// subpackage per kind (terraformcluster, terraformmachine,
// terraformmachinetemplate, terraformclusteridentity), plus sweep for the
// orphan RBAC sweep runnable and shared for the dependencies, clock and
// helpers every reconciler uses. cmd/manager wires each subpackage's
// Reconciler into the controller-runtime manager; this package itself
// holds no reconciler, only what needs to sit above every subpackage.
//
// rbac.go is that: it carries every one of the manager's RBAC markers in
// one place, covering the permissions the manager needs for the v1
// kinds. `make manifests` turns the +kubebuilder:rbac markers into
// config/rbac/role.yaml.
// Resources are listed explicitly rather than with a `terraform*` glob,
// since that is not a valid RBAC resource pattern, and the generated role
// is deliberately not labeled cluster.x-k8s.io/aggregate-to-manager, so it
// is not merged into another aggregate ClusterRole by accident. The
// markers cover the own Terraform* kinds and their status and finalizers
// subresources, the CAPI Cluster and Machine types reconcilers read and
// the remediate-machine annotation they patch, credentials and state
// Secrets, the ConfigMaps and Secrets spec.variablesFrom reads, the
// runner ServiceAccount, RoleBinding and ClusterRole bind permission,
// coordination Leases for state-lock cleanup and the run and cluster
// write leases, Jobs and their Pods and logs, event creation on both the
// legacy core and events.k8s.io/v1 groups, and the TokenReview and
// SubjectAccessReview permissions the secured diagnostics endpoint needs.
package controllers
