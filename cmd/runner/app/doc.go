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

// Package app implements the runner command line: the cobra command tree
// NewRunnerCommand builds and Run executes. It holds `runner copy <dest>`
// (copy.go), `runner run --op <op>` (run.go) and `runner version`
// (version.go), plus the component-base version flags (--version,
// --version=raw and --version=vX.Y.Z, k8s.io/component-base/version/verflag)
// and the component-base logging flags (--logging-format, -v, --vmodule,
// --log-flush-frequency and the rest, k8s.io/component-base/logs/api/v1)
// with --feature-gates for the logging gates, all registered on the root
// command's persistent flags as Kubernetes components register them.
// cmd/runner/main.go is a thin wrapper that calls Run under a context
// canceled on SIGTERM/SIGINT and calls os.Exit with the code it returns;
// this package never calls os.Exit itself, so it stays testable.
//
// The root command's PersistentPreRunE validates and applies the logging
// configuration (logsv1.ValidateAndApply) before any subcommand runs, and
// Run brackets execution with logs.InitLogs and logs.FlushLogs, as
// component-base/cli.Run does. runner.Run and this package's own
// diagnostics (writeFlagsFailure, runOperation) then log through
// klog.FromContext, which falls back to that configured klog logger.
//
// RunOptions (in run.go) binds `runner run`'s flags with AddFlags, using
// the same flag names, defaults and help text the runner has always used,
// so internal/jobs.Build's arguments and this package's ParseRunFlags stay
// in step (internal/jobs/jobs_test.go parses Build's output with it
// directly). A flag-parse or validation failure still writes a result
// document to --result (or DefaultResultPath), so the controller never
// reads "no result: not terminated or pod gone" for a Job pod that did
// terminate.
package app
