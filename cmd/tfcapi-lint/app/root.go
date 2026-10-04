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
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/spf13/cobra"
	"k8s.io/component-base/version"
	"k8s.io/component-base/version/verflag"
)

// runMu serializes run: k8s.io/component-base/version/verflag registers
// its --version flag as one *pflag.Flag shared by every command instance,
// and both registering it (verflag.AddFlags) and executing a
// command that carries it (cobra merges persistent flags into every
// subcommand on each run) mutate that shared Flag. Without this lock,
// concurrent Run calls - as tfcapi-lint's own parallel tests make - race
// on it.
var runMu sync.Mutex

// NewLintCommand returns the root tfcapi-lint cobra.Command with the
// module, image and version subcommands attached. remoteOpts extends every
// image pull the image subcommand makes with extra go-containerregistry
// options; production callers pass nil, and tests pass an in-memory
// registry's options. The root command registers
// k8s.io/component-base/version/verflag's --version flag and, when it is
// given with no subcommand, prints the build stamp and exits instead of
// linting anything.
func NewLintCommand(remoteOpts []remote.Option) *cobra.Command {
	root := &cobra.Command{
		Use:   "tfcapi-lint",
		Short: "Lint Terraform modules and source images against the CAPTF module contract",
		Long: "Lint Terraform modules and source images against the CAPTF module contract.\n\n" +
			"Exit codes: 0 clean, 1 errors (or warnings under --strict), 2 unparsable or unpullable, 3 usage.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch cmd.Flags().Lookup("version").Value.String() {
			case "true":
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", cmd.Root().Name(), version.Get())
				return nil
			case "raw":
				fmt.Fprintf(cmd.OutOrStdout(), "%#v\n", version.Get())
				return nil
			}
			fmt.Fprint(cmd.ErrOrStderr(), cmd.UsageString())
			return &exitCodeErr{Code: ExitUsage}
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	// Every subcommand inherits this: a bad flag is a usage error (exit 3)
	// explained with that command's own generated usage.
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return usageErr(cmd, err.Error())
	})
	verflag.AddFlags(root.PersistentFlags())
	root.AddCommand(newModuleCommand(), newImageCommand(remoteOpts), newVersionCommand())
	return root
}

// Run builds the tfcapi-lint command tree, executes it against args using
// ctx for cancellation, stdout for normal output and stderr for errors and
// usage text, and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return run(ctx, args, stdout, stderr, nil)
}

// run is Run with an explicit remoteOpts, the go-containerregistry options
// tests use to reach an in-memory registry instead of a real one; ctx,
// args, stdout and stderr are as in Run, and run returns the same exit
// code Run does.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, remoteOpts []remote.Option) int {
	runMu.Lock()
	defer runMu.Unlock()
	cmd := NewLintCommand(remoteOpts)
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	err := cmd.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	var ec *exitCodeErr
	if errors.As(err, &ec) {
		return ec.Code
	}
	// Only cobra's own argument errors (an unknown command) get here.
	fmt.Fprintf(stderr, "%s: %v\n\n%s", cmd.CommandPath(), err, cmd.UsageString())
	return ExitUsage
}
