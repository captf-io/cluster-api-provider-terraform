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

// Package terraformmachine reconciles TerraformMachines: the machine-role
// adapter of internal/controllers/shared. A machine is immutable: its
// first apply's inputs, identity and image digest are pinned, and every
// later operation runs them. The adapter looks the Machine and Cluster up
// (a control-plane-owned machine waits for its Machine ownerRef), gates on
// cluster infrastructure, the cluster's exports and the bootstrap data,
// renders base64 bootstrap data, and maps provider_id once and never
// blanks it.
package terraformmachine
