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

package metrics

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apimachineryversion "k8s.io/apimachinery/pkg/version"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	cbmetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/testutil"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

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

// TestRegister proves Register registers captf_build_info set to 1 with
// info's labels, and that registering twice on the same registry fails.
func TestRegister(t *testing.T) {
	t.Parallel()
	reg := cbmetrics.NewKubeRegistry()
	info := apimachineryversion.Info{GitVersion: "v0.1.0", GitCommit: "abc123"}
	if err := Register(reg, info); err != nil {
		t.Fatalf("Register: %v", err)
	}
	want := `
# HELP captf_build_info [ALPHA] A metric with a constant '1' value labeled by the version, commit and module contract of the manager.
# TYPE captf_build_info gauge
captf_build_info{commit="abc123",contract="v1alpha1",version="v0.1.0"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), BuildInfoName); err != nil {
		t.Error(err)
	}
	// A second registration is a duplicate and must be reported.
	if err := Register(reg, info); err == nil {
		t.Error("second Register succeeded, want a duplicate-registration error")
	}
}

// TestRecorderSeries: every series of the table is registered with the
// table's name, type and labels, and registering twice fails.
func TestRecorderSeries(t *testing.T) {
	t.Parallel()
	reg := cbmetrics.NewKubeRegistry()
	r := New()
	if err := r.Register(reg); err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, apimachineryversion.Info{GitVersion: "v", GitCommit: "c"}); err != nil {
		t.Fatal(err)
	}
	// A wholly independent Recorder registers cleanly on its own fresh
	// registry: nothing about r leaked into the package.
	if err := New().Register(cbmetrics.NewKubeRegistry()); err != nil {
		t.Fatalf("a fresh registry: %v", err)
	}
	if err := r.Register(reg); err == nil {
		t.Error("second Register succeeded")
	}

	// One child per series, so each one exports.
	r.JobFinished(Job{Kind: "TerraformCluster", Op: "apply", Result: ResultSucceeded, Duration: time.Minute, Queue: time.Second, Attempt: 2,
		Steps: []Step{{Name: "init", Seconds: 3}}, Changes: &Changes{Create: 1}})
	r.JobFinished(Job{Kind: "TerraformCluster", Op: "drift", Result: ResultFailed, ErrorKind: "step", ErrorStep: "plan", Drift: &Changes{Update: 1}})
	r.Decision("TerraformCluster", "apply", "NoState")
	r.StateReadError("TerraformCluster", "encrypted")
	r.OutputsInvalid("TerraformMachine", "OutputsInvalid")
	r.SetObject("TerraformCluster", "ns", "c1", Object{Ready: 1, Healthy: 1, Drift: true})
	r.SetState("TerraformCluster", "ns", "c1", 3, 1024)
	r.SetInputsBytes("TerraformCluster", "ns", "c1", 2048)
	r.SetLastSuccess("TerraformCluster", "ns", "c1", "apply", time.Unix(1, 0))
	r.SetUnhealthySamples("ns", "m1", 2)
	r.ForceUnlock("TerraformMachine")
	r.IdentityDenied("namespace")
	r.ImageInspectError("ImageInspectFailed")
	r.InputsHashChanged("TerraformCluster")
	r.RemediationRequest(RemediationRequested)
	r.ApprovalConsumed("TerraformCluster")
	r.LeaseWait("TerraformMachine", LeaseWaitClusterOperation)
	r.StateBackup("TerraformCluster", BackupTaken, 1)
	r.StateRestore("TerraformMachine", ResultSucceeded)
	r.PlanApproval("TerraformCluster", PlanApproved)
	r.ActiveJobs().Bind(fakeJobs(t, runningJob("j1", "apply")))

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	types := map[string]string{}
	for _, f := range families {
		types[f.GetName()] = strings.ToLower(f.GetType().String())
		for _, l := range f.GetMetric()[0].GetLabel() {
			got[f.GetName()] = append(got[f.GetName()], l.GetName())
		}
	}
	for _, s := range Specs() {
		want := slices.Sorted(slices.Values(s.Labels))
		if !slices.Equal(got[s.Name], want) || types[s.Name] != s.Type {
			t.Errorf("%s: type %s labels %v, want %s %v", s.Name, types[s.Name], got[s.Name], s.Type, want)
		}
	}
	if len(families) != len(Specs()) {
		t.Errorf("%d families gathered, want %d", len(families), len(Specs()))
	}
}

// TestCardinality: only the per-object gauges carry namespace or name, and
// they are gauges.
func TestCardinality(t *testing.T) {
	t.Parallel()
	for _, s := range Specs() {
		perObject := slices.Contains(s.Labels, "namespace") || slices.Contains(s.Labels, "name")
		if perObject != slices.Contains(PerObject, s.Name) || (perObject && s.Type != "gauge") {
			t.Errorf("%s (%s) has labels %v", s.Name, s.Type, s.Labels)
		}
	}
}

// TestJobFinished proves JobFinished records job errors only for a
// non-succeeded result, resource and drift counters only for positive
// actions, and skips duration, queue and step observations for unknown
// (non-positive) times.
func TestJobFinished(t *testing.T) {
	t.Parallel()
	reg := cbmetrics.NewKubeRegistry()
	r := New()
	if err := r.Register(reg); err != nil {
		t.Fatal(err)
	}
	r.JobFinished(Job{Kind: "TerraformMachine", Op: "apply", Result: ResultFailed, Duration: time.Minute, Attempt: 1,
		Steps: []Step{{Name: "init", Seconds: 4}, {Name: "apply", Seconds: 50}}, ErrorKind: "step", ErrorStep: "apply"})
	r.JobFinished(Job{Kind: "TerraformMachine", Op: "apply", Result: ResultSucceeded, Attempt: 2, Queue: 40 * time.Second,
		Steps:   []Step{{Name: "init", Seconds: 3}, {Name: "apply", Seconds: -1}},
		Changes: &Changes{Create: 2, Delete: 1}})
	// No result: unknown error kind, no step.
	r.JobFinished(Job{Kind: "TerraformCluster", Op: "drift", Result: ResultDeadline, Duration: time.Hour})
	r.JobFinished(Job{Kind: "TerraformCluster", Op: "drift", Result: ResultSucceeded, Drift: &Changes{Update: 3, Delete: 1}})
	if n, err := testutil.GetCounterMetricValue(r.jobsTotal.WithLabelValues("TerraformMachine", "apply", ResultFailed)); err != nil || n != 1 {
		t.Errorf("failed = %v (%v)", n, err)
	}
	want := `
# HELP captf_job_errors_total [ALPHA] Jobs that did not succeed, by the runner's error kind (step, image-layout, interrupted, blocked, plan-changed; deadline or unknown without a result) and failing step (none when no step failed).
# TYPE captf_job_errors_total counter
captf_job_errors_total{error_kind="step",kind="TerraformMachine",op="apply",step="apply"} 1
captf_job_errors_total{error_kind="unknown",kind="TerraformCluster",op="drift",step="none"} 1
# HELP captf_resources_changed_total [ALPHA] Resources an apply or destroy Job changed, by action (create, update, delete, import), from the runtime's final summary line.
# TYPE captf_resources_changed_total counter
captf_resources_changed_total{action="create",kind="TerraformMachine",op="apply"} 2
captf_resources_changed_total{action="delete",kind="TerraformMachine",op="apply"} 1
# HELP captf_drift_resources_total [ALPHA] Resources a drift Job that detected drift found to create, update, replace or delete.
# TYPE captf_drift_resources_total counter
captf_drift_resources_total{action="update",kind="TerraformCluster"} 3
captf_drift_resources_total{action="delete",kind="TerraformCluster"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), JobErrorsName, ResourcesChangedName, DriftResourcesName); err != nil {
		t.Error(err)
	}
	// A failure records no attempt; an unknown duration or queue time
	// nothing; a negative step time nothing.
	for name, want := range map[string]int{
		JobAttemptsName: 1, JobDurationName: 2, JobQueueName: 1, JobStepDurationName: 2,
	} {
		if n, err := gatherAndCount(reg, name); err != nil || n != want {
			t.Errorf("%s: %d series (%v), want %d", name, n, err, want)
		}
	}
	// Duration is by result.
	if count, err := testutil.GetHistogramMetricCount(r.jobDuration.WithLabelValues("TerraformCluster", "drift", ResultDeadline)); err != nil || count != 1 {
		t.Errorf("deadline duration count = %v (%v)", count, err)
	}
	if sum, err := testutil.GetHistogramMetricValue(r.jobDuration.WithLabelValues("TerraformCluster", "drift", ResultDeadline)); err != nil || sum != 3600 {
		t.Errorf("deadline duration sum = %v (%v)", sum, err)
	}
	if count, err := testutil.GetHistogramMetricCount(r.stepDuration.WithLabelValues("TerraformMachine", "apply", "init")); err != nil || count != 2 {
		t.Errorf("init steps count = %v (%v)", count, err)
	}
	if sum, err := testutil.GetHistogramMetricValue(r.stepDuration.WithLabelValues("TerraformMachine", "apply", "init")); err != nil || sum != 7 {
		t.Errorf("init steps sum = %v (%v)", sum, err)
	}
}

// TestObjectGauges: SetObject, SetState, SetInputsBytes, SetLastSuccess and
// SetUnhealthySamples export their values.
func TestObjectGauges(t *testing.T) {
	t.Parallel()
	reg := cbmetrics.NewKubeRegistry()
	r := New()
	if err := r.Register(reg); err != nil {
		t.Fatal(err)
	}
	r.SetObject("TerraformMachine", "ns", "m1", Object{Ready: 1, Healthy: -1})
	r.SetState("TerraformMachine", "ns", "m1", 4, 921601)
	r.SetInputsBytes("TerraformMachine", "ns", "m1", 1234)
	r.SetLastSuccess("TerraformMachine", "ns", "m1", "apply", time.Unix(1700000000, 5e8))
	r.SetLastSuccess("TerraformMachine", "ns", "m1", "drift", time.Unix(1700000100, 0))
	r.DeleteLastSuccess("TerraformMachine", "ns", "m1", "drift")
	r.SetUnhealthySamples("ns", "m1", 2)
	want := `
# HELP captf_infrastructure_healthy [ALPHA] 1, 0 or -1 for a True, False or Unknown InfrastructureHealthy condition.
# TYPE captf_infrastructure_healthy gauge
captf_infrastructure_healthy{kind="TerraformMachine",name="m1",namespace="ns"} -1
# HELP captf_inputs_bytes [ALPHA] Size of the object's rendered main.tf.json and terraform.tfvars.json (the durable inputs, or the last render); no Job starts above 1000000.
# TYPE captf_inputs_bytes gauge
captf_inputs_bytes{kind="TerraformMachine",name="m1",namespace="ns"} 1234
# HELP captf_last_success_timestamp_seconds [ALPHA] Unix time the newest successful Job of an op finished. Drift and refresh are exported only while that op is scheduled (not deleting or paused, with a drift interval or health checks).
# TYPE captf_last_success_timestamp_seconds gauge
captf_last_success_timestamp_seconds{kind="TerraformMachine",name="m1",namespace="ns",op="apply"} 1.7e+09
# HELP captf_state_bytes [ALPHA] Compressed size of the object's state summed over its Secrets, as last read; the kubernetes backend holds at most 1 MiB per Secret.
# TYPE captf_state_bytes gauge
captf_state_bytes{kind="TerraformMachine",name="m1",namespace="ns"} 921601
# HELP captf_state_resources [ALPHA] Managed resources (not data sources) in the object's state, as last read.
# TYPE captf_state_resources gauge
captf_state_resources{kind="TerraformMachine",name="m1",namespace="ns"} 4
# HELP captf_unhealthy_samples [ALPHA] A TerraformMachine's consecutive unhealthy health samples (status.unhealthySamples).
# TYPE captf_unhealthy_samples gauge
captf_unhealthy_samples{name="m1",namespace="ns"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), InfraHealthyName, InputsBytesName, LastSuccessName,
		StateBytesName, StateResourcesName, UnhealthySamplesName); err != nil {
		t.Error(err)
	}
}

// TestDeleteObject: every per-object gauge of the object goes, and nothing
// of another object, including a TerraformCluster's delete sparing a
// TerraformMachine's unhealthy samples of the same name.
func TestDeleteObject(t *testing.T) {
	t.Parallel()
	reg := cbmetrics.NewKubeRegistry()
	r := New()
	if err := r.Register(reg); err != nil {
		t.Fatal(err)
	}
	set := func(kind, name string) {
		r.SetObject(kind, "ns", name, Object{Ready: -1, Healthy: 1, Drift: name == "keep"})
		r.SetState(kind, "ns", name, 1, 100)
		r.SetInputsBytes(kind, "ns", name, 10)
		for _, op := range []string{"apply", "destroy", "refresh", "drift"} {
			r.SetLastSuccess(kind, "ns", name, op, time.Unix(1, 0))
		}
	}
	set("TerraformCluster", "gone")
	set("TerraformMachine", "gone")
	set("TerraformMachine", "keep")
	r.SetUnhealthySamples("ns", "gone", 1)
	r.SetUnhealthySamples("ns", "keep", 1)

	r.DeleteObject("TerraformCluster", "ns", "gone")
	for _, name := range PerObject {
		want := map[string]int{LastSuccessName: 8, UnhealthySamplesName: 2}[name]
		if want == 0 {
			want = 2
		}
		if n, err := gatherAndCount(reg, name); err != nil || n != want {
			t.Errorf("after the cluster's delete %s: %d series (%v), want %d", name, n, err, want)
		}
	}
	r.DeleteObject("TerraformMachine", "ns", "gone")
	for _, name := range PerObject {
		want := map[string]int{LastSuccessName: 4}[name]
		if want == 0 {
			want = 1
		}
		if n, err := gatherAndCount(reg, name); err != nil || n != want {
			t.Errorf("after the machine's delete %s: %d series (%v), want %d", name, n, err, want)
		}
	}
	if v, err := testutil.GetGaugeMetricValue(r.driftDetected.WithLabelValues("TerraformMachine", "ns", "keep")); err != nil || v != 1 {
		t.Errorf("drift = %v (%v)", v, err)
	}
}

// TestCountersByKindAndAction proves RemediationRequest, ApprovalConsumed
// and LeaseWait count by their bounded label values and drop any other
// value.
func TestCountersByKindAndAction(t *testing.T) {
	t.Parallel()
	reg := cbmetrics.NewKubeRegistry()
	r := New()
	if err := r.Register(reg); err != nil {
		t.Fatal(err)
	}
	r.RemediationRequest(RemediationRequested)
	r.RemediationRequest(RemediationRequested)
	r.RemediationRequest(RemediationWithdrawn)
	r.RemediationRequest("bogus") // not a label value
	r.ApprovalConsumed("TerraformCluster")
	r.LeaseWait("TerraformCluster", LeaseWaitMachineOperations)
	r.LeaseWait("TerraformMachine", LeaseWaitRunLease)
	r.LeaseWait("TerraformMachine", LeaseWaitRunLease)
	r.LeaseWait("TerraformMachine", "bogus") // not a label value
	want := `
# HELP captf_destructive_plan_approvals_consumed_total [ALPHA] Destructive-plan approvals removed after the approved apply succeeded.
# TYPE captf_destructive_plan_approvals_consumed_total counter
captf_destructive_plan_approvals_consumed_total{kind="TerraformCluster"} 1
# HELP captf_lease_waits_total [ALPHA] ` + spec(LeaseWaitsName).Help + `
# TYPE captf_lease_waits_total counter
captf_lease_waits_total{kind="TerraformCluster",reason="machine_operations"} 1
captf_lease_waits_total{kind="TerraformMachine",reason="run_lease"} 2
# HELP captf_remediation_requests_total [ALPHA] cluster.x-k8s.io/remediate-machine annotations set on (requested) or removed from (withdrawn) a Machine.
# TYPE captf_remediation_requests_total counter
captf_remediation_requests_total{action="requested"} 2
captf_remediation_requests_total{action="withdrawn"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), RemediationRequestsName, ApprovalsConsumedName, LeaseWaitsName); err != nil {
		t.Error(err)
	}
}

// TestNilRecorder: code under test runs with no Recorder.
func TestNilRecorder(t *testing.T) {
	t.Parallel()
	var r *Recorder
	r.JobFinished(Job{Kind: "k", Op: "apply", Result: ResultFailed, Duration: time.Second, Changes: &Changes{Create: 1}, Drift: &Changes{}})
	r.Decision("k", "none", "UpToDate")
	r.StateReadError("k", "corrupt")
	r.OutputsInvalid("k", "x")
	r.SetObject("k", "ns", "n", Object{Ready: 1})
	r.SetState("k", "ns", "n", 1, 1)
	r.SetInputsBytes("k", "ns", "n", 1)
	r.SetLastSuccess("k", "ns", "n", "apply", time.Now())
	r.DeleteLastSuccess("k", "ns", "n", "apply")
	r.SetUnhealthySamples("ns", "n", 1)
	r.DeleteObject("k", "ns", "n")
	r.ForceUnlock("k")
	r.IdentityDenied("notfound")
	r.ImageInspectError("x")
	r.InputsHashChanged("k")
	r.RemediationRequest(RemediationRequested)
	r.ApprovalConsumed("k")
	r.LeaseWait("k", LeaseWaitRunLease)
	r.StateBackup("k", BackupTaken, 1)
	r.StateRestore("k", ResultFailed)
	r.PlanApproval("k", PlanChanged)
	r.ActiveJobs().Bind(nil)
}

// TestUnregisteredRecorder: every method of a Recorder that was built but
// never registered is a safe no-op, since component-base's lazily
// instantiated series drop writes until Create (via Register) runs.
func TestUnregisteredRecorder(t *testing.T) {
	t.Parallel()
	r := New()
	r.JobFinished(Job{Kind: "k", Op: "apply", Result: ResultFailed, Duration: time.Second, Changes: &Changes{Create: 1}, Drift: &Changes{}})
	r.Decision("k", "none", "UpToDate")
	r.StateReadError("k", "corrupt")
	r.OutputsInvalid("k", "x")
	r.SetObject("k", "ns", "n", Object{Ready: 1})
	r.SetState("k", "ns", "n", 1, 1)
	r.SetInputsBytes("k", "ns", "n", 1)
	r.SetLastSuccess("k", "ns", "n", "apply", time.Now())
	r.DeleteLastSuccess("k", "ns", "n", "apply")
	r.SetUnhealthySamples("ns", "n", 1)
	r.DeleteObject("k", "ns", "n")
	r.ForceUnlock("k")
	r.IdentityDenied("notfound")
	r.ImageInspectError("x")
	r.InputsHashChanged("k")
	r.RemediationRequest(RemediationRequested)
	r.ApprovalConsumed("k")
	r.LeaseWait("k", LeaseWaitRunLease)
	r.StateBackup("k", BackupTaken, 1)
	r.StateRestore("k", ResultFailed)
	r.PlanApproval("k", PlanChanged)
	r.ActiveJobs().Bind(fakeJobs(t, runningJob("j1", "apply")))
}

// TestPlanApprovals: bounded results only.
func TestPlanApprovals(t *testing.T) {
	t.Parallel()
	reg := cbmetrics.NewKubeRegistry()
	r := New()
	if err := r.Register(reg); err != nil {
		t.Fatal(err)
	}
	r.PlanApproval("TerraformCluster", PlanApproved)
	r.PlanApproval("TerraformCluster", PlanChanged)
	r.PlanApproval("TerraformCluster", PlanChanged)
	r.PlanApproval("TerraformCluster", "bogus")
	want := `
# HELP captf_plan_approvals_total [ALPHA] ` + spec(PlanApprovalsName).Help + `
# TYPE captf_plan_approvals_total counter
captf_plan_approvals_total{kind="TerraformCluster",result="approved"} 1
captf_plan_approvals_total{kind="TerraformCluster",result="changed"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), PlanApprovalsName); err != nil {
		t.Error(err)
	}
}

// TestStateBackupsAndRestores: bounded results only, n at a time, nothing
// for n 0.
func TestStateBackupsAndRestores(t *testing.T) {
	t.Parallel()
	reg := cbmetrics.NewKubeRegistry()
	r := New()
	if err := r.Register(reg); err != nil {
		t.Fatal(err)
	}
	r.StateBackup("TerraformCluster", BackupTaken, 1)
	r.StateBackup("TerraformCluster", BackupPruned, 3)
	r.StateBackup("TerraformCluster", BackupPruned, 0)
	r.StateBackup("TerraformMachine", BackupSkipped, 1)
	r.StateBackup("TerraformMachine", "bogus", 1)
	r.StateRestore("TerraformMachine", ResultSucceeded)
	r.StateRestore("TerraformMachine", RestoreNotFound)
	r.StateRestore("TerraformMachine", ResultDeadline) // not a restore result
	want := `
# HELP captf_state_backups_total [ALPHA] ` + spec(StateBackupsName).Help + `
# TYPE captf_state_backups_total counter
captf_state_backups_total{kind="TerraformCluster",result="pruned"} 3
captf_state_backups_total{kind="TerraformCluster",result="taken"} 1
captf_state_backups_total{kind="TerraformMachine",result="skipped"} 1
# HELP captf_state_restores_total [ALPHA] ` + spec(StateRestoresName).Help + `
# TYPE captf_state_restores_total counter
captf_state_restores_total{kind="TerraformMachine",result="not_found"} 1
captf_state_restores_total{kind="TerraformMachine",result="succeeded"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), StateBackupsName, StateRestoresName); err != nil {
		t.Error(err)
	}
}

// runningJob returns a Job named name in namespace "ns", labeled as a
// TerraformCluster Job running op.
func runningJob(name, op string) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, Labels: map[string]string{
		state.OwnerKindLabel: "TerraformCluster", jobs.OpLabel: op, state.ManagedLabel: "true",
	}}}
}

// fakeJobs returns a fake client seeded with objs; t fails the test if the
// scheme registration errors.
func fakeJobs(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// TestActiveJobs proves ActiveJobs exports nothing until Bind, then one
// sample per kind and op with a running Job, counting only running Jobs
// and ignoring an unlabelled one, and exports nothing again once bound to
// a client whose List fails. It registers once (a component-base
// StableCollector may not be Create'd twice) and gathers the one registry
// repeatedly as its state changes.
func TestActiveJobs(t *testing.T) {
	t.Parallel()
	done := runningJob("done", "apply")
	done.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	unlabelled := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "other"}}
	a := &ActiveJobs{desc: activeDesc()}
	reg := cbmetrics.NewKubeRegistry()
	if err := reg.CustomRegister(a); err != nil {
		t.Fatal(err)
	}
	if err := testutil.GatherAndCompare(reg, strings.NewReader("")); err != nil {
		t.Errorf("unbound: %v", err)
	}
	// Forged Jobs: an unknown kind, an unknown op, and a known pair without
	// the managed label must not add or change a series.
	badKind := runningJob("bad-kind", "apply")
	badKind.Labels[state.OwnerKindLabel] = "Evil"
	badOp := runningJob("bad-op", "apply")
	badOp.Labels[jobs.OpLabel] = "evil"
	unmanaged := runningJob("unmanaged", "apply")
	delete(unmanaged.Labels, state.ManagedLabel)
	a.Bind(fakeJobs(t, runningJob("a1", "apply"), runningJob("a2", "apply"), runningJob("d1", "drift"), done, unlabelled, badKind, badOp, unmanaged))
	want := `
# HELP captf_jobs_active [ALPHA] Jobs currently running, counted from the Job cache at scrape time.
# TYPE captf_jobs_active gauge
captf_jobs_active{kind="TerraformCluster",op="apply"} 2
captf_jobs_active{kind="TerraformCluster",op="drift"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want)); err != nil {
		t.Error(err)
	}
	// A failed list exports nothing rather than a wrong count.
	a.Bind(fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("down")
		},
	}).Build())
	if err := testutil.GatherAndCompare(reg, strings.NewReader("")); err != nil {
		t.Errorf("failed list: %v", err)
	}
}
