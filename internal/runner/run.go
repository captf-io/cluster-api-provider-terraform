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

package runner

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"github.com/captf-io/cluster-api-provider-terraform/internal/strutil"
)

// Options configure one run (the flags of `runner run`).
type Options struct {
	Op            string
	Bin           []string
	Image         string
	ModuleDir     string
	ProvidersDir  string
	WorkDir       string
	ConfigDir     string
	BackendConfig []string
	LockTimeout   time.Duration
	// StopTimeout is how long an interrupted step may take before SIGKILL.
	StopTimeout   time.Duration
	ForceUnlockID string
	// GuardDeletes makes an apply stop, with an ErrorKindBlocked result,
	// before a plan that deletes or replaces a resource, unless
	// AllowDeletesHash is InputsHash: the hash an approval of the inputs
	// the Job renders must name (a cluster's inputs hash; a pool's inputs
	// hash without bootstrap_data). The cluster role's apply Jobs set it,
	// and a pool's apply that renders a change of the cluster's exports.
	GuardDeletes     bool
	InputsHash       string
	AllowDeletesHash string
	// ExpectPlan is the approved PlanHash of an apply under applyPolicy
	// Manual: the apply plans to a file and applies it only when its
	// PlanHash is this one, else it stops with ErrorKindPlanChanged and the
	// new plan. Approving the plan approves its deletes: the destructive
	// guard does not apply on top.
	ExpectPlan string
	// PlanKeyFile is the file holding the object's plan key, which keys
	// the plan fingerprint. A plan, and an apply with ExpectPlan, fail
	// before their first step when it cannot be read; no other op reads it.
	PlanKeyFile string
	// RestoreChunks is the number of backup chunks a restore reads from
	// <ConfigDir>/restore; RestoreResources is the backup's managed
	// resource count, which state list must confirm.
	RestoreChunks    int
	RestoreResources int
	// Env is the environment passed to the runtime (the container's).
	Env []string
	// Stdout and Stderr receive the runtime's output (the container log).
	Stdout, Stderr io.Writer
	// EventObject is --event-object, the owning Terraform* object events
	// are about; JobName is --job-name, their related object. Without
	// EventObject the runner emits no events.
	EventObject string
	JobName     string
	// Events receives the run's progress events; nil emits none.
	Events Recorder

	// red removes the run's secret values from every free text the run
	// reports; run sets it once Prepare has built the environment.
	red *Redactor
}

// Run executes one operation under ctx and returns its result and the
// process exit code: ExitOK, ExitUsage for an unknown op, else ExitFailure
// (a failing step's own exit code is in the result's Steps). The result is
// complete even on preflight or preparation failure; the caller writes it.
// With o.Events it reports its progress as events (RunStarted, Step*,
// PlanSummary, ResourcesChanged and, on every path, RunFinished).
func Run(ctx context.Context, o Options) (Result, int) {
	start := time.Now()
	// run sets o.red, which RunFinished redacts with too.
	r, code := run(ctx, &o)
	o.runFinished(ctx, r, code, time.Since(start))
	return r, code
}

// runFinished emits RunFinished using ctx, for a run whose result was r,
// exit code and time taken were code and took: Normal on success, Warning
// otherwise.
func (o Options) runFinished(ctx context.Context, r Result, code int, took time.Duration) {
	took = took.Round(100 * time.Millisecond)
	action := cmp.Or(o.Op, "run")
	if code == 0 {
		o.emit(ctx, EventTypeNormal, EventRunFinished, action, "%s succeeded in %s", o.Op, took)
		return
	}
	outcome := "failed"
	if r.Error != nil {
		switch r.Error.Kind {
		case ErrorKindInterrupted:
			outcome = "was interrupted"
		case ErrorKindBlocked:
			outcome = "was blocked before a plan that deletes or replaces resources"
		case ErrorKindPlanChanged:
			outcome = "stopped: the plan differs from the approved one"
		case ErrorKindImageLayout:
			outcome = "failed: the image does not follow the image contract"
		}
		if r.Error.Step != nil && r.Error.Kind != ErrorKindImageLayout {
			outcome += " in step " + *r.Error.Step
		}
	}
	o.emit(ctx, EventTypeWarning, EventRunFinished, action, "%s %s after %s (exit %d)", action, outcome, took, code)
}

// run is Run without the RunFinished event: it executes o's operation
// under ctx, step by step, sets o.red once the environment is prepared,
// and returns the result and the process exit code.
func run(ctx context.Context, o *Options) (Result, int) {
	logger := klog.FromContext(ctx)
	r := Result{Version: ResultVersion, Op: o.Op, Image: ResultImage{Ref: o.Image}, Runtime: Runtime{Command: slices.Clone(o.Bin)}, Steps: []Step{}}
	steps, err := Steps(o.Op, PlanOptions{
		BackendConfig: o.BackendConfig, LockTimeout: o.LockTimeout, ForceUnlockID: o.ForceUnlockID, WorkDir: o.WorkDir,
		GuardDeletes: o.GuardDeletes, ExpectPlan: o.ExpectPlan,
	})
	if err != nil {
		return failed(r, ErrorKindStep, "plan", err.Error()), ExitUsage
	}
	if err := Preflight(o.ModuleDir, o.Bin); err != nil {
		logger.Error(err, "Preflight failed")
		return failed(r, ErrorKindImageLayout, "", err.Error()), ExitFailure
	}
	prep, err := Prepare(o.ConfigDir, o.WorkDir, o.ProvidersDir, o.Env)
	if err != nil {
		logger.Error(err, "Prepare failed")
		return failed(r, ErrorKindStep, StepPrepare, err.Error()), ExitFailure
	}
	if len(prep.Dropped) > 0 {
		logger.Info("Ignoring environment variables the runner owns", "vars", prep.Dropped)
	}
	if o.Op == OpRestore {
		if err := AssembleRestore(o.ConfigDir, o.WorkDir, o.RestoreChunks); err != nil {
			logger.Error(err, "Prepare failed")
			return failed(r, ErrorKindStep, StepPrepare, err.Error()), ExitFailure
		}
	}
	var planKey []byte
	if o.fingerprints() {
		// Fail closed: an unkeyed fingerprint would bind no value, and a
		// plan nobody can approve is better than one that approves too much.
		if planKey, err = loadPlanKey(o.PlanKeyFile); err != nil {
			logger.Error(err, "Loading the plan key failed")
			return failed(r, ErrorKindStep, StepPrepare, "the plan key could not be read: "+err.Error()), ExitFailure
		}
	}
	// Everything the run reports is redacted: the environment's credentials
	// and the sensitive variables now, the plan's sensitive values once a
	// plan is parsed.
	secrets := append(secretsOf(envSecrets(prep.Env)), varSecrets(prep.RootDir)...)
	o.red = newRedactor(secrets)
	r.Image.ProvidersMirror = prep.ProvidersMirror
	r.Runtime.Version = runtimeVersion(ctx, o.Bin, prep)
	o.emit(ctx, EventTypeNormal, EventRunStarted, o.Op, "%s started: image %s, runtime %s", o.Op, o.Image, cmp.Or(r.Runtime.Version, "version unknown"))

	for i, s := range steps {
		if s.Name == StepShowJSON && (i == 0 || r.Steps[len(r.Steps)-1].Exit != 2) {
			// Only a plan with changes (exit 2) is shown.
			continue
		}
		// One line before and after each step; the runtime's own output
		// passes through between them. Never the environment.
		logger.Info("Step started", "step", s.Name)
		o.emit(ctx, EventTypeNormal, EventStepStarted, s.Name, "step %s started", s.Name)
		stdout, stderr := o.Stdout, o.Stderr
		var scan *changesScanner
		var ui *uiRenderer
		if s.Name == StepApply || s.Name == StepDestroy {
			// The output still streams to the log; the scanner only keeps
			// the summary line's counts.
			scan = &changesScanner{}
			stdout = io.MultiWriter(o.Stdout, scan)
			// -json output is rendered back to readable log lines (the
			// scanner sees those) and its error diagnostics kept.
			// Both pipes of the step write the error stream: serialize.
			stderr = &syncWriter{w: o.Stderr}
			ui = newUIRenderer(stdout, stderr, o.red)
			stdout = ui
		}
		res := Exec(ctx, o.Bin, s, prep.Env, prep.RootDir, stdout, stderr, o.StopTimeout)
		var diags []Diagnostic
		var errored []string
		if ui != nil {
			ui.Flush()
			diags, errored = ui.Diagnostics(), ui.Errored()
		}
		logger.Info("Step finished", "step", s.Name, "exit", res.Exit, "seconds", res.Seconds)
		r.Steps = append(r.Steps, Step{Name: s.Name, Exit: res.Exit, Seconds: round(res.Seconds)})
		if scan != nil {
			r.Changes = scan.Changes()
		}
		if res.Failed(s) {
			fallback := fmt.Sprintf("step %s exited %d; see the Job's logs", s.Name, res.Exit)
			if res.Err != nil {
				fallback = fmt.Sprintf("step %s exited %d: %s", s.Name, res.Exit, res.Err.Error())
			}
			summary := FailureSummary(o.red, s.Name, res.Stdout, res.Tail, fallback)
			if len(diags) > 0 {
				summary = strutil.Truncate(o.red.Redact(strings.Join(errorLines(diags), "\n")), MaxSummary)
			}
			kind := ErrorKindStep
			if ctx.Err() != nil {
				// Terraform/OpenTofu catch SIGTERM and exit non-zero on their
				// own accord: only the canceled context marks this
				// as an interruption, not a module failure.
				kind = ErrorKindInterrupted
			}
			o.stepFailed(ctx, s.Name, res, summary)
			if s.Name == StepApply || s.Name == StepDestroy {
				if push, ok := o.pushErroredState(ctx, prep); ok {
					r.Steps = append(r.Steps, push)
				}
			}
			// The step's own code is in r.Steps; the process reports a
			// failure. Passing a runtime's code through would make a
			// Terraform panic (exit 2) look like ExitUsage.
			r = o.fail(r, kind, s.Name, summary)
			r.Error.Resources = failedResources(o.red, diags, errored)
			return r, ExitFailure
		}
		if s.Name == StepShowJSON {
			// A plan that does not parse fails the step: one StepFailed, not
			// a StepSucceeded first. The parses below then cannot fail.
			if _, err := parsePlan(res.Stdout); err != nil {
				o.stepFailed(ctx, s.Name, res, "the plan could not be parsed")
				return o.fail(r, ErrorKindStep, s.Name, err.Error()), ExitFailure
			}
		}
		if s.Name == StepStateList {
			// The restore's sanity check, before StepSucceeded: a push that
			// left no managed resource of a backup that has some failed.
			n := ManagedAddresses(res.Stdout)
			logger.Info("Restored state lists managed resource instances", "count", n)
			if n == 0 && o.RestoreResources > 0 {
				summary := fmt.Sprintf("the restored state lists no managed resources, but the backup has %d", o.RestoreResources)
				o.stepFailed(ctx, s.Name, res, summary)
				return o.fail(r, ErrorKindStep, s.Name, summary), ExitFailure
			}
		}
		o.emit(ctx, EventTypeNormal, EventStepSucceeded, s.Name, "step %s succeeded (exit %d) in %.1fs", s.Name, res.Exit, round(res.Seconds))
		if scan != nil && r.Changes != nil {
			c := r.Changes
			o.emit(ctx, EventTypeNormal, EventResourcesChanged, s.Name, "%s changed resources: %d added, %d changed, %d destroyed, %d imported",
				s.Name, c.Add, c.Change, c.Destroy, c.Import)
		}
		switch {
		case s.Name == StepPlan && res.Exit == 0:
			o.emit(ctx, EventTypeNormal, EventPlanSummary, s.Name, "plan: no changes")
			if o.Op == OpDrift {
				r.Drift = &Drift{Resources: []string{}}
			}
			if o.Op == OpPlan {
				r.Plan = EmptyPlan()
			}
			if o.Op == OpApply && o.ExpectPlan != "" && o.ExpectPlan != EmptyPlanHash {
				return o.planChanged(ctx, r, EmptyPlan()), ExitFailure
			}
			if o.Op == OpApply {
				// A guarded or approved apply whose plan has no changes (not
				// even to outputs) has nothing to apply: the Job ends here,
				// without an apply step, and reports that it changed nothing.
				logger.Info("Plan has no changes; skipping the apply step")
				r.Changes = &Changes{}
				return r, ExitOK
			}
		case o.Op == OpPlan && s.Name == StepShowJSON:
			p, err := ParsePlan(res.Stdout, planKey)
			if err != nil {
				return o.fail(r, ErrorKindStep, s.Name, err.Error()), ExitFailure
			}
			r.Plan = p
			secrets = append(secrets, planSecrets(p.SensitiveValues())...)
			o.red = newRedactor(secrets)
			o.emit(ctx, EventTypeNormal, EventPlanSummary, s.Name, "plan: %s", p.counts())
			logger.Info("Plan computed", "hash", p.Hash, "create", p.Create, "update", p.Update, "replace", p.Replace,
				"delete", p.Delete, "import", p.Import, "move", p.Move, "forget", p.Forget, "outputChanges", p.OutputChanges)
		case o.Op == OpApply && s.Name == StepShowJSON && o.ExpectPlan != "":
			p, err := ParsePlan(res.Stdout, planKey)
			if err != nil {
				return o.fail(r, ErrorKindStep, s.Name, err.Error()), ExitFailure
			}
			secrets = append(secrets, planSecrets(p.SensitiveValues())...)
			o.red = newRedactor(secrets)
			o.emit(ctx, EventTypeNormal, EventPlanSummary, s.Name, "plan: %s", p.counts())
			if p.Hash != o.ExpectPlan {
				return o.planChanged(ctx, r, p), ExitFailure
			}
			// The approval of the plan covers its deletes and replacements:
			// the approver saw them in its TerraformPlan.
			logger.Info("Applying approved plan", "hash", p.Hash)
		case o.Op == OpDrift && s.Name == StepShowJSON:
			d, err := ParseDrift(res.Stdout)
			if err != nil {
				return o.fail(r, ErrorKindStep, s.Name, err.Error()), ExitFailure
			}
			r.Drift = d
			o.planSummary(ctx, s.Name, d)
		case o.Op == OpApply && s.Name == StepShowJSON:
			// A guarded apply fingerprints its plan, to report it when it
			// blocks, and its apply step can echo a plan-sensitive value.
			p, err := ParsePlan(res.Stdout, planKey)
			if err != nil {
				return o.fail(r, ErrorKindStep, s.Name, err.Error()), ExitFailure
			}
			secrets = append(secrets, planSecrets(p.SensitiveValues())...)
			o.red = newRedactor(secrets)
			destructive, err := DestructiveChanges(res.Stdout)
			if err != nil {
				return o.fail(r, ErrorKindStep, s.Name, err.Error()), ExitFailure
			}
			if d, err := ParseDrift(res.Stdout); err == nil {
				o.planSummary(ctx, s.Name, d)
			}
			if len(destructive) > 0 && !o.deletesApproved() {
				// The addresses may be logged; the plan's values never are.
				summary := strutil.Truncate(o.red.Redact(blockedSummary(destructive)), MaxSummary)
				logger.Info("Apply blocked", "summary", summary, "inputsHash", o.InputsHash, "planHash", p.Hash)
				r.Plan = p
				return o.fail(r, ErrorKindBlocked, "", summary), ExitFailure
			}
			if len(destructive) > 0 {
				logger.Info("Applying approved destructive plan", "inputsHash", o.InputsHash, "summary", strutil.Truncate(o.red.Redact(blockedSummary(destructive)), MaxSummary))
			}
		}
	}
	return r, ExitOK
}

// planChanged returns r for an approved apply whose plan p is not the
// approved one, using ctx's logger: it stops before the apply step, with
// the new plan for the controller to show and approve.
func (o Options) planChanged(ctx context.Context, r Result, p *Plan) Result {
	summary := strutil.Truncate(o.red.Redact(planChangedSummary(o.ExpectPlan, p)), MaxSummary)
	klog.FromContext(ctx).Info("Apply stopped: plan changed", "summary", summary)
	r.Plan = p
	return failed(r, ErrorKindPlanChanged, "", summary)
}

// stepFailed emits StepFailed using ctx, for step and its result res, with
// the curated failure summary (never raw stderr or plan values).
func (o Options) stepFailed(ctx context.Context, step string, res StepResult, summary string) {
	o.emit(ctx, EventTypeWarning, EventStepFailed, step, "step %s failed (exit %d) after %.1fs: %s", step, res.Exit, round(res.Seconds), summary)
}

// planSummary emits PlanSummary using ctx, for step and drift summary d:
// counts only, never addresses or values.
func (o Options) planSummary(ctx context.Context, step string, d *Drift) {
	o.emit(ctx, EventTypeNormal, EventPlanSummary, step, "plan: %d to create, %d to update, %d to replace, %d to delete", d.Create, d.Update, d.Replace, d.Delete)
}

// fingerprints reports whether this run computes a plan fingerprint, and
// so needs the plan key: a plan Job, and an approved apply (ExpectPlan)
// that compares its plan's fingerprint with the approved one, and a guarded
// apply (GuardDeletes), which reports its plan's fingerprint when it blocks.
// Drift reads the plan without fingerprinting it, so no run ever computes an
// unkeyed fingerprint.
func (o Options) fingerprints() bool {
	return o.Op == OpPlan || (o.Op == OpApply && (o.GuardDeletes || o.ExpectPlan != ""))
}

// deletesApproved reports whether this run's inputs hash was approved for
// a destructive plan. An empty hash approves nothing.
func (o Options) deletesApproved() bool {
	return o.AllowDeletesHash != "" && o.AllowDeletesHash == o.InputsHash
}

// fail returns r with its Error set like failed, from kind, step and tail,
// after redacting tail with the run's secrets, so no runtime output leaves
// the runner unredacted.
func (o Options) fail(r Result, kind, step, tail string) Result {
	return failed(r, kind, step, o.red.Redact(tail))
}

// failed returns r with its Error set from kind, step (omitted when "") and
// tail, capped to MaxTail bytes.
func failed(r Result, kind, step, tail string) Result {
	e := &Error{Kind: kind, Tail: lastBytes(tail, MaxTail)}
	if step != "" {
		e.Step = &step
	}
	r.Error = e
	return r
}

// lastBytes returns the last n bytes of s, or s itself when it is no
// longer than n.
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ErroredStateFile is the file Terraform and OpenTofu write the state to,
// in the working directory, when an apply or destroy cannot persist it to
// the backend at the end of the run.
const ErroredStateFile = "errored.tfstate"

// pushErroredState pushes ErroredStateFile, when the failed apply or
// destroy step left one in prep's root directory, to the backend with
// `state push`, so the resources that step created are recorded and the
// next run finds them instead of no state. It runs even when ctx is
// canceled (the pod is stopping: this is the last chance to persist),
// bounded by o.StopTimeout. It returns the push as a step for the result,
// and false when there was nothing to push.
func (o Options) pushErroredState(ctx context.Context, prep Prepared) (Step, bool) {
	file := filepath.Join(prep.RootDir, ErroredStateFile)
	if _, err := os.Stat(file); err != nil {
		return Step{}, false
	}
	logger := klog.FromContext(ctx)
	logger.Info("The state could not be saved; pushing the errored state to the backend", "file", ErroredStateFile)
	pushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cmp.Or(o.StopTimeout, DefaultStopTimeout))
	defer cancel()
	lock := "-lock-timeout=" + strconv.FormatInt(int64(o.LockTimeout/time.Second), 10) + "s"
	inv := Invocation{Name: StepStatePush, Args: []string{"state", "push", "-input=false", lock, file}}
	res := Exec(pushCtx, o.Bin, inv, prep.Env, prep.RootDir, o.Stdout, o.Stderr, o.StopTimeout)
	logger.Info("Step finished", "step", inv.Name, "exit", res.Exit, "seconds", res.Seconds)
	if res.Failed(inv) {
		o.emit(ctx, EventTypeWarning, EventStepFailed, inv.Name, "step %s exited %d: the state the failed step left was not saved", inv.Name, res.Exit)
	}
	return Step{Name: inv.Name, Exit: res.Exit, Seconds: round(res.Seconds)}, true
}

// round returns s rounded to one decimal place.
func round(s float64) float64 {
	return float64(int64(s*10+0.5)) / 10
}

// runtimeVersion runs `<bin> version -json` under ctx, in prep's root
// directory and environment, and returns key terraform_version, present in
// Terraform 1.16.4 and OpenTofu 1.12.6 alike (verified). A failure leaves it
// empty: the version is informational.
func runtimeVersion(ctx context.Context, bin []string, prep Prepared) string {
	res := Exec(ctx, bin, Invocation{Name: "version", Args: []string{"version", "-json"}, Capture: true}, prep.Env, prep.RootDir, io.Discard, io.Discard, 0)
	if res.Failed(Invocation{}) {
		return ""
	}
	var v struct {
		TerraformVersion string `json:"terraform_version"`
	}
	if json.NewDecoder(bytes.NewReader(res.Stdout)).Decode(&v) != nil {
		return ""
	}
	return v.TerraformVersion
}
