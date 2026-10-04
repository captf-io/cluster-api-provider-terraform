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

package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// maxStderrInError caps how much of a command's stderr RunError.Error
// includes: the tail, where the actual failure usually is.
const maxStderrInError = 4096

// Runner runs one command to completion and returns its stdout. A failed
// command returns a *RunError, whose message includes the command line,
// the exit status and stderr. Implementations are safe for concurrent use.
// It is the only seam that spawns processes, so tests replace it with a
// fake (enginetest.Runner).
type Runner interface {
	// Run runs the program name with args under ctx and returns its
	// stdout, or a *RunError when it cannot start or exits non-zero.
	// Cancelling ctx kills the process.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// OSRunner is the os/exec Runner. The zero value runs in the current
// directory with the current process's environment.
type OSRunner struct {
	// Dir is the working directory; empty means the current one.
	Dir string
	// Env, when non-nil, is the child's complete environment ("KEY=value"
	// entries); nothing is inherited. Use it to give a child an explicit
	// KUBECONFIG and an isolated HOME. Nil inherits the current
	// environment.
	Env []string
}

// ExecRunner returns the os/exec Runner with the current directory and
// environment (a zero OSRunner).
func ExecRunner() Runner {
	return OSRunner{}
}

// Run runs name with args under ctx, in r.Dir with r.Env, and returns its
// stdout. It returns a *RunError when the command cannot start or exits
// non-zero.
func (r OSRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.Dir
	cmd.Env = r.Env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		code := -1
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			code = exitErr.ExitCode()
		}
		return stdout.Bytes(), &RunError{
			Name:     name,
			Args:     args,
			ExitCode: code,
			Stderr:   stderr.String(),
			Err:      err,
		}
	}
	return stdout.Bytes(), nil
}

// RunError is a command that could not start or exited non-zero.
type RunError struct {
	// Name is the program that was run.
	Name string
	// Args are its arguments.
	Args []string
	// ExitCode is the exit status, or -1 when the process never started
	// or was killed by a signal.
	ExitCode int
	// Stderr is everything the command wrote to stderr.
	Stderr string
	// Err is the underlying error (an *exec.ExitError, exec.ErrNotFound,
	// a context error, ...).
	Err error
}

// Error returns "<command line>: <cause>", followed by the tail of stderr
// when there is any.
func (e *RunError) Error() string {
	var b strings.Builder
	b.WriteString(strings.Join(append([]string{e.Name}, e.Args...), " "))
	fmt.Fprintf(&b, ": %v", e.Err)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		if len(s) > maxStderrInError {
			s = "..." + s[len(s)-maxStderrInError:]
		}
		b.WriteString(": ")
		b.WriteString(s)
	}
	return b.String()
}

// Unwrap returns the underlying error, so errors.Is sees context and exec
// errors through a RunError.
func (e *RunError) Unwrap() error {
	return e.Err
}
