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

// Package kindcluster creates, lists and deletes the test environment's
// kind clusters through kind's Go library (sigs.k8s.io/kind/pkg/cluster),
// never the kind CLI and never KIND_EXPERIMENTAL_PROVIDER: the node
// provider comes from engine.Engine.KindProvider.
//
// Safety: every Manager method that takes a cluster name runs it through
// ValidateName first, so a name outside the "captf-test-" prefix, and the
// operator's kube-php-client cluster explicitly, is refused even when a
// method is called directly. List only reports prefixed clusters. Create
// always writes to an explicit kubeconfig path, never ~/.kube/config.
//
// Network: kind reads the node network from a process environment variable
// (engine.Engine.KindNetworkEnv). Create sets it to the dedicated network
// around the provider call and restores the previous value afterwards,
// under a package mutex. That makes concurrent Creates safe with each
// other, but not with other kind users or environment readers in the same
// process.
//
// The kind provider sits behind the small Provider interface, which
// *cluster.Provider satisfies, so unit tests run with a fake and start
// nothing.
package kindcluster
