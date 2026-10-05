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

// Package framework is the root of CAPTF's container test environment: a
// reproducible management cluster (kind on podman or docker, cluster-api
// v1.14.2, cert-manager and a CAPTF manager image built from the tree) that
// future test suites run against. This package holds the pins every other
// package shares (versions.go); the work is split into subpackages:
//
//   - engine: container-engine detection (podman preferred, then docker)
//     and the one exec wrapper every engine command goes through;
//   - kindcluster, images, providers, wait and diag: cluster lifecycle,
//     image build and side-load, clusterctl repository and init, readiness
//     polling and diagnostics, each built on engine;
//   - env: the orchestrator behind the e2e-tagged lifecycle entry point.
//
// The test module is independent of the product: it never imports the root
// or api modules, and it uses unstructured objects or client-go where
// product types would otherwise be needed. Its k8s.io and controller-runtime
// versions match the root module's, so go.work's minimal version selection
// never shifts the product's dependencies.
//
// Safety rules, which every package enforces where it applies:
//
//   - Cluster names start with "captf-test-"; any other name is refused,
//     and the operator's kube-php-client cluster explicitly.
//   - kind runs on the dedicated "captf-test" network, never "kind".
//   - Every child process gets an explicit KUBECONFIG and an isolated HOME
//     and XDG_CONFIG_HOME, so ~/.kube and ~/.cluster-api are never read or
//     written.
//   - Host sysctls and limits are never changed.
//   - Every downloaded artifact is checked against its sha256 pin, and
//     every image is pulled by digest.
//
// Unit tests in this module spawn nothing: they run engine commands
// through a fake Runner. Anything that starts a container lives behind the
// e2e build tag.
package framework
