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

// Package noop is the e2e noop data-flow suite: one ordered test,
// TestNoop, that drives the published noop modules
// (ghcr.io/captf-io/noop-{cluster,machine,machinepool}) through real
// Cluster API objects on the green-lit e2e cluster and proves, with no
// cloud, that data flows end to end: the CAPI spec into the module
// inputs, the module outputs into CAPTF status and on into CAPI, the
// cluster's exports into machine inputs, the pinned digests into later
// Jobs, and deletion into a full cleanup.
//
// The seven stages are setup, cluster, machines, pool (with a scale
// down), drift (with a failed apply and its recovery), teardown and
// health. A failed stage stops the rest, writes a diagnostics bundle and
// leaves the cluster as it is; t.Cleanup removes whatever the run left.
// Each run uses a fresh e2e-noop-<random> namespace and its own
// TerraformClusterIdentity.
//
// TestMain refuses to run unless -run selects TestNoop, and TestNoop
// fails at once unless the cluster is green-lit (greenlight.Require):
// run `make e2e-foundation` first, then `make e2e-noop`. The suite reads
// CAPTF_E2E_CLUSTER and, for the negative check only,
// CAPTF_E2E_NOOP_BAD_DIGEST. Every file carries the e2e build tag, so
// the default `make test` never compiles the package. test/README.md
// documents the stages and the values asserted.
package noop
