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

package enginetest_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine/enginetest"
)

// TestOn checks scripted responses, replacement and the unexpected-command
// error, and that every call is recorded in order.
func TestOn(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	r := enginetest.New().
		On("podman info", "old", nil).
		On("podman info", "v5", nil).
		On("podman pull x", "", boom)
	ctx := t.Context()

	out, err := r.Run(ctx, "podman", "info")
	if err != nil || string(out) != "v5" {
		t.Errorf("podman info = %q, %v; want the replacing response", out, err)
	}
	if _, err := r.Run(ctx, "podman", "pull", "x"); !errors.Is(err, boom) {
		t.Errorf("podman pull x error = %v, want boom", err)
	}
	_, err = r.Run(ctx, "docker", "info")
	if err == nil || !strings.Contains(err.Error(), `unexpected command "docker info"`) {
		t.Errorf("unscripted error = %v", err)
	}
	if got, want := r.Lines(), []string{"podman info", "podman pull x", "docker info"}; !slices.Equal(got, want) {
		t.Errorf("Lines() = %q, want %q", got, want)
	}
}

// TestRespond checks Respond answers unscripted calls and that On wins.
func TestRespond(t *testing.T) {
	t.Parallel()
	r := enginetest.New().
		On("a", "scripted", nil).
		Respond(func(c enginetest.Call) enginetest.Response {
			return enginetest.Response{Stdout: []byte(c.Line() + "!")}
		})
	ctx := t.Context()
	if out, _ := r.Run(ctx, "a"); string(out) != "scripted" {
		t.Errorf("a = %q, want the On response", out)
	}
	if out, _ := r.Run(ctx, "b", "c"); string(out) != "b c!" {
		t.Errorf("b c = %q, want the Respond answer", out)
	}
}

// TestCancelled checks a done context returns its error, and the call is
// still recorded.
func TestCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := enginetest.New().On("podman info", "", nil)
	if _, err := r.Run(ctx, "podman", "info"); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if got := r.Lines(); len(got) != 1 {
		t.Errorf("Lines() = %q, want the cancelled call recorded", got)
	}
}

// TestCallsAreCopies checks neither the caller's args nor Calls' result
// alias the recorded state.
func TestCallsAreCopies(t *testing.T) {
	t.Parallel()
	r := enginetest.New().On("p x", "", nil)
	args := []string{"x"}
	if _, err := r.Run(t.Context(), "p", args...); err != nil {
		t.Fatal(err)
	}
	args[0] = "mutated"
	calls := r.Calls()
	calls[0].Args[0] = "mutated"
	if got := r.Calls()[0].Line(); got != "p x" {
		t.Errorf("recorded call = %q, want %q", got, "p x")
	}
}

// TestConcurrent checks concurrent calls are all recorded (run with -race).
func TestConcurrent(t *testing.T) {
	t.Parallel()
	r := enginetest.New().Respond(func(enginetest.Call) enginetest.Response { return enginetest.Response{} })
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if _, err := r.Run(t.Context(), "podman", "ps"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := len(r.Calls()); got != 16 {
		t.Errorf("recorded %d calls, want 16", got)
	}
}

// TestExit checks Exit builds a RunError with the code and stderr.
func TestExit(t *testing.T) {
	t.Parallel()
	err := enginetest.Exit(125, "Error: no")
	var re *engine.RunError
	if !errors.As(err, &re) || re.ExitCode != 125 || re.Stderr != "Error: no" || re.Err == nil {
		t.Errorf("Exit(125) = %#v", err)
	}
}
