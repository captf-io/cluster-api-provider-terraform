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

// Package main is tfcapi-lint, the CAPTF module and image contract checker:
// `tfcapi-lint module --role <role> <dir>` lints a
// Terraform module directory against the contract for a role (cluster or
// machine), and `tfcapi-lint image --role <role> <ref>` pulls a built OCI
// image and checks it against the corresponding image contract. Both
// commands accept --contract to pick a contract version, --json for a
// versioned JSON report instead of text, --strict to fail on warnings too,
// and --allow-warning to downgrade a named check's warnings to info. A
// third command, `tfcapi-lint version`, prints the build stamp and the
// contract versions this binary understands.
//
// main itself is a thin entry point: it delegates everything, including
// flag parsing and exit-code selection, to
// cmd/tfcapi-lint/app.Run, so that package's tests can drive the whole
// command line in process without forking this binary. The command tree
// (module, image and version) is a cobra.Command built by app.NewLintCommand,
// laid out like a Kubernetes command: this package is a thin main, and
// app holds the full implementation. A module repository can run the
// built binary with --strict over its modules and images; its exit code
// (0 clean, 1 findings, 2 unparsable, 3 usage) is what a build pipeline
// branches on to fail a change that breaks the contract.
package main
