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

package shared

import (
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	captfconds "github.com/captf-io/cluster-api-provider-terraform/internal/conditions"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// done returns a finished value for a Job named name, of op, that finished
// at at: succeeded with result r if ok, failed otherwise.
func done(name string, op jobs.Op, ok bool, at time.Time, r *runner.Result) finished {
	outcome := jobs.Failed
	if ok {
		outcome = jobs.Succeeded
	}
	j := job(name, op, outcome, at)
	return finished{job: &j, ok: ok, result: r}
}

// driftRun returns a drift-op runner.Result carrying d as its Drift.
func driftRun(d *runner.Drift) *runner.Result { return &runner.Result{Op: "drift", Drift: d} }

// TestSetDriftResults proves setDriftResults sets DriftDetected, lastRefresh
// and lastDriftCheck from the newest relevant finished Job per case: no
// checks yet, a clean or dirty drift check under Report or Remediate, a
// failed check that keeps the previous verdict and moves no stamp, a
// refresh-only pass, and the newest successful check winning over an older
// one.
func TestSetDriftResults(t *testing.T) {
	t.Parallel()
	found := &runner.Drift{Detected: true, Add: 1, Change: 2, Destroy: 3}
	for _, tt := range []struct {
		name         string
		action       infrav1.DriftAction
		done         []finished // newest first
		refresh      *time.Time
		check        *time.Time
		status       metav1.ConditionStatus
		reason       string
		keepPrevious bool
	}{
		{name: "nothing yet", status: metav1.ConditionUnknown, reason: infrav1.DriftNotCheckedReason},
		{
			name: "no drift", done: []finished{done("d", jobs.OpDrift, true, t0, driftRun(nil))},
			refresh: &t0, check: &t0, status: metav1.ConditionFalse, reason: infrav1.NoDriftReason,
		},
		{
			name: "reported", action: infrav1.DriftActionReport, done: []finished{done("d", jobs.OpDrift, true, t0, driftRun(found))},
			refresh: &t0, check: &t0, status: metav1.ConditionTrue, reason: infrav1.DriftReportedReason,
		},
		{
			name: "remediation pending", action: infrav1.DriftActionRemediate, done: []finished{done("d", jobs.OpDrift, true, t0, driftRun(found))},
			refresh: &t0, check: &t0, status: metav1.ConditionTrue, reason: infrav1.DriftPendingReason,
		},
		{
			// A failed check moves no stamp and keeps the previous verdict,
			// so the drift stays due and retries under its backoff.
			name: "failed check", done: []finished{done("d", jobs.OpDrift, false, t0, nil)},
			keepPrevious: true,
		},
		{
			name: "refresh only", done: []finished{done("r", jobs.OpRefresh, true, t0, nil), done("a", jobs.OpApply, true, t0.Add(-time.Minute), nil)},
			refresh: &t0, status: metav1.ConditionUnknown, reason: infrav1.DriftNotCheckedReason,
		},
		{
			name: "newest success wins", done: []finished{
				done("d2", jobs.OpDrift, false, t0, nil),
				done("d1", jobs.OpDrift, true, t0.Add(-time.Hour), driftRun(nil)),
			},
			refresh: new(t0.Add(-time.Hour)), check: new(t0.Add(-time.Hour)), status: metav1.ConditionFalse, reason: infrav1.NoDriftReason,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := machine()
			// As in production: the first-visit conditions come first, and
			// DriftNotChecked is what SetInitial leaves until a check.
			captfconds.SetInitial(m, captfconds.KindMachine)
			if tt.keepPrevious {
				conditions.Set(m, metav1.Condition{Type: infrav1.DriftDetectedCondition, Status: metav1.ConditionTrue, Reason: infrav1.DriftReportedReason})
				tt.status, tt.reason = metav1.ConditionTrue, infrav1.DriftReportedReason
			}
			st := CommonStatus{WorkspaceStatus: &m.Status.WorkspaceStatus}
			setDriftResults(m, st, tt.action, tt.done, nil, false, false)
			if !sameTime(m.Status.LastRefresh, tt.refresh) || !sameTime(m.Status.LastDriftCheck, tt.check) {
				t.Errorf("lastRefresh %v, lastDriftCheck %v", m.Status.LastRefresh, m.Status.LastDriftCheck)
			}
			if c := conditions.Get(m, infrav1.DriftDetectedCondition); c == nil || c.Status != tt.status || c.Reason != tt.reason {
				t.Errorf("DriftDetected = %+v, want %s/%s", c, tt.status, tt.reason)
			}
		})
	}
}

// TestDriftRemediationTransitions: Remediating falls back to Pending each
// pass; an apply newer than the last check clears the drift for Remediate,
// never for Report; a check newer than the apply wins.
func TestDriftRemediationTransitions(t *testing.T) {
	t.Parallel()
	found := driftRun(&runner.Drift{Detected: true, Change: 1})
	for _, tt := range []struct {
		name   string
		action infrav1.DriftAction
		prior  string // DriftDetected=True reason before the pass
		done   []finished
		reason string
		status metav1.ConditionStatus
	}{
		{"remediating falls back to pending", infrav1.DriftActionRemediate, infrav1.DriftRemediatingReason,
			nil, infrav1.DriftPendingReason, metav1.ConditionTrue},
		{"a successful apply after the check clears", infrav1.DriftActionRemediate, infrav1.DriftPendingReason,
			[]finished{done("a", jobs.OpApply, true, t0, nil), done("d", jobs.OpDrift, true, t0.Add(-time.Minute), found)},
			infrav1.NoDriftReason, metav1.ConditionFalse},
		{"a failed apply does not", infrav1.DriftActionRemediate, infrav1.DriftPendingReason,
			[]finished{done("a", jobs.OpApply, false, t0, nil), done("d", jobs.OpDrift, true, t0.Add(-time.Minute), found)},
			infrav1.DriftPendingReason, metav1.ConditionTrue},
		{"a check after the apply wins", infrav1.DriftActionRemediate, infrav1.DriftPendingReason,
			[]finished{done("d", jobs.OpDrift, true, t0, found), done("a", jobs.OpApply, true, t0.Add(-time.Minute), nil)},
			infrav1.DriftPendingReason, metav1.ConditionTrue},
		{"Report is not cleared by an apply", infrav1.DriftActionReport, infrav1.DriftReportedReason,
			[]finished{done("a", jobs.OpApply, true, t0, nil), done("d", jobs.OpDrift, true, t0.Add(-time.Minute), found)},
			infrav1.DriftReportedReason, metav1.ConditionTrue},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := machine()
			m.Status.LastDriftCheck = &metav1.Time{Time: t0.Add(-time.Hour)}
			conditions.Set(m, metav1.Condition{Type: infrav1.DriftDetectedCondition, Status: metav1.ConditionTrue, Reason: tt.prior})
			var lastApply *batchv1.Job
			ok := false
			for _, f := range tt.done {
				if jobs.OpOf(f.job) == jobs.OpApply && lastApply == nil {
					lastApply, ok = f.job, f.ok
				}
			}
			st := CommonStatus{WorkspaceStatus: &m.Status.WorkspaceStatus}
			setDriftResults(m, st, tt.action, tt.done, lastApply, ok, false)
			if c := conditions.Get(m, infrav1.DriftDetectedCondition); c.Status != tt.status || c.Reason != tt.reason {
				t.Errorf("DriftDetected = %s/%s, want %s/%s", c.Status, c.Reason, tt.status, tt.reason)
			}
		})
	}
}

// TestSetDriftRemediating proves setDriftRemediating sets DriftDetected's
// reason to DriftRemediatingReason only for an apply Job under
// DriftActionRemediate with drift detected, and to DriftPendingReason for a
// drift Job, for Report action, or with no drift detected.
func TestSetDriftRemediating(t *testing.T) {
	t.Parallel()
	apply, drift := job("a", jobs.OpApply, jobs.Running, t0), job("d", jobs.OpDrift, jobs.Running, t0)
	for _, tt := range []struct {
		name   string
		action infrav1.DriftAction
		active *batchv1.Job
		status metav1.ConditionStatus
		want   string
	}{
		{"apply under Remediate", infrav1.DriftActionRemediate, &apply, metav1.ConditionTrue, infrav1.DriftRemediatingReason},
		{"apply under Report", infrav1.DriftActionReport, &apply, metav1.ConditionTrue, infrav1.DriftPendingReason},
		{"a drift Job", infrav1.DriftActionRemediate, &drift, metav1.ConditionTrue, infrav1.DriftPendingReason},
		{"no drift", infrav1.DriftActionRemediate, &apply, metav1.ConditionFalse, infrav1.DriftPendingReason},
	} {
		m := machine()
		conditions.Set(m, metav1.Condition{Type: infrav1.DriftDetectedCondition, Status: tt.status, Reason: infrav1.DriftPendingReason})
		setDriftRemediating(m, tt.action, tt.active)
		if got := conditions.Get(m, infrav1.DriftDetectedCondition).Reason; got != tt.want {
			t.Errorf("%s: reason %s, want %s", tt.name, got, tt.want)
		}
	}
}

// TestRemediationFailures proves remediationFailures counts failed apply
// Jobs since the drift check (or since the last successful apply, if
// later), and counts all failed applies when the check is nil.
func TestRemediationFailures(t *testing.T) {
	t.Parallel()
	check := &metav1.Time{Time: t0.Add(-10 * time.Minute)}
	jobsDone := []finished{
		done("a3", jobs.OpApply, false, t0, nil),
		done("r", jobs.OpRefresh, true, t0.Add(-time.Minute), nil),
		done("a2", jobs.OpApply, false, t0.Add(-2*time.Minute), nil),
		done("a1", jobs.OpApply, false, t0.Add(-20*time.Minute), nil), // before the check
	}
	if n := remediationFailures(jobsDone, check); n != 2 {
		t.Errorf("failures since the check = %d, want 2", n)
	}
	withSuccess := append([]finished{jobsDone[0], done("ok", jobs.OpApply, true, t0.Add(-30*time.Second), nil)}, jobsDone[2:]...)
	if n := remediationFailures(withSuccess, check); n != 1 {
		t.Errorf("failures since the last success = %d, want 1", n)
	}
	if n := remediationFailures(jobsDone, nil); n != 3 {
		t.Errorf("failures with no check = %d, want 3", n)
	}
}

// sameTime reports whether got and want refer to the same instant, or are
// both nil.
func sameTime(got *metav1.Time, want *time.Time) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return got.Time.Equal(*want)
}

// TestAdvanceOnlyForward: stamps never move back when an older Job is the
// newest retained one.
func TestAdvanceOnlyForward(t *testing.T) {
	t.Parallel()
	later := metav1.NewTime(t0)
	p := &later
	advance(&p, t0.Add(-time.Hour))
	advance(&p, time.Time{})
	if !p.Time.Equal(t0) {
		t.Errorf("stamp moved back to %s", p)
	}
	advance(&p, t0.Add(time.Minute))
	if !p.Time.Equal(t0.Add(time.Minute)) {
		t.Errorf("stamp did not advance: %s", p)
	}
}

// TestDriftSummary proves driftSummary returns a zero summary for nil or
// undetected drift, and otherwise copies the counts and resource addresses,
// dropping empty and over-length addresses.
func TestDriftSummary(t *testing.T) {
	t.Parallel()
	if s := driftSummary(nil); s.Add != nil || s.Resources != nil {
		t.Errorf("nil drift = %+v", s)
	}
	if s := driftSummary(&runner.Drift{}); s.Add != nil {
		t.Errorf("no drift = %+v", s)
	}
	long := strings.Repeat("x", maxAddressLength+1)
	s := driftSummary(&runner.Drift{Detected: true, Add: 1, Resources: []string{"module.role.aws_instance.this", long, ""}})
	if *s.Add != 1 || *s.Change != 0 || len(s.Resources) != 1 {
		t.Errorf("summary = %+v", s)
	}
}

// TestReconcileStartsDrift: a provisioned immutable machine whose refresh
// after apply ran starts a drift Job on the pinned repo@digest, against the
// pinned inputs, without touching the durable Secret; the next reconcile
// reports it running.
func TestReconcileStartsDrift(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused, func(m *infrav1.TerraformMachine) {
		m.Status.Initialization.Provisioned = new(true)
	}))...)
	k := e.kindFor(t, readyOwner)
	k.in = machineIn()
	if err := inputs.Write(t.Context(), e.c, k.obj, renderMachine(t), inputs.Meta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
		t.Fatal(err)
	}
	digest := "registry.example/mod@sha256:" + strings.Repeat("c", 64)
	if _, err := inputs.PinDigest(t.Context(), e.c, k.obj, digest, false); err != nil {
		t.Fatal(err)
	}
	applied := job("a", jobs.OpApply, jobs.Succeeded, t0.Add(-time.Hour))
	applied.Annotations = map[string]string{state.InputsHashAnnotation: "h1:x"}
	e.runner.jobs = append(e.runner.jobs, applied, job("r", jobs.OpRefresh, jobs.Succeeded, t0.Add(-50*time.Minute)))
	e.state.st = &state.State{InputsHash: "h1:x"}
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	before, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil {
		t.Fatal(err)
	}
	// Bookkeeping of the successful apply marks the object applied; the
	// drift changes nothing else.
	before.Meta.Applied = true
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 1 {
		t.Fatalf("created %v, want one drift Job", e.runner.created)
	}
	var drift *batchv1.Job
	for i := range e.runner.jobs {
		if e.runner.jobs[i].Name == e.runner.created[0] {
			drift = &e.runner.jobs[i]
		}
	}
	if jobs.OpOf(drift) != jobs.OpDrift || drift.Spec.Template.Spec.Containers[0].Image != digest {
		t.Errorf("Job %s op %s image %s, want drift on %s", drift.Name, jobs.OpOf(drift), drift.Spec.Template.Spec.Containers[0].Image, digest)
	}
	after, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil || !reflect.DeepEqual(after.Meta, before.Meta) || !reflect.DeepEqual(after.Files, before.Files) {
		t.Errorf("drift touched the durable Secret: %+v → %+v (%v)", before.Meta, after.Meta, err)
	}
	m := e.get(t)
	if !sameTime(m.Status.LastRefresh, new(t0.Add(-50*time.Minute))) || m.Status.LastDriftCheck != nil {
		t.Errorf("lastRefresh %v, lastDriftCheck %v", m.Status.LastRefresh, m.Status.LastDriftCheck)
	}

	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	m = e.get(t)
	if c := conditions.Get(m, infrav1.DriftJobSucceededCondition); c == nil || c.Reason != infrav1.DriftJobRunningReason {
		t.Errorf("DriftJobSucceeded = %+v, want DriftJobRunning", c)
	}
	if len(e.runner.created) != 1 {
		t.Errorf("a second Job started while the drift runs: %v", e.runner.created)
	}
}
