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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cbmetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/klog/v2/ktesting"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runlease"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// --- helpers ----------------------------------------------------------------

// noFuncs is an empty interceptor.Funcs, for a fake client with no
// injected failures.
var noFuncs = interceptor.Funcs{}

// resultCount returns the sum, failing t on error, of the series of the
// counter name in reg whose result label is result.
func resultCount(t *testing.T, reg cbmetrics.KubeRegistry, name, result string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "result" && l.GetValue() == result {
					total += m.GetCounter().GetValue()
				}
			}
		}
	}
	return total
}

// metricsHelp returns the help text of the series name, or "" when no such
// series is registered.
func metricsHelp(name string) string {
	for _, s := range metrics.Specs() {
		if s.Name == name {
			return s.Help
		}
	}
	return ""
}

// gzState returns a gzip-compressed Terraform state document of serial,
// failing t on error.
func gzState(t *testing.T, serial int64) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	fmt.Fprintf(w, `{"version":4,"terraform_version":"1.16.4","serial":%d,"lineage":"l1","outputs":{},"resources":[{"mode":"managed","type":"t","name":"a"}]}`, serial)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// suffixOf returns the backend suffix of the object of kind and name,
// failing t on error.
func suffixOf(t *testing.T, kind, name string) string {
	t.Helper()
	s, err := state.Suffix(testNS, kind, name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// stateSecret returns the base state Secret of suffix at serial, as the
// backend writes it, with inputs hash hash ("" for none), failing t on
// error.
func stateSecret(t *testing.T, suffix string, serial int64, hash string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: state.SecretName(suffix), Labels: map[string]string{
			state.BackendStateLabel: "true", state.BackendSuffixLabel: suffix, state.BackendWorkspaceLabel: state.Workspace,
		}},
		Data: map[string][]byte{state.DataKey: gzState(t, serial)},
	}
	if hash != "" {
		s.Annotations = map[string]string{state.InputsHashAnnotation: hash}
	}
	return s
}

// setState writes the state of suffix at serial, with inputs hash hash,
// replacing what is there, failing t on error.
func (e *env) setState(t *testing.T, suffix string, serial int64, hash string) {
	t.Helper()
	s := stateSecret(t, suffix, serial, hash)
	var live corev1.Secret
	if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(s), &live); err == nil {
		live.Data = s.Data
		if err := e.c.Update(t.Context(), &live); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := e.c.Create(t.Context(), s); err != nil {
		t.Fatal(err)
	}
}

// backupOf takes a backup of serial (with inputs hash hash) for the stored
// object of kind and name, then removes the state: the state is lost, the
// backup remains. It returns the backup, failing t on error.
func (e *env) backupOf(t *testing.T, kind, name string, serial int64, hash string) state.Backup {
	t.Helper()
	suffix := suffixOf(t, kind, name)
	e.setState(t, suffix, serial, hash)
	m := &infrav1.TerraformMachine{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, m); err != nil {
		t.Fatal(err)
	}
	b, created, err := state.TakeBackup(t.Context(), e.c, state.BackupOptions{
		Owner: m, OwnerKind: kind, ClusterName: "c1", Suffix: suffix, Now: t0.Add(-time.Duration(10-serial) * time.Hour),
	})
	if err != nil || !created {
		t.Fatalf("backup: %v, %v", created, err)
	}
	if err := state.Cleanup(t.Context(), e.c, testNS, suffix); err != nil {
		t.Fatal(err)
	}
	return b
}

// restoring returns a machine-mutator that sets the restore-state
// annotation to serial.
func restoring(serial string) func(*infrav1.TerraformMachine) {
	return func(m *infrav1.TerraformMachine) {
		if m.Annotations == nil {
			m.Annotations = map[string]string{}
		}
		m.Annotations[infrav1.RestoreStateAnnotation] = serial
	}
}

// healthyKind returns e's fakeKind adapter for the object named name, set
// to a definite, healthy reading, failing t on error.
func healthyKind(t *testing.T, e *env, name string) *fakeKind {
	t.Helper()
	k := e.kindNamed(t, name)
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	return k
}

// restoreJobs returns e's restore Jobs, failing t on error.
func restoreJobs(t *testing.T, e *env) []batchv1.Job {
	t.Helper()
	return slices.DeleteFunc(e.jobsOf(t), func(j batchv1.Job) bool { return jobs.OpOf(&j) != jobs.OpRestore })
}

// restoreReason returns m's RestoreJobSucceeded condition's reason, or ""
// when the condition is unset.
func restoreReason(m *infrav1.TerraformMachine) string {
	if c := conditions.Get(m, infrav1.RestoreJobSucceededCondition); c != nil {
		return c.Reason
	}
	return ""
}

// restoreEnv returns, failing t on error, a provisioned machine m1 whose
// state was lost after a backup of serial 5 (inputs hash h1:backup), with
// the restore-state annotation set to annotation (skipped when "") and
// each mut applied to the machine; Jobs live in the fake client, the state
// reader is the real one. It also returns that clientRunner.
func restoreEnv(t *testing.T, annotation string, mut ...func(*infrav1.TerraformMachine)) (*env, *clientRunner) {
	t.Helper()
	mut = append([]func(*infrav1.TerraformMachine){withFinalizer, notPaused, provisioned}, mut...)
	if annotation != "" {
		mut = append(mut, restoring(annotation))
	}
	e := newEnv(t, world(machine(mut...))...)
	jr := &clientRunner{c: e.c}
	e.d.Jobs = jr
	e.d.State = state.NewReader(e.c)
	e.backupOf(t, state.KindTerraformMachine, testName, 5, "h1:backup")
	return e, jr
}

// --- DecideOp ---------------------------------------------------------------

// TestDecideOpRestorePrecedence proves DecideOp ranks a requested restore
// above an input change, no state and the op's own backoff, but below
// deletion (with state, or without one that never applied) and an
// already-active Job; a deletion held on a lost or unreadable state
// restores.
func TestDecideOpRestorePrecedence(t *testing.T) {
	t.Parallel()
	applied := StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:b"}
	for _, tt := range []struct {
		name string
		in   DecideInput
		want Decision
	}{
		{"restore beats an input change", DecideInput{Mutable: true, State: applied, Restore: true, Now: t0},
			Decision{Action: ActionJob, Op: jobs.OpRestore, Reason: ReasonRestoreRequested}},
		{"restore beats no state", DecideInput{Restore: true, Now: t0}, Decision{Action: ActionJob, Op: jobs.OpRestore, Reason: ReasonRestoreRequested}},
		{"restore ignores the op's backoff", DecideInput{State: applied, Restore: true, Now: t0, Jobs: JobsView{
			Failures: map[jobs.Op]int{jobs.OpRestore: 2}, LastFailure: map[jobs.Op]time.Time{jobs.OpRestore: t0}, FailedLimit: 3,
		}}, Decision{Action: ActionJob, Op: jobs.OpRestore, Reason: ReasonRestoreRequested}},
		{"deletion beats restore", DecideInput{Deleting: true, State: applied, Restore: true, Now: t0},
			Decision{Action: ActionJob, Op: jobs.OpDestroy, Reason: "Deleting"}},
		{"deletion without state, never applied, beats restore", DecideInput{Deleting: true, Restore: true, Now: t0},
			Decision{Action: ActionDropFinalizer, Reason: "DeletingWithoutState"}},
		{"deletion held on a lost state restores", DecideInput{Deleting: true, StateHeld: true, Restore: true, Now: t0},
			Decision{Action: ActionJob, Op: jobs.OpRestore, Reason: ReasonRestoreRequested}},
		{"deletion held on an unreadable state restores", DecideInput{Deleting: true, StateHeld: true, State: StateView{Exists: true}, Restore: true, Now: t0},
			Decision{Action: ActionJob, Op: jobs.OpRestore, Reason: ReasonRestoreRequested}},
		{"a held deletion without a restore waits", DecideInput{Deleting: true, StateHeld: true, Now: t0},
			Decision{RequeueAfter: StateRequeue, Reason: ReasonDeletionHeld}},
		{"a running Job beats restore", DecideInput{Restore: true, Jobs: JobsView{Active: true}, Now: t0},
			Decision{RequeueAfter: ActiveJobRequeue, Reason: "JobActive"}},
	} {
		if got := DecideOp(tt.in); got != tt.want {
			t.Errorf("%s: DecideOp = %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

// --- Backups ----------------------------------------------------------------

// TestReconcileBacksUpEachNewSerialOnce: a backup per new state serial,
// none for an unchanged state, pruning to --state-backups, and
// status.stateBackups, the event and the metric in step.
func TestReconcileBacksUpEachNewSerialOnce(t *testing.T) {
	t.Parallel()
	r, reg := recorder(t)
	e, _ := leaseEnv(t, false, noFuncs)
	e.d.Metrics, e.d.State, e.d.StateBackups = r, state.NewReader(e.c), 2
	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	pass := func() {
		t.Helper()
		if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
			t.Fatal(err)
		}
	}
	e.setState(t, suffix, 7, "h1:x")
	pass()
	pass()
	if n := e.rec.count(EventStateBackedUp); n != 1 {
		t.Fatalf("StateBackedUp events after two passes on serial 7 = %d, want 1", n)
	}
	for _, serial := range []int64{8, 9} {
		e.setState(t, suffix, serial, "h1:x")
		pass()
	}
	list, err := state.ListBackups(t.Context(), e.c, testNS, suffix)
	if err != nil || len(list) != 2 || list[0].Serial != 9 || list[1].Serial != 8 || list[0].InputsHash != "h1:x" {
		t.Fatalf("backups = %+v, %v", list, err)
	}
	m := e.get(t)
	if got := m.Status.StateBackups; len(got) != 2 || got[0].Serial != 9 || got[1].Serial != 8 || got[0].Bytes == 0 || got[0].TakenAt == nil {
		t.Errorf("status.stateBackups = %+v", got)
	}
	if n := e.rec.count(EventStateBackedUp); n != 3 {
		t.Errorf("StateBackedUp events = %d, want 3", n)
	}
	want := `
# HELP captf_state_backups_total [ALPHA] ` + metricsHelp(metrics.StateBackupsName) + `
# TYPE captf_state_backups_total counter
captf_state_backups_total{kind="TerraformMachine",result="pruned"} 1
captf_state_backups_total{kind="TerraformMachine",result="taken"} 3
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), metrics.StateBackupsName); err != nil {
		t.Error(err)
	}
}

// TestReconcileBackupRetriedAfterAPIError: a failed backup create is not a
// skipped state: the serial is observed again next pass and backed up then.
func TestReconcileBackupRetriedAfterAPIError(t *testing.T) {
	t.Parallel()
	failed := false
	funcs := interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if obj.GetLabels()[state.BackupLabel] == "true" && !failed {
			failed = true
			return apierrors.NewServiceUnavailable("etcd is busy")
		}
		return c.Create(ctx, obj, opts...)
	}}
	e := newEnvWith(t, funcs, world(machine(withFinalizer, notPaused, provisioned))...)
	e.d.State, e.d.StateBackups = state.NewReader(e.c), 5
	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	e.setState(t, suffix, 7, "h1:x")
	for pass := range 2 {
		k := e.kindFor(t, readyOwner())
		k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		if pass == 0 && e.get(t).Status.ObservedStateSerial == 7 {
			t.Error("a failed backup recorded the serial as observed")
		}
	}
	if list, _ := state.ListBackups(t.Context(), e.c, testNS, suffix); len(list) != 1 || list[0].Serial != 7 || !failed {
		t.Errorf("backups = %+v, want serial 7 taken on the retry", list)
	}
	if e.get(t).Status.ObservedStateSerial != 7 {
		t.Error("the serial was not observed after the retry")
	}
}

// TestReconcilePruneSparesRequestedRestore: the backup a pending restore
// names outlives --state-backups newer serials.
func TestReconcilePruneSparesRequestedRestore(t *testing.T) {
	t.Parallel()
	e, _ := leaseEnv(t, false, noFuncs)
	e.d.State, e.d.StateBackups = state.NewReader(e.c), 1
	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	e.backupOf(t, state.KindTerraformMachine, testName, 5, "h1:x")
	m := e.get(t)
	restoring("5")(m)
	if err := e.c.Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	logger, _ := ktesting.NewTestContext(t)
	for _, serial := range []int64{6, 7} {
		e.setState(t, suffix, serial, "h1:x")
		k := healthyKind(t, e, testName)
		r := &reconciler{d: e.d, k: k, obj: k.obj, st: k.Status(), suffix: suffix, logger: logger}
		r.backupState(t.Context(), &Bookkeeping{})
	}
	list, _ := state.ListBackups(t.Context(), e.c, testNS, suffix)
	var serials []int64
	for _, b := range list {
		serials = append(serials, b.Serial)
	}
	if !slices.Equal(serials, []int64{7, 5}) {
		t.Errorf("backups = %v, want the newest (7) and the requested (5)", serials)
	}
}

// TestReconcileBackupsOff: --state-backups=0 takes none.
func TestReconcileBackupsOff(t *testing.T) {
	t.Parallel()
	e, _ := leaseEnv(t, false, noFuncs)
	e.d.State = state.NewReader(e.c)
	e.setState(t, suffixOf(t, state.KindTerraformMachine, testName), 7, "h1:x")
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	if list, _ := state.ListBackups(t.Context(), e.c, testNS, suffixOf(t, state.KindTerraformMachine, testName)); len(list) != 0 || e.rec.count(EventStateBackedUp) != 0 {
		t.Errorf("backups = %+v with the flag at 0", list)
	}
}

// TestReconcileBackupSkipsUnreadable: a state the backup cannot parse (here
// the reader seam says serial 7, the Secret is garbage) is skipped and
// counted, and the reconcile goes on; an encrypted state is never read far
// enough to be backed up.
func TestReconcileBackupSkipsUnreadable(t *testing.T) {
	t.Parallel()
	r, reg := recorder(t)
	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	garbage := stateSecret(t, suffix, 7, "h1:x")
	garbage.Data[state.DataKey] = []byte("not gzip")
	e := newEnv(t, world(machine(withFinalizer, notPaused, provisioned), garbage)...)
	e.d.Metrics, e.d.StateBackups = r, 5
	e.state.st = &state.State{Serial: 7, InputsHash: "h1:x"}
	k := e.kindFor(t, readyOwner())
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatalf("a skipped backup failed the reconcile: %v", err)
	}
	if n := resultCount(t, reg, metrics.StateBackupsName, metrics.BackupSkipped); n != 1 {
		t.Errorf("skipped = %v, want 1", n)
	}

	enc := newEnv(t, world(machine(withFinalizer, notPaused, provisioned), stateSecret(t, suffix, 7, "h1:x"))...)
	enc.d.StateBackups = 5
	enc.state.err = state.ErrStateEncrypted
	if _, err := reconcileOnce(t, enc, enc.kindFor(t, readyOwner())); err != nil {
		t.Fatal(err)
	}
	if list, _ := state.ListBackups(t.Context(), enc.c, testNS, suffix); len(list) != 0 {
		t.Errorf("an encrypted state was backed up: %+v", list)
	}
}

// --- Restore ----------------------------------------------------------------

// TestReconcileRestoreLifecycle: a lost state with a restore annotation
// starts a restore Job under the run lease; its success removes the
// annotation, emits StateRestored once, adopts the backup's inputs hash,
// and the restored state reads normally (and is backed up again as a new
// serial).
func TestReconcileRestoreLifecycle(t *testing.T) {
	t.Parallel()
	r, reg := recorder(t)
	e, jr := restoreEnv(t, "5")
	e.d.Metrics, e.d.StateBackups = r, 5
	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	k := healthyKind(t, e, testName)
	if _, err := Reconcile(t.Context(), e.d, k); err != nil {
		t.Fatal(err)
	}
	restores := restoreJobs(t, e)
	if jr.created.Load() != 1 || len(restores) != 1 {
		t.Fatalf("created %d Jobs, restores %d", jr.created.Load(), len(restores))
	}
	job := restores[0]
	if !strings.Contains(job.Name, "-restore-a1-") || job.Annotations[jobs.RestoreSerialAnnotation] != "5" ||
		job.Annotations[state.InputsHashAnnotation] != "h1:backup" {
		t.Errorf("restore Job %s annotations %v", job.Name, job.Annotations)
	}
	args := job.Spec.Template.Spec.Containers[0].Args
	if !slices.Contains(args, "--op=restore") || !slices.Contains(args, "--restore-chunks=1") || !slices.Contains(args, "--restore-resources=1") {
		t.Errorf("args = %v", args)
	}
	if l := e.lease(t, runlease.RunName(suffix)); l == nil || runlease.HolderOf(l) != job.Name {
		t.Errorf("run lease = %+v, want held by %s", l, job.Name)
	}
	run := &corev1.Secret{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: inputs.RunName(job.Name)}, run); err != nil ||
		!bytes.Equal(run.Data[inputs.MainTFKey], render.BackendRoot().MainTF) {
		t.Errorf("per-run Secret = %v, %v; want the backend-only root", run.Data, err)
	}
	m := e.get(t)
	if m.Status.ActiveJob.Operation != infrav1.OperationRestore || m.Annotations[infrav1.RestoreStateAnnotation] != "5" {
		t.Errorf("activeJob %+v, annotations %v", m.Status.ActiveJob, m.Annotations)
	}
	if c := conditions.Get(m, infrav1.StateReadableCondition); c == nil || c.Reason != infrav1.StateLostReason {
		t.Errorf("StateReadable = %+v", c)
	}

	// The Job pushed the backup; the backend continued its serial.
	e.finishJob(t, job.Name, jobs.Succeeded)
	e.setState(t, suffix, 5, "")
	for range 2 {
		if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
			t.Fatal(err)
		}
	}
	m = e.get(t)
	if _, ok := m.Annotations[infrav1.RestoreStateAnnotation]; ok {
		t.Error("the restore annotation was not consumed")
	}
	if c := conditions.Get(m, infrav1.RestoreJobSucceededCondition); c == nil || c.Status != metav1.ConditionTrue || c.Reason != infrav1.StateRestoredReason {
		t.Errorf("RestoreJobSucceeded = %+v", c)
	}
	if c := conditions.Get(m, infrav1.StateReadableCondition); c == nil || c.Status != metav1.ConditionTrue || m.Status.ObservedStateSerial != 5 {
		t.Errorf("StateReadable = %+v, observed serial %d", c, m.Status.ObservedStateSerial)
	}
	if m.Status.LastRun.Operation != infrav1.OperationRestore || m.Status.LastRun.Job != job.Name {
		t.Errorf("lastRun = %+v", m.Status.LastRun)
	}
	base := &corev1.Secret{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: state.SecretName(suffix)}, base); err != nil ||
		base.Annotations[state.InputsHashAnnotation] != "h1:backup" || len(base.OwnerReferences) != 1 {
		t.Errorf("restored state not adopted: %+v, %v", base.ObjectMeta, err)
	}
	if n := e.rec.count(EventStateRestored); n != 1 {
		t.Errorf("StateRestored events = %d, want 1", n)
	}
	if len(restoreJobs(t, e)) != 1 || e.lease(t, runlease.RunName(suffix)) != nil && runlease.HolderOf(e.lease(t, runlease.RunName(suffix))) == job.Name {
		t.Errorf("a second restore started, or the run lease was not released")
	}
	if n := resultCount(t, reg, metrics.StateRestoresName, metrics.ResultSucceeded); n != 1 {
		t.Errorf("restores succeeded = %v, want 1", n)
	}
}

// TestReconcileRestoreFailureNotRetried: a failed restore is reported once
// and not retried for the same serial (status.lastRestoredSerial), even
// once the failed Job is deleted; withdrawing the annotation and setting
// it again retries it.
func TestReconcileRestoreFailureNotRetried(t *testing.T) {
	t.Parallel()
	e, jr := restoreEnv(t, "5")
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	first := restoreJobs(t, e)[0]
	e.finishJob(t, first.Name, jobs.Failed)
	for range 3 {
		if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
			t.Fatal(err)
		}
	}
	m := e.get(t)
	if jr.created.Load() != 1 || restoreReason(m) != infrav1.RestoreFailedReason || m.Annotations[infrav1.RestoreStateAnnotation] != "5" {
		t.Fatalf("created %d, RestoreJobSucceeded %s, annotations %v", jr.created.Load(), restoreReason(m), m.Annotations)
	}
	if n := e.rec.count(EventStateRestoreFailed); n != 1 || m.Status.LastRestoredSerial != 5 {
		t.Errorf("StateRestoreFailed events = %d, want 1; lastRestoredSerial %d", n, m.Status.LastRestoredSerial)
	}
	if err := e.c.Delete(t.Context(), &first); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	if jr.created.Load() != 1 {
		t.Fatalf("deleting the failed Job retried the restore (created %d)", jr.created.Load())
	}

	m = e.get(t)
	delete(m.Annotations, infrav1.RestoreStateAnnotation)
	if err := e.c.Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	if n := e.get(t).Status.LastRestoredSerial; n != 0 {
		t.Fatalf("withdrawing the annotation left lastRestoredSerial %d", n)
	}
	e.annotate(t, infrav1.RestoreStateAnnotation, "5")
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	if jr.created.Load() != 2 {
		t.Errorf("setting the annotation again did not retry the restore (created %d)", jr.created.Load())
	}
}

// TestReconcileRestoreNotRepeatedFromStaleObject: once a restore succeeded
// and its annotation was consumed, a reconcile of an object read before
// that (a lagging cache: the annotation still set, lastRestoredSerial not
// yet) starts no second restore of the serial; after the annotation is
// seen gone, setting it again is a new request.
func TestReconcileRestoreNotRepeatedFromStaleObject(t *testing.T) {
	t.Parallel()
	e, jr := restoreEnv(t, "5")
	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	restores := restoreJobs(t, e)
	if len(restores) != 1 {
		t.Fatalf("restores %d", len(restores))
	}
	stale := e.get(t)
	e.finishJob(t, restores[0].Name, jobs.Succeeded)
	e.setState(t, suffix, 5, "")
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	if m := e.get(t); m.Annotations[infrav1.RestoreStateAnnotation] != "" || m.Status.LastRestoredSerial != 5 {
		t.Fatalf("annotations %v, lastRestoredSerial %d", m.Annotations, m.Status.LastRestoredSerial)
	}

	k := healthyKind(t, e, testName)
	k.obj = stale
	if _, err := Reconcile(t.Context(), e.d, k); err != nil {
		t.Fatal(err)
	}
	if jr.created.Load() != 1 {
		t.Fatalf("a stale object restored serial 5 again (created %d)", jr.created.Load())
	}

	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	if n := e.get(t).Status.LastRestoredSerial; n != 0 {
		t.Fatalf("lastRestoredSerial %d after the annotation was seen gone", n)
	}
	e.annotate(t, infrav1.RestoreStateAnnotation, "5")
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	if jr.created.Load() != 2 {
		t.Errorf("a new request of serial 5 did not restore (created %d)", jr.created.Load())
	}
}

// TestReconcileFailedRestoreNotRepeatedFromStaleObject: once a failed
// restore was consumed (its Job bookkept, its serial recorded in
// status.lastRestoredSerial), a reconcile of an object read before that
// (the annotation still set, lastRestoredSerial not yet) with the Job
// cache already showing the Job bookkept starts no second restore: the
// live object's lastRestoredSerial stops it.
func TestReconcileFailedRestoreNotRepeatedFromStaleObject(t *testing.T) {
	t.Parallel()
	e, jr := restoreEnv(t, "5")
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	restores := restoreJobs(t, e)
	if len(restores) != 1 {
		t.Fatalf("restores %d", len(restores))
	}
	stale := e.get(t)
	e.finishJob(t, restores[0].Name, jobs.Failed)
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	if m := e.get(t); m.Annotations[infrav1.RestoreStateAnnotation] != "5" || m.Status.LastRestoredSerial != 5 {
		t.Fatalf("annotations %v, lastRestoredSerial %d", m.Annotations, m.Status.LastRestoredSerial)
	}

	k := healthyKind(t, e, testName)
	k.obj = stale
	if _, err := Reconcile(t.Context(), e.d, k); err != nil {
		t.Fatal(err)
	}
	if jr.created.Load() != 1 {
		t.Errorf("a stale object retried the failed restore of serial 5 (created %d)", jr.created.Load())
	}
}

// TestReconcileRestoreBackupNotFound: a serial without a backup, or not a
// serial, sets RestoreBackupNotFound and starts nothing; the lost state
// stays lost.
func TestReconcileRestoreBackupNotFound(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"9", "latest"} {
		r, reg := recorder(t)
		e, jr := restoreEnv(t, value)
		e.d.Metrics = r
		for range 2 {
			if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
				t.Fatal(err)
			}
		}
		m := e.get(t)
		c := conditions.Get(m, infrav1.RestoreJobSucceededCondition)
		if jr.created.Load() != 0 || c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.RestoreBackupNotFoundReason {
			t.Errorf("%s: created %d, RestoreJobSucceeded %+v", value, jr.created.Load(), c)
		}
		if n := resultCount(t, reg, metrics.StateRestoresName, metrics.RestoreNotFound); n != 1 {
			t.Errorf("%s: not_found = %v, want 1", value, n)
		}
		if value == "9" && (len(m.Status.StateBackups) != 1 || m.Status.StateBackups[0].Serial != 5) {
			t.Errorf("status.stateBackups = %+v", m.Status.StateBackups)
		}
		// Withdrawing the request clears the report.
		delete(m.Annotations, infrav1.RestoreStateAnnotation)
		if err := e.c.Update(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
			t.Fatal(err)
		}
		if c := conditions.Get(e.get(t), infrav1.RestoreJobSucceededCondition); c != nil {
			t.Errorf("%s: RestoreJobSucceeded after the annotation was removed = %+v", value, c)
		}
	}
}

// TestReconcileDeletionBeatsRestore: a deleting object whose state reads
// never restores; it destroys.
func TestReconcileDeletionBeatsRestore(t *testing.T) {
	t.Parallel()
	e, _ := restoreEnv(t, "5", deleting)
	e.setState(t, suffixOf(t, state.KindTerraformMachine, testName), 6, "h1:backup")
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	if n := len(restoreJobs(t, e)); n != 0 {
		t.Errorf("%d restore Jobs while deleting", n)
	}
}

// TestReconcileClusterRestoreTakesWriteLease: under the cluster operation
// gate, a TerraformCluster's restore holds the cluster write lease like its
// apply.
func TestReconcileClusterRestoreTakesWriteLease(t *testing.T) {
	t.Parallel()
	e, jr := leaseEnv(t, true, noFuncs)
	e.d.State = state.NewReader(e.c)
	e.backupOf(t, state.KindTerraformCluster, "tc", 3, "h1:c")
	tc := &infrav1.TerraformMachine{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "tc"}, tc); err != nil {
		t.Fatal(err)
	}
	restoring("3")(tc)
	if err := e.c.Update(t.Context(), tc); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, "tc")); err != nil {
		t.Fatal(err)
	}
	restores := restoreJobs(t, e)
	if jr.created.Load() != 1 || len(restores) != 1 {
		t.Fatalf("created %d, restores %v", jr.created.Load(), restores)
	}
	if l := e.lease(t, runlease.ClusterName(testNS, "c1")); l == nil || runlease.HolderOf(l) != restores[0].Name {
		t.Errorf("cluster write lease = %+v, want held by %s", l, restores[0].Name)
	}
}

// TestReconcileRestoreWaitsForRunLease: another live holder of the run
// lease makes the restore wait on RestoreJobSucceeded, not the apply's
// condition.
func TestReconcileRestoreWaitsForRunLease(t *testing.T) {
	t.Parallel()
	e, jr := restoreEnv(t, "5")
	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	if err := e.c.Create(t.Context(), foreignLease(runlease.RunName(suffix), "other-job", jobs.OpApply, t0)); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Create(t.Context(), jobObj("other-job", jobs.Running)); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	m := e.get(t)
	if jr.created.Load() != 0 || restoreReason(m) != infrav1.WaitingForRunLeaseReason || applyReason(m) == infrav1.WaitingForRunLeaseReason {
		t.Errorf("created %d, restore %s, apply %s", jr.created.Load(), restoreReason(m), applyReason(m))
	}
	if n := e.rec.count(EventWaitingForRunLease); n != 1 {
		t.Errorf("WaitingForRunLease events = %d, want 1", n)
	}
}

// TestReconcileRestoreDeferredByPause: a Cluster paused live after the
// pass read it defers the restore's start (ErrStartDeferred): no Job, no
// run lease kept, and a quiet requeue rather than a reconcile error.
func TestReconcileRestoreDeferredByPause(t *testing.T) {
	t.Parallel()
	e, jr := restoreEnv(t, "5")
	if err := e.c.Create(t.Context(), cluster(true)); err != nil {
		t.Fatal(err)
	}
	res, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName))
	if err != nil || res.RequeueAfter != LagRequeue {
		t.Fatalf("Reconcile = %+v, %v; want a quiet requeue after %s", res, err, LagRequeue)
	}
	if jr.created.Load() != 0 {
		t.Errorf("created %d Jobs while the Cluster is paused, want none", jr.created.Load())
	}
	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	if l := e.lease(t, runlease.RunName(suffix)); l != nil {
		t.Errorf("the run lease of a restore never created is held by %s", runlease.HolderOf(l))
	}
}

// mergingGet is an interceptor Get that reads the object at key through
// c using ctx and decodes its JSON into obj without clearing obj first,
// as the real API reader does: what the response omits keeps obj's
// value. It returns any error from the read or the JSON round trip.
func mergingGet(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	stored, err := zeroOf(obj)
	if err != nil {
		return err
	}
	if err := c.Get(ctx, key, stored); err != nil {
		return err
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, obj)
}

// TestLiveRestoreReadsFreshObject: the live read of the restore request
// does not inherit the cached object's fields, so a withdrawn annotation
// and a lastRestoredSerial gone back to zero (both omitted from the
// response) are seen as such, and a live request is seen.
func TestLiveRestoreReadsFreshObject(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		live       string
		wantSerial int64
	}{
		{"withdrawn", "", 0},
		{"requested", "7", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stored := machine(withFinalizer, notPaused)
			if tt.live != "" {
				restoring(tt.live)(stored)
			}
			e := newEnvWith(t, interceptor.Funcs{Get: mergingGet}, stored)
			cached := e.get(t)
			restoring("5")(cached)
			cached.Status.LastRestoredSerial = 5
			r := &reconciler{d: e.d, obj: cached}
			got, serial, err := r.liveRestore(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.live || serial != tt.wantSerial {
				t.Errorf("liveRestore = %q, %d; want %q, %d", got, serial, tt.live, tt.wantSerial)
			}
		})
	}
}
