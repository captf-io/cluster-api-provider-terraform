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

package app

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Exit codes.
const (
	// ExitOK: no errors (and, with --strict, no warnings).
	ExitOK = 0
	// ExitFindings: at least one error, or a warning under --strict.
	ExitFindings = 1
	// ExitUnparsable: the module could not be read or parsed, or the image
	// could not be pulled or extracted.
	ExitUnparsable = 2
	// ExitUsage: bad command line.
	ExitUsage = 3
)

// exitCodeErr is returned by a command's RunE once it has already printed
// everything the user needs to see to cmd's error stream; Run and run map
// it to Code instead of printing cobra's own error text.
type exitCodeErr struct {
	// Code is the process exit code the command has already explained.
	Code int
}

// Error returns exitCodeErr's message, a placeholder that is never printed
// (SilenceErrors keeps cobra from showing it; the command that returned e
// already wrote the real explanation).
func (e *exitCodeErr) Error() string {
	return fmt.Sprintf("tfcapi-lint: exit %d", e.Code)
}

// usageErr writes msg to cmd's error stream, headed by cmd's command path
// and followed by cmd's cobra-generated usage, and returns an exitCodeErr
// for ExitUsage.
func usageErr(cmd *cobra.Command, msg string) error {
	fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s\n\n%s", cmd.CommandPath(), msg, cmd.UsageString())
	return &exitCodeErr{Code: ExitUsage}
}
