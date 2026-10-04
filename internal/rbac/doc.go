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

// Package rbac gives the runner its identity in each tenant namespace: the
// ServiceAccount captf-runner (or an operator's opted-in ServiceAccount) and
// a RoleBinding to the static ClusterRole captf-runner. It also sweeps both,
// with the lock Leases, out of namespaces that no longer hold any Terraform*
// object.
//
// No Role is ever created. The manager binds the ClusterRole through the
// bind verb restricted to its name, so it does not need to hold the
// ClusterRole's permissions itself.
package rbac
