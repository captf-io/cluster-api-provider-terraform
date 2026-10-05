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

// Package jobs builds the batch/v1 Job of an operation, names it, lists,
// prunes and reads it.
//
// Build renders one Job's PodSpec from a Spec: the init container that
// copies the static runner binary, the source container that runs it against
// the operation's image, and the fixed resources, security contexts and
// volumes the image contract requires. Name and Hash6 give every Job a
// deterministic, collision-resistant name capped to fit the kubelet's pod
// hostname limit; OwnsPod recovers that name's owner from a pod name for
// stale-lock detection. Active, Attempt and Prune read a listed set of an
// object's Jobs to find the running one, compute the next attempt number,
// and delete finished Jobs beyond the retained history per operation.
// OutcomeOf, FinishedAt, DeadlineExceeded and ParseResult read a Job or its
// pod's status and termination message to learn how it finished. Runner is
// the seam the controller and its tests use to create, list and delete Jobs
// and list their pods, so unit tests can fake it.
package jobs
