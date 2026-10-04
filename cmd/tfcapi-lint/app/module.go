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

	"github.com/captf-io/cluster-api-provider-terraform/internal/lint"
)

// newModuleCommand returns the "module" subcommand: it loads the module
// directory named by its single positional argument, lints it against the
// contract for --role, prints the report, and exits with the code the
// report implies (ExitFindings, ExitUnparsable or ExitUsage).
func newModuleCommand() *cobra.Command {
	var f commonFlags
	cmd := &cobra.Command{
		Use:           "module <module-dir>",
		Short:         "Lint a module directory against the contract",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			role, dir, err := validateCommon(cmd, "module directory", args, f)
			if err != nil {
				return err
			}
			m, err := lint.LoadModule(dir)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", cmd.CommandPath(), err)
				return &exitCodeErr{Code: ExitUnparsable}
			}
			report, err := lint.Lint(m, role)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", cmd.CommandPath(), err)
				return &exitCodeErr{Code: ExitUsage}
			}
			return finish(cmd, f, jsonReport{Module: dir, Contract: f.contract, Role: string(role)}, report)
		},
	}
	bindCommonFlags(cmd, &f)
	return cmd
}
