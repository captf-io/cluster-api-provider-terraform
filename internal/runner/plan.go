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
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
)

// Operations, matching internal/jobs.Op (the runner cannot import it).
const (
	OpApply   = "apply"
	OpDestroy = "destroy"
	OpRefresh = "refresh"
	OpDrift   = "drift"
	OpRestore = "restore"
	// OpPlan plans the current inputs for review and applies nothing
	// (applyPolicy Manual).
	OpPlan = "plan"
)

// RestoreStateFile is the decompressed backup a restore pushes, under the
// work directory.
const RestoreStateFile = "restore.tfstate"

// Step names, the Name of each Invocation and of each result Step.
const (
	StepInit             = "init"
	StepForceUnlock      = "force-unlock"
	StepValidate         = "validate"
	StepApply            = "apply"
	StepDestroy          = "destroy"
	StepApplyRefreshOnly = "apply-refresh-only"
	StepPlan             = "plan"
	StepShowJSON         = "show-json"
	StepStatePush        = "state-push"
	StepStateList        = "state-list"
)

// StepPrepare and StepOther are metric step labels besides the Invocation
// names: a failure before any runtime step (a bad op, the work directory's
// preparation) reports step "prepare" or "plan", and any name outside the
// known set maps to "other".
const (
	StepPrepare = "prepare"
	StepOther   = "other"
)

// knownSteps are the step names a result can carry.
var knownSteps = []string{
	StepInit, StepForceUnlock, StepValidate, StepApply, StepDestroy,
	StepApplyRefreshOnly, StepPlan, StepShowJSON, StepStatePush, StepStateList, StepPrepare,
}

// StepLabel returns name itself when it is one of the runner's steps, else
// StepOther, as a bounded metric label. A result comes from the termination
// message of a pod running the module image, so its names are not trusted
// to be few.
func StepLabel(name string) string {
	if slices.Contains(knownSteps, name) {
		return name
	}
	return StepOther
}

// PlanFile is the drift or review plan, under the work directory.
const PlanFile = "plan.tfplan"

// ApplyPlanFile is a guarded apply's saved plan, under the work directory.
const ApplyPlanFile = "apply.tfplan"

// Invocation is one runtime command in a sequence.
type Invocation struct {
	Name string
	// Args follow the runtime command.
	Args []string
	// OK lists the exit codes that are not failures; nil means only 0.
	OK []int
	// OKIf lists stderr messages that make a failing exit acceptable.
	OKIf []string
	// Capture keeps stdout in memory instead of passing it to the log. It
	// is used for show -json, whose plan document carries every input value,
	// and for validate -json, whose diagnostics the runner parses into the
	// failure summary.
	Capture bool
	// LogCapture also writes a Capture step's stdout to the log: safe for
	// validate (no input values in its diagnostics), never for show -json.
	LogCapture bool
}

// PlanOptions parameterize the step sequence.
type PlanOptions struct {
	BackendConfig []string
	LockTimeout   time.Duration
	ForceUnlockID string
	// WorkDir holds the plan file.
	WorkDir string
	// GuardDeletes makes apply plan to a file, show it and apply that
	// saved plan, so Run can stop before a plan that deletes or replaces
	// anything (the cluster role, --guard-deletes).
	GuardDeletes bool
	// ExpectPlan, when set, makes apply plan to a file and show it like
	// GuardDeletes, so Run can stop unless the plan's PlanHash is this
	// approved one (applyPolicy Manual, --expect-plan).
	ExpectPlan string
}

// lockTimeout returns o.LockTimeout as a -lock-timeout=<n>s flag value.
func (o PlanOptions) lockTimeout() string {
	return "-lock-timeout=" + strconv.FormatInt(int64(o.LockTimeout/time.Second), 10) + "s"
}

// Steps returns the sequence of op, parameterized by o.
//
// force-unlock runs after init, not before: verified with Terraform 1.16.4
// and OpenTofu 1.12.6, force-unlock in an uninitialized directory fails with
// "Backend initialization required", while init against existing state
// succeeds with the lock held.
//
// The show-json step (drift, plan, and a guarded or approved apply) only
// runs when the plan exits 2; Run decides that. A guarded or approved apply
// whose plan exits 0 (no changes) also skips its apply step: Run ends the
// Job successfully after the plan, with zero changes.
func Steps(op string, o PlanOptions) ([]Invocation, error) {
	common := []string{"-input=false", "-no-color", o.lockTimeout()}
	initArgs := append([]string{"init"}, common...)
	for _, bc := range o.BackendConfig {
		initArgs = append(initArgs, "-backend-config="+bc)
	}
	varFile := "-var-file=" + render.TFVarsFile
	steps := []Invocation{{Name: StepInit, Args: initArgs}}
	if o.ForceUnlockID != "" {
		// A retried pod, or another holder that took the lock since, finds
		// the stale lock already gone. The kubernetes backend's Unlock
		// says so in both runtimes (Terraform v1.16.4, OpenTofu v1.12.6
		// internal/backend/remote-state/kubernetes/client.go) and exits 1.
		steps = append(steps, Invocation{
			Name: StepForceUnlock, Args: []string{"force-unlock", "-force", o.ForceUnlockID},
			OKIf: []string{"state is already unlocked", "does not match existing lock"},
		})
	}
	applyRefreshOnly := Invocation{Name: StepApplyRefreshOnly, Args: append(append([]string{"apply"}, common...), "-refresh-only", "-auto-approve", varFile)}
	if op == OpApply && (o.GuardDeletes || o.ExpectPlan != "") {
		// The plan refreshes as a plain apply would. A saved plan carries its
		// variables: apply rejects -var-file next to it, and needs no
		// -auto-approve. Run checks show-json's plan before the apply step.
		planOut := filepath.Join(o.WorkDir, ApplyPlanFile)
		return append(steps,
			Invocation{Name: StepValidate, Args: []string{"validate", "-json", "-no-color"}, Capture: true, LogCapture: true},
			Invocation{Name: StepPlan, Args: append(append([]string{"plan"}, common...), "-detailed-exitcode", varFile, "-out="+planOut), OK: []int{0, 2}},
			Invocation{Name: StepShowJSON, Args: []string{"show", "-json", "-no-color", planOut}, Capture: true},
			Invocation{Name: StepApply, Args: append(append([]string{"apply"}, common...), planOut)},
		), nil
	}
	switch op {
	case OpApply:
		steps = append(steps,
			Invocation{Name: StepValidate, Args: []string{"validate", "-json", "-no-color"}, Capture: true, LogCapture: true},
			Invocation{Name: StepApply, Args: append(append([]string{"apply"}, common...), "-auto-approve", varFile)},
		)
	case OpDestroy:
		steps = append(steps, Invocation{Name: StepDestroy, Args: append(append([]string{"destroy"}, common...), "-auto-approve", varFile)})
	case OpRefresh:
		steps = append(steps, applyRefreshOnly)
	case OpDrift:
		planOut := filepath.Join(o.WorkDir, PlanFile)
		steps = append(steps,
			applyRefreshOnly,
			Invocation{Name: StepPlan, Args: append(append([]string{"plan"}, common...), "-detailed-exitcode", "-refresh=false", varFile, "-out="+planOut), OK: []int{0, 2}},
			Invocation{Name: StepShowJSON, Args: []string{"show", "-json", "-no-color", planOut}, Capture: true},
		)
	case OpPlan:
		// What the apply would do, for review: the plan refreshes as the
		// apply's does, and nothing is applied. Run summarizes show-json.
		planOut := filepath.Join(o.WorkDir, PlanFile)
		steps = append(steps,
			Invocation{Name: StepValidate, Args: []string{"validate", "-json", "-no-color"}, Capture: true, LogCapture: true},
			Invocation{Name: StepPlan, Args: append(append([]string{"plan"}, common...), "-detailed-exitcode", varFile, "-out="+planOut), OK: []int{0, 2}},
			Invocation{Name: StepShowJSON, Args: []string{"show", "-json", "-no-color", planOut}, Capture: true},
		)
	case OpRestore:
		// -force: the backup's serial is older than the current state's,
		// or its lineage differs. push holds the backend lock like any
		// write; list reads the pushed state back as the sanity check (its
		// output is addresses only, never values).
		steps = append(steps,
			Invocation{Name: StepStatePush, Args: []string{"state", "push", "-force", o.lockTimeout(), filepath.Join(o.WorkDir, RestoreStateFile)}},
			Invocation{Name: StepStateList, Args: []string{"state", "list"}, Capture: true, LogCapture: true},
		)
	default:
		return nil, fmt.Errorf("runner: unknown op %q", op)
	}
	return steps, nil
}
