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
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/lint"
)

// commonFlags are the flags module and image both take.
type commonFlags struct {
	role, contract string
	json, strict   bool
	// allow are check IDs whose warnings are downgraded to info: an
	// explicit, reviewable exception (a module whose provider cannot tag
	// anything allows input/tags-unused). Errors are never downgraded.
	allow []string
}

// bindCommonFlags registers f's fields as pflag bindings on cmd's flag set:
// --role, --contract, --json, --strict and --allow-warning.
func bindCommonFlags(cmd *cobra.Command, f *commonFlags) {
	fs := cmd.Flags()
	fs.StringVar(&f.role, "role", "", "module role: cluster, machine or machinepool (required)")
	fs.StringVar(&f.contract, "contract", contract.Version, "contract version")
	fs.BoolVar(&f.json, "json", false, "print JSON instead of text")
	fs.BoolVar(&f.strict, "strict", false, "treat warnings as errors for the exit code")
	fs.StringSliceVar(&f.allow, "allow-warning", nil, "downgrade this check ID's warnings to info (repeatable); errors cannot be allowed")
}

// validateCommon validates f and the single positional argument args must
// carry for cmd, named argName ("module directory" or "image reference")
// in the usage errors it writes to cmd's error stream. It returns the
// resolved role and that argument, or a non-nil error already reported to
// the user (an *exitCodeErr for ExitUsage).
func validateCommon(cmd *cobra.Command, argName string, args []string, f commonFlags) (contract.Role, string, error) {
	switch {
	case f.role == "":
		return "", "", usageErr(cmd, "--role is required")
	case len(args) != 1:
		return "", "", usageErr(cmd, "exactly one "+argName+" is required")
	case !slices.Contains(contract.Versions(), f.contract):
		return "", "", usageErr(cmd, fmt.Sprintf("unknown contract %q (known: %s)", f.contract, strings.Join(contract.Versions(), ", ")))
	}
	role := contract.Role(f.role)
	if _, err := lint.Checks(role); err != nil {
		return "", "", usageErr(cmd, err.Error())
	}
	return role, args[0], nil
}
