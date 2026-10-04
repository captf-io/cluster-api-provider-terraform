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
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// MaxTail is the size of the stderr tail kept in memory: large enough that
// a failing step's "Error: " diagnostic block survives behind whatever
// provider debug noise precedes it; the result's error summary is capped
// separately, at MaxSummary bytes.
const MaxTail = 64 << 10

// DefaultStopTimeout is how long an interrupted step may take to stop when
// --stop-timeout is not given. The Job passes its pod's
// terminationGracePeriodSeconds less a margin (jobs.StopTimeout).
const DefaultStopTimeout = 60 * time.Second

// StepResult is the outcome of one Invocation.
type StepResult struct {
	Exit    int
	Seconds float64
	// Stdout is set only for Capture steps.
	Stdout []byte
	// Tail is the last MaxTail bytes of stderr.
	Tail string
	// Err is set when the command could not be started or was killed.
	Err error
}

// Failed reports whether the step's exit code is not acceptable.
func (r StepResult) Failed(s Invocation) bool {
	ok := s.OK
	if ok == nil {
		ok = []int{0}
	}
	if r.Err != nil {
		return true
	}
	if slices.Contains(ok, r.Exit) {
		return false
	}
	return !slices.ContainsFunc(s.OKIf, func(msg string) bool { return strings.Contains(r.Tail, msg) })
}

// Exec runs bin with the step's arguments in dir with env, under ctx, and
// returns the resulting StepResult. stdout goes to the log (or memory for
// Capture steps); stderr goes to the log and its tail is kept. On ctx
// cancellation (the pod's SIGTERM) the child gets SIGTERM, and SIGKILL only
// after stop (DefaultStopTimeout when 0): an interrupted apply waits for
// in-flight provider calls, then writes the state and releases the lock,
// which a kill at 10 s would lose. Only the runtime is signaled: it stops
// its provider plugins itself, and a SIGTERM to a plugin would abort the
// very calls being waited for.
func Exec(ctx context.Context, bin []string, s Invocation, env []string, dir string, stdout, stderr io.Writer, stop time.Duration) StepResult {
	if stop <= 0 {
		stop = DefaultStopTimeout
	}
	start := time.Now()
	cmd := exec.CommandContext(ctx, bin[0], append(slices.Clone(bin[1:]), s.Args...)...) // #nosec G204 -- runs the module's terraform or tofu runtime by design
	cmd.Dir = dir
	cmd.Env = env
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = stop
	// Its own process group keeps a terminal Ctrl-C (SIGINT to the
	// foreground group) from reaching the child as well: the runner
	// forwards exactly one SIGTERM, to the child's pid, itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var captured bytes.Buffer
	switch {
	case s.Capture && s.LogCapture:
		cmd.Stdout = io.MultiWriter(&captured, stdout)
	case s.Capture:
		cmd.Stdout = &captured
	default:
		cmd.Stdout = stdout
	}
	tail := &tailWriter{max: MaxTail}
	cmd.Stderr = io.MultiWriter(stderr, tail)

	err := cmd.Run()
	res := StepResult{Seconds: time.Since(start).Seconds(), Tail: tail.String()}
	if s.Capture {
		res.Stdout = captured.Bytes()
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		res.Exit = exitErr.ExitCode()
	case cmd.ProcessState != nil && cmd.ProcessState.Success():
		// The process exited 0 but Wait reported the context's error (a
		// cancellation that raced the exit) or ErrWaitDelay (stdio held
		// open past WaitDelay). The work finished; it was not interrupted.
	default:
		// Not started, or killed by a signal (ExitCode -1).
		res.Exit = 1
		res.Err = err
	}
	return res
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	mu  sync.Mutex
	max int
	buf []byte
}

// Write appends p to w's buffer, trimming it to the last w.max bytes, and
// always reports len(p) written with a nil error.
func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = append([]byte(nil), w.buf[len(w.buf)-w.max:]...)
	}
	return len(p), nil
}

// String returns the bytes currently buffered in w.
func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}
