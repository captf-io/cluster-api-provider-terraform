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
	"k8s.io/component-base/version"
)

// newVersionCommand returns the `version` leaf command, which prints the
// same line `runner --version` prints and exits 0.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version and exit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			printVersion(cmd, false)
			return nil
		},
	}
}

// printVersion writes cmd's version to cmd's stdout: raw selects the Go
// syntax representation of version.Get() ("%#v", for --version=raw);
// otherwise it writes cmd's root command name followed by the human
// version string.
func printVersion(cmd *cobra.Command, raw bool) {
	if raw {
		fmt.Fprintf(cmd.OutOrStdout(), "%#v\n", version.Get())
		return
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", cmd.Root().Name(), version.Get())
}
