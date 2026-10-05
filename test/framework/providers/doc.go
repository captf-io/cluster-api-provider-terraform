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

// Package providers prepares and runs `clusterctl init` entirely from local
// files, so the management cluster is built without clusterctl reaching
// GitHub.
//
// EnsureCache downloads the pinned CAPI and cert-manager artifacts
// (framework.Artifacts) into a content-addressed cache, verifying each
// sha256; the network is needed only the first time. RenderCAPTF builds the
// CAPTF provider from the tree with `make manifests-release`.
// WriteRepository then lays out a clusterctl local repository
// (<provider-label>/<version>/<file>, the layout clusterctl's local
// repository requires) plus a clusterctl.yaml whose providers use file://
// URLs and whose cert-manager url points at the cached manifest. Init runs
// `clusterctl init` against it with an isolated environment: its own HOME,
// an explicit KUBECONFIG, GOPROXY=off and the version check disabled.
//
// Every error is prefixed "providers: <op>:" and wraps its cause.
package providers
