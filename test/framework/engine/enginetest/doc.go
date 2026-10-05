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

// Package enginetest provides a fake engine.Runner for unit tests: it
// records every call, answers from scripted responses and spawns nothing.
//
// Script a response per command line with On (exact match on the joined
// argv, for example "podman pull --quiet ghcr.io/x@sha256:..."), or with
// Respond for anything computed. An unscripted command fails with an
// "unexpected command" error, so a test sees every command it did not
// plan for. Exit builds the *engine.RunError a failed command returns.
//
//	r := enginetest.New().
//		On("podman info", "", nil).
//		On("podman image exists foo", "", enginetest.Exit(1, ""))
//	e, err := engine.Detect(ctx, "", r)
//	...
//	if got := r.Lines(); !slices.Equal(got, want) { ... }
package enginetest
