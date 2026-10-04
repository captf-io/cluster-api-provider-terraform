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

// Package terraformcluster reconciles TerraformClusters: the cluster-role
// adapter of internal/controllers/shared. It builds the cluster inputs,
// with the control_plane_initialized latch and the endpoint provenance
// that keeps a module-created endpoint from ever being fed back to its
// module, maps the outputs to spec.controlPlaneEndpoint and
// status.failureDomains, and holds deletion until the cluster's machines
// are gone.
package terraformcluster
