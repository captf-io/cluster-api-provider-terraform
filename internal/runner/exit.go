/*
Copyright 2026 The cluster-api-provider-terraform Authors.

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

package runner

// Exit codes for the runner binary: Run returns one of these, and
// cmd/runner/app reuses them for its own copy and version subcommands, so
// the whole binary reports exit status consistently.
const (
	// ExitOK is a successful run: every step succeeded, or an apply's plan
	// had no changes to apply.
	ExitOK = 0
	// ExitFailure is a run that did not succeed: a step failed, was
	// interrupted, stopped before a destructive plan (blocked) or found its
	// approved plan had changed, or setup (preflight, prepare) itself
	// failed. cmd/runner/app also returns it for a `copy` I/O error.
	ExitFailure = 1
	// ExitUsage is bad input: an --op Steps does not recognize, a bad flag,
	// unexpected arguments, or invalid logging flags.
	ExitUsage = 2
)
