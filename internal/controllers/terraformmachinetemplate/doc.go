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

// Package terraformmachinetemplate reconciles TerraformMachineTemplates:
// status.capacity and status.nodeInfo from the source image's capacity
// labels, for Cluster Autoscaler scale-from-zero. No finalizer, no Jobs,
// no state.
//
// Reconciler.Reconcile inspects the template's spec.template.spec.source
// image once per image reference: Resolved reports when the status already
// describes that image (resolved, or invalidly labeled, since the tag is
// never re-polled), and a fresh image is picked up because a new image
// reference means a new, unresolved template. A failed inspection (a
// missing pull Secret, an unreachable or unauthorized registry) keeps the
// previous capacity and retries with a backoff computed by failedRetry
// (InspectFailure classifies the failure for the condition message and
// event, without ever surfacing the registry's own error text). Deps
// (internal/controllers/shared) supplies the image inspector, clock, event
// recorder and metrics this controller uses.
package terraformmachinetemplate
