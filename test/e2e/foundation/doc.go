//go:build e2e

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

// Package foundation is the e2e foundation suite: one ordered test,
// TestFoundation, that builds the e2e kind cluster through
// test/framework/env and proves in six stages that it is sound enough for
// harder tests. The stages are cluster-build, base-components,
// captf-install, captf-components, together (a real reconcile, a
// stability window and a log scan) and green-light, which writes
// bin/testenv/<name>/greenlight.json. A failed stage stops the suite,
// collects a diagnostics bundle and keeps the cluster for inspection; a
// passing suite keeps the cluster too, green-lit, unless
// CAPTF_E2E_TEARDOWN=1.
//
// Later e2e tests call greenlight.Require against that record before they
// rely on the cluster. The suite is configured through CAPTF_E2E_CLUSTER,
// CAPTF_E2E_REUSE, CAPTF_E2E_TEARDOWN, CAPTF_E2E_STABILITY and
// CAPTF_E2E_WORKERS; `make e2e-foundation` passes them along with the
// pinned clusterctl and kustomize. Every file carries the e2e build tag,
// so the default `make test` never compiles the package, and TestMain
// refuses to run unless -run selects TestFoundation. test/README.md
// documents the stages, the variables and the allowlists.
package foundation
