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
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	testingclock "k8s.io/utils/clock/testing"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/locks"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
	"github.com/captf-io/cluster-api-provider-terraform/internal/rbac"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// --- Bookkeeping pieces -----------------------------------------------------

// podWith returns a fake source-container Pod: terminated with message
// result when result is set, or waiting with reason waitingReason when
// that is set.
func podWith(result string, waitingReason string) *corev1.Pod {
	p := &corev1.Pod{}
	st := corev1.ContainerStatus{Name: jobs.SourceContainer}
	if result != "" {
		st.State.Terminated = &corev1.ContainerStateTerminated{Message: result}
	}
	if waitingReason != "" {
		st.State.Waiting = &corev1.ContainerStateWaiting{Reason: waitingReason}
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{st}
	return p
}

// TestApplyDestroyCondition proves applyDestroyCondition maps a finished
// Job to ApplyJobSucceeded/DestroyJobSucceeded: success, an image pull
// failure winning over a deadline, a bare deadline, an image-layout error,
// a step failure and a failed destroy each set their own status and
// reason.
func TestApplyDestroyCondition(t *testing.T) {
	t.Parallel()
	step := "apply"
	layout := &jobs.Result{Error: &runner.Error{Kind: "image-layout"}}
	stepErr := &jobs.Result{Error: &runner.Error{Kind: "step", Step: &step}}
	deadline := job("j", jobs.OpApply, jobs.Failed, t0)
	deadline.Status.Conditions[0].Reason = batchv1.JobReasonDeadlineExceeded
	tests := []struct {
		name   string
		f      finished
		status metav1.ConditionStatus
		reason string
	}{
		{"apply succeeded", finished{job: ptr(job("j", jobs.OpApply, jobs.Succeeded, t0)), ok: true}, metav1.ConditionTrue, infrav1.ApplySucceededReason},
		{"destroy succeeded", finished{job: ptr(job("j", jobs.OpDestroy, jobs.Succeeded, t0)), ok: true}, metav1.ConditionTrue, infrav1.DestroySucceededReason},
		{"pull failure wins over the deadline", finished{job: &deadline, pod: podWith("", "ImagePullBackOff")}, metav1.ConditionFalse, infrav1.ImagePullFailedReason},
		{"deadline", finished{job: &deadline}, metav1.ConditionFalse, infrav1.JobDeadlineExceededReason},
		{"image layout", finished{job: ptr(job("j", jobs.OpApply, jobs.Failed, t0)), result: layout}, metav1.ConditionFalse, infrav1.ImageInvalidReason},
		{"apply step failed", finished{job: ptr(job("j", jobs.OpApply, jobs.Failed, t0)), result: stepErr}, metav1.ConditionFalse, infrav1.ApplyFailedReason},
		{"destroy failed", finished{job: ptr(job("j", jobs.OpDestroy, jobs.Failed, t0))}, metav1.ConditionFalse, infrav1.DestroyFailedReason},
	}
	for _, tt := range tests {
		c := applyDestroyCondition(tt.f, machine())
		if c.Status != tt.status || c.Reason != tt.reason {
			t.Errorf("%s: %s/%s, want %s/%s", tt.name, c.Status, c.Reason, tt.status, tt.reason)
		}
	}
	if c := applyDestroyCondition(finished{job: ptr(job("j", jobs.OpApply, jobs.Failed, t0)), result: stepErr}, machine()); !strings.Contains(c.Message, "step apply failed") {
		t.Errorf("message = %q", c.Message)
	}
}

// TestFailedResourcesReported proves a failed apply's resources reach
// status.lastRun.error (capped) and its condition message names the first.
func TestFailedResourcesReported(t *testing.T) {
	t.Parallel()
	step := "apply"
	long := strings.Repeat("x", MaxRunResourceBytes+10)
	res := []string{"aws_instance.web: InvalidAMI", long}
	for range MaxRunResources {
		res = append(res, "a.b: c")
	}
	f := finished{job: ptr(job("j", jobs.OpApply, jobs.Failed, t0)), result: &jobs.Result{Error: &runner.Error{Kind: "step", Step: &step, Resources: res}}}
	c := applyDestroyCondition(f, machine())
	if want := "Job j: step apply failed: aws_instance.web: InvalidAMI"; c.Message != want {
		t.Errorf("message = %q, want %q", c.Message, want)
	}
	m := machine()
	setLastRun((&fakeKind{obj: m}).Status(), f)
	got := m.Status.LastRun.Error.Resources
	if len(got) != MaxRunResources || got[0] != res[0] || len(got[1]) != MaxRunResourceBytes {
		t.Errorf("resources = %d items, first %q", len(got), got[0])
	}
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// TestCountFailures proves Bookkeeping.countFailures counts each op's
// failures since its newest success and records the newest failure's time,
// leaving an op with no failures at zero.
func TestCountFailures(t *testing.T) {
	t.Parallel()
	// Newest first: two failed applies after a success, then an older
	// failure that must not count; one failed destroy.
	done := []finished{
		{job: ptr(job("a4", jobs.OpApply, jobs.Failed, t0))},
		{job: ptr(job("d1", jobs.OpDestroy, jobs.Failed, t0.Add(-time.Minute)))},
		{job: ptr(job("a3", jobs.OpApply, jobs.Failed, t0.Add(-2*time.Minute)))},
		{job: ptr(job("a2", jobs.OpApply, jobs.Succeeded, t0.Add(-3*time.Minute))), ok: true},
		{job: ptr(job("a1", jobs.OpApply, jobs.Failed, t0.Add(-4*time.Minute)))},
	}
	bk := &Bookkeeping{View: JobsView{Failures: map[jobs.Op]int{}, LastFailure: map[jobs.Op]time.Time{}}}
	bk.countFailures(done)
	if bk.View.Failures[jobs.OpApply] != 2 || !bk.View.LastFailure[jobs.OpApply].Equal(t0) {
		t.Errorf("apply failures = %d at %s", bk.View.Failures[jobs.OpApply], bk.View.LastFailure[jobs.OpApply])
	}
	if bk.View.Failures[jobs.OpDestroy] != 1 || bk.View.Failures[jobs.OpDrift] != 0 {
		t.Errorf("failures = %v", bk.View.Failures)
	}
}

// TestInterruptedJobsCostNoBackoff: a Job the runner reports as interrupted
// (a drain, an eviction, a deletion) is not a module failure, so it counts
// toward no retry backoff, whether its result was just read or it is only
// known from InterruptedAnnotation on a bookkept Job.
func TestInterruptedJobsCostNoBackoff(t *testing.T) {
	t.Parallel()
	bookkeptInterrupted := job("a3", jobs.OpApply, jobs.Failed, t0.Add(-time.Minute))
	bookkeptInterrupted.Annotations = map[string]string{BookkeptAnnotation: "true", InterruptedAnnotation: "true"}
	done := []finished{
		{job: ptr(job("a4", jobs.OpApply, jobs.Failed, t0)), interrupted: true},
		{job: &bookkeptInterrupted, bookkept: true, interrupted: true},
		{job: ptr(job("a2", jobs.OpApply, jobs.Failed, t0.Add(-2*time.Minute)))},
	}
	bk := &Bookkeeping{View: JobsView{Failures: map[jobs.Op]int{}, LastFailure: map[jobs.Op]time.Time{}}}
	bk.countFailures(done)
	if bk.View.Failures[jobs.OpApply] != 1 || !bk.View.LastFailure[jobs.OpApply].Equal(t0.Add(-2*time.Minute)) {
		t.Errorf("apply failures = %d at %s, want 1 (only a2)", bk.View.Failures[jobs.OpApply], bk.View.LastFailure[jobs.OpApply])
	}

	// The mark survives bookkeeping: the collector reads it back from the Job.
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	e.runner.jobs = append(e.runner.jobs, bookkeptInterrupted)
	got, err := collectFinished(t.Context(), e.d, e.runner.jobs)
	if err != nil || len(got) != 1 || !got[0].interrupted || !got[0].bookkept {
		t.Errorf("collectFinished = %+v, %v; want the bookkept Job marked interrupted", got, err)
	}
}

// TestSetLastRun proves setLastRun copies a finished Job's result into
// status.lastRun (job name, operation, steps with durations and exit
// codes, error step, kind and summary) and the image/runtime into
// status.source, cutting an overlong error summary to MaxRunSummary bytes
// on a UTF-8 rune boundary.
func TestSetLastRun(t *testing.T) {
	t.Parallel()
	m := machine()
	k := &fakeKind{obj: m}
	step := "apply"
	f := finished{job: ptr(job("j9", jobs.OpApply, jobs.Failed, t0)), result: &jobs.Result{
		Image:   runner.ResultImage{Ref: "registry.example/mod:1.0"},
		Runtime: runner.Runtime{Version: "1.16.4"},
		Steps:   []runner.Step{{Name: "init", Exit: 0, Seconds: 1.5}, {Name: "apply", Exit: 1, Seconds: 0.25}},
		Error:   &runner.Error{Kind: "step", Step: &step, Tail: "Error: boom"},
	}}
	setLastRun(k.Status(), f)
	run := m.Status.LastRun
	if run.Job != "j9" || run.Operation != infrav1.OperationApply || len(run.Steps) != 2 ||
		*run.Steps[0].DurationMilliseconds != 1500 || *run.Steps[1].ExitCode != 1 ||
		run.Error.Step != "apply" || run.Error.Summary != "Error: boom" || run.Error.Kind != infrav1.RunErrorKindStep {
		t.Errorf("lastRun = %+v", run)
	}

	// A longer runner summary is cut to the CRD's 512 bytes on a rune
	// boundary, or the status update would fail validation.
	long := strings.Repeat("a", MaxRunSummary-1) + "é and more"
	f.result.Error.Tail = long
	setLastRun(k.Status(), f)
	if got := m.Status.LastRun.Error.Summary; len(got) != MaxRunSummary-1 || !utf8.ValidString(got) {
		t.Errorf("summary is %d bytes (valid UTF-8: %v), want %d", len(got), utf8.ValidString(got), MaxRunSummary-1)
	}
	f.result.Error.Tail = strings.Repeat("b", MaxRunSummary)
	setLastRun(k.Status(), f)
	if got := m.Status.LastRun.Error.Summary; len(got) != MaxRunSummary {
		t.Errorf("summary of exactly %d bytes cut to %d", MaxRunSummary, len(got))
	}
	if m.Status.Source.Image != "registry.example/mod:1.0" || m.Status.Source.RuntimeVersion != "1.16.4" {
		t.Errorf("source = %+v", m.Status.Source)
	}
}

// TestChooseImage proves ChooseImage runs an apply on the spec image, a
// destroy on the pinned digest, and reports a drift Job's image unknown
// when there is no pin.
func TestChooseImage(t *testing.T) {
	t.Parallel()
	pinned := "registry.example/mod@sha256:abc"
	if ref, unknown := ChooseImage(jobs.OpApply, "mod:1", pinned); ref != "mod:1" || unknown {
		t.Errorf("apply runs %s", ref)
	}
	if ref, unknown := ChooseImage(jobs.OpDestroy, "mod:1", pinned); ref != pinned || unknown {
		t.Errorf("destroy runs %s", ref)
	}
	if ref, unknown := ChooseImage(jobs.OpDrift, "mod:1", ""); ref != "mod:1" || !unknown {
		t.Errorf("drift without pin runs %s (unknown=%v)", ref, unknown)
	}
}

// --- Reconcile --------------------------------------------------------------

// systemNS is the namespace the fake identity's credentials Secret lives
// in, distinct from testNS.
const systemNS = "captf-system"

// machineIn returns a MachineInputs for cluster c1, object testName, with
// cloud-config bootstrap data.
func machineIn() contract.MachineInputs {
	return contract.MachineInputs{
		CommonInputs: contract.CommonInputs{
			Contract: contract.Version,
			Cluster:  contract.Cluster{Name: "c1", Namespace: testNS},
			Object:   contract.Object{Kind: "TerraformMachine", Name: testName, Namespace: testNS},
			Tags:     contract.Tags("c1", testNS, "TerraformMachine", testName, ""),
		},
		ClusterOutputs:  json.RawMessage(`{}`),
		MachineName:     "m1",
		BootstrapData:   base64.StdEncoding.EncodeToString([]byte("#cloud-config\n")),
		BootstrapFormat: "cloud-config",
	}
}

// world returns a namespace with an allowed identity and its credentials
// Secret, followed by objs.
func world(objs ...client.Object) []client.Object {
	return append([]client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS}},
		&infrav1.TerraformClusterIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: testIdentity, UID: "id-uid"},
			Spec: infrav1.TerraformClusterIdentitySpec{
				SecretRef:         infrav1.SecretReference{Name: "aws-creds", Namespace: systemNS},
				AllowedNamespaces: &infrav1.AllowedNamespaces{Selector: &metav1.LabelSelector{}},
			},
		},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: systemNS, Name: "aws-creds"}, Data: map[string][]byte{"KEY": []byte("v")}},
	}, objs...)
}

// readyOwner is an OwnerInfo with a Machine ownerRef, an unpaused Cluster
// and its TerraformCluster, which sets no policy for its machines.
var readyOwner = OwnerInfo{HasOwnerRef: true, Cluster: cluster(false),
	InfraCluster: &infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "c1"}}}

// reconcileOnce runs one Reconcile of k against e.d, using t for context,
// and returns the requeue duration and any error.
func reconcileOnce(t *testing.T, e *env, k *fakeKind) (time.Duration, error) {
	t.Helper()
	res, err := Reconcile(t.Context(), e.d, k)
	return res.RequeueAfter, err
}

// TestReconcileStartsApply proves the first reconcile of a fresh object
// writes the block-move annotation before the Job exists, starts an
// apply carrying an inputs hash, records it in status.activeJob, sets each
// first-visit condition to its expected reason, and creates the durable
// and per-run Secrets, the identity mirror and the runner RBAC.
func TestReconcileStartsApply(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	sawBlockMove := false
	e.runner.onCreate = func(*batchv1.Job) {
		// Block-move is persisted before the Job exists.
		sawBlockMove = HasBlockMove(e.get(t))
	}
	k := e.kindFor(t, readyOwner)
	k.in = machineIn()
	requeue, err := reconcileOnce(t, e, k)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(e.runner.created) != 1 || !strings.Contains(e.runner.created[0], "-apply-a1-") || !sawBlockMove || requeue != ActiveJobRequeue {
		t.Fatalf("created %v, block-move before create %v, requeue %s", e.runner.created, sawBlockMove, requeue)
	}
	m := e.get(t)
	if m.Status.ActiveJob.Name != e.runner.created[0] || m.Status.ActiveJob.Operation != infrav1.OperationApply || !HasBlockMove(m) {
		t.Errorf("activeJob = %+v, annotations %v", m.Status.ActiveJob, m.Annotations)
	}
	for typ, want := range map[string]string{
		infrav1.IdentityAllowedCondition:       infrav1.IdentityAllowedReason,
		infrav1.CredentialsMirroredCondition:   infrav1.MirroredReason,
		infrav1.RunnerRBACReadyCondition:       infrav1.RBACReadyReason,
		infrav1.DependenciesReadyCondition:     infrav1.DependenciesReadyReason,
		infrav1.InfrastructureHealthyCondition: infrav1.ProvisioningReason,
		infrav1.ApplyJobSucceededCondition:     infrav1.NoApplyYetReason,
		infrav1.ReadyCondition:                 infrav1.NotReadyReason,
	} {
		if c := conditions.Get(m, typ); c == nil || c.Reason != want {
			t.Errorf("%s = %+v, want reason %s", typ, c, want)
		}
	}
	// Durable and per-run Secrets, the mirror and the runner RBAC exist.
	for _, name := range []string{inputs.Name("m", testName), inputs.RunName(e.runner.created[0]), identity.MirrorName(testIdentity)} {
		if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, &corev1.Secret{}); err != nil {
			t.Errorf("Secret %s: %v", name, err)
		}
	}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: rbac.RoleBinding}, &rbacv1.RoleBinding{}); err != nil {
		t.Errorf("RoleBinding: %v", err)
	}
	durable, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil || durable.Meta.Identity != testIdentity || durable.Meta.Image != "registry.example/mod:1.0" {
		t.Errorf("durable = %+v, %v", durable, err)
	}
	created := e.runner.jobs[0]
	if created.Annotations[state.InputsHashAnnotation] == "" {
		t.Error("apply Job carries no inputs hash")
	}
}

// TestReconcileJobActive proves a running Job with a readable pod records
// status.activeJob and starts nothing more, while a Job stuck without a
// per-run Secret and a pod that never mounted it is deleted so the
// next reconcile can recreate it with its Secret.
func TestReconcileJobActive(t *testing.T) {
	t.Parallel()
	const name = "captf-m-m1-apply-a1-abcdef"
	runSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: inputs.RunName(name)}}
	pod := func(phase corev1.PodPhase) corev1.Pod { return corev1.Pod{Status: corev1.PodStatus{Phase: phase}} }
	for _, tt := range []struct {
		name    string
		secret  bool
		pods    []corev1.Pod
		deleted bool
	}{
		{name: "per-run Secret present", secret: true, pods: []corev1.Pod{pod(corev1.PodPending)}},
		// The pod could never mount its inputs; the Job is deleted so
		// the next reconcile recreates it with its Secret.
		{name: "Secret missing, no pod", deleted: true},
		{name: "Secret missing, pod pending", pods: []corev1.Pod{pod(corev1.PodPending)}, deleted: true},
		{name: "Secret missing, pod started", pods: []corev1.Pod{pod(corev1.PodRunning)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			objs := world(machine(withFinalizer, notPaused))
			if tt.secret {
				objs = append(objs, runSecret.DeepCopy())
			}
			e := newEnv(t, objs...)
			running := job(name, jobs.OpApply, jobs.Running, t0)
			running.Labels[jobs.AttemptLabel] = "1"
			e.runner.jobs = append(e.runner.jobs, running)
			e.runner.pods[name] = tt.pods
			k := e.kindFor(t, readyOwner)
			k.in = machineIn()
			requeue, err := reconcileOnce(t, e, k)
			if err != nil {
				t.Fatal(err)
			}
			if tt.deleted {
				if !slices.Equal(e.runner.deleted, []string{name}) || len(e.runner.created) != 0 || requeue != time.Second {
					t.Errorf("deleted %v, created %v, requeue %s; want the stuck Job deleted", e.runner.deleted, e.runner.created, requeue)
				}
				return
			}
			m := e.get(t)
			if len(e.runner.created) != 0 || len(e.runner.deleted) != 0 || requeue != ActiveJobRequeue || !HasBlockMove(m) ||
				m.Status.ActiveJob.Name != running.Name || m.Status.ActiveJob.Attempt != 1 {
				t.Errorf("created %v, deleted %v, requeue %s, activeJob %+v", e.runner.created, e.runner.deleted, requeue, m.Status.ActiveJob)
			}
		})
	}
}

// TestReconcilePaused proves a paused reconcile with no Job clears and
// persists block-move and starts nothing, keeps block-move while an active
// Job runs, and deletes a stuck Job (clearing block-move) so it cannot
// survive a move.
func TestReconcilePaused(t *testing.T) {
	t.Parallel()
	pausedOwner := OwnerInfo{HasOwnerRef: true, Cluster: cluster(true)}
	withBlockMove := func(m *infrav1.TerraformMachine) { SetBlockMove(m) }

	t.Run("no Job: block-move cleared and persisted, no Job started", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(withFinalizer, withBlockMove))...)
		e.runner.jobs = append(e.runner.jobs, job("done", jobs.OpApply, jobs.Succeeded, t0))
		k := e.kindFor(t, pausedOwner)
		k.in = machineIn()
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		m := e.get(t)
		if HasBlockMove(m) || len(e.runner.created) != 0 || m.Status.LastRun.Job != "done" {
			t.Errorf("block-move %v, created %v, lastRun %+v", HasBlockMove(m), e.runner.created, m.Status.LastRun)
		}
		if c := conditions.Get(m, clusterv1.PausedCondition); c == nil || c.Status != metav1.ConditionTrue {
			t.Errorf("Paused = %+v", c)
		}
	})
	t.Run("active Job: block-move stays", func(t *testing.T) {
		t.Parallel()
		runSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: inputs.RunName("running")}}
		e := newEnv(t, world(machine(withFinalizer, withBlockMove), runSecret)...)
		e.runner.jobs = append(e.runner.jobs, job("running", jobs.OpApply, jobs.Running, t0))
		k := e.kindFor(t, pausedOwner)
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		if m := e.get(t); !HasBlockMove(m) || m.Status.ActiveJob.Name != "running" || len(e.runner.created) != 0 {
			t.Errorf("block-move %v, activeJob %+v", HasBlockMove(m), m.Status.ActiveJob)
		}
	})
	// A Job that can never start (no per-run Secret, no pod) is deleted
	// while paused too, so it cannot hold block-move through a move.
	t.Run("stuck Job: deleted, block-move cleared", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(withFinalizer, withBlockMove))...)
		e.runner.jobs = append(e.runner.jobs, job("stuck", jobs.OpApply, jobs.Running, t0.Add(-time.Minute)))
		k := e.kindFor(t, pausedOwner)
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		if m := e.get(t); HasBlockMove(m) || m.Status.ActiveJob.Name != "" || !slices.Equal(e.runner.deleted, []string{"stuck"}) {
			t.Errorf("block-move %v, activeJob %+v, deleted %v", HasBlockMove(m), m.Status.ActiveJob, e.runner.deleted)
		}
	})
	// A deleting object waits for the unpause: no destroy, finalizer kept,
	// and the Deleting condition says why.
	t.Run("deleting: waits, says why, no Job, finalizer kept", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(withFinalizer, deleting))...)
		k := e.kindFor(t, pausedOwner)
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		m := e.get(t)
		c := conditions.Get(m, clusterv1.DeletingCondition)
		if c == nil || c.Status != metav1.ConditionTrue || c.Reason != clusterv1.DeletingReason ||
			!strings.Contains(c.Message, "Deletion waits until the object is unpaused") {
			t.Errorf("Deleting = %+v", c)
		}
		if len(e.runner.created) != 0 || len(m.Finalizers) == 0 {
			t.Errorf("created %v, finalizers %v", e.runner.created, m.Finalizers)
		}
	})
}

// TestDeleteStuckJobYoung: a Job younger than StuckJobAge is not looked at,
// even with no per-run Secret yet: no reads, no delete.
func TestDeleteStuckJobYoung(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world()...)
	young := job("young", jobs.OpApply, jobs.Running, t0)
	young.CreationTimestamp = metav1.NewTime(t0.Add(-10 * time.Second))
	deleted, err := DeleteStuckJob(t.Context(), e.d, machine(), &young)
	if err != nil || deleted || len(e.runner.podLists) != 0 || len(e.runner.deleted) != 0 {
		t.Errorf("deleted %v (%v), pod lists %v, deletes %v", deleted, err, e.runner.podLists, e.runner.deleted)
	}
}

// TestReconcileGatedAndIdentity proves a Gate on the kind sets
// DependenciesReady, starts no Job and leaves Ready Unknown, and losing
// identity access sets IdentityAllowed False/NamespaceNotAllowed, revokes
// the mirror and starts no Job.
func TestReconcileGatedAndIdentity(t *testing.T) {
	t.Parallel()
	t.Run("gate: DependenciesReady, no Job, Ready Unknown", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(withFinalizer, notPaused))...)
		k := e.kindFor(t, readyOwner)
		k.gate = &Gate{Status: metav1.ConditionUnknown, Reason: infrav1.WaitingForBootstrapDataReason, Message: "waiting"}
		requeue, err := reconcileOnce(t, e, k)
		if err != nil {
			t.Fatal(err)
		}
		m := e.get(t)
		if len(e.runner.created) != 0 || requeue != GateRequeue {
			t.Errorf("created %v, requeue %s", e.runner.created, requeue)
		}
		if c := conditions.Get(m, infrav1.ReadyCondition); c == nil || c.Status != metav1.ConditionUnknown {
			t.Errorf("Ready = %+v", c)
		}
	})
	t.Run("identity not allowed: mirror revoked, no Job", func(t *testing.T) {
		t.Parallel()
		objs := world(machine(withFinalizer, notPaused))
		objs[1].(*infrav1.TerraformClusterIdentity).Spec.AllowedNamespaces = nil
		mirror := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Namespace: testNS, Name: identity.MirrorName(testIdentity),
			Labels:      map[string]string{identity.MirroredLabel: "true"},
			Annotations: map[string]string{inputs.IdentityAnnotation: testIdentity},
		}}
		e := newEnv(t, append(objs, mirror)...)
		k := e.kindFor(t, readyOwner)
		k.in = machineIn()
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		m := e.get(t)
		if c := conditions.Get(m, infrav1.IdentityAllowedCondition); c == nil || c.Reason != infrav1.NamespaceNotAllowedReason || len(e.runner.created) != 0 {
			t.Errorf("IdentityAllowed = %+v, created %v", c, e.runner.created)
		}
		if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(mirror), &corev1.Secret{}); client.IgnoreNotFound(err) != nil || err == nil {
			t.Errorf("mirror not revoked: %v", err)
		}
	})
	t.Run("destroy under a disallowed identity: IdentityNotAllowed, finalizer stays", func(t *testing.T) {
		t.Parallel()
		objs := world(machine(deleting, notPaused))
		objs[1].(*infrav1.TerraformClusterIdentity).Spec.AllowedNamespaces = nil
		e := newEnv(t, objs...)
		e.state.st = &state.State{InputsHash: "h1:x"}
		k := e.kindFor(t, readyOwner)
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		m := e.get(t)
		if c := conditions.Get(m, infrav1.ApplyJobSucceededCondition); m == nil || c == nil || c.Reason != infrav1.IdentityNotAllowedReason || len(e.runner.created) != 0 {
			t.Errorf("ApplyJobSucceeded = %+v, created %v", c, e.runner.created)
		}
		if c := conditions.Get(m, clusterv1.DeletingCondition); c == nil || c.Status != metav1.ConditionTrue {
			t.Errorf("Deleting = %+v", c)
		}
	})
}

// TestReconcileDestroy proves a deleting, owned object with state destroys
// from the durable inputs (running the pinned digest, not the spec tag), a
// succeeded destroy runs cleanup and drops the finalizer, a missing
// durable Secret on an immutable kind sets ApplyJobSucceeded/DestroyFailed
// and backs off at RetryMax, and deleting with no state and no Job drops
// the finalizer without starting a Job.
func TestReconcileDestroy(t *testing.T) {
	t.Parallel()
	t.Run("deleting with state starts destroy from the durable inputs", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused))...)
		k := e.kindFor(t, readyOwner)
		if err := inputs.Write(t.Context(), e.c, k.obj, renderMachine(t), inputs.Meta{Image: "registry.example/mod:0.9", Identity: testIdentity, ImageDigest: "registry.example/mod@sha256:abc"}); err != nil {
			t.Fatal(err)
		}
		e.state.st = &state.State{InputsHash: "h1:x"}
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		if len(e.runner.jobs) != 1 || jobs.OpOf(&e.runner.jobs[0]) != jobs.OpDestroy {
			t.Fatalf("jobs = %v", e.runner.created)
		}
		// Immutable: the pinned digest runs, not the spec tag.
		if img := e.runner.jobs[0].Spec.Template.Spec.Containers[0].Image; img != "registry.example/mod@sha256:abc" {
			t.Errorf("destroy runs %s", img)
		}
	})
	t.Run("destroy succeeded: cleanup and finalizer removed", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused))...)
		e.state.st = &state.State{InputsHash: "h1:x"}
		e.runner.jobs = append(e.runner.jobs, job("d", jobs.OpDestroy, jobs.Succeeded, t0))
		k := e.kindFor(t, readyOwner)
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		if m := e.get(t); m != nil {
			t.Errorf("object still exists: finalizers %v", m.Finalizers)
		}
	})
	t.Run("durable Secret missing on an immutable kind: DestroyFailed", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused))...)
		e.state.st = &state.State{InputsHash: "h1:x"}
		k := e.kindFor(t, readyOwner)
		requeue, err := reconcileOnce(t, e, k)
		if err != nil {
			t.Fatal(err)
		}
		c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition)
		if c == nil || c.Reason != infrav1.DestroyFailedReason || requeue != RetryMax || len(e.runner.created) != 0 {
			t.Errorf("ApplyJobSucceeded = %+v, requeue %s", c, requeue)
		}
	})
	t.Run("owned, deleting, no state and no Job: finalizer dropped without a Job", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused))...)
		k := e.kindFor(t, readyOwner)
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		if m := e.get(t); m != nil || len(e.runner.created) != 0 {
			t.Errorf("object %v, created %v", m, e.runner.created)
		}
	})
	// A forged or stale ownerRef (OwnerMismatch) resolves no
	// TerraformCluster, so an inherited deletionPolicy is unknown and the
	// deletion holds; with a policy of its own the object deletes as an
	// object whose owner is gone does: destroy still proceeds from the
	// durable inputs, and dropping the finalizer with no state and no Job
	// does not depend on ever resolving a real owner.
	mismatchOwner := OwnerInfo{HasOwnerRef: true, Gate: &Gate{Status: metav1.ConditionFalse, Reason: infrav1.OwnerMismatchReason, Message: "forged"}}
	ownDestroy := func(m *infrav1.TerraformMachine) { m.Spec.DeletionPolicy = infrav1.DeletionPolicyDestroy }
	t.Run("OwnerMismatch, deleting with state: destroy still starts from the durable inputs", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused, ownDestroy))...)
		k := e.kindFor(t, mismatchOwner)
		if err := inputs.Write(t.Context(), e.c, k.obj, renderMachine(t), inputs.Meta{Image: "registry.example/mod:0.9", Identity: testIdentity, ImageDigest: "registry.example/mod@sha256:abc"}); err != nil {
			t.Fatal(err)
		}
		e.state.st = &state.State{InputsHash: "h1:x"}
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		if len(e.runner.jobs) != 1 || jobs.OpOf(&e.runner.jobs[0]) != jobs.OpDestroy {
			t.Fatalf("jobs = %v", e.runner.created)
		}
		if img := e.runner.jobs[0].Spec.Template.Spec.Containers[0].Image; img != "registry.example/mod@sha256:abc" {
			t.Errorf("destroy runs %s", img)
		}
	})
	t.Run("OwnerMismatch, deleting, no state and no Job: finalizer dropped without a Job", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused, ownDestroy))...)
		k := e.kindFor(t, mismatchOwner)
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		if m := e.get(t); m != nil || len(e.runner.created) != 0 {
			t.Errorf("object %v, created %v", m, e.runner.created)
		}
	})
}

// TestReconcileAdoptsAndProvisions proves that once a successful apply's
// state exists, the reconcile adopts the base state Secret (annotating it
// with the apply's inputs hash and setting an ownerRef), sets
// status.initialization.provisioned and observedStateSerial, sets Ready
// True, emits Provisioned once, and requeues at the first drift check (one
// interval plus jitter after the apply, not right after it).
func TestReconcileAdoptsAndProvisions(t *testing.T) {
	t.Parallel()
	suffix, err := state.Suffix(testNS, state.KindTerraformMachine, testName)
	if err != nil {
		t.Fatal(err)
	}
	base := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: state.SecretName(suffix), Labels: map[string]string{
		state.BackendStateLabel: "true", state.BackendSuffixLabel: suffix, state.BackendWorkspaceLabel: state.Workspace,
	}}}
	e := newEnv(t, world(machine(withFinalizer, notPaused), base)...)
	applied := job("a", jobs.OpApply, jobs.Succeeded, t0)
	applied.Annotations = map[string]string{state.InputsHashAnnotation: "h1:applied"}
	e.runner.jobs = append(e.runner.jobs, applied)
	e.state.st = &state.State{Serial: 7}
	k := e.kindFor(t, readyOwner)
	k.in = machineIn()
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	requeue, err := reconcileOnce(t, e, k)
	if err != nil {
		t.Fatal(err)
	}
	stored := &corev1.Secret{}
	if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(base), stored); err != nil {
		t.Fatal(err)
	}
	if stored.Annotations[state.InputsHashAnnotation] != "h1:applied" || len(stored.OwnerReferences) != 1 {
		t.Errorf("state not adopted: %+v", stored.ObjectMeta)
	}
	m := e.get(t)
	if m.Status.Initialization.Provisioned == nil || !*m.Status.Initialization.Provisioned || m.Status.ObservedStateSerial != 7 {
		t.Errorf("status = %+v", m.Status)
	}
	if c := conditions.Get(m, infrav1.ReadyCondition); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v", c)
	}
	// The first drift check is due one interval (plus the object's jitter)
	// after the successful apply, not right after it.
	wantRequeue := 30*time.Minute + Jitter(string(m.UID), 30*time.Minute)
	if m.Status.LastRefresh != nil || requeue != wantRequeue || len(e.runner.created) != 0 {
		t.Errorf("lastRefresh %v, requeue %s (want %s), created %v", m.Status.LastRefresh, requeue, wantRequeue, e.runner.created)
	}
	if e.rec.count(EventProvisioned) != 1 {
		t.Errorf("events = %v, want one Provisioned", e.rec.reasons)
	}
}

// TestReconcileStateUnreadable proves an encrypted-state read sets
// StateReadable False/StateEncrypted, requeues at StateRequeue and starts
// no Job.
func TestReconcileStateUnreadable(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	e.state.err = state.ErrStateEncrypted
	k := e.kindFor(t, readyOwner)
	k.in = machineIn()
	requeue, err := reconcileOnce(t, e, k)
	if err != nil {
		t.Fatal(err)
	}
	c := conditions.Get(e.get(t), infrav1.StateReadableCondition)
	if c == nil || c.Reason != infrav1.StateEncryptedReason || requeue != StateRequeue || len(e.runner.created) != 0 {
		t.Errorf("StateReadable = %+v, requeue %s, created %v", c, requeue, e.runner.created)
	}
}

// renderMachine returns the rendered root module files for machineIn(),
// failing t on error.
func renderMachine(t *testing.T) render.Files {
	t.Helper()
	files, err := render.Root(contract.RoleMachine, machineIn())
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestReconcileFailedLimitZeroBacksOff: with failedJobsHistoryLimit 0
// the failed apply is still retained and counted, so the retry waits
// RetryMax instead of starting at once.
func TestReconcileFailedLimitZeroBacksOff(t *testing.T) {
	t.Parallel()
	zero := int32(0)
	e := newEnv(t, world(machine(withFinalizer, notPaused, func(m *infrav1.TerraformMachine) {
		m.Spec.Jobs = &infrav1.JobPolicy{FailedJobsHistoryLimit: &zero}
	}))...)
	k := e.kindFor(t, readyOwner)
	k.in = machineIn()
	e.runner.jobs = append(e.runner.jobs, job("f1", jobs.OpApply, jobs.Failed, t0.Add(-time.Minute)))
	requeue, err := reconcileOnce(t, e, k)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 0 || slices.Contains(e.runner.deleted, "f1") || requeue != RetryMax-time.Minute {
		t.Errorf("created %v, deleted %v, requeue %s; want no Job, f1 kept, requeue %s", e.runner.created, e.runner.deleted, requeue, RetryMax-time.Minute)
	}
}

// TestReconcileBookkeepingPinsDigest proves bookkeeping pins the newest
// finished apply's source-container image digest (not an older pod's) onto
// the durable Secret and status.source.imageDigest, and that the newest
// finished Job overall (a failed drift, here) is the one recorded in
// status.lastRun.
func TestReconcileBookkeepingPinsDigest(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	k := e.kindFor(t, readyOwner)
	k.in = machineIn()
	if err := inputs.Write(t.Context(), e.c, k.obj, renderMachine(t), inputs.Meta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
		t.Fatal(err)
	}
	digest := "registry.example/mod@sha256:" + strings.Repeat("a", 64)
	applied := job("a", jobs.OpApply, jobs.Succeeded, t0.Add(-time.Hour))
	older, newer := corev1.Pod{}, corev1.Pod{}
	older.CreationTimestamp = metav1.NewTime(t0.Add(-2 * time.Hour))
	older.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: jobs.SourceContainer, ImageID: "registry.example/mod@sha256:" + strings.Repeat("b", 64)}}
	newer.CreationTimestamp = metav1.NewTime(t0.Add(-time.Hour))
	newer.Spec.Containers = []corev1.Container{{Name: jobs.SourceContainer, Image: "registry.example/mod:1.0"}}
	newer.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: jobs.SourceContainer, ImageID: "docker-pullable://" + digest}}
	e.runner.pods["a"] = []corev1.Pod{older, newer}
	drift := job("d", jobs.OpDrift, jobs.Failed, t0)
	e.runner.jobs = append(e.runner.jobs, applied, drift)
	e.state.st = &state.State{InputsHash: "h1:x"}
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	durable, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil || durable.Meta.ImageDigest != digest {
		t.Fatalf("pinned digest = %+v, %v", durable, err)
	}
	m := e.get(t)
	if m.Status.Source.ImageDigest != digest {
		t.Errorf("status.source.imageDigest = %q", m.Status.Source.ImageDigest)
	}
	if c := conditions.Get(m, infrav1.DriftJobSucceededCondition); c == nil || c.Reason != infrav1.DriftJobFailedReason {
		t.Errorf("DriftJobSucceeded = %+v", c)
	}
	if m.Status.LastRun.Job != "d" {
		t.Errorf("lastRun = %+v, want the newest Job", m.Status.LastRun)
	}
}

// TestReconcileMutableDestroyWithoutDurable proves a mutable kind's destroy
// with no digest pin runs the spec image, not an error.
func TestReconcileMutableDestroyWithoutDurable(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(deleting, notPaused))...)
	e.state.st = &state.State{InputsHash: "h1:x"}
	k := e.kindFor(t, readyOwner)
	k.mutable = true
	k.in = machineIn()
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	if len(e.runner.jobs) != 1 || jobs.OpOf(&e.runner.jobs[0]) != jobs.OpDestroy {
		t.Fatalf("jobs = %v", e.runner.created)
	}
	// A mutable kind without a pin runs the spec image.
	if img := e.runner.jobs[0].Spec.Template.Spec.Containers[0].Image; img != "registry.example/mod:1.0" {
		t.Errorf("destroy runs %s", img)
	}
}

// TestReconcileMutableApplyOnHashChange proves a mutable kind whose
// current inputs hash differs from the state's starts an apply.
func TestReconcileMutableApplyOnHashChange(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	e.state.st = &state.State{InputsHash: "h1:old"}
	k := e.kindFor(t, readyOwner)
	k.mutable = true
	k.in = machineIn()
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 1 || !strings.Contains(e.runner.created[0], "-apply-") {
		t.Errorf("created %v, want an apply for the changed inputs", e.runner.created)
	}
}

// TestReconcileIdentityFailures proves a missing identity object, a
// missing identity Secret, or no identityRef at all each set
// IdentityAllowed False with the matching reason, start no Job, requeue at
// GateRequeue, and leave CredentialsMirrored at MirrorPending.
func TestReconcileIdentityFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func([]client.Object) []client.Object
		reason string
	}{
		{"identity object missing", without[*infrav1.TerraformClusterIdentity], infrav1.IdentityNotFoundReason},
		{"identity Secret missing", without[*corev1.Secret], infrav1.SecretNotFoundReason},
		{"identity Secret lacks a required key", func(o []client.Object) []client.Object {
			for _, x := range o {
				if id, ok := x.(*infrav1.TerraformClusterIdentity); ok {
					id.Spec.RequiredKeys = []string{"never-there"}
				}
			}
			return o
		}, infrav1.CredentialsIncompleteReason},
		{"no identityRef at all", func(o []client.Object) []client.Object {
			o[len(o)-1].(*infrav1.TerraformMachine).Spec.IdentityRef = infrav1.IdentityReference{}
			return o
		}, infrav1.IdentityNotFoundReason},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, tt.mutate(world(machine(withFinalizer, notPaused)))...)
			k := e.kindFor(t, readyOwner)
			k.in = machineIn()
			requeue, err := reconcileOnce(t, e, k)
			if err != nil {
				t.Fatal(err)
			}
			m := e.get(t)
			if c := conditions.Get(m, infrav1.IdentityAllowedCondition); c == nil || c.Reason != tt.reason || len(e.runner.created) != 0 || requeue != GateRequeue {
				t.Errorf("IdentityAllowed = %+v, created %v, requeue %s", c, e.runner.created, requeue)
			}
			if c := conditions.Get(m, infrav1.CredentialsMirroredCondition); c == nil || c.Reason != infrav1.MirrorPendingReason {
				t.Errorf("CredentialsMirrored = %+v", c)
			}
		})
	}
}

// TestClusterName proves ClusterName reads the cluster.x-k8s.io/cluster-name
// label when present, falls back to the owner's Cluster, and returns "" when
// neither is known.
func TestClusterName(t *testing.T) {
	t.Parallel()
	if n := ClusterName(machine(), OwnerInfo{}); n != "c1" {
		t.Errorf("from label: %q", n)
	}
	bare := machine(func(m *infrav1.TerraformMachine) { m.Labels = nil })
	if n := ClusterName(bare, OwnerInfo{Cluster: cluster(false)}); n != "c1" {
		t.Errorf("from owner: %q", n)
	}
	if n := ClusterName(bare, OwnerInfo{}); n != "" {
		t.Errorf("unknown: %q", n)
	}
}

// without returns objs with every object of type T dropped.
func without[T client.Object](objs []client.Object) []client.Object {
	var out []client.Object
	for _, o := range objs {
		if _, ok := o.(T); !ok {
			out = append(out, o)
		}
	}
	return out
}

// --- Force-unlock -------------------------------------------------------

// TestBookkeepForceUnlock: a stale state lock held by a dead pod of this
// object's own Job (bookkeeping.go's ours/locks.Check) makes the next Job
// force-unlock it: --force-unlock=<lockID> in its runner args (run.go),
// EventForceUnlocked and the force-unlock metric. A lock held from a
// workstation is left alone, even with no such pod, because Who names no
// pod of this object's Jobs.
func TestBookkeepForceUnlock(t *testing.T) {
	t.Parallel()
	const lockID = "9f1c0c5e-0000-4000-8000-000000000001"
	suffix, err := state.Suffix(testNS, state.KindTerraformMachine, testName)
	if err != nil {
		t.Fatal(err)
	}
	deadPod := jobs.Name("m", testName, jobs.OpApply, 1, "abc123") + "-x7k2p"
	lease := func(who string) *coordinationv1.Lease {
		holder := lockID
		info := `{"ID":"` + lockID + `","Who":"` + who + `","Operation":"OperationTypeApply","Version":"1.16.4"}`
		return &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: testNS, Name: state.LeaseName(suffix),
				Annotations: map[string]string{locks.LockInfoAnnotation: info},
			},
			Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder},
		}
	}
	holderPod := func(phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: deadPod}, Status: corev1.PodStatus{Phase: phase}}
	}
	tests := []struct {
		name string
		who  string
		pod  *corev1.Pod
		want bool
	}{
		{"dead pod of this object's own Job: force-unlock", "runner@" + deadPod, nil, true},
		// The finished Job still exists, so its OOM-killed pod does too.
		{"holder pod Failed (OOM, eviction): force-unlock", "runner@" + deadPod, holderPod(corev1.PodFailed), true},
		{"holder pod still Running: left alone", "runner@" + deadPod, holderPod(corev1.PodRunning), false},
		{"workstation holder: left alone", "steven@laptop", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			objs := world(machine(withFinalizer, notPaused), lease(tt.who))
			if tt.pod != nil {
				objs = append(objs, tt.pod)
			}
			e := newEnv(t, objs...)
			r, reg := recorder(t)
			e.d.Metrics = r
			k := e.kindFor(t, readyOwner)
			k.in = machineIn()
			if _, err := reconcileOnce(t, e, k); err != nil {
				t.Fatal(err)
			}
			if len(e.runner.created) != 1 {
				t.Fatalf("created %v, want exactly one apply Job", e.runner.created)
			}
			args := e.runner.jobs[0].Spec.Template.Spec.Containers[0].Args
			if forced := slices.Contains(args, "--force-unlock="+lockID); forced != tt.want {
				t.Errorf("args %v, force-unlock wanted %v", args, tt.want)
			}
			if n := e.rec.count(EventForceUnlocked); (n == 1) != tt.want {
				t.Errorf("ForceUnlocked events = %d, want forced=%v", n, tt.want)
			}
			wantMetric := 0
			if tt.want {
				wantMetric = 1
			}
			if n, err := gatherAndCount(reg, metrics.ForceUnlocksName); err != nil || n != wantMetric {
				t.Errorf("force-unlock metric series = %d (%v), want %d", n, err, wantMetric)
			}
		})
	}
}

// --- Drift Job deadline (bookkeeping.go:265) --------------------------------

// TestSetDriftJobDeadlineExceeded: a drift or refresh Job that failed by
// exceeding its activeDeadlineSeconds sets DriftJobSucceeded False with
// DriftJobDeadlineExceededReason, distinct from a plain failure.
func TestSetDriftJobDeadlineExceeded(t *testing.T) {
	t.Parallel()
	deadline := job("d", jobs.OpDrift, jobs.Failed, t0)
	deadline.Status.Conditions[0].Reason = batchv1.JobReasonDeadlineExceeded
	m := machine()
	setDriftJob(m, []finished{{job: &deadline}})
	c := conditions.Get(m, infrav1.DriftJobSucceededCondition)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.DriftJobDeadlineExceededReason {
		t.Errorf("DriftJobSucceeded = %+v, want False/%s", c, infrav1.DriftJobDeadlineExceededReason)
	}

	// A plain failure (no deadline) still maps to DriftJobFailedReason.
	failed := job("d2", jobs.OpDrift, jobs.Failed, t0)
	m2 := machine()
	setDriftJob(m2, []finished{{job: &failed}})
	if c := conditions.Get(m2, infrav1.DriftJobSucceededCondition); c == nil || c.Reason != infrav1.DriftJobFailedReason {
		t.Errorf("DriftJobSucceeded = %+v, want %s", c, infrav1.DriftJobFailedReason)
	}
}

// --- pinDigest DigestUnknown (bookkeeping.go:283-305) -----------------------

// TestPinDigestDigestUnknown: a successful apply whose pod is gone (or
// reports no digest) keeps the previous pin; when the durable Secret has no
// digest either, nothing can be pinned and pinDigest warns once with
// EventDigestUnknown.
func TestPinDigestDigestUnknown(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	k := e.kindFor(t, readyOwner)
	if err := inputs.Write(t.Context(), e.c, k.obj, renderMachine(t), inputs.Meta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
		t.Fatal(err)
	}
	f := &finished{job: ptr(job("a1", jobs.OpApply, jobs.Succeeded, t0))} // no pod: f.pod is nil
	durable, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil {
		t.Fatal(err)
	}
	if pinned, err := pinDigest(t.Context(), e.d, k, f, durable); err != nil || pinned != "" {
		t.Fatalf("pinDigest = %q, %v", pinned, err)
	}
	if n := e.rec.count(EventDigestUnknown); n != 1 {
		t.Errorf("DigestUnknown events = %d, want 1", n)
	}
	durable, err = inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil || durable.Meta.ImageDigest != "" {
		t.Errorf("durable = %+v, %v; want the digest still unset", durable, err)
	}

	// Once a digest is pinned, a later succeeded Job whose pod is gone
	// keeps it quietly: no second warning.
	digest := "registry.example/mod@sha256:" + strings.Repeat("c", 64)
	if _, err := inputs.PinDigest(t.Context(), e.c, k.obj, digest, k.Mutable()); err != nil {
		t.Fatal(err)
	}
	durable.Meta.ImageDigest = digest
	if _, err := pinDigest(t.Context(), e.d, k, f, durable); err != nil {
		t.Fatal(err)
	}
	if n := e.rec.count(EventDigestUnknown); n != 1 {
		t.Errorf("DigestUnknown events = %d after a pin exists, want still 1 (no repeat warning)", n)
	}
}

// --- persistBlockMove no-op (run.go:193-202) --------------------------------

// TestPersistBlockMoveAlreadySet: an object that already carries block-move
// is not patched again; persistBlockMove returns without touching the
// stored object (its resourceVersion is unchanged).
func TestPersistBlockMoveAlreadySet(t *testing.T) {
	t.Parallel()
	withBlockMove := func(m *infrav1.TerraformMachine) { SetBlockMove(m) }
	e := newEnv(t, world(machine(withFinalizer, notPaused, withBlockMove))...)
	before := e.get(t)
	wantRV := before.ResourceVersion
	if err := persistBlockMove(t.Context(), e.c, before); err != nil {
		t.Fatal(err)
	}
	if after := e.get(t); after.ResourceVersion != wantRV {
		t.Errorf("resourceVersion changed from %s to %s: a patch was issued for an unchanged annotation", wantRV, after.ResourceVersion)
	}
}

// --- StartJob retry after a crash (run.go:153-161) --------------------------

// TestStartJobRetryAfterCrash: a Job that already exists (Create returns
// AlreadyExists, as after a crash between creating the Job and recording
// status.activeJob) is not an error: the existing Job becomes
// status.activeJob and its per-run Secret is (re)created, owned by it.
func TestStartJobRetryAfterCrash(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	suffix, err := state.Suffix(testNS, state.KindTerraformMachine, testName)
	if err != nil {
		t.Fatal(err)
	}
	req := JobRequest{
		Op: jobs.OpApply, Files: renderMachine(t), InputsHash: "h1:x",
		Source: infrav1.Source{Image: "registry.example/mod:1.0"}, Identity: testIdentity,
		Suffix: suffix, ClusterName: "c1", Attempt: 1,
	}
	k := e.kindFor(t, readyOwner)
	first, err := StartJob(t.Context(), e.d, k, req)
	if err != nil {
		t.Fatalf("first StartJob: %v", err)
	}
	// The real Job made it to the cluster (here, our fake client) before
	// the crash; give it a distinct UID so the assertions below can tell it
	// apart from anything a second, buggy create might make.
	existing := first.DeepCopy()
	existing.ResourceVersion = ""
	existing.UID = types.UID("existing-job-uid")
	if err := e.c.Create(t.Context(), existing); err != nil {
		t.Fatal(err)
	}
	e.runner.createErr = apierrors.NewAlreadyExists(schema.GroupResource{Resource: "jobs"}, first.Name)

	k2 := e.kindFor(t, readyOwner)
	second, err := StartJob(t.Context(), e.d, k2, req)
	if err != nil {
		t.Fatalf("second StartJob: %v", err)
	}
	if second.UID != existing.UID || second.Name != first.Name {
		t.Errorf("second StartJob returned %+v, want the existing Job %+v", second.ObjectMeta, existing.ObjectMeta)
	}
	if len(e.runner.jobs) != 1 {
		t.Errorf("fake runner holds %d Jobs, want 1 (no duplicate create)", len(e.runner.jobs))
	}
	if k2.obj.Status.ActiveJob.Name != first.Name {
		t.Errorf("activeJob = %+v, want the existing Job %s", k2.obj.Status.ActiveJob, first.Name)
	}
	runSecret := &corev1.Secret{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: inputs.RunName(first.Name)}, runSecret); err != nil {
		t.Fatalf("per-run Secret: %v", err)
	}
	if len(runSecret.OwnerReferences) != 1 || runSecret.OwnerReferences[0].UID != existing.UID {
		t.Errorf("run Secret owner = %+v, want the existing Job %s", runSecret.OwnerReferences, existing.UID)
	}
}

// --- RunnerRBACReady (reconcile.go:488-510) ---------------------------------

// TestReconcileRunnerRBACReady: an override ServiceAccount that exists but
// does not carry captf.io/runner=true fails RunnerRBACReady with the
// opt-in message, gating the Job (no Job starts); an unexpected error from
// the RBAC check is reported verbatim in the condition message and fails
// the reconcile.
func TestReconcileRunnerRBACReady(t *testing.T) {
	t.Parallel()
	t.Run("ServiceAccount not opted in", func(t *testing.T) {
		t.Parallel()
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "deployer"}}
		withOverride := func(m *infrav1.TerraformMachine) { m.Spec.Jobs = &infrav1.JobPolicy{ServiceAccountName: "deployer"} }
		e := newEnv(t, world(machine(withFinalizer, notPaused, withOverride), sa)...)
		k := e.kindFor(t, readyOwner)
		k.in = machineIn()
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		m := e.get(t)
		c := conditions.Get(m, infrav1.RunnerRBACReadyCondition)
		wantMsg := "ServiceAccount deployer does not carry " + rbac.RunnerLabel + "=true"
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.ServiceAccountNotOptedInReason || c.Message != wantMsg || len(e.runner.created) != 0 {
			t.Errorf("RunnerRBACReady = %+v, created %v", c, e.runner.created)
		}
	})
	t.Run("RBAC check errors: message reported, reconcile fails", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("boom")
		withOverride := func(m *infrav1.TerraformMachine) { m.Spec.Jobs = &infrav1.JobPolicy{ServiceAccountName: "deployer"} }
		s := testScheme(t)
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(world(machine(withFinalizer, notPaused, withOverride))...).
			WithStatusSubresource(&infrav1.TerraformMachine{}).
			WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.ServiceAccount); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			}}).Build()
		e := &env{c: c, runner: &fakeRunner{pods: map[string][]corev1.Pod{}}, state: &fakeState{}, rec: &fakeRecorder{}}
		e.d = Deps{
			Client: c, APIReader: c, Scheme: s, Jobs: e.runner, State: e.state, Recorder: e.rec,
			Clock: testingclock.NewFakePassiveClock(t0), RunnerImage: "registry.example/captf:dev", DriftDefault: 30 * time.Minute,
		}
		k := e.kindFor(t, readyOwner)
		k.in = machineIn()
		_, err := reconcileOnce(t, e, k)
		if err == nil {
			t.Fatal("Reconcile: want the RBAC check's error propagated")
		}
		m := e.get(t)
		c2 := conditions.Get(m, infrav1.RunnerRBACReadyCondition)
		if c2 == nil || c2.Status != metav1.ConditionFalse || c2.Reason != infrav1.RBACFailedReason || !strings.Contains(c2.Message, "boom") {
			t.Errorf("RunnerRBACReady = %+v, want RBACFailed carrying the error", c2)
		}
	})
}

// --- Mirror conflict (reconcile.go:565-575) ---------------------------------

// TestReconcileMirrorConflict: a same-named Secret that is not this
// identity's mirror (identity.ErrMirrorConflict) sets CredentialsMirrored
// False MirrorFailedReason, is never overwritten, and does not fail the
// reconcile (the conflict is reported, not fatal); no Job starts while
// credentials are not ready.
func TestReconcileMirrorConflict(t *testing.T) {
	t.Parallel()
	conflict := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: identity.MirrorName(testIdentity)}, Data: map[string][]byte{"x": []byte("y")}}
	e := newEnv(t, world(machine(withFinalizer, notPaused), conflict)...)
	k := e.kindFor(t, readyOwner)
	k.in = machineIn()
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	m := e.get(t)
	c := conditions.Get(m, infrav1.CredentialsMirroredCondition)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.MirrorFailedReason {
		t.Errorf("CredentialsMirrored = %+v", c)
	}
	if len(e.runner.created) != 0 {
		t.Errorf("created %v, want no Job while credentials are not ready", e.runner.created)
	}
	stored := &corev1.Secret{}
	if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(conflict), stored); err != nil || len(stored.Data) != 1 || string(stored.Data["x"]) != "y" {
		t.Errorf("conflicting Secret was overwritten: %+v, %v", stored, err)
	}
}

// --- readState errors (reconcile.go:600-613) --------------------------------

// TestReconcileStateReadErrors: state.ErrStateInconsistent maps to
// StateReadable False StateInconsistentReason; both ErrStateCorrupt and
// ErrUnsupportedStateVersion map to the same StateCorruptReason (an
// unsupported version is corrupt as far as CAPTF is concerned). Either way
// the reconcile requeues at StateRequeue and starts no Job.
func TestReconcileStateReadErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		reason string
	}{
		{"inconsistent chunk set", state.ErrStateInconsistent, infrav1.StateInconsistentReason},
		{"corrupt payload", state.ErrStateCorrupt, infrav1.StateCorruptReason},
		{"unsupported version", state.ErrUnsupportedStateVersion, infrav1.StateCorruptReason},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, world(machine(withFinalizer, notPaused))...)
			e.state.err = tt.err
			k := e.kindFor(t, readyOwner)
			k.in = machineIn()
			requeue, err := reconcileOnce(t, e, k)
			if err != nil {
				t.Fatal(err)
			}
			c := conditions.Get(e.get(t), infrav1.StateReadableCondition)
			if c == nil || c.Status != metav1.ConditionFalse || c.Reason != tt.reason || requeue != StateRequeue || len(e.runner.created) != 0 {
				t.Errorf("StateReadable = %+v, requeue %s, created %v", c, requeue, e.runner.created)
			}
		})
	}
}

// fakePoolKind is a fakeKind that also implements MembershipObserver.
type fakePoolKind struct {
	*fakeKind
	converging bool
}

// MembershipConverging returns f.converging.
func (f *fakePoolKind) MembershipConverging() bool { return f.converging }

// TestDecideInputMembership proves decideInput passes the resolved
// membership interval through, provisioned or not, and sets Converging only
// for a MembershipObserver with state: from its own report or a pending
// health reading. Kinds without the interface never converge.
func TestDecideInputMembership(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		observer bool
		reports  bool
		view     StateView
		want     bool
	}{
		{"observer converging", true, true, StateView{Exists: true}, true},
		{"observer pending", true, false, StateView{Exists: true, Pending: true}, true},
		{"observer converged", true, false, StateView{Exists: true}, false},
		{"observer without state", true, true, StateView{}, false},
		{"machine pending does not converge", false, false, StateView{Exists: true, Pending: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fk := &fakeKind{obj: machine(), mutable: true}
			var k Kind = fk
			if tt.observer {
				k = &fakePoolKind{fakeKind: fk, converging: tt.reports}
			}
			r := &reconciler{
				d: Deps{Clock: testingclock.NewFakePassiveClock(t0)}, k: k, obj: fk.obj, st: fk.Status(),
				eff: EffectiveConfig{MembershipRefreshInterval: time.Minute, HealthCheckInterval: 5 * time.Minute},
			}
			in := r.decideInput(&Bookkeeping{}, tt.view)
			if in.Converging != tt.want {
				t.Errorf("Converging = %v, want %v", in.Converging, tt.want)
			}
			// Not provisioned: health checks wait, membership does not.
			if in.MembershipInterval != time.Minute || in.HealthInterval != 0 {
				t.Errorf("MembershipInterval = %s, HealthInterval = %s; want 1m, 0", in.MembershipInterval, in.HealthInterval)
			}
		})
	}
}
