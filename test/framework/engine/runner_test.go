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

package engine_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
)

// missingProgram is a program name no PATH holds, so exec fails at lookup
// and nothing is spawned.
const missingProgram = "captf-enginetest-no-such-program"

// TestExecRunnerNotFound checks a program that cannot be found returns a
// *RunError with exit code -1 that wraps exec.ErrNotFound.
func TestExecRunnerNotFound(t *testing.T) {
	t.Parallel()
	_, err := engine.ExecRunner().Run(t.Context(), missingProgram, "info")
	var re *engine.RunError
	if !errors.As(err, &re) {
		t.Fatalf("error = %v, want a *RunError", err)
	}
	if re.ExitCode != -1 || re.Name != missingProgram || len(re.Args) != 1 || re.Args[0] != "info" {
		t.Errorf("RunError = %+v", re)
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("error %q does not wrap exec.ErrNotFound", err)
	}
	if !strings.HasPrefix(err.Error(), missingProgram+" info: ") {
		t.Errorf("error = %q, want it to start with the command line", err)
	}
}

// TestOSRunnerCancelled checks a done context fails the command before it
// starts and surfaces the context error. The program path is absolute, so
// no PATH lookup happens either; Start checks the context first.
func TestOSRunnerCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := engine.OSRunner{Dir: t.TempDir(), Env: []string{"HOME=/nonexistent"}}
	_, err := r.Run(ctx, "/"+missingProgram)
	if err == nil {
		t.Fatal("Run succeeded with a cancelled context")
	}
	var re *engine.RunError
	if !errors.As(err, &re) || re.ExitCode != -1 {
		t.Errorf("error = %v, want a *RunError with exit code -1", err)
	}
}

// TestRunErrorMessage checks the message carries the command line, the
// cause and the trimmed stderr tail, and that Unwrap exposes the cause.
func TestRunErrorMessage(t *testing.T) {
	t.Parallel()
	cause := errors.New("exit status 125")
	e := &engine.RunError{Name: "podman", Args: []string{"pull", "x"}, ExitCode: 125, Stderr: "  Error: denied\n", Err: cause}
	if got, want := e.Error(), "podman pull x: exit status 125: Error: denied"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(e, cause) {
		t.Error("RunError does not unwrap to its cause")
	}

	quiet := &engine.RunError{Name: "docker", Args: []string{"info"}, Err: cause}
	if got, want := quiet.Error(), "docker info: exit status 125"; got != want {
		t.Errorf("Error() without stderr = %q, want %q", got, want)
	}

	long := &engine.RunError{Name: "podman", Err: cause, Stderr: strings.Repeat("a", 10000) + "TAIL"}
	msg := long.Error()
	if !strings.HasSuffix(msg, "TAIL") || !strings.Contains(msg, ": ...") || len(msg) > 4200 {
		t.Errorf("long stderr not truncated to its tail: len %d, suffix %q", len(msg), msg[len(msg)-10:])
	}
}
