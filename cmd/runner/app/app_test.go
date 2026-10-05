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

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	logsv1 "k8s.io/component-base/logs/api/v1"
	"k8s.io/component-base/version"

	"github.com/captf-io/cluster-api-provider-terraform/cmd/runner/app"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
)

// TestMain lets m's tests apply the logging configuration once per app.Run:
// a process normally applies it once, and logsv1 rejects a second apply
// unless told to accept an unchanged one. It reports m's exit code to the
// process.
func TestMain(m *testing.M) {
	logsv1.ReapplyHandling = logsv1.ReapplyHandlingIgnoreUnchanged
	os.Exit(m.Run())
}

// TestRunLoggingFlagsFailureWritesResult checks that an invalid logging
// flag on `runner run` exits 2 and still writes the flags-failure result,
// like any other flag error.
func TestRunLoggingFlagsFailureWritesResult(t *testing.T) {
	result := filepath.Join(t.TempDir(), "result.json")
	var stderr bytes.Buffer
	code := app.Run(context.Background(), []string{"run", "--op=apply", "--result=" + result, "--logging-format=bogus"}, &bytes.Buffer{}, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "Unsupported log format") {
		t.Fatalf("code = %d, stderr %q; want 2 and the logging error", code, stderr.String())
	}
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("no result written: %v", err)
	}
	var r runner.Result
	if err := json.Unmarshal(data, &r); err != nil || r.Error == nil || r.Error.Step == nil || *r.Error.Step != "flags" {
		t.Errorf("result = %s (%v), want a flags failure", data, err)
	}
}

// resetVersionFlag restores component-base/version/verflag's shared
// --version flag to its default "false" in t's cleanup. verflag.AddFlags
// adds the very same *pflag.Flag (from the global pflag.CommandLine) to
// every command tree app.NewRunnerCommand builds, so a test that sets it
// must put it back for every test that runs after it, in this or any other
// package.
func resetVersionFlag(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if err := pflag.CommandLine.Set("version", "false"); err != nil {
			t.Fatalf("reset --version: %v", err)
		}
	})
}

// TestRun checks Run's dispatch of help, version, copy and run command
// lines, including their exit codes and stdout/stderr output. Neither it
// nor its subtests run in parallel: every case builds a command tree that
// shares component-base/version/verflag's global flag (see
// resetVersionFlag), so concurrent execution would race on it.
func TestRun(t *testing.T) {
	tmp := t.TempDir()
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no args", args: nil, wantCode: 2, wantStderr: "Usage:"},
		{name: "help", args: []string{"--help"}, wantCode: 0, wantStdout: "Usage:"},
		{name: "short help", args: []string{"-h"}, wantCode: 0, wantStdout: "Usage:"},
		{name: "version flag", args: []string{"--version"}, wantCode: 0, wantStdout: version.Get().GitVersion},
		{name: "version raw", args: []string{"--version=raw"}, wantCode: 0, wantStdout: version.Get().GitVersion},
		{name: "version command", args: []string{"version"}, wantCode: 0, wantStdout: version.Get().GitVersion},
		{name: "copy without destination", args: []string{"copy"}, wantCode: 2, wantStderr: "want exactly one destination"},
		{name: "copy to a missing directory", args: []string{"copy", "/nonexistent/dir/runner"}, wantCode: 1, wantStderr: "runner copy:"},
		// --result comes before --bogus: pflag stops parsing at the first
		// bad flag, so --result must already be set or the flags-failure
		// result would be written to the real DefaultResultPath.
		{name: "run with an unknown flag", args: []string{"run", "--result=" + filepath.Join(tmp, "unknown-flag.json"), "--bogus"}, wantCode: 2, wantStderr: "unknown flag: --bogus"},
		{name: "run with an unknown op", args: []string{"run", "--op=bogus", "--result=" + filepath.Join(tmp, "unknown-op.json")}, wantCode: 2, wantStderr: `unknown op "bogus"`},
		{name: "run help", args: []string{"run", "-h"}, wantCode: 0},
		{name: "unknown", args: []string{"bogus"}, wantCode: 2, wantStderr: `unknown command "bogus"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if strings.HasPrefix(tt.name, "version") {
				resetVersionFlag(t)
			}
			var stdout, stderr bytes.Buffer
			if code := app.Run(context.Background(), tt.args, &stdout, &stderr); code != tt.wantCode {
				t.Errorf("Run(%q) = %d, want %d (stderr %q)", tt.args, code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

// TestCopy checks that `runner copy` reproduces the running binary,
// executable. It does not run in parallel (see TestRun).
func TestCopy(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "runner")
	var stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"copy", dst}, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("copy = %d: %s", code, stderr.String())
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	want, _ := os.ReadFile(self)
	got, _ := os.ReadFile(dst)
	info, err := os.Stat(dst)
	if err != nil || !bytes.Equal(got, want) || info.Mode().Perm() != 0o755 {
		t.Errorf("copy: %d/%d bytes, mode %v (err %v)", len(got), len(want), info.Mode(), err)
	}
}

// TestRunWritesResult checks that a run whose image has no module still
// writes a result, with error kind image-layout, and exits non-zero, and
// that an unwritable result path turns a would-be success into a failure.
// It does not run in parallel (see TestRun).
func TestRunWritesResult(t *testing.T) {
	dir := t.TempDir()
	result := filepath.Join(dir, "result.json")
	var stderr bytes.Buffer
	code := app.Run(context.Background(), []string{"run", "--op=apply", "--module=" + filepath.Join(dir, "missing"),
		"--workdir=" + dir, "--config=" + dir, "--result=" + result, "--image=r:v1"}, &bytes.Buffer{}, &stderr)
	if code == 0 {
		t.Fatalf("run succeeded without a module")
	}
	var r runner.Result
	raw, err := os.ReadFile(result)
	if err != nil || json.Unmarshal(raw, &r) != nil {
		t.Fatalf("result: %s (err %v)", raw, err)
	}
	if r.Error == nil || r.Error.Kind != runner.ErrorKindImageLayout || r.Image.Ref != "r:v1" || r.Op != "apply" {
		t.Errorf("result = %+v", r)
	}
	// An unwritable result path turns a would-be failure code into a failure
	// too, never success.
	code = app.Run(context.Background(), []string{"run", "--op=apply", "--module=" + dir, "--result=/nonexistent/x"}, &bytes.Buffer{}, &stderr)
	if code == 0 {
		t.Error("unwritable result path returned 0")
	}
}

// TestRunFlagsFailureWritesResult checks that a flag-validation failure (an
// old Job spec against a newer/older runner image) still writes a result,
// so the controller does not read "no result: not terminated or pod gone"
// for a pod that did terminate. It does not run in parallel (see TestRun).
func TestRunFlagsFailureWritesResult(t *testing.T) {
	dir := t.TempDir()
	result := filepath.Join(dir, "result.json")
	var stderr bytes.Buffer
	code := app.Run(context.Background(), []string{"run", "--op=bogus", "--result=" + result}, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Fatalf("code = %d, want 2 (stderr %q)", code, stderr.String())
	}
	var r runner.Result
	raw, err := os.ReadFile(result)
	if err != nil || json.Unmarshal(raw, &r) != nil {
		t.Fatalf("result: %s (err %v)", raw, err)
	}
	if r.Error == nil || r.Error.Kind != runner.ErrorKindStep || r.Error.Step == nil || *r.Error.Step != "flags" {
		t.Errorf("result = %+v", r)
	}
}
