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

// Package images prepares the container images the test environment runs.
// TreeID names the working tree (short HEAD sha, plus a hash of any
// uncommitted change), ManagerRef turns it into the manager image
// reference, BuildManager builds that image with `make docker-build`, and
// SmokeManager runs both /manager and /runner with --help so a stale or
// broken image fails in seconds rather than crash-looping in the cluster.
// NodePull pulls an image inside a kind node with crictl, so the node holds
// it with the registry's real repo digests (the pinned noop module images
// need those for CAPTF's digest pinning); SideLoad copies images from the
// engine's storage into every kind node, with no registry, which drops
// repo digests and so suits only the manager image; NodeImage reads back
// what a node's containerd holds, including its repo digests.
//
// Engine commands and git run through engine.Runner and nodes through the
// kind nodes.Node interface, so unit tests use enginetest.Runner and a
// fake node and spawn nothing. Every error is prefixed "images: <op>:" and
// wraps its cause. Long operations honor the context between steps and
// log progress to the writer given with WithLog.
package images
