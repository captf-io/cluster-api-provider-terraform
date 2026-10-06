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
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/lint"
	"github.com/captf-io/cluster-api-provider-terraform/internal/varschema"
)

// newSchemaCommand returns the "schema" subcommand: it loads the module
// directory named by its single positional argument and prints, on one
// line, the compact JSON Schema of the module's user variables, the value
// of the io.captf.variables-schema image label. It exits ExitFindings
// when the schema exceeds varschema.MaxLabelBytes, ExitUnparsable for a
// module that does not parse, and ExitUsage for a bad command line.
func newSchemaCommand() *cobra.Command {
	var role, version string
	cmd := &cobra.Command{
		Use:           "schema <module-dir>",
		Short:         "Print the JSON Schema of a module's user variables (the io.captf.variables-schema label)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case role == "":
				return usageErr(cmd, "--role is required")
			case len(args) != 1:
				return usageErr(cmd, "exactly one module directory is required")
			case !slices.Contains(contract.Versions(), version):
				return usageErr(cmd, fmt.Sprintf("unknown contract %q (known: %s)", version, strings.Join(contract.Versions(), ", ")))
			}
			if err := contract.Role(role).Validate(); err != nil {
				return usageErr(cmd, err.Error())
			}
			m, err := lint.LoadModule(args[0])
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", cmd.CommandPath(), err)
				return &exitCodeErr{Code: ExitUnparsable}
			}
			s, err := lint.VariablesSchema(m, contract.Role(role))
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", cmd.CommandPath(), err)
				return &exitCodeErr{Code: ExitUsage}
			}
			out, err := varschema.Marshal(s)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v; simplify the variable types or drop variables the image need not describe\n", cmd.CommandPath(), err)
				return &exitCodeErr{Code: ExitFindings}
			}
			fmt.Fprintln(cmd.OutOrStdout(), out)
			return nil
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&role, "role", "", "module role: cluster, machine or machinepool (required)")
	fs.StringVar(&version, "contract", contract.Version, "contract version")
	return cmd
}
