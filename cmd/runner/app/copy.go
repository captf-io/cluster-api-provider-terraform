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
	"errors"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
)

// newCopyCommand returns the `copy <dest>` leaf command: it copies the
// running binary to dest, mode 0755, for the init container to hand off to
// the main container's read-only filesystem (distroless has no cp).
func newCopyCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "copy <dest>",
		Short: "Copy this binary to <dest> (init container)",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return &exitError{code: runner.ExitUsage, err: errors.New("want exactly one destination")}
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			self, err := os.Executable()
			if err != nil {
				return &exitError{code: runner.ExitFailure, err: err}
			}
			if err := copyFile(self, args[0]); err != nil {
				return &exitError{code: runner.ExitFailure, err: err}
			}
			return nil
		},
	}
}

// copyFile copies src to dst, mode 0755, and returns a non-nil error if
// either the open, create or copy fails.
func copyFile(src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- src is the runner's own executable path
	if err != nil {
		return err
	}
	defer in.Close()
	// #nosec G304 G302 -- dst is the CLI's argument; the runner binary copy must be executable
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
