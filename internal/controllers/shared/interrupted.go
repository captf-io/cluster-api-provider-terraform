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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
)

// recordVanishedApply records, using ctx, an apply Job of a mutable kind
// that is gone before bookkeeping read it finish (deleted while it ran):
// status.activeJob names an apply that bk, this pass's bookkeeping, does
// not list, and the caller found that the API server does not have it
// either (cacheLag). Bookkeeping never sees such a Job finish, so nothing
// else records that it may have changed resources, and the state's
// inputs hash, which only a successful apply writes, does not show it.
// It records:
//
//   - the Job as interrupted (inputs.SetInterruptedApply), unless one is
//     recorded already: the first stays, and every apply started since
//     carries its name (AfterInterruptedApplyAnnotation). Until one of
//     those succeeds, an apply stays due (lastApplyFailed), even of the
//     state's own inputs, and ApplyJobSucceeded says why
//     (interruptedCondition). Every kind's marker, the cluster's included.
//   - the attempt record as one whose Job may have changed resources
//     (inputs.SetMayHaveApplied), when it is the Job's: a destroy then
//     renders it (runRecord).
//   - for an ExportsGuard kind, also the change of the cluster's exports
//     the Job may have partly applied (vanishedPartial), which stops the
//     pool from holding the exports of its last successful apply and
//     guards its every apply until one succeeds.
//
// Nothing is recorded unless the API server's status.activeJob still
// names the Job (liveActiveJob): a cache that lags the pass that deleted
// a stuck Job, which never started, and cleared it would name it here.
// DeleteStuckJob clears it on the server before the delete, so a status
// write of that pass that is lost leaves it cleared there too.
// An immutable kind (a machine) records nothing: until its first apply
// succeeds, an apply is due anyway, and it never applies again. It sets
// r.durable's fields, so this pass reads them, and returns any error from
// reading the status or recording.
func (r *reconciler) recordVanishedApply(ctx context.Context, bk *Bookkeeping) error {
	a, d := r.st.ActiveJob, r.durable
	if !r.k.Mutable() || a.Name == "" || a.Operation != infrav1.Operation(jobs.OpApply) || d == nil ||
		slices.ContainsFunc(bk.Jobs, func(j batchv1.Job) bool { return j.Name == a.Name }) {
		return nil
	}
	partial := r.vanishedPartial(a.Name)
	mark := d.Attempt != nil && d.Attempt.Job == a.Name && !d.Attempt.MayHaveApplied
	if d.InterruptedApply != "" && partial == nil && !mark {
		return nil
	}
	live, err := r.liveActiveJob(ctx)
	if err != nil || live != a.Name {
		return err
	}
	if d.InterruptedApply == "" {
		err := inputs.SetInterruptedApply(ctx, r.d.Client, r.obj, a.Name)
		switch {
		case errors.Is(err, inputs.ErrNotFound):
			return nil
		case err != nil:
			return err
		}
		d.InterruptedApply = a.Name
		klog.FromContext(ctx).Info("An apply Job is gone before it finished, and may have applied part of its change; "+
			"an apply of the current inputs stays due until one succeeds", "Job", a.Name)
	}
	if mark {
		switch err := inputs.SetMayHaveApplied(ctx, r.d.Client, r.obj); {
		case errors.Is(err, inputs.ErrNotFound):
		case err != nil:
			return err
		default:
			d.Attempt.MayHaveApplied = true
		}
	}
	if partial == nil {
		return nil
	}
	err = inputs.SetPartial(ctx, r.d.Client, r.obj, *partial)
	switch {
	case errors.Is(err, inputs.ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	d.Partial = partial
	klog.FromContext(ctx).Info("An apply Job of a change of the cluster's exports is gone before it finished, and may have applied part of it; "+
		"until an apply succeeds, every apply of the pool is guarded and none falls back to the exports of its last successful apply", "Job", a.Name)
	return nil
}

// vanishedPartial returns the change of the cluster's exports that job,
// an ExportsGuard kind's apply gone before it finished, may have partly
// applied, or nil when there is none to record: another kind, a change
// already recorded (it is kept), no applied exports recorded (no hash to
// compare), or a Job that rendered the recorded applied exports (a held
// apply). The attempt record is written once a Job exists, and not again
// until the next one does, so its tfvars are still the vanished Job's.
// Exports that cannot be hashed count as a change: the record only ever
// makes the pool more careful.
func (r *reconciler) vanishedPartial(job string) *inputs.Partial {
	d := r.durable
	if _, ok := r.k.(ExportsGuard); !ok || d.Partial != nil || d.AppliedExportsHash == "" {
		return nil
	}
	rendered, err := hash.Exports(inputs.LastClusterOutputs(d.Attempt))
	if err == nil && rendered == d.AppliedExportsHash {
		return nil
	}
	return &inputs.Partial{ExportsHash: rendered, Job: job}
}

// liveActiveJob returns the name of the Job status.activeJob names on the
// object as the API server has it, read using ctx through the API reader,
// bypassing the cache, or "" when it names none or the object is gone.
// A pass may read the object from a cache that still names a Job the
// controller deleted, and cleared from status.activeJob, the pass before
// (DeleteStuckJob). It returns any error from the read.
func (r *reconciler) liveActiveJob(ctx context.Context) (string, error) {
	live, err := zeroOf(r.obj)
	if err != nil {
		return "", fmt.Errorf("read status.activeJob: %w", err)
	}
	switch err := r.d.APIReader.Get(ctx, client.ObjectKeyFromObject(r.obj), live); {
	case apierrors.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read status.activeJob: %w", err)
	}
	return activeJobNameOf(live)
}

// activeJobNameOf returns the name of the Job status.activeJob names on
// obj, a kind's object, or "" when it names none. It returns an error
// when obj cannot be read as unstructured.
func activeJobNameOf(obj client.Object) (string, error) {
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return "", fmt.Errorf("read status.activeJob: %w", err)
	}
	// A missing field is "", as the omitzero field is when cleared.
	name, _, err := unstructured.NestedString(u, "status", "activeJob", "name")
	if err != nil {
		return "", fmt.Errorf("read status.activeJob: %w", err)
	}
	return name, nil
}

// releaseActiveJob clears, using ctx and d, status.activeJob on the API
// server's copy of obj, a kind's object, when obj names job there, with a
// patch of that status alone that carries obj's resourceVersion: a
// concurrent change, or a cache older than the server's object, fails it
// with a conflict, and the caller retries on the next pass. It leaves obj
// as it is, with its pending changes, for the pass's own status patch, and
// does nothing when obj names another Job or none. It returns any error
// from the patch.
func releaseActiveJob(ctx context.Context, d Deps, obj client.Object, job string) error {
	if name, err := activeJobNameOf(obj); err != nil || name != job {
		return err
	}
	target, err := zeroOf(obj)
	if err != nil {
		return fmt.Errorf("release status.activeJob: %w", err)
	}
	target.SetNamespace(obj.GetNamespace())
	target.SetName(obj.GetName())
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": obj.GetResourceVersion()},
		"status":   map[string]any{"activeJob": nil},
	})
	if err != nil {
		return fmt.Errorf("release status.activeJob: %w", err)
	}
	if err := d.Client.Status().Patch(ctx, target, client.RawPatch(types.MergePatchType, body)); err != nil {
		return fmt.Errorf("release status.activeJob: %w", err)
	}
	return nil
}

// interruptedApply returns the apply Job recorded as gone before it
// finished (inputs.Durable.InterruptedApply), or "" when none is.
func (r *reconciler) interruptedApply() string {
	if r.durable == nil {
		return ""
	}
	return r.durable.InterruptedApply
}

// clearInterrupted removes, using ctx and d, k's record of an apply Job
// gone before it finished (inputs.ClearInterruptedApply) once f, a newly
// finished successful apply, was started after it: it carries its name
// (AfterInterruptedApplyAnnotation), so it applied the current inputs in
// full over whatever that Job left. durable is the durable Secret as read
// this pass; an older success read again (its bookkept mark failed)
// clears nothing. It returns any error from removing the record.
func (bk *Bookkeeping) clearInterrupted(ctx context.Context, d Deps, k Kind, f *finished, durable *inputs.Durable) error {
	if durable == nil || durable.InterruptedApply == "" || f.job.Annotations[AfterInterruptedApplyAnnotation] != durable.InterruptedApply {
		return nil
	}
	err := inputs.ClearInterruptedApply(ctx, d.Client, k.Object())
	switch {
	case errors.Is(err, inputs.ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	bk.InterruptedCleared = true
	klog.FromContext(ctx).Info("An apply started after an apply Job that disappeared succeeded; no apply is due for it any more",
		"Job", klog.KObj(f.job), "interrupted", durable.InterruptedApply)
	return nil
}

// interruptedDue is what ApplyJobSucceeded says while an apply Job that
// disappeared keeps an apply due (interruptedCondition), after its name and
// a colon, like every Job condition, so the event dedup names the Job.
const interruptedDue = ": disappeared while it ran and may have applied part of its change; an apply of the current inputs is due"

// Whether the due apply is guarded against a destructive plan
// (interruptedCondition).
const (
	// interruptedGuarded: a cluster's apply, or a pool's that renders a
	// change of the cluster's exports or follows one partly applied.
	interruptedGuarded = " (it is guarded, and a plan that deletes or replaces resources waits for approval)"
	// interruptedUnguarded: a pool's apply that renders no change of the
	// cluster's exports.
	interruptedUnguarded = " (it is not guarded: it renders no change of the cluster's exports)"
	// interruptedMaybe: a pool's, on a pass that built no inputs.
	interruptedMaybe = " (it is guarded if it renders a change of the cluster's exports, " +
		"and a plan that deletes or replaces resources then waits for approval)"
)

// interruptedCondition returns ApplyJobSucceeded, and true, while an apply
// Job that disappeared keeps an apply due (interruptedApply) and bk's
// newest apply predates it: it says nothing of that Job, whose own result
// is gone, so the condition says the Job disappeared, that it may have
// applied part of its change, that an apply of the current inputs is due,
// and whether that apply is guarded (a cluster's always is; a pool's is
// when this pass's guard says so). An apply started since carries the
// Job's name (AfterInterruptedApplyAnnotation) and reports itself, a
// block or a changed plan included: its condition says what the due apply
// waits for. An older apply's block or changed plan does not: that apply
// predates the Job, which an approval may have started since. A pass that
// built no inputs keeps a
// pool's condition already reported for the Job, and a deleting object's
// destroy says what stands instead. It returns false when the condition
// is not this one.
func (r *reconciler) interruptedCondition(bk *Bookkeeping) (metav1.Condition, bool) {
	job, last := r.interruptedApply(), bk.LastApply
	if job == "" || r.deleting || (last != nil && last.Annotations[AfterInterruptedApplyAnnotation] == job) {
		return metav1.Condition{}, false
	}
	note := interruptedGuarded
	if _, pool := r.k.(ExportsGuard); pool {
		switch prev := conditions.Get(r.obj, infrav1.ApplyJobSucceededCondition); {
		case r.guard == nil && prev != nil && prev.Reason == infrav1.ApplyFailedReason && strings.HasPrefix(prev.Message, "Job "+job+interruptedDue):
			return *prev, true
		case r.guard == nil:
			note = interruptedMaybe
		case !r.guard.Guarded:
			note = interruptedUnguarded
		}
	}
	return metav1.Condition{
		Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionFalse, Reason: infrav1.ApplyFailedReason,
		Message: "Job " + job + interruptedDue + note,
	}, true
}
