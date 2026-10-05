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

// Package options turns the manager's command-line flags into typed
// options and into controller-runtime manager options. The flag set
// follows CAPD, not the
// kubebuilder scaffold. Options is the parsed, typed form of every flag;
// NewOptions allocates one with its logging configuration (default
// verbosity 2) and feature gates ready, and Flags binds it to a
// cliflag.NamedFlagSets with defaults, grouped into the generic, leader
// election, webhook, runner, diagnostics, logs and feature gates sections
// that cmd/manager/app adds to the manager command and to --version and
// --help. Complete fills in the values that come from the environment
// rather than a flag (--runner-image from $CAPTF_MANAGER_IMAGE), Validate
// rejects option combinations the manager cannot run with, and
// ManagerOptions builds the ctrl.Options cmd/manager/app passes to
// controller-runtime: diagnostics with authn/authz, the webhook server,
// leader election, and the cache scoping from internal/manager.
//
// This package holds only the manager's own flags: the --version flag is
// k8s.io/component-base/version/verflag's, added directly to the command by
// cmd/manager/app, and the cache and scheme helpers Options.ManagerOptions
// calls stay in internal/manager because they have no dependency on the
// command line.
package options
