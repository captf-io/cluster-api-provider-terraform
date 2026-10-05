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

// Command runner is the CAPTF runner: a static binary injected into every
// Terraform/OpenTofu Job pod by an init container (`runner copy`) and then
// run as the main container's entrypoint (`runner run --op …`), so source
// images need no shell.
//
// main itself is a thin wrapper: it builds a context canceled on
// SIGTERM/SIGINT and hands it, os.Args and os.Stdout/os.Stderr to
// cmd/runner/app.Run, then calls os.Exit with the code Run returns. Every
// command is fully implemented in cmd/runner/app, a cobra command tree
// (`copy <dest>`, `run` and `version`) laid out kube-style: main.go stays
// free of flag parsing and command logic, both testable only through app.
//
// Like every Kubernetes component, it is configured through
// k8s.io/component-base: version flags from component-base/version, and
// logging through component-base/logs (--logging-format text or json, -v,
// --vmodule, --feature-gates for the logging gates) onto klog. main
// registers the json log format (component-base/logs/json/register).
//
// `runner copy <dest>` copies the running binary to dest so the init
// container can hand it to the main container's read-only filesystem.
// `runner run --op <op>` parses the flags cmd/runner/app.RunOptions.AddFlags
// defines, runs the operation through internal/runner.Run under a context
// whose diagnostics log through klog as the logging flags configured it,
// and always writes a result document
// (internal/runner.Write) even when flag parsing itself fails, so the
// controller never sees a pod that terminated without one. `runner version`
// (or --version, or --version=raw) prints the build version, stamped into
// k8s.io/component-base/version at link time (Makefile), and exits.
package main
