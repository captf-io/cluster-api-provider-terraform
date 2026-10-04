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

package framework

// Names shared by every package, so the safety rules in doc.go are spelled
// once. The guards that enforce them live in kindcluster and env.

// ClusterNamePrefix is the prefix every test-environment kind cluster name
// carries. Any other name is refused, and Down only ever deletes clusters
// with this prefix.
const ClusterNamePrefix = "captf-test-"

// DefaultClusterName is the cluster name used when none is given.
const DefaultClusterName = ClusterNamePrefix + "dev"

// ProtectedClusterName is the operator's own kind cluster. The guards
// refuse it by name, in addition to the prefix rule, and nothing in this
// module may touch it.
const ProtectedClusterName = "kube-php-client"

// KindNetwork is the dedicated container network kind places the nodes
// on, set in-process through the engine's KindNetworkEnv variable, so a
// test cluster never shares kind's default "kind" network.
const KindNetwork = "captf-test"

// EngineEnv is the environment variable that overrides container-engine
// detection: "podman" or "docker". Empty means auto-detect.
const EngineEnv = "CAPTF_TESTENV_ENGINE"
