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

// Package engine detects the container engine (podman or docker) and runs
// every engine command through one exec wrapper, so commands, output and
// errors look the same whichever engine is in use.
//
// Detect picks the engine: an explicit override (the CAPTF_TESTENV_ENGINE
// value, framework.EngineEnv) wins; otherwise podman if `podman info`
// succeeds, else docker if `docker info` succeeds, else an error. Podman
// comes first because kind's Docker provider rejects the podman compat
// API, which is what a docker CLI talking to a podman socket reaches.
//
// An Engine wraps a Name and a Runner. Its methods (Pull, Tag, Exists,
// ImageID, Save, RunRemove and the raw Run) build each engine's argv and
// hide the differences between the two CLIs; KindProvider and
// KindNetworkEnv map the engine onto kind's library, which ignores
// KIND_EXPERIMENTAL_PROVIDER and must be told the provider in code.
//
// Runner is the only seam that spawns processes. ExecRunner (an OSRunner)
// is the os/exec implementation, and a failed command returns a *RunError
// carrying the exit code and stderr. Unit tests use enginetest.Runner,
// which records calls and spawns nothing. Every error this package returns
// is prefixed "engine: <op>:" and wraps its cause.
package engine
