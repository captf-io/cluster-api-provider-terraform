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

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/spf13/pflag"
	"k8s.io/component-base/version"

	"github.com/captf-io/cluster-api-provider-terraform/internal/lint"
)

// fixtures are tfcapi-lint's checked-in modules; cmd/tfcapi-lint's own
// test compares their JSON output with the same golden files.
var fixtures = filepath.Join("..", "testdata")

// TestUsage: usage errors exit 3 with the reason on stderr; version and
// help exit 0. It is not t.Parallel: the "--version" case sets the
// verflag package's global pflag.CommandLine flag, which every command
// instance shares, and resets it in t.Cleanup before any other test can
// observe it.
func TestUsage(t *testing.T) {
	t.Cleanup(func() {
		if err := pflag.CommandLine.Set("version", "false"); err != nil {
			t.Fatal(err)
		}
	})
	good := filepath.Join(fixtures, "good", "machine")
	for _, tt := range []struct {
		name string
		args []string
		exit int
		out  string // expected in stdout or stderr
	}{
		{"no role", []string{"module", good}, ExitUsage, "--role is required"},
		{"no directory", []string{"module", "--role", "machine"}, ExitUsage, "exactly one module directory"},
		{"unknown role", []string{"module", "--role", "worker", good}, ExitUsage, "unknown role"},
		{"machinepool wrong module", []string{"module", "--role", "machinepool", good}, ExitFindings, "input/required"},
		{"machinepool good module", []string{"module", "--role", "machinepool", filepath.Join(fixtures, "good", "machinepool")}, ExitOK, "0 error(s), 0 warning(s), 0 info"},
		{"unknown contract", []string{"module", "--role", "machine", "--contract", "v9", good}, ExitUsage, "unknown contract"},
		{"bad flag", []string{"module", "--frobnicate"}, ExitUsage, "unknown flag"},
		{"no command", nil, ExitUsage, "Usage:"},
		{"unknown command", []string{"lint"}, ExitUsage, "unknown command"},
		{"image without role", []string{"image", "r:v1"}, ExitUsage, "--role is required"},
		{"image bad reference", []string{"image", "--role", "cluster", "Not A Ref"}, ExitUsage, "invalid reference"},
		{"image unreachable", []string{"image", "--role", "cluster", "--insecure", "localhost:59999/noop:v1"}, ExitUnparsable, "cannot read the image"},
		{"missing directory", []string{"module", "--role", "machine", filepath.Join(fixtures, "nope")}, ExitUnparsable, "read module"},
		{"version", []string{"version"}, ExitOK, ""},
		{"--version", []string{"--version"}, ExitOK, ""},
		{"version bad flag", []string{"version", "--yaml"}, ExitUsage, "unknown flag"},
		{"version argument", []string{"version", "extra"}, ExitUsage, "unexpected argument"},
		{"version help", []string{"version", "--help"}, ExitOK, "Usage:"},
		{"help", []string{"--help"}, ExitOK, "Usage:"},
		{"module help", []string{"module", "--help"}, ExitOK, "Usage:"},
		{"text output", []string{"module", "--role", "machine", good}, ExitOK, "0 error(s), 0 warning(s), 0 info (read terraform files)"},
	} {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), tt.args, &stdout, &stderr)
		if code != tt.exit || !strings.Contains(stdout.String()+stderr.String(), tt.out) {
			t.Errorf("%s: exit %d (want %d)\nstdout: %s\nstderr: %s", tt.name, code, tt.exit, stdout.String(), stderr.String())
		}
	}
}

// TestVersionText: the version subcommand's text form prints the same
// line as a bare --version on the root command, built from
// k8s.io/component-base/version.Get() rather than a literal.
func TestVersionText(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	if code := Run(context.Background(), []string{"version"}, &stdout, io.Discard); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	want := "tfcapi-lint " + version.Get().String() + "\n"
	if stdout.String() != want {
		t.Errorf("text = %q, want %q", stdout.String(), want)
	}
}

// TestVersionJSON: version --json reports the build stamp and the contract
// versions as a list.
func TestVersionJSON(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	if code := Run(context.Background(), []string{"version", "--json"}, &stdout, io.Discard); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", stdout.String(), err)
	}
	for _, k := range []string{"version", "commit", "date"} {
		if s, ok := got[k].(string); !ok || s == "" {
			t.Errorf("%s = %v, want a non-empty string", k, got[k])
		}
	}
	if c, ok := got["contract"].([]any); !ok || len(c) != 1 || c[0] != "v1alpha1" {
		t.Errorf("contract = %v, want [v1alpha1]", got["contract"])
	}
	if len(got) != 4 {
		t.Errorf("keys = %v, want version, commit, date, contract", got)
	}
}

// TestTextOutput: <severity> <id> <file>:<line> <message>, "-" for a
// finding about the module as a whole; warnings fail only under --strict.
func TestTextOutput(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	dir := filepath.Join(fixtures, "bad", "missing-output")
	if code := Run(context.Background(), []string{"module", "--role", "machine", dir}, &stdout, &bytes.Buffer{}); code != ExitFindings {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(stdout.String(), "error output/required -:0 contract output interruptible is not declared\n") {
		t.Errorf("text = %q", stdout.String())
	}
	dir = filepath.Join(fixtures, "bad", "tofu-shadow")
	for _, tt := range []struct {
		strict bool
		exit   int
	}{{false, ExitOK}, {true, ExitFindings}} {
		args := []string{"module", "--role", "machine", dir}
		if tt.strict {
			args = append(args, "--strict")
		}
		var out bytes.Buffer
		if code := Run(context.Background(), args, &out, &bytes.Buffer{}); code != tt.exit {
			t.Errorf("strict=%v: exit %d, want %d: %s", tt.strict, code, tt.exit, out.String())
		}
	}
}

// TestApplyAllow: --allow-warning downgrades only that check's warnings to
// info (visible, annotated, not counted under --strict); errors and other
// warnings are untouched.
func TestApplyAllow(t *testing.T) {
	t.Parallel()
	report := lint.Report{Findings: []lint.Finding{
		{ID: lint.IDInputTagsUnused, Severity: lint.SeverityWarning, Message: "unused"},
		{ID: lint.IDInputTagsUnused, Severity: lint.SeverityError, Message: "not downgradable"},
		{ID: lint.IDOutputReserved, Severity: lint.SeverityWarning, Message: "other"},
	}}
	got := applyAllow(report, []string{lint.IDInputTagsUnused})
	if got.Findings[0].Severity != lint.SeverityInfo || !strings.Contains(got.Findings[0].Message, "--allow-warning") {
		t.Errorf("allowed warning = %+v, want info and annotated", got.Findings[0])
	}
	if got.Findings[1].Severity != lint.SeverityError || got.Findings[2].Severity != lint.SeverityWarning {
		t.Errorf("errors and other warnings changed: %+v", got.Findings)
	}
	if got.Summary != (lint.Summary{Errors: 1, Warnings: 1, Infos: 1}) {
		t.Errorf("summary = %+v", got.Summary)
	}
}

// TestImageCommand: tfcapi-lint image against an in-memory registry. A
// random image breaks the contract (exit 1); the JSON document carries
// "image" instead of "module"; the text output names the digest.
func TestImageCommand(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	ref := strings.TrimPrefix(srv.URL, "http://") + "/random:v1"
	r, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(r, img); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"image", "--role", "cluster", "--insecure", "--json", ref}, &stdout, &stderr); code != ExitFindings {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	info, ok := doc["image"].(map[string]any)
	if _, hasModule := doc["module"]; hasModule || !ok || info["ref"] != ref || !strings.HasPrefix(info["digest"].(string), "sha256:") {
		t.Errorf("document = %s", stdout.String())
	}
	stdout.Reset()
	if code := Run(context.Background(), []string{"image", "--role", "cluster", "--insecure", "--platform", "linux/arm64", ref}, &stdout, &stderr); code != ExitFindings ||
		!strings.Contains(stdout.String(), "image/platform") || !strings.HasPrefix(stdout.String(), "image "+ref) {
		t.Errorf("text = %s", stdout.String())
	}
}

// failWriter is an io.Writer that always fails, for exercising a write
// failure without a real closed connection.
type failWriter struct{}

// Write always fails and returns 0 bytes written and a "closed" error.
func (failWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

// TestJSONWriteFailure: a JSON output that cannot be written is not a
// clean run.
func TestJSONWriteFailure(t *testing.T) {
	t.Parallel()
	args := []string{"module", "--role", "machine", "--json", filepath.Join(fixtures, "good", "machine")}
	if code := Run(context.Background(), args, failWriter{}, &bytes.Buffer{}); code != ExitUnparsable {
		t.Errorf("exit %d", code)
	}
}

// TestRunRemoteOpts proves run, the unexported test seam Run wraps, still
// accepts remoteOpts and reaches the tfcapi-lint command tree exactly like
// Run does, so an image test that needs an in-memory registry's
// remote.Option values can use it.
func TestRunRemoteOpts(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	var remoteOpts []remote.Option
	good := filepath.Join(fixtures, "good", "machine")
	code := run(context.Background(), []string{"module", "--role", "machine", good}, &stdout, io.Discard, remoteOpts)
	if code != ExitOK {
		t.Errorf("exit %d: %s", code, stdout.String())
	}
}
