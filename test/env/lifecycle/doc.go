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

// Package lifecycle is the only entry point of the CAPTF test
// environment. Each of its tests runs one operation of
// test/framework/env, streaming progress to stderr: TestUp, TestDown,
// TestStatus, TestCollect and TestReload. They are configured entirely
// through the environment (TESTENV_NAME, CAPTF_TESTENV_ENGINE,
// TESTENV_WORKERS, CAPTF_TESTENV_REUSE, TESTENV_ALL, CLUSTERCTL,
// KUSTOMIZE); see test/README.md.
//
// The operations are driven through `go test -tags=e2e` rather than a
// command because that process is the one allowed to drive podman and
// kind on the development host; the make targets testenv-up,
// testenv-down, testenv-status, testenv-logs and testenv-reload wrap it.
// Every file carries the e2e build tag, so `make test` and a plain
// `go vet ./...` never compile the package (`make vet` and `make lint`
// check it in a separate e2e-tagged pass), and TestMain refuses to run
// unless -run selects the operation.
package lifecycle
