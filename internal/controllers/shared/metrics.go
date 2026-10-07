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

package shared

import (
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
)

// runnerErrorKinds are the error kinds a result may carry; anything else
// is reported as unknown, so a result cannot mint label values.
var runnerErrorKinds = []string{runner.ErrorKindStep, runner.ErrorKindImageLayout, runner.ErrorKindInterrupted, runner.ErrorKindBlocked, runner.ErrorKindPlanChanged}

// recordFinished counts f, a finished Job of a kind, once through d:
// Bookkeep calls it only when it deleted the Job's per-run Secret, which
// happens once per Job across reconciles, restarts and replicas. retry is
// the Job's retry number (retryNumber), not the lifetime sequence number
// in its name. Everything per Job (steps, queue time, error, resources
// changed, drift found) is recorded here and nowhere else: a bookkept
// Job's pod is never read again.
func recordFinished(d Deps, kind string, f finished, retry int) {
	j := metrics.Job{Kind: kind, Op: string(jobs.OpOf(f.job)), Result: jobResult(f), Attempt: retry}
	j.Duration = jobs.FinishedAt(f.job).Sub(f.job.CreationTimestamp.Time)
	if start := f.job.Status.StartTime; start != nil {
		j.Duration = jobs.FinishedAt(f.job).Sub(start.Time)
	}
	if f.pod != nil {
		if at, ok := jobs.SourceStartedAt(f.pod); ok {
			j.Queue = at.Sub(f.job.CreationTimestamp.Time)
		}
	}
	if r := f.result; r != nil {
		for _, s := range r.Steps {
			j.Steps = append(j.Steps, metrics.Step{Name: runner.StepLabel(s.Name), Seconds: s.Seconds})
		}
		if c := r.Changes; c != nil {
			j.Changes = &metrics.Changes{Create: c.Add, Update: c.Change, Delete: c.Destroy, Import: c.Import}
		}
		if dr := r.Drift; jobs.OpOf(f.job) == jobs.OpDrift && dr != nil && dr.Detected {
			j.Drift = &metrics.Changes{Create: dr.Create, Update: dr.Update, Replace: dr.Replace, Delete: dr.Delete}
		}
	}
	if !f.ok {
		j.ErrorKind, j.ErrorStep = errorLabels(f)
	}
	d.Metrics.JobFinished(j)
}

// jobResult returns the result label of f, a finished Job. A Job past its
// deadline is a deadline even when the runner, stopped by it, reported an
// interruption.
func jobResult(f finished) string {
	switch {
	case f.ok:
		return metrics.ResultSucceeded
	case f.blocked:
		return metrics.ResultBlocked
	case f.planChanged:
		return metrics.ResultPlanChanged
	case jobs.DeadlineExceeded(f.job):
		return metrics.ResultDeadline
	case f.interrupted:
		return metrics.ResultInterrupted
	}
	return metrics.ResultFailed
}

// errorLabels returns the error_kind and step labels of f, a Job that did
// not succeed: the runner's error kind and failing step when its result
// says, else deadline or unknown and none.
func errorLabels(f finished) (kind, step string) {
	step = metrics.ErrorStepNone
	if r := f.result; r != nil && r.Error != nil {
		kind = metrics.ErrorKindUnknown
		if slices.Contains(runnerErrorKinds, r.Error.Kind) {
			kind = r.Error.Kind
		}
		if r.Error.Step != nil {
			step = runner.StepLabel(*r.Error.Step)
		}
		return kind, step
	}
	if jobs.DeadlineExceeded(f.job) {
		return metrics.ErrorKindDeadline, step
	}
	return metrics.ErrorKindUnknown, step
}

// decisionOp returns the op label of dec: the op, "finalizer" for
// dropping the finalizer without a Job, "none" when nothing runs.
func decisionOp(dec Decision) string {
	switch dec.Action {
	case ActionJob:
		return string(dec.Op)
	case ActionDropFinalizer, ActionRetain:
		return "finalizer"
	}
	return "none"
}

// conditionValue returns 1, 0 or -1 for a True, False or Unknown (or
// absent) condition of type t on obj.
func conditionValue(obj Object, t string) float64 {
	switch c := conditions.Get(obj, t); {
	case c == nil:
	case c.Status == metav1.ConditionTrue:
		return 1
	case c.Status == metav1.ConditionFalse:
		return 0
	}
	return -1
}

// recordObject sets, through d, the condition-derived per-object gauges
// of obj, a kind.
func recordObject(d Deps, kind string, obj Object) {
	d.Metrics.SetObject(kind, obj.GetNamespace(), obj.GetName(), metrics.Object{
		Ready:   conditionValue(obj, infrav1.ReadyCondition),
		Healthy: conditionValue(obj, infrav1.InfrastructureHealthyCondition),
		Drift:   conditions.IsTrue(obj, infrav1.DriftDetectedCondition),
	})
}

// recordGauges sets every per-object gauge the reconcile knows: the
// condition-derived ones, the unhealthy samples, the inputs size and the
// ops' last successes, using bk's Jobs (nil when bookkeeping did not
// run). The state gauges are set where the state is read.
func (r *reconciler) recordGauges(bk *Bookkeeping) {
	kind, ns, name := r.k.Kind(), r.obj.GetNamespace(), r.obj.GetName()
	recordObject(r.d, kind, r.obj)
	if s := r.st.UnhealthySamples; s != nil {
		r.d.Metrics.SetUnhealthySamples(ns, name, *s)
	}
	if r.inputsBytes > 0 {
		r.d.Metrics.SetInputsBytes(kind, ns, name, r.inputsBytes)
	}
	var list []batchv1.Job
	if bk != nil {
		list = bk.Jobs
	}
	last := lastSuccesses(list)
	// The drift stamp survives pruning; the newest drift Job may be gone.
	if t := r.st.LastDriftCheck; t != nil && t.After(last[jobs.OpDrift]) {
		last[jobs.OpDrift] = t.Time
	}
	for _, op := range []jobs.Op{jobs.OpApply, jobs.OpDestroy, jobs.OpRefresh, jobs.OpDrift} {
		t, ok := last[op]
		switch {
		case !r.scheduled(op):
			// No success is expected: CAPTFNoRecentSuccess must not fire.
			r.d.Metrics.DeleteLastSuccess(kind, ns, name, string(op))
		case ok:
			r.d.Metrics.SetLastSuccess(kind, ns, name, string(op), t)
		}
	}
}

// scheduled reports whether op runs on a schedule for this object right
// now: drift while it has a drift interval, refresh while it samples health,
// neither while it is deleting or paused. Apply and destroy run on demand
// and always report their last success.
func (r *reconciler) scheduled(op jobs.Op) bool {
	idle := r.deleting || r.isPaused
	switch op {
	case jobs.OpDrift:
		return !idle && r.eff.DriftInterval > 0
	case jobs.OpRefresh:
		return !idle && r.provisioned() && r.eff.HealthCheckInterval > 0
	}
	return true
}

// lastSuccesses returns, per op, when its newest successful Job in list
// finished.
func lastSuccesses(list []batchv1.Job) map[jobs.Op]time.Time {
	last := map[jobs.Op]time.Time{}
	for i := range list {
		job := &list[i]
		if jobs.OutcomeOf(job) != jobs.Succeeded {
			continue
		}
		op := jobs.OpOf(job)
		if at := jobs.FinishedAt(job); at.After(last[op]) {
			last[op] = at
		}
	}
	return last
}

// recordTransition counts, through d, c, a condition of a kind that just
// entered its bad state (emitTransitions), so a condition that stays bad
// counts once.
func recordTransition(d Deps, kind string, c *metav1.Condition) {
	switch c.Type {
	case infrav1.StateReadableCondition:
		d.Metrics.StateReadError(kind, stateReason(c.Reason))
	case infrav1.OutputsValidCondition:
		d.Metrics.OutputsInvalid(kind, c.Reason)
	case infrav1.IdentityAllowedCondition:
		reason := "namespace"
		if c.Reason == infrav1.IdentityNotFoundReason || c.Reason == infrav1.SecretNotFoundReason {
			reason = "notfound"
		}
		d.Metrics.IdentityDenied(reason)
	}
}

// stateReason maps reason, a StateReadable reason, to the metric's short
// reason: StateInconsistent → inconsistent, StateEncrypted → encrypted,
// StateCorrupt → corrupt (a version the reader does not support is
// reported as StateCorrupt too). It returns the mapped reason.
func stateReason(reason string) string {
	return strings.ToLower(strings.TrimPrefix(reason, "State"))
}
