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

// Command covercheck gates per-package statement coverage. It reads one or
// more Go cover profiles (as written by go test -coverprofile, mode set,
// count or atomic), merges them, computes statement coverage for every
// package and for the whole tree, and exits non-zero when a package is
// below its floor. `make cover-check` runs it after `make test-cover`.
//
// Profiles merge block by block: a block that appears in several profiles,
// or several times in one, counts as covered when any occurrence has a
// count above zero. A package is the directory of a profile file path,
// which is its import path. Packages with no statements are skipped.
//
// The floors file holds comments (#), blank lines, a "default <pct>" line,
// "<import-path> <pct>" lines and "<import-path> exempt" lines, each with
// an optional trailing "# reason". Import paths are relative to the module
// path (cmd/manager, not github.com/.../cmd/manager). A package without a
// line is held to the default; an exempt package is reported but never
// fails the gate.
//
// Usage:
//
//	covercheck -floors file [-module path] [-summary file] profile...
//
// The table goes to stdout. With -summary, a GitHub-flavored Markdown
// section is appended to the named file, which is how CI fills
// GITHUB_STEP_SUMMARY.
package main
