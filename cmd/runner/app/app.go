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
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/spf13/cobra"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	logsv1 "k8s.io/component-base/logs/api/v1"
	"k8s.io/component-base/version/verflag"

	"github.com/captf-io/cluster-api-provider-terraform/internal/feature"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
)

// runMu serializes NewRunnerCommand's construction and Run's execution of
// it. verflag.AddFlags adds the very same *pflag.Flag, from the global
// pflag.CommandLine, to every command tree NewRunnerCommand builds, and
// cobra's own persistent-flag merging writes into that shared Flag too,
// so two command trees built or executed at once would race on it.
var runMu sync.Mutex

// exitError pairs a process exit code with the error Run should report for
// it. err may be nil when the command already printed everything it needs
// (such as its own usage) and Run should just return code without printing
// anything further.
type exitError struct {
	code int
	err  error
}

// Error returns e's message: the empty string when e wraps no error, else
// its wrapped error's message.
func (e *exitError) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

// Unwrap returns e's wrapped error, so errors.Is and errors.As see through
// e to the underlying cause.
func (e *exitError) Unwrap() error {
	return e.err
}

// NewRunnerCommand returns the root `runner` command: its `copy`, `run` and
// `version` subcommands attached, and on its persistent flags the
// component-base version flags (verflag.AddFlags), the component-base
// logging flags (logsv1.AddFlags: --logging-format, -v, --vmodule,
// --log-flush-frequency, ...) and --feature-gates for the logging gates,
// exactly as Kubernetes components register them. The logging
// configuration is validated and applied (logsv1.ValidateAndApply) before
// any subcommand runs. Shell completion is disabled (this binary is copied
// into arbitrary source images, never installed as a user's shell tool).
// Building or executing more than one of its command trees at once races
// on their shared version flag (runMu); Run is the safe, serialized entry
// point for that.
func NewRunnerCommand() *cobra.Command {
	logConfig := logsv1.NewLoggingConfiguration()
	gates := feature.NewGates()
	root := &cobra.Command{
		Use:           "runner",
		Short:         "Copy or run the CAPTF Job runner",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if err := logsv1.ValidateAndApply(logConfig, gates); err != nil {
				return &exitError{code: runner.ExitUsage, err: fmt.Errorf("logging flags: %w", err)}
			}
			return nil
		},
		RunE: runRoot,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	fs := root.PersistentFlags()
	fs.SetNormalizeFunc(cliflag.WordSepNormalizeFunc)
	verflag.AddFlags(fs)
	logsv1.AddFlags(logConfig, fs)
	gates.AddFlag(fs)
	root.AddCommand(newCopyCommand(), newRunCommand(), newVersionCommand())
	return root
}

// runRoot is the root command's RunE, reached only for a bare `runner`
// invocation or one carrying just the version flags: cobra runs a
// subcommand's own RunE instead once one is matched. cmd is the root
// command being executed; the positional arguments are always empty, since
// cobra strips recognized flags before calling RunE and any unrecognized
// leading token would already have failed as an unknown command. It prints
// the version and returns nil when --version or --version=raw was given;
// otherwise it prints usage to cmd's stderr and returns the exit-2 error
// for a missing command.
func runRoot(cmd *cobra.Command, _ []string) error {
	switch cmd.Flags().Lookup("version").Value.String() {
	case "true":
		printVersion(cmd, false)
		return nil
	case "raw":
		printVersion(cmd, true)
		return nil
	}
	fmt.Fprint(cmd.ErrOrStderr(), cmd.UsageString())
	return &exitError{code: runner.ExitUsage}
}

// Run builds the runner command tree and executes it against args under
// ctx (which bounds `runner run`'s operation), writing the command's output
// to stdout and its errors and usage to stderr; logging goes through klog
// as the component-base logging flags configured it (logs.InitLogs before,
// logs.FlushLogs after, as component-base/cli.Run does). It returns the
// process exit code: an *exitError's own code for a command failure, or 2
// for any other error cobra itself reports, such as an unknown command.
// Concurrent calls to Run are serialized (runMu), since they would
// otherwise race on the version flag's shared state.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if args == nil {
		args = []string{}
	}
	runMu.Lock()
	defer runMu.Unlock()
	root := NewRunnerCommand()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	// As component-base/cli.Run does for every Kubernetes component: route
	// the standard library logger through klog, and flush before exiting.
	logs.InitLogs()
	defer logs.FlushLogs()

	target, err := root.ExecuteContextC(ctx)
	if err == nil {
		return runner.ExitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", target.CommandPath(), ee.err)
		}
		return ee.code
	}
	fmt.Fprintf(stderr, "%s: %v\n\n", target.CommandPath(), err)
	fmt.Fprint(stderr, target.UsageString())
	return runner.ExitUsage
}
