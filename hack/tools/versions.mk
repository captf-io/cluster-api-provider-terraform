# Copyright 2026 The CAPTF Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Pinned tool versions and checksums. Kept apart from the Makefile so CI can
# key its hack/tools/bin cache on this file alone: an unrelated Makefile edit
# must not rebuild every tool.

CONTROLLER_GEN_VER := v0.21.0
KUSTOMIZE_VER := v5.7.0
GOLANGCI_LINT_VER := v2.13.1
# kube-api-linter publishes no tags; this is the newest pseudo-version on
# proxy.golang.org at the time of pinning.
KUBE_API_LINTER_VER := v0.0.0-20260716143926-092fe0c72997
CLUSTERCTL_VER := v1.14.2
# promtool only: the latest Prometheus release on 2026-09-26.
PROMTOOL_VER := v3.15.0
# Highest release whose go directive builds on Go 1.26.x (v2.18.x needs 1.27).
GORELEASER_VER := v2.17.1
# golang.org/x/tools as in the CAPI v1.14.2 module graph.
GOIMPORTS_VER := v0.48.0
# gotestsum wraps `go test` for JUnit output and CI-friendly formatting.
GOTESTSUM_VER := v1.13.0
# setup-envtest is versioned with controller-runtime (v0.24.x); the Kubernetes
# release is the newest 1.36 asset in its index (kube-apiserver and etcd for
# the envtest tier).
SETUP_ENVTEST_VER := v0.24.1
ENVTEST_K8S_VERSION := 1.36.2

# clusterctl cannot be `go install`ed (CAPI's go.mod carries replace
# directives), so the release binary is downloaded and checked against the
# sha256 GitHub reports for each release asset.
CLUSTERCTL_SHA256_linux_amd64 := 01122674fd3c47a33206ab1b8b81d437afbcf5dd25d126535564f24a2cdf676e
CLUSTERCTL_SHA256_linux_arm64 := 83976008aa9ddb81dab01443c646aaa125e4993e17bf24e790e29779f712d79d
CLUSTERCTL_SHA256_darwin_amd64 := 07a8c84719e1c9f8a1f4e9c6f398423ac47de8fb0284ac2145acc134e6ffc1bd
CLUSTERCTL_SHA256_darwin_arm64 := ea2285445da861b2ec96e948563cf158f4e0fd89a36238267b62e43a1bb00da8
# promtool comes out of the Prometheus release tarball, checked against the
# release's sha256sums.txt.
PROMTOOL_SHA256_linux_amd64 := 2a542df32eac02ee17b9d844fb2aa1de00dafa5476579ba8a3ba862e9d572ea0
PROMTOOL_SHA256_linux_arm64 := f1f90ec08e849d494ca66c611470afc50192f0355f1a61c33f2cbde02d067823
PROMTOOL_SHA256_darwin_amd64 := 2d79e744c2d7e505db936fbc898e05abc74fcb6e437c25befd26e9c9f00aa58b
PROMTOOL_SHA256_darwin_arm64 := 920df4d17e78b3b0175af144eb318b0c74d1cf7b1d1251b326966f0e81977260
