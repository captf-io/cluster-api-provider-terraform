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
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// restoreSerial returns the backup serial job, a restore Job, pushes, 0
// when unknown.
func restoreSerial(job *batchv1.Job) int64 {
	n, _ := strconv.ParseInt(job.Annotations[jobs.RestoreSerialAnnotation], 10, 64)
	return n
}

// restores finds the newest finished restore Job in done, sets
// RestoreJobSucceeded on k's object from it, and emits, through d,
// StateRestored or StateRestoreFailed (and counts
// captf_state_restores_total) once per restore Job, when it is counted.
// Without a restore Job, a lease wait left over from a restore that did not
// start is cleared: the reconcile sets it again if it still waits.
func (bk *Bookkeeping) restores(d Deps, k Kind, done []finished) {
	obj := k.Object()
	for i := range done {
		f := &done[i]
		if jobs.OpOf(f.job) != jobs.OpRestore {
			continue
		}
		if f.counted {
			recordRestore(d, k.Kind(), obj, f)
		}
		if bk.lastRestore == nil {
			bk.lastRestore = f
			conditions.Set(obj, restoreCondition(f))
		}
	}
	if bk.lastRestore == nil {
		if c := conditions.Get(obj, infrav1.RestoreJobSucceededCondition); c != nil && leaseWaitEvents[c.Reason] != "" {
			conditions.Delete(obj, infrav1.RestoreJobSucceededCondition)
		}
	}
}

// restoreCondition returns the RestoreJobSucceeded condition for f, a
// finished restore Job.
func restoreCondition(f *finished) metav1.Condition {
	serial := restoreSerial(f.job)
	c := metav1.Condition{Type: infrav1.RestoreJobSucceededCondition, Message: "Job " + f.job.Name}
	if f.ok {
		c.Status, c.Reason = metav1.ConditionTrue, infrav1.StateRestoredReason
		c.Message += fmt.Sprintf(": pushed the backup of state serial %d into the backend", serial)
		return c
	}
	c.Status, c.Reason = metav1.ConditionFalse, infrav1.RestoreFailedReason
	switch {
	case jobs.DeadlineExceeded(f.job):
		c.Message += ": exceeded activeDeadlineSeconds"
	case f.result != nil && f.result.Error != nil && f.result.Error.Step != nil:
		c.Message += ": step " + *f.result.Error.Step + " failed"
	default:
		c.Message += ": failed"
	}
	c.Message += fmt.Sprintf(". Serial %d is not restored again while %s names it: set it to another serial, "+
		"or remove it and, once status.lastRestoredSerial is cleared, set it again to retry", serial, infrav1.RestoreStateAnnotation)
	return c
}

// recordRestore emits, through d, the event and metric of f, a restore
// Job of obj, a kind, counted now.
func recordRestore(d Deps, kind string, obj Object, f *finished) {
	serial := restoreSerial(f.job)
	if f.ok {
		d.Metrics.StateRestore(kind, metrics.ResultSucceeded)
		d.EmitRelated(obj, f.job, corev1.EventTypeNormal, EventStateRestored, "Restore",
			"Job %s restored the state backup of serial %d. Resources created after that backup are no longer managed by the state; run a drift check or plan to see the difference",
			f.job.Name, serial)
		return
	}
	d.Metrics.StateRestore(kind, metrics.ResultFailed)
	d.EmitRelated(obj, f.job, corev1.EventTypeWarning, EventStateRestoreFailed, "Restore",
		"Job %s failed to restore the state backup of serial %d; it is not retried for that serial (see the Job's logs)", f.job.Name, serial)
}

// consumeRestore consumes bk's newest restore Job once, while it is not
// yet bookkept: it records the Job's serial in status.lastRestoredSerial,
// succeeded or failed, so restoreTarget does not run that serial again,
// and after a success removes the restore annotation naming it, logging
// with ctx. It returns any error from removing the annotation
// (removeAnnotation).
func (r *reconciler) consumeRestore(ctx context.Context, bk *Bookkeeping) error {
	f := bk.lastRestore
	if f == nil || f.bookkept {
		return nil
	}
	if n := restoreSerial(f.job); n > 0 {
		r.st.LastRestoredSerial = n
	}
	raw := r.annotation(infrav1.RestoreStateAnnotation)
	if raw == "" || !f.ok {
		return nil
	}
	serial, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || serial != restoreSerial(f.job) {
		return nil
	}
	if err := r.removeAnnotation(ctx, infrav1.RestoreStateAnnotation); err != nil {
		return err
	}
	klog.FromContext(ctx).Info("The state restore succeeded; removed its annotation", "stateSerial", serial, "Job", f.job.Name)
	return nil
}

// restoreTarget returns, using ctx, the backup a requested restore pushes,
// and whether one should start: the annotation names the serial of an
// existing, complete backup that is not status.lastRestoredSerial (a
// restore Job of it was consumed: a failed one is not retried, and a
// successful one removed the annotation), and the API server still has
// the annotation and records another lastRestoredSerial (the object cache
// may predate the annotation's removal or the serial's record). A withdrawn
// annotation clears status.lastRestoredSerial, so setting it again to the
// same serial is a new request. A serial without a backup sets
// RestoreJobSucceeded=False/RestoreBackupNotFound. Deletion wins: nothing
// is restored while deleting, unless held is true (the state is lost or
// unreadable, so the deletion waits for the restore). It returns any error
// listing the backups or reading the object.
func (r *reconciler) restoreTarget(ctx context.Context, held bool) (state.Backup, bool, error) {
	raw := r.annotation(infrav1.RestoreStateAnnotation)
	if raw == "" {
		// A request withdrawn without a restore leaves nothing to report.
		if c := conditions.Get(r.obj, infrav1.RestoreJobSucceededCondition); c != nil && c.Reason == infrav1.RestoreBackupNotFoundReason {
			conditions.Delete(r.obj, infrav1.RestoreJobSucceededCondition)
		}
		// Consumed this pass: the serial is recorded until the next pass
		// sees the annotation gone.
		if !r.consumed[infrav1.RestoreStateAnnotation] {
			r.st.LastRestoredSerial = 0
		}
		return state.Backup{}, false, nil
	}
	if r.deleting && !held {
		return state.Backup{}, false, nil
	}
	serial, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || serial < 1 {
		r.restoreNotFound(fmt.Sprintf("%s=%q is not a state serial; status.stateBackups lists the backups", infrav1.RestoreStateAnnotation, raw))
		return state.Backup{}, false, nil
	}
	if serial == r.st.LastRestoredSerial {
		return state.Backup{}, false, nil
	}
	backups, err := state.ListBackups(ctx, r.d.Client, r.obj.GetNamespace(), r.suffix)
	if err != nil {
		return state.Backup{}, false, err
	}
	r.recordBackups(backups)
	b, ok := state.FindBackup(backups, serial)
	if !ok {
		r.restoreNotFound(fmt.Sprintf("No complete state backup of serial %d exists; status.stateBackups lists the backups", serial))
		return state.Backup{}, false, nil
	}
	// The Job cache may be ahead of the object cache: a restore Job of this
	// serial, already consumed (bookkept, its serial recorded live), is no
	// longer consumed again, so the live record is what stops a retry.
	live, consumed, err := r.liveRestore(ctx)
	if err != nil || live != raw || consumed == serial {
		return state.Backup{}, false, err
	}
	return b, true, nil
}

// liveRestore returns the object's restore annotation and
// status.lastRestoredSerial as the API server has them, read using ctx
// through the API reader, bypassing the cache. It returns any error from
// that read or from reading the status.
func (r *reconciler) liveRestore(ctx context.Context) (string, int64, error) {
	live, err := zeroOf(r.obj)
	if err != nil {
		return "", 0, fmt.Errorf("read %s: %w", infrav1.RestoreStateAnnotation, err)
	}
	if err := r.d.APIReader.Get(ctx, client.ObjectKeyFromObject(r.obj), live); err != nil {
		return "", 0, fmt.Errorf("read %s: %w", infrav1.RestoreStateAnnotation, err)
	}
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(live)
	if err != nil {
		return "", 0, fmt.Errorf("read status.lastRestoredSerial: %w", err)
	}
	// A missing field is 0, as the omitempty field is when unset.
	serial, _, err := unstructured.NestedInt64(u, "status", "lastRestoredSerial")
	if err != nil {
		return "", 0, fmt.Errorf("read status.lastRestoredSerial: %w", err)
	}
	return live.GetAnnotations()[infrav1.RestoreStateAnnotation], serial, nil
}

// zeroOf returns a new, zero object of obj's type, to read into. The API
// reader decodes JSON into the object it is given without clearing it, so
// reading into a populated copy would keep what the response omits: a
// withdrawn annotation, or an omitempty field gone back to zero. It
// returns an error when obj is not a pointer to a struct.
func zeroOf(obj client.Object) (client.Object, error) {
	t := reflect.TypeOf(obj)
	if t == nil || t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("%T is not a pointer to a struct", obj)
	}
	zero, ok := reflect.New(t.Elem()).Interface().(client.Object)
	if !ok {
		return nil, fmt.Errorf("%T is not a client.Object", obj)
	}
	return zero, nil
}

// restoreNotFound sets RestoreJobSucceeded=False/RestoreBackupNotFound with
// msg, counting it once per new message.
func (r *reconciler) restoreNotFound(msg string) {
	c := metav1.Condition{
		Type: infrav1.RestoreJobSucceededCondition, Status: metav1.ConditionFalse, Reason: infrav1.RestoreBackupNotFoundReason, Message: msg,
	}
	if prev, ok := r.before[c.Type]; !ok || prev.Reason != c.Reason || prev.Message != c.Message {
		r.d.Metrics.StateRestore(r.k.Kind(), metrics.RestoreNotFound)
	}
	conditions.Set(r.obj, c)
}

// startRestore starts, using ctx, the restore Job of backup b for the
// decision dec: a backend-only root (render.BackendRoot), the backup's
// chunks projected into the Job's config volume, the run lease (and,
// under the cluster operation gate, the cluster write lease or check, as
// for an apply). The credentials must be ready (the Job mounts the
// mirror); the inputs need not build. bk is the pass's bookkeeping. It
// returns the result and error from finish or from starting the Job.
func (r *reconciler) startRestore(ctx context.Context, bk *Bookkeeping, dec Decision, b state.Backup) (ctrl.Result, error) {
	if !r.credsReady {
		return r.waitForCredentials(bk, jobs.OpRestore)
	}
	req := JobRequest{
		Op:             jobs.OpRestore,
		Files:          render.BackendRoot(),
		InputsHash:     b.InputsHash,
		Identity:       r.identityName,
		IdentityKind:   r.identityKind,
		ServiceAccount: r.serviceAccount,
		Suffix:         r.suffix,
		ClusterName:    ClusterName(r.obj, r.owner),
		Attempt:        jobs.Attempt(bk.Jobs, jobs.OpRestore),
		ForceUnlockID:  bk.ForceUnlockID,
		Policy:         r.eff.Jobs,
		Source:         r.jobSource(jobs.OpRestore),
		// The backup's name tells two backups of one serial apart.
		DriftTick: "restore/" + b.Name,
		Why:       dec.Reason,
		Restore:   &jobs.Restore{Serial: b.Serial, Secrets: b.Secrets, ManagedResources: b.ManagedResources},
	}
	if r.durable != nil {
		req.PinnedDigest = r.durable.Meta.ImageDigest
	}
	wait, err := r.takeLeases(ctx, req, JobName(r.k, req))
	if err != nil {
		return ctrl.Result{}, err
	}
	if wait.reason != "" {
		return r.waitForLease(ctx, bk, req.Op, wait)
	}
	job, err := StartJob(ctx, r.d, r.k, req)
	if errors.Is(err, ErrStartDeferred) {
		return r.startDeferred(ctx, bk, req.Op, err)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	klog.FromContext(ctx).Info("Restoring a state backup", "backup", b.Name, "stateSerial", b.Serial, "Job", job.Name)
	return r.finish(bk, nil, ctrl.Result{RequeueAfter: ActiveJobRequeue})
}

// adoptSource returns the Job of bk whose inputs hash the state is
// adopted with: the newer of the newest successful apply and the newest
// successful restore (which carries its backup's hash); nil when neither.
func adoptSource(bk *Bookkeeping) *batchv1.Job {
	var apply, restore *batchv1.Job
	if bk.LastApply != nil && bk.LastApplySucceeded {
		apply = bk.LastApply
	}
	if f := bk.lastRestore; f != nil && f.ok {
		restore = f.job
	}
	switch {
	case restore == nil:
		return apply
	case apply == nil:
		return restore
	case jobs.FinishedAt(restore).After(jobs.FinishedAt(apply)):
		return restore
	}
	return apply
}
