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
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cbmetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/testutil"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// recorder returns a fresh metrics.Recorder registered on a fresh
// component-base metrics.KubeRegistry, failing t on error.
func recorder(t *testing.T) (*metrics.Recorder, cbmetrics.KubeRegistry) {
	t.Helper()
	reg := cbmetrics.NewKubeRegistry()
	r := metrics.New()
	if err := r.Register(reg); err != nil {
		t.Fatal(err)
	}
	return r, reg
}

// gatherAndCount gathers g and returns the number of series in the family
// named name (0 if the family was not gathered at all), and any error
// Gather returned. component-base's testutil has no GatherAndCount.
func gatherAndCount(g cbmetrics.Gatherer, name string) (int, error) {
	mfs, err := g.Gather()
	if err != nil {
		return 0, err
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return len(mf.GetMetric()), nil
		}
	}
	return 0, nil
}

// TestRecordFinished proves recordFinished counts captf_jobs_total and
// captf_job_errors_total by kind, op and outcome for a succeeded, deadline,
// failed, blocked, interrupted and unrecognized-error Job, and records a
// duration, attempt and step-duration series for each.
func TestRecordFinished(t *testing.T) {
	t.Parallel()
	r, reg := recorder(t)
	d := Deps{Metrics: r}
	ok := done("a", jobs.OpApply, true, t0, nil)
	ok.job.Labels[jobs.AttemptLabel] = "2"
	ok.job.Status.StartTime = &metav1.Time{Time: t0.Add(-2 * time.Minute)}
	deadline := done("d", jobs.OpDrift, false, t0, nil)
	deadline.job.Status.Conditions = append(deadline.job.Status.Conditions,
		batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded})
	blocked := done("b", jobs.OpApply, false, t0, &runner.Result{Error: &runner.Error{Kind: runner.ErrorKindBlocked}})
	blocked.blocked = true
	interrupted := done("i", jobs.OpRefresh, false, t0, &runner.Result{
		Steps: []runner.Step{{Name: runner.StepInit, Seconds: 2}, {Name: runner.StepApplyRefreshOnly, Seconds: 30}},
		Error: &runner.Error{Kind: runner.ErrorKindInterrupted, Step: new(runner.StepApplyRefreshOnly)},
	})
	interrupted.interrupted = true
	// A result may name anything: the labels stay bounded.
	odd := done("o", jobs.OpApply, false, t0, &runner.Result{
		Steps: []runner.Step{{Name: "weird-step"}}, Error: &runner.Error{Kind: "novel", Step: new("weird-step")},
	})
	for _, f := range []finished{ok, deadline, done("f", jobs.OpDestroy, false, t0, nil), interrupted, odd} {
		recordFinished(d, "TerraformMachine", f, 1)
	}
	recordFinished(d, "TerraformCluster", blocked, 1)
	want := `
# HELP captf_jobs_total [ALPHA] Jobs completed, by result: succeeded, failed, deadline, interrupted (stopped from outside: a drain, eviction or deletion), blocked (a guarded apply, of a TerraformCluster or of a TerraformMachinePool's change of the cluster's exports, stopped before a plan that deletes or replaces resources, awaiting approval) or plan_changed (an apply approved for one plan planned other changes and stopped, applyPolicy Manual). Op plan is a plan Job that applies nothing.
# TYPE captf_jobs_total counter
captf_jobs_total{kind="TerraformCluster",op="apply",result="blocked"} 1
captf_jobs_total{kind="TerraformMachine",op="apply",result="failed"} 1
captf_jobs_total{kind="TerraformMachine",op="apply",result="succeeded"} 1
captf_jobs_total{kind="TerraformMachine",op="destroy",result="failed"} 1
captf_jobs_total{kind="TerraformMachine",op="drift",result="deadline"} 1
captf_jobs_total{kind="TerraformMachine",op="refresh",result="interrupted"} 1
# HELP captf_job_errors_total [ALPHA] Jobs that did not succeed, by the runner's error kind (step, image-layout, interrupted, blocked, plan-changed; deadline or unknown without a result) and failing step (none when no step failed).
# TYPE captf_job_errors_total counter
captf_job_errors_total{error_kind="blocked",kind="TerraformCluster",op="apply",step="none"} 1
captf_job_errors_total{error_kind="deadline",kind="TerraformMachine",op="drift",step="none"} 1
captf_job_errors_total{error_kind="interrupted",kind="TerraformMachine",op="refresh",step="apply-refresh-only"} 1
captf_job_errors_total{error_kind="unknown",kind="TerraformMachine",op="apply",step="other"} 1
captf_job_errors_total{error_kind="unknown",kind="TerraformMachine",op="destroy",step="none"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), metrics.JobsTotalName, metrics.JobErrorsName); err != nil {
		t.Error(err)
	}
	for name, n := range map[string]int{
		metrics.JobAttemptsName: 1,
		// Every Job has a duration, by its result.
		metrics.JobDurationName: 6,
		// init and apply-refresh-only of the refresh, other of the odd one.
		metrics.JobStepDurationName: 3,
		// No pod reported a start.
		metrics.JobQueueName: 0,
	} {
		if got, err := gatherAndCount(reg, name); err != nil || got != n {
			t.Errorf("%s series = %d (%v), want %d", name, got, err, n)
		}
	}
}

// TestRecordFinishedResult: the queue time, steps, resources changed and
// drift found of a Job's result.
func TestRecordFinishedResult(t *testing.T) {
	t.Parallel()
	r, reg := recorder(t)
	d := Deps{Metrics: r}
	applied := done("a", jobs.OpApply, true, t0, &runner.Result{
		Steps:   []runner.Step{{Name: runner.StepInit, Seconds: 5}, {Name: runner.StepApply, Seconds: 90}},
		Changes: &runner.Changes{Add: 3, Change: 1, Import: 2},
	})
	applied.pod = podWith("{}", "")
	applied.pod.Status.ContainerStatuses[0].State.Terminated.StartedAt = metav1.NewTime(applied.job.CreationTimestamp.Add(400 * time.Second))
	destroyed := done("x", jobs.OpDestroy, true, t0, &runner.Result{Changes: &runner.Changes{Destroy: 4}})
	drifted := done("d", jobs.OpDrift, true, t0, driftRun(&runner.Drift{Detected: true, Add: 1, Destroy: 2}))
	clean := done("c", jobs.OpDrift, true, t0, driftRun(&runner.Drift{}))
	for _, f := range []finished{applied, destroyed, drifted, clean} {
		recordFinished(d, "TerraformCluster", f, 1)
	}
	want := `
# HELP captf_resources_changed_total [ALPHA] Resources an apply or destroy Job changed, by action (add, change, destroy, import), from the runtime's final summary line.
# TYPE captf_resources_changed_total counter
captf_resources_changed_total{action="add",kind="TerraformCluster",op="apply"} 3
captf_resources_changed_total{action="change",kind="TerraformCluster",op="apply"} 1
captf_resources_changed_total{action="destroy",kind="TerraformCluster",op="destroy"} 4
captf_resources_changed_total{action="import",kind="TerraformCluster",op="apply"} 2
# HELP captf_drift_resources_total [ALPHA] Resources a drift Job that detected drift found to add, change or destroy.
# TYPE captf_drift_resources_total counter
captf_drift_resources_total{action="add",kind="TerraformCluster"} 1
captf_drift_resources_total{action="destroy",kind="TerraformCluster"} 2
# HELP captf_job_queue_seconds [ALPHA] Time from a Job's creation to its source container's start: scheduling, image pulls and the runner copy. Not observed when the pod reports no start.
# TYPE captf_job_queue_seconds histogram
captf_job_queue_seconds_bucket{kind="TerraformCluster",op="apply",le="5"} 0
captf_job_queue_seconds_bucket{kind="TerraformCluster",op="apply",le="10"} 0
captf_job_queue_seconds_bucket{kind="TerraformCluster",op="apply",le="20"} 0
captf_job_queue_seconds_bucket{kind="TerraformCluster",op="apply",le="30"} 0
captf_job_queue_seconds_bucket{kind="TerraformCluster",op="apply",le="60"} 0
captf_job_queue_seconds_bucket{kind="TerraformCluster",op="apply",le="120"} 0
captf_job_queue_seconds_bucket{kind="TerraformCluster",op="apply",le="180"} 0
captf_job_queue_seconds_bucket{kind="TerraformCluster",op="apply",le="300"} 0
captf_job_queue_seconds_bucket{kind="TerraformCluster",op="apply",le="600"} 1
captf_job_queue_seconds_bucket{kind="TerraformCluster",op="apply",le="900"} 1
captf_job_queue_seconds_bucket{kind="TerraformCluster",op="apply",le="1800"} 1
captf_job_queue_seconds_bucket{kind="TerraformCluster",op="apply",le="+Inf"} 1
captf_job_queue_seconds_sum{kind="TerraformCluster",op="apply"} 400
captf_job_queue_seconds_count{kind="TerraformCluster",op="apply"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), metrics.ResourcesChangedName, metrics.DriftResourcesName, metrics.JobQueueName); err != nil {
		t.Error(err)
	}
	if n, err := gatherAndCount(reg, metrics.JobStepDurationName); err != nil || n != 2 {
		t.Errorf("step series = %d (%v)", n, err)
	}
}

// TestJobMetricsCountedOnce: a finished Job's per-Job series are recorded
// when bookkeeping first reads it; once it is bookkept, later reconciles
// neither read its pod nor count it again. The per-object gauges are set
// along the way.
func TestJobMetricsCountedOnce(t *testing.T) {
	t.Parallel()
	r, reg := recorder(t)
	runSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: inputs.RunName("a")}}
	e := newEnv(t, world(machine(withFinalizer, notPaused, provisioned), runSecret)...)
	e.d.Metrics = r
	applied := job("a", jobs.OpApply, jobs.Succeeded, t0.Add(-time.Hour))
	applied.Annotations = map[string]string{state.InputsHashAnnotation: "h1:x"}
	if err := e.c.Create(t.Context(), applied.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	e.runner.jobs = append(e.runner.jobs, applied)
	res, err := json.Marshal(runner.Result{Version: runner.ResultVersion, Op: "apply",
		Steps: []runner.Step{{Name: runner.StepInit, Seconds: 3}, {Name: runner.StepApply, Seconds: 40}}, Changes: &runner.Changes{Add: 2}})
	if err != nil {
		t.Fatal(err)
	}
	pod := podWith(string(res), "")
	pod.Status.ContainerStatuses[0].State.Terminated.StartedAt = metav1.NewTime(applied.CreationTimestamp.Add(20 * time.Second))
	e.runner.pods["a"] = []corev1.Pod{*pod}
	e.state.st = &state.State{InputsHash: "h1:x", ManagedResources: 3, Bytes: 4096}
	k := e.kindFor(t, readyOwner)
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	if err := inputs.Write(t.Context(), e.c, k.obj, renderMachine(t), inputs.Meta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
		t.Fatal(err)
	}

	for pass := range 2 {
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		stored := &batchv1.Job{}
		if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "a"}, stored); err != nil {
			t.Fatal(err)
		}
		if stored.Annotations[BookkeptAnnotation] != "true" {
			t.Fatalf("pass %d: Job not bookkept", pass)
		}
		e.jobNamed(t, "a").Annotations = stored.Annotations
		k = e.kindFor(t, readyOwner)
		k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}

		want := `
# HELP captf_jobs_total [ALPHA] Jobs completed, by result: succeeded, failed, deadline, interrupted (stopped from outside: a drain, eviction or deletion), blocked (a guarded apply, of a TerraformCluster or of a TerraformMachinePool's change of the cluster's exports, stopped before a plan that deletes or replaces resources, awaiting approval) or plan_changed (an apply approved for one plan planned other changes and stopped, applyPolicy Manual). Op plan is a plan Job that applies nothing.
# TYPE captf_jobs_total counter
captf_jobs_total{kind="TerraformMachine",op="apply",result="succeeded"} 1
# HELP captf_resources_changed_total [ALPHA] Resources an apply or destroy Job changed, by action (add, change, destroy, import), from the runtime's final summary line.
# TYPE captf_resources_changed_total counter
captf_resources_changed_total{action="add",kind="TerraformMachine",op="apply"} 2
`
		if err := testutil.GatherAndCompare(reg, strings.NewReader(want), metrics.JobsTotalName, metrics.ResourcesChangedName); err != nil {
			t.Errorf("pass %d: %v", pass, err)
		}
		for name, n := range map[string]int{metrics.JobStepDurationName: 2, metrics.JobQueueName: 1, metrics.JobDurationName: 1} {
			if got, err := gatherAndCount(reg, name); err != nil || got != n {
				t.Errorf("pass %d: %s series = %d (%v), want %d", pass, name, got, err, n)
			}
		}
	}
	if pods := e.runner.podLists; len(pods) != 1 {
		t.Errorf("pods listed for %v, want once", pods)
	}

	want := `
# HELP captf_infrastructure_healthy [ALPHA] 1, 0 or -1 for a True, False or Unknown InfrastructureHealthy condition.
# TYPE captf_infrastructure_healthy gauge
captf_infrastructure_healthy{kind="TerraformMachine",name="m1",namespace="team-a"} 1
# HELP captf_inputs_bytes [ALPHA] Size of the object's rendered main.tf.json and terraform.tfvars.json (the durable inputs, or the last render); no Job starts above 1000000.
# TYPE captf_inputs_bytes gauge
captf_inputs_bytes{kind="TerraformMachine",name="m1",namespace="team-a"} ` + strconv.Itoa(renderMachine(t).Size()) + `
# HELP captf_last_success_timestamp_seconds [ALPHA] Unix time the newest successful Job of an op finished. Drift and refresh are exported only while that op is scheduled (not deleting or paused, with a drift interval or health checks).
# TYPE captf_last_success_timestamp_seconds gauge
captf_last_success_timestamp_seconds{kind="TerraformMachine",name="m1",namespace="team-a",op="apply"} ` + strconv.FormatInt(t0.Add(-time.Hour).Unix(), 10) + `
# HELP captf_state_bytes [ALPHA] Compressed size of the object's state summed over its Secrets, as last read; the kubernetes backend holds at most 1 MiB per Secret.
# TYPE captf_state_bytes gauge
captf_state_bytes{kind="TerraformMachine",name="m1",namespace="team-a"} 4096
# HELP captf_state_resources [ALPHA] Managed resources (not data sources) in the object's state, as last read.
# TYPE captf_state_resources gauge
captf_state_resources{kind="TerraformMachine",name="m1",namespace="team-a"} 3
# HELP captf_unhealthy_samples [ALPHA] A TerraformMachine's consecutive unhealthy health samples (status.unhealthySamples).
# TYPE captf_unhealthy_samples gauge
captf_unhealthy_samples{name="m1",namespace="team-a"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), metrics.InfraHealthyName, metrics.InputsBytesName,
		metrics.LastSuccessName, metrics.StateBytesName, metrics.StateResourcesName, metrics.UnhealthySamplesName); err != nil {
		t.Error(err)
	}
}

// TestObjectGaugesDeletedWithObject: dropping the finalizer removes every
// per-object gauge of the object.
func TestObjectGaugesDeletedWithObject(t *testing.T) {
	t.Parallel()
	r, reg := recorder(t)
	e := newEnv(t, world(machine(deleting, notPaused))...)
	e.d.Metrics = r
	r.SetState(state.KindTerraformMachine, testNS, testName, 1, 10)
	r.SetInputsBytes(state.KindTerraformMachine, testNS, testName, 10)
	r.SetLastSuccess(state.KindTerraformMachine, testNS, testName, "drift", t0)
	r.SetUnhealthySamples(testNS, testName, 1)
	r.SetObject(state.KindTerraformMachine, testNS, testName, metrics.Object{})
	e.state.st = &state.State{InputsHash: "h1:x"}
	e.runner.jobs = append(e.runner.jobs, job("d", jobs.OpDestroy, jobs.Succeeded, t0))
	if _, err := reconcileOnce(t, e, e.kindFor(t, readyOwner)); err != nil {
		t.Fatal(err)
	}
	if e.get(t) != nil {
		t.Fatal("finalizer not dropped")
	}
	for _, name := range metrics.PerObject {
		if n, err := gatherAndCount(reg, name); err != nil || n != 0 {
			t.Errorf("%s: %d series (%v) after the object went", name, n, err)
		}
	}
}

// TestLastSuccessScheduled: drift and refresh report their last success
// only while they are scheduled, so CAPTFNoRecentSuccess does not fire for
// a paused object or one without drift checks.
func TestLastSuccessScheduled(t *testing.T) {
	t.Parallel()
	check := t0.Add(-2 * time.Hour)
	checked := func(m *infrav1.TerraformMachine) { m.Status.LastDriftCheck = &metav1.Time{Time: check} }
	for _, tt := range []struct {
		name   string
		paused bool
		off    bool
		want   int
	}{
		{name: "scheduled", want: 1},
		{name: "paused", paused: true},
		{name: "drift off", off: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, reg := recorder(t)
			noDrift := func(m *infrav1.TerraformMachine) {
				if tt.off {
					m.Spec.Drift = &infrav1.MachineDriftPolicy{IntervalSeconds: new(int32(0))}
				}
			}
			e := newEnv(t, world(machine(withFinalizer, notPaused, provisioned, checked, noDrift))...)
			e.d.Metrics = r
			// A value from before: a paused reconcile removes it.
			r.SetLastSuccess(state.KindTerraformMachine, testNS, testName, "drift", check)
			e.state.st = &state.State{InputsHash: "h1:x"}
			owner := readyOwner
			if tt.paused {
				owner = OwnerInfo{HasOwnerRef: true, Cluster: cluster(true)}
			}
			k := e.kindFor(t, owner)
			k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
			if _, err := reconcileOnce(t, e, k); err != nil {
				t.Fatal(err)
			}
			if n, err := gatherAndCount(reg, metrics.LastSuccessName); err != nil || n != tt.want {
				t.Errorf("last-success series = %d (%v), want %d", n, err, tt.want)
			}
			// With no drift Job retained, the status stamp is the value.
			if got := gaugeValues(t, reg, metrics.LastSuccessName); tt.want == 1 && got[0] != float64(check.Unix()) {
				t.Errorf("drift last success = %v, want the drift stamp %d", got, check.Unix())
			}
		})
	}
}

// gaugeValues returns the values of the series of the gauge name in reg,
// failing t on error.
func gaugeValues(t *testing.T, reg cbmetrics.KubeRegistry, name string) []float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var out []float64
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			out = append(out, m.GetGauge().GetValue())
		}
	}
	return out
}

// TestInputsBytesTooLarge: inputs too large to run still report their size.
func TestInputsBytesTooLarge(t *testing.T) {
	t.Parallel()
	r, reg := recorder(t)
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	e.d.Metrics = r
	k := e.kindFor(t, readyOwner)
	big := machineIn()
	big.BootstrapData = base64.StdEncoding.EncodeToString(make([]byte, 800_000))
	k.in = big
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	if c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition); c == nil || c.Reason != infrav1.InputsTooLargeReason {
		t.Fatalf("ApplyJobSucceeded = %+v", c)
	}
	if got := gaugeValues(t, reg, metrics.InputsBytesName); len(got) != 1 || got[0] <= 1_000_000 {
		t.Errorf("inputs bytes = %v, want the too-large size", got)
	}
}

// TestApprovalConsumedMetric: the consumed approval is counted.
func TestApprovalConsumedMetric(t *testing.T) {
	t.Parallel()
	r, reg := recorder(t)
	approved := func(m *infrav1.TerraformMachine) {
		metav1.SetMetaDataAnnotation(&m.ObjectMeta, infrav1.ApproveDestructivePlanAnnotation, "h2:approved")
	}
	e := newEnv(t, world(machine(withFinalizer, notPaused, provisioned, approved))...)
	e.d.Metrics = r
	e.state.st = &state.State{InputsHash: "h2:approved"}
	ok := job("a", jobs.OpApply, jobs.Succeeded, t0.Add(-time.Hour))
	ok.Annotations = map[string]string{state.InputsHashAnnotation: "h2:approved", BookkeptAnnotation: "true"}
	e.runner.jobs = append(e.runner.jobs, ok)
	k := e.kindFor(t, readyOwner)
	k.asCluster, k.mutable, k.in = true, true, machineIn()
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	k.clusterDrift = &infrav1.DriftPolicy{IntervalSeconds: new(int32(0))}
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	want := `
# HELP captf_destructive_plan_approvals_consumed_total [ALPHA] Destructive-plan approvals removed after the approved apply succeeded.
# TYPE captf_destructive_plan_approvals_consumed_total counter
captf_destructive_plan_approvals_consumed_total{kind="TerraformCluster"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), metrics.ApprovalsConsumedName); err != nil {
		t.Error(err)
	}
}

// TestRecordTransitionAndObject proves recordTransition counts identity
// refusals and state-read errors by reason, and recordObject sets the Ready
// and InfrastructureHealthy gauges from an object's conditions.
func TestRecordTransitionAndObject(t *testing.T) {
	t.Parallel()
	r, reg := recorder(t)
	d := Deps{Metrics: r}
	for _, c := range []metav1.Condition{
		{Type: infrav1.StateReadableCondition, Reason: infrav1.StateEncryptedReason},
		{Type: infrav1.OutputsValidCondition, Reason: infrav1.OutputsInvalidReason},
		{Type: infrav1.IdentityAllowedCondition, Reason: infrav1.IdentityNotFoundReason},
		{Type: infrav1.IdentityAllowedCondition, Reason: infrav1.NamespaceNotAllowedReason},
	} {
		recordTransition(d, "TerraformCluster", &c)
	}
	want := `
# HELP captf_identity_denied_total [ALPHA] Identity refusals: notfound or namespace.
# TYPE captf_identity_denied_total counter
captf_identity_denied_total{reason="namespace"} 1
captf_identity_denied_total{reason="notfound"} 1
# HELP captf_state_read_errors_total [ALPHA] State reads that turned unreadable: inconsistent, encrypted or corrupt (an unsupported state version counts as corrupt), lost (a provisioned object's state is gone) or locked (held by a holder that is not this object's runner).
# TYPE captf_state_read_errors_total counter
captf_state_read_errors_total{kind="TerraformCluster",reason="encrypted"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), metrics.IdentityDeniedName, metrics.StateReadErrorsName); err != nil {
		t.Error(err)
	}

	m := machine()
	recordObject(d, "TerraformMachine", m) // no Ready yet: Unknown
	conditions.Set(m, metav1.Condition{Type: infrav1.ReadyCondition, Status: metav1.ConditionFalse, Reason: "NotReady"})
	conditions.Set(m, metav1.Condition{Type: infrav1.DriftDetectedCondition, Status: metav1.ConditionTrue, Reason: infrav1.DriftReportedReason})
	m2 := machine(func(m *infrav1.TerraformMachine) { m.Name = "other" })
	conditions.Set(m2, metav1.Condition{Type: infrav1.ReadyCondition, Status: metav1.ConditionTrue, Reason: "Ready"})
	conditions.Set(m2, metav1.Condition{Type: infrav1.InfrastructureHealthyCondition, Status: metav1.ConditionFalse, Reason: infrav1.InstanceUnhealthyReason})
	conditions.Set(m, metav1.Condition{Type: infrav1.InfrastructureHealthyCondition, Status: metav1.ConditionTrue, Reason: infrav1.HealthyReason})
	recordObject(d, "TerraformMachine", m)
	recordObject(d, "TerraformMachine", m2)
	want = `
# HELP captf_ready [ALPHA] 1, 0 or -1 for a True, False or Unknown Ready condition.
# TYPE captf_ready gauge
captf_ready{kind="TerraformMachine",name="` + m.Name + `",namespace="` + m.Namespace + `"} 0
captf_ready{kind="TerraformMachine",name="other",namespace="` + m.Namespace + `"} 1
# HELP captf_infrastructure_healthy [ALPHA] 1, 0 or -1 for a True, False or Unknown InfrastructureHealthy condition.
# TYPE captf_infrastructure_healthy gauge
captf_infrastructure_healthy{kind="TerraformMachine",name="` + m.Name + `",namespace="` + m.Namespace + `"} 1
captf_infrastructure_healthy{kind="TerraformMachine",name="other",namespace="` + m.Namespace + `"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), metrics.ReadyName, metrics.InfraHealthyName); err != nil {
		t.Error(err)
	}
}

// TestDecisionOp proves decisionOp names a Decision's op label (the Job's
// op, "finalizer" for dropping the finalizer, or "none" otherwise), and
// stateReason maps a state-unreadable condition reason to its metric
// reason label.
func TestDecisionOp(t *testing.T) {
	t.Parallel()
	for dec, want := range map[Decision]string{
		{Action: ActionJob, Op: jobs.OpDrift}: "drift",
		{Action: ActionDropFinalizer}:         "finalizer",
		{RequeueAfter: time.Minute}:           "none",
	} {
		if got := decisionOp(dec); got != want {
			t.Errorf("decisionOp(%+v) = %s, want %s", dec, got, want)
		}
	}
	for reason, want := range map[string]string{
		infrav1.StateInconsistentReason: "inconsistent", infrav1.StateEncryptedReason: "encrypted", infrav1.StateCorruptReason: "corrupt",
	} {
		if got := stateReason(reason); got != want {
			t.Errorf("stateReason(%s) = %s", reason, got)
		}
	}
}
