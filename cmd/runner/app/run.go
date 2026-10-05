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
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/klog/v2"

	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
)

// DefaultResultPath is the termination log the kubelet reads.
const DefaultResultPath = "/dev/termination-log"

// RunOptions holds `runner run`'s flags before Validate and Complete turn
// them into a runner.Options and a result path. Its fields, names, defaults
// and help text match internal/jobs.Build's arguments, which
// internal/jobs/jobs_test.go parses with ParseRunFlags to keep the two in
// step.
type RunOptions struct {
	Op               string
	Bin              []string
	Image            string
	ModuleDir        string
	ProvidersDir     string
	WorkDir          string
	ConfigDir        string
	BackendConfig    []string
	LockTimeout      time.Duration
	StopTimeout      time.Duration
	ForceUnlockID    string
	GuardDeletes     bool
	InputsHash       string
	AllowDeletesHash string
	ExpectPlan       string
	PlanKeyFile      string
	RestoreChunks    int
	RestoreResources int
	EventObject      string
	JobName          string
	// ResultPath is --result: where the result document is written.
	ResultPath string
}

// AddFlags registers o's flags on fs: exactly the flags internal/jobs.Build
// emits, plus --result. --bin and --backend-config are repeatable and use
// StringArrayVar rather than a comma-splitting slice flag, since their
// values (a runtime command, or a backend-config value such as
// labels={"x"="y",...}) can themselves contain commas.
func (o *RunOptions) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.Op, "op", "", "operation: apply, destroy, refresh, drift, restore or plan")
	fs.StringArrayVar(&o.Bin, "bin", nil, "runtime command, one flag per element (default "+render.RuntimePath+")")
	fs.StringVar(&o.Image, "image", "", "source image reference, echoed in the result")
	fs.StringVar(&o.ModuleDir, "module", render.ModuleDir, "module directory")
	fs.StringVar(&o.ProvidersDir, "providers", render.ProvidersDir, "provider mirror directory (optional)")
	fs.StringVar(&o.WorkDir, "workdir", render.WorkDir, "writable work directory")
	fs.StringVar(&o.ConfigDir, "config", "/captf/config", "directory holding the rendered root module and tfvars (the per-run Secret's mount)")
	fs.StringArrayVar(&o.BackendConfig, "backend-config", nil, "init -backend-config value (repeatable)")
	fs.DurationVar(&o.LockTimeout, "lock-timeout", 5*time.Minute, "state lock timeout")
	fs.DurationVar(&o.StopTimeout, "stop-timeout", runner.DefaultStopTimeout, "time an interrupted step gets to stop before SIGKILL")
	fs.StringVar(&o.ForceUnlockID, "force-unlock", "", "stale lock to force-unlock after init")
	fs.BoolVar(&o.GuardDeletes, "guard-deletes", false, "apply: stop before a plan that deletes or replaces a resource unless --allow-deletes-hash is --inputs-hash")
	fs.StringVar(&o.InputsHash, "inputs-hash", "", "the hash an approval of the inputs the Job renders must name")
	fs.StringVar(&o.AllowDeletesHash, "allow-deletes-hash", "", "the hash approved for a destructive plan")
	fs.StringVar(&o.ExpectPlan, "expect-plan", "", "apply: the approved plan hash; stop with error kind plan-changed unless the plan's hash is this one")
	fs.StringVar(&o.PlanKeyFile, "plan-key-file", runner.DefaultPlanKeyFile, "plan and approved apply: the file holding the key of the plan fingerprint; they fail when it cannot be read")
	fs.IntVar(&o.RestoreChunks, "restore-chunks", 0, "restore: the number of backup chunks under <config>/restore")
	fs.IntVar(&o.RestoreResources, "restore-resources", 0, "restore: the backup's managed resource count; state list must show one when it is not 0")
	fs.StringVar(&o.EventObject, "event-object", "", "emit progress events about <apiVersion>/<kind>/<namespace>/<name>/<uid> (none when unset)")
	fs.StringVar(&o.JobName, "job-name", "", "the Job the events relate to")
	fs.StringVar(&o.ResultPath, "result", DefaultResultPath, "where to write the result")
}

// Validate returns a non-nil error when o.Op is not one runner.Steps
// recognizes.
func (o *RunOptions) Validate() error {
	_, err := runner.Steps(o.Op, runner.PlanOptions{})
	return err
}

// Complete fills the one default AddFlags cannot (an empty --bin defaults
// to a single render.RuntimePath element) and returns the runner.Options
// and result path built from o.
func (o *RunOptions) Complete() (runner.Options, string) {
	bin := o.Bin
	if len(bin) == 0 {
		bin = []string{render.RuntimePath}
	}
	ro := runner.Options{
		Op: o.Op, Bin: bin, Image: o.Image, ModuleDir: o.ModuleDir, ProvidersDir: o.ProvidersDir,
		WorkDir: o.WorkDir, ConfigDir: o.ConfigDir, BackendConfig: o.BackendConfig, LockTimeout: o.LockTimeout,
		StopTimeout: o.StopTimeout, ForceUnlockID: o.ForceUnlockID, GuardDeletes: o.GuardDeletes,
		InputsHash: o.InputsHash, AllowDeletesHash: o.AllowDeletesHash, ExpectPlan: o.ExpectPlan, PlanKeyFile: o.PlanKeyFile,
		RestoreChunks: o.RestoreChunks, RestoreResources: o.RestoreResources, EventObject: o.EventObject, JobName: o.JobName,
	}
	return ro, o.ResultPath
}

// ParseRunFlags parses args as the flags of `runner run` into a
// runner.Options, using a fresh RunOptions and pflag.FlagSet so repeated
// calls (internal/jobs/jobs_test.go parses internal/jobs.Build's output
// with it) never share state. It returns the parsed Options, the result
// path, and a non-nil error when a flag is invalid, extra arguments remain,
// or the operation is not one runner.Steps recognizes.
func ParseRunFlags(args []string) (runner.Options, string, error) {
	fs := pflag.NewFlagSet("run", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := &RunOptions{}
	o.AddFlags(fs)
	if err := fs.Parse(args); err != nil {
		return runner.Options{}, o.ResultPath, err
	}
	if fs.NArg() != 0 {
		return runner.Options{}, o.ResultPath, fmt.Errorf("unexpected arguments %v", fs.Args())
	}
	if err := o.Validate(); err != nil {
		return runner.Options{}, o.ResultPath, err
	}
	ro, resultPath := o.Complete()
	return ro, resultPath, nil
}

// newRunCommand returns the `run` leaf command: it parses its own flags
// into a RunOptions, runs the operation through runner.Run, and always
// writes a result document (runner.Write), even when flag parsing or
// validation itself fails, so the controller never sees a Job pod that
// terminated without one.
func newRunCommand() *cobra.Command {
	o := &RunOptions{}
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run one operation in the Job's main container",
		// Cobra runs only the nearest PersistentPreRunE: run the root's
		// (which applies the logging flags) and, should the logging flags
		// be invalid, still write the result document.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			err := cmd.Root().PersistentPreRunE(cmd, args)
			if err != nil {
				writeFlagsFailure(cmd.Context(), o.ResultPath, err)
			}
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				err := fmt.Errorf("unexpected arguments %v", args)
				writeFlagsFailure(cmd.Context(), o.ResultPath, err)
				return &exitError{code: runner.ExitUsage, err: err}
			}
			if err := o.Validate(); err != nil {
				writeFlagsFailure(cmd.Context(), o.ResultPath, err)
				return &exitError{code: runner.ExitUsage, err: err}
			}
			return runOperation(cmd, o)
		},
	}
	o.AddFlags(cmd.Flags())
	cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		writeFlagsFailure(cmd.Context(), o.ResultPath, err)
		return &exitError{code: runner.ExitUsage, err: err}
	})
	return cmd
}

// runOperation completes o into a runner.Options, runs it under cmd's
// context, and always writes the result to the path o.Complete returns,
// even when the write itself fails. It returns nil on success or the
// exit-code error the process should report.
func runOperation(cmd *cobra.Command, o *RunOptions) error {
	ctx := cmd.Context()
	ro, resultPath := o.Complete()
	ro.Env, ro.Stdout, ro.Stderr = os.Environ(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	ro.Events = runner.NewEventRecorder(ctx, ro, runner.InClusterFromEnv(os.Getenv), podName())
	res, code := runner.Run(ctx, ro)
	if err := runner.Write(resultPath, res); err != nil {
		klog.FromContext(ctx).Error(err, "Writing the result failed")
		if code == runner.ExitOK {
			code = runner.ExitFailure
		}
	}
	if code != 0 {
		return &exitError{code: code}
	}
	return nil
}

// podName returns the reporting instance of the runner's events: the pod
// name, which the kubelet sets as the hostname.
func podName() string {
	if h := os.Getenv("HOSTNAME"); h != "" {
		return h
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "runner"
	}
	return h
}

// writeFlagsFailure writes a minimal result to resultPath (or
// DefaultResultPath when it is "") for the flag-parse or validation
// failure parseErr: kind step, step "flags", so the controller sees a
// result instead of "no result" for a pod that did terminate. It logs,
// using ctx's logger, any write failure of its own.
func writeFlagsFailure(ctx context.Context, resultPath string, parseErr error) {
	if resultPath == "" {
		resultPath = DefaultResultPath
	}
	step := "flags"
	res := runner.Result{
		Version: runner.ResultVersion,
		Steps:   []runner.Step{},
		Error:   &runner.Error{Kind: runner.ErrorKindStep, Step: &step, Tail: parseErr.Error()},
	}
	if err := runner.Write(resultPath, res); err != nil {
		klog.FromContext(ctx).Error(err, "Writing the result failed")
	}
}
