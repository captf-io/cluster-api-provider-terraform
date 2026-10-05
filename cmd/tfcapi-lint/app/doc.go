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

// Package app is tfcapi-lint's command tree: the module, image and version
// commands, text and JSON output, and the exit codes CI scripts and the
// operator depend on. It is a
// package, not main, so cmd/tfcapi-lint's tests can drive the whole command
// line in process without forking a binary; cmd/tfcapi-lint/main.go itself
// is a thin wrapper that calls Run.
//
// NewLintCommand builds the root cobra.Command ("tfcapi-lint") with the
// module, image and version subcommands attached. Run parses the command
// line, executes the matched command, and returns the process exit code;
// run is its unexported form, taking the go-containerregistry remote
// options tests use to reach an in-memory registry instead of a real one.
// module lints a Terraform module directory against the contract for a
// role (cluster, machine or machinepool); image pulls a built OCI image
// and checks it against the image contract, optionally across every
// platform of a multi-platform manifest; version reports the build stamp (from
// k8s.io/component-base/version) and the contract versions this binary
// understands. A bare --version on the root command prints the same line
// and exits, following k8s.io/component-base/version/verflag.
//
// Every finding carries a severity of error, warning or info. --strict
// makes warnings fail the run; --allow-warning downgrades specific check
// IDs to info so a module or image can carry a reviewed, explained
// exception without silencing every other warning. The package defines the
// exit codes (ExitOK, ExitFindings, ExitUnparsable, ExitUsage) that CI
// scripts branch on, so a caller never needs to parse output to know
// whether the run passed. A command that has already printed its error to
// stderr returns an *exitCodeErr, so the top-level dispatcher in Run and
// run never prints the same failure twice.
package app
