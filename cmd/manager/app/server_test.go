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
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"k8s.io/component-base/version"
)

// TestHelp proves --help lists every named flag section NewManagerCommand
// registers and exits nil. It does not run t.Parallel: --version tests in
// this package share the verflag global on pflag.CommandLine.
func TestHelp(t *testing.T) {
	cmd := NewManagerCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute --help: %v", err)
	}
	for _, section := range []string{
		"Generic flags:", "Leader election flags:", "Webhook flags:", "Runner flags:",
		"Diagnostics flags:", "Logs flags:", "Feature gates flags:", "Global flags:",
	} {
		if !strings.Contains(out.String(), section) {
			t.Errorf("--help output missing %q\n%s", section, out.String())
		}
	}
}

// TestVersion proves --version prints "manager <version>" to the command's
// out buffer, compared against version.Get() rather than a literal, and
// returns nil. It resets the shared verflag global in t.Cleanup and does
// not run t.Parallel, since every test in this package that executes the
// command shares it.
func TestVersion(t *testing.T) {
	t.Cleanup(func() { _ = pflag.CommandLine.Set("version", "false") })

	cmd := NewManagerCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute --version: %v", err)
	}
	want := fmt.Sprintf("manager %s\n", version.Get())
	if out.String() != want {
		t.Errorf("--version output = %q, want %q", out.String(), want)
	}
}

// TestVersionRaw proves --version=raw prints the Go-syntax representation
// of version.Get() to the command's out buffer and returns nil. It resets
// the shared verflag global in t.Cleanup and does not run t.Parallel.
func TestVersionRaw(t *testing.T) {
	t.Cleanup(func() { _ = pflag.CommandLine.Set("version", "false") })

	cmd := NewManagerCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--version=raw"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute --version=raw: %v", err)
	}
	want := fmt.Sprintf("%#v\n", version.Get())
	if out.String() != want {
		t.Errorf("--version=raw output = %q, want %q", out.String(), want)
	}
}

// TestVersionSubcommand proves `manager version` prints the same line
// `manager --version` does and returns nil. It does not run t.Parallel,
// matching the other command-executing tests in this package.
func TestVersionSubcommand(t *testing.T) {
	cmd := NewManagerCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute version: %v", err)
	}
	want := fmt.Sprintf("manager %s\n", version.Get())
	if out.String() != want {
		t.Errorf("version output = %q, want %q", out.String(), want)
	}
}

// TestPositionalArgsRejected proves the manager command's Args func
// rejects a positional argument, naming it in the returned error.
func TestPositionalArgsRejected(t *testing.T) {
	cmd := NewManagerCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"bogus"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("Execute bogus = %v, want an error naming the argument", err)
	}
}

// TestInvalidFlagValue proves an invalid flag value fails command
// execution with a non-nil error, before RunE ever runs.
func TestInvalidFlagValue(t *testing.T) {
	cmd := NewManagerCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--sync-period=not-a-duration"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "sync-period") {
		t.Errorf("Execute with an invalid --sync-period = %v, want an error naming sync-period", err)
	}
}
