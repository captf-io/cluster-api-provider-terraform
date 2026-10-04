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

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// execShell runs script under /bin/sh with Exec, canceling ctx once the
// script creates its ready file, and returns the result; t is used for
// temporary files and failure reporting.
func execShell(t *testing.T, script string) StepResult {
	t.Helper()
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(ready); err == nil {
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	done := make(chan StepResult, 1)
	go func() {
		done <- Exec(ctx, []string{"/bin/sh", "-c", script, "sh", ready}, Invocation{Name: "t"}, os.Environ(), dir, io.Discard, io.Discard, 5*time.Second)
	}()
	select {
	case res := <-done:
		return res
	case <-time.After(30 * time.Second):
		t.Fatal("Exec did not return")
		return StepResult{}
	}
}

// TestExecCanceledButSucceeded: a process that handles the cancellation
// SIGTERM and exits 0 is a success, not an interruption.
func TestExecCanceledButSucceeded(t *testing.T) {
	t.Parallel()
	res := execShell(t, `trap 'exit 0' TERM; : > "$1"; while :; do sleep 0.02; done`)
	if res.Exit != 0 || res.Err != nil {
		t.Errorf("result = exit %d, err %v; want exit 0, nil", res.Exit, res.Err)
	}
}

// TestExecCanceledNonZero: a non-zero exit after cancellation is still
// reported as that exit code.
func TestExecCanceledNonZero(t *testing.T) {
	t.Parallel()
	res := execShell(t, `trap 'exit 3' TERM; : > "$1"; while :; do sleep 0.02; done`)
	if res.Exit != 3 {
		t.Errorf("exit = %d, want 3 (err %v)", res.Exit, res.Err)
	}
}

// TestExecSignalKilled: a child killed by the cancellation signal keeps
// Err and a failing exit.
func TestExecSignalKilled(t *testing.T) {
	t.Parallel()
	res := execShell(t, `: > "$1"; while :; do sleep 0.02; done`)
	if res.Exit == 0 || res.Err == nil {
		t.Errorf("result = exit %d, err %v; want failure with Err", res.Exit, res.Err)
	}
}

// TestExecStartFailure: a missing binary records Err.
func TestExecStartFailure(t *testing.T) {
	t.Parallel()
	res := Exec(context.Background(), []string{"/nonexistent/bin"}, Invocation{Name: "t"}, nil, t.TempDir(), io.Discard, io.Discard, time.Second)
	if res.Exit == 0 || res.Err == nil {
		t.Errorf("result = exit %d, err %v; want failure with Err", res.Exit, res.Err)
	}
}
