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
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/component-base/version"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// versionReport is the version --json output: the build stamp and every
// contract version this binary lints against.
type versionReport struct {
	Version  string   `json:"version"`
	Commit   string   `json:"commit"`
	Date     string   `json:"date"`
	Contract []string `json:"contract"`
}

// newVersionCommand returns the "version" subcommand: with no flags it
// prints the same one-line build stamp as a bare --version on the root
// command; --json prints the build stamp and the supported contract
// versions as a versionReport instead. It takes no positional arguments
// and exits ExitUsage if given one, or ExitUnparsable if its JSON cannot
// be written.
func newVersionCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:           "version",
		Short:         "Print the version and supported contract versions",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageErr(cmd, fmt.Sprintf("unexpected argument %q", args[0]))
			}
			info := version.Get()
			if !asJSON {
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", cmd.Root().Name(), info)
				return nil
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			if err := enc.Encode(versionReport{
				Version:  info.GitVersion,
				Commit:   info.GitCommit,
				Date:     info.BuildDate,
				Contract: contract.Versions(),
			}); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", cmd.CommandPath(), err)
				return &exitCodeErr{Code: ExitUnparsable}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}
