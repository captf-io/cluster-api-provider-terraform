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
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
)

// maxAddressLength is the API's limit on one drifted resource address.
const maxAddressLength = 512

// setDriftResults consumes the finished refresh and drift Jobs, newest
// first:
//   - status.lastRefresh advances to the newest successful refresh or drift
//     Job (a drift run refreshes first), and status.lastDriftCheck to the
//     newest successful drift Job. A failed Job leaves them, so the op stays
//     due and retries under its backoff; the stamps only move forward, so
//     pruned Jobs lose nothing.
//   - DriftDetected follows the newest successful drift Job: True with
//     DriftReported, or DriftPending when a mutable kind remediates;
//     False/NoDrift. SetInitial makes it Unknown/DriftNotChecked until the
//     first check completes. A failed check leaves it as it was. Neither
//     condition is in Ready.
//   - With Remediate, a successful apply that finished after the last
//     drift check clears it to False/NoDrift, and
//     DriftRemediating falls back to DriftPending with the drift summary
//     (and the remediation Job's failure, or its block before a destructive
//     plan); Bookkeep sets it again while the apply still runs, in the same
//     pass.
//
// obj is the object whose conditions are set and st its status, holding
// LastRefresh and LastDriftCheck; action is the configured drift action;
// done are this pass's finished Jobs; lastApply, lastApplyOK and
// lastApplyBlocked describe the newest finished apply Job, if any.
func setDriftResults(obj Object, st CommonStatus, action infrav1.DriftAction, done []finished, lastApply *batchv1.Job, lastApplyOK, lastApplyBlocked bool) {
	if c := conditions.Get(obj, infrav1.DriftDetectedCondition); c != nil && c.Reason == infrav1.DriftRemediatingReason {
		msg := driftSummaryMessage(c.Message)
		switch {
		case lastApply != nil && lastApplyBlocked:
			msg += remediationMarker + lastApply.Name + " was blocked: its plan deletes or replaces resources; see ApplyJobSucceeded to approve it"
		case lastApply != nil && planChangedIn(done, lastApply.Name):
			msg += remediationMarker + lastApply.Name + " stopped: its plan changed since it was approved; see ApplyJobSucceeded to approve the new plan"
		case lastApply != nil && !lastApplyOK:
			msg += remediationMarker + lastApply.Name + " failed"
		}
		conditions.Set(obj, metav1.Condition{Type: c.Type, Status: c.Status, Reason: infrav1.DriftPendingReason, Message: msg})
	}
	var refreshed, checked *finished
	for i := range done {
		f := &done[i]
		op := jobs.OpOf(f.job)
		if !f.ok || (op != jobs.OpRefresh && op != jobs.OpDrift) {
			continue
		}
		if refreshed == nil {
			refreshed = f
		}
		if op == jobs.OpDrift && checked == nil {
			checked = f
		}
	}
	if refreshed != nil {
		advance(&st.LastRefresh, jobs.FinishedAt(refreshed.job))
	}
	if checked != nil {
		advance(&st.LastDriftCheck, jobs.FinishedAt(checked.job))
	}

	// A bookkept drift Job has no result any more; the condition it set
	// stands.
	if checked != nil && checked.result != nil {
		conditions.Set(obj, driftDetected(checked, action))
	}

	// The stamp survives pruning, so compare against it rather than the
	// drift Job: an apply newer than the last successful check converged
	// config and reality.
	if action == infrav1.DriftActionRemediate && lastApply != nil && lastApplyOK && st.LastDriftCheck != nil &&
		jobs.FinishedAt(lastApply).After(st.LastDriftCheck.Time) && conditions.IsTrue(obj, infrav1.DriftDetectedCondition) {
		conditions.Set(obj, metav1.Condition{
			Type: infrav1.DriftDetectedCondition, Status: metav1.ConditionFalse,
			Reason: infrav1.NoDriftReason, Message: "Remediated by Job " + lastApply.Name,
		})
	}
}

// planChangedIn reports whether the finished Job name in done stopped
// because its approved plan changed.
func planChangedIn(done []finished, name string) bool {
	for _, f := range done {
		if f.job.Name == name {
			return f.planChanged
		}
	}
	return false
}

// setDriftRemediating marks obj's DriftDetected True/DriftRemediating
// while active, an apply Job, runs against pending drift with action
// Remediate.
func setDriftRemediating(obj Object, action infrav1.DriftAction, active *batchv1.Job) {
	c := conditions.Get(obj, infrav1.DriftDetectedCondition)
	if action != infrav1.DriftActionRemediate || jobs.OpOf(active) != jobs.OpApply || c == nil || c.Status != metav1.ConditionTrue {
		return
	}
	conditions.Set(obj, metav1.Condition{
		Type: infrav1.DriftDetectedCondition, Status: metav1.ConditionTrue, Reason: infrav1.DriftRemediatingReason,
		Message: driftSummaryMessage(c.Message) + remediationMarker + active.Name + " applies the current inputs",
	})
}

// remediationMarker separates the drift summary in a DriftDetected message
// from what the remediation Job is doing.
const remediationMarker = "; remediation Job "

// driftSummaryMessage returns the drift check's part of msg, a
// DriftDetected message: everything before remediationMarker.
func driftSummaryMessage(msg string) string {
	summary, _, _ := strings.Cut(msg, remediationMarker)
	return summary
}

// remediationFailures returns the count of failed apply Jobs in done that
// finished after lastCheck, the last successful drift check, newest
// first, stopping at an apply success. A blocked apply is not a failure:
// it waits for an approval in DecideOp.
func remediationFailures(done []finished, lastCheck *metav1.Time) int {
	n := 0
	for _, f := range done {
		if jobs.OpOf(f.job) != jobs.OpApply || f.blocked || f.planChanged {
			continue
		}
		if f.ok || (lastCheck != nil && !jobs.FinishedAt(f.job).After(lastCheck.Time)) {
			break
		}
		n++
	}
	return n
}

// driftDetected maps f, a successful drift Job's result, to the
// DriftDetected condition to set, treating the finding as DriftPending
// instead of DriftReported when action is Remediate. It returns the
// condition to set.
func driftDetected(f *finished, action infrav1.DriftAction) metav1.Condition {
	c := metav1.Condition{Type: infrav1.DriftDetectedCondition, Status: metav1.ConditionFalse, Reason: infrav1.NoDriftReason, Message: "Job " + f.job.Name}
	d := f.result.Drift
	if d == nil || !d.Detected {
		return c
	}
	c.Status, c.Reason = metav1.ConditionTrue, infrav1.DriftReportedReason
	if action == infrav1.DriftActionRemediate {
		c.Reason = infrav1.DriftPendingReason
	}
	c.Message = fmt.Sprintf("Job %s: %d to add, %d to change, %d to destroy", f.job.Name, d.Add, d.Change, d.Destroy)
	return c
}

// setDriftRunning marks DriftJobSucceeded Unknown/DriftJobRunning while a
// drift Job runs. A refresh Job does not: objects refresh repeatedly while
// health is pending, and the condition would flap with each one.
// setDriftRunning marks obj's DriftJobSucceeded Unknown/DriftJobRunning
// while the Job named active runs.
func setDriftRunning(obj Object, active string) {
	conditions.Set(obj, metav1.Condition{
		Type: infrav1.DriftJobSucceededCondition, Status: metav1.ConditionUnknown,
		Reason: infrav1.DriftJobRunningReason, Message: "Job " + active,
	})
}

// driftSummary returns d, a drift result, copied into a status.lastRun
// DriftSummary; addresses longer than the API allows are left out rather
// than failing the status patch.
func driftSummary(d *runner.Drift) infrav1.DriftSummary {
	if d == nil || !d.Detected {
		return infrav1.DriftSummary{}
	}
	s := infrav1.DriftSummary{Add: new(int32(d.Add)), Change: new(int32(d.Change)), Destroy: new(int32(d.Destroy))} // #nosec G115 -- drift resource counts, far below MaxInt32
	for _, r := range d.Resources {
		if r != "" && len(r) <= maxAddressLength {
			s.Resources = append(s.Resources, r)
		}
	}
	return s
}

// advance moves *p forward to t.
func advance(p **metav1.Time, t time.Time) {
	if t.IsZero() || (*p != nil && !(*p).Before(&metav1.Time{Time: t})) {
		return
	}
	v := metav1.NewTime(t)
	*p = &v
}
