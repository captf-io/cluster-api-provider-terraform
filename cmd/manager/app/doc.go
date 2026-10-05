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

// Package app builds and runs the CAPTF manager's cobra command, laid out
// the way kube-controller-manager's cmd/kube-controller-manager/app is:
// NewManagerCommand assembles the *cobra.Command from
// cmd/manager/app/options's named flag sets, plus the --version flag from
// k8s.io/component-base/version/verflag and the standard --help flag on a
// "global" section, sets the manager's usage and help output to
// cliflag's sectioned form, and registers the `manager version`
// subcommand. Its RunE handles --version/--version=raw itself, rather than
// through verflag.PrintAndExitIfRequested (which hardcodes the program name
// "Kubernetes" and calls os.Exit, neither of which this binary wants),
// then validates and applies the logging configuration, completes and
// validates the parsed options, and hands off to Run.
//
// Run builds the typed Kubernetes scheme and controller-runtime manager,
// registers Prometheus metrics, installs the admission webhooks, sets up
// the shared indexes and reconciler dependencies, adds the label-scoped
// variables cache and every reconciler and the orphan RBAC sweep as
// manager runnables, registers health and readiness checks, and finally
// starts the manager; it blocks until its context is canceled. main.go
// only calls NewManagerCommand and hands the result to
// k8s.io/component-base/cli.Run, which parses flags, initializes logging
// and prints or logs any error Run or RunE returns exactly once.
package app
