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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// A deletion is held while the state is lost or unreadable: a destroy
// cannot run against it, and dropping the finalizer would leave whatever
// the module created running with nothing tracking it. The finalizer also
// keeps the state backups, which are owner-referenced and go with the
// object. The hold ends with a restore (the destroy follows), with the
// abandon annotation, or once the state reads again. The abandon
// annotation also releases a deletion whose destroy failed (also after a
// restore) or cannot start (it cannot be rendered, the identity does not
// allow the namespace, or its credentials cannot be prepared), so a
// destroy that can never succeed has an escape.

// everApplied reports, using ctx and the shared dependencies d, whether
// k's object ever applied, so a missing state means a lost one rather
// than none yet: status.initialization.provisioned (which clusterctl move
// does not carry over), the applied marker on durable (the durable inputs
// as read, nil when none; set at the first successful apply or restore,
// moved with the Secret and never cleared), a digest pinned on durable
// (only a successful apply pins one, but an image change clears it), an
// interrupted apply recorded on durable while k's object is deleting (the
// first apply's Job vanished before any state was written, so it may have
// created resources: the deletion is held, not released), or any
// state backup of suffix (a state existed; none are kept with
// --state-backups=0). Secrets are read live through d.Client. It returns
// any error listing the backups.
func everApplied(ctx context.Context, d Deps, k Kind, suffix string, durable *inputs.Durable) (bool, error) {
	if p := k.Status().Initialization.Provisioned; p != nil && *p {
		return true, nil
	}
	if durable != nil && (durable.Meta.Applied || durable.Meta.ImageDigest != "") {
		return true, nil
	}
	// Only a deleting object counts an interrupted apply: a live one applies
	// again (the vanished Job is recorded so the next apply can be told),
	// where deleting it would drop the finalizer over resources the lost
	// Job may have created.
	if durable != nil && durable.InterruptedApply != "" && !k.Object().GetDeletionTimestamp().IsZero() {
		return true, nil
	}
	backups, err := state.ListBackups(ctx, d.Client, k.Object().GetNamespace(), suffix)
	if err != nil {
		return false, err
	}
	return len(backups) > 0, nil
}

// heldNote returns what a deleting object's StateReadable message adds
// while its deletion is held: how to restore, and how to abandon with the
// object's uid.
func (r *reconciler) heldNote() string {
	return fmt.Sprintf("Deletion is held: no destroy runs and the finalizer stays (keeping the state backups). "+
		"Restore a backup listed in status.stateBackups with the %s annotation, and the destroy follows; "+
		"or set %s=%s (this object's uid) to remove the finalizer without a destroy, leaving the infrastructure running and untracked; "+
		"see https://captf.io/docs/operator-guide/runbooks/state-restore.html",
		infrav1.RestoreStateAnnotation, infrav1.AbandonInfrastructureAnnotation, r.obj.GetUID())
}

// lostOnDelete sets StateReadable False/StateLost for a deleting object
// whose state is missing although it applied before.
func (r *reconciler) lostOnDelete() {
	msg := "The state Secret is missing although the object applied before. "
	if r.durable != nil && r.durable.InterruptedApply != "" && !r.durable.Meta.Applied {
		msg = "The state Secret is missing and apply Job " + r.durable.InterruptedApply +
			" disappeared before it finished, so it may have created resources. "
	}
	conditions.Set(r.obj, metav1.Condition{
		Type: infrav1.StateReadableCondition, Status: metav1.ConditionFalse, Reason: infrav1.StateLostReason,
		Message: msg + r.heldNote(),
	})
}

// abandonRequested reports whether the abandon annotation names the
// object's uid; any other value is ignored.
func (r *reconciler) abandonRequested() bool {
	v, ok := r.obj.GetAnnotations()[infrav1.AbandonInfrastructureAnnotation]
	return ok && v == string(r.obj.GetUID())
}

// abandonCause returns why a deleting object's destroy cannot run, as
// known before it is prepared, so the abandon annotation may skip it:
// held (the state is lost or unreadable) names StateReadable's reason,
// otherwise destroyBlocked reads bk, the pass's bookkeeping. It returns
// "" when the destroy may run. The run checks it before the restore and
// destroy decisions, so neither a pending restore that cannot start nor
// a destroy that keeps failing hides it; a destroy that is about to start
// and cannot is checked where it stops (abandonStart). An object whose
// state reads and whose destroy can start is destroyed regardless: the
// annotation would only skip a teardown that may well succeed, and is
// honored once it fails or cannot start.
func (r *reconciler) abandonCause(held bool, bk *Bookkeeping) string {
	if held {
		return infrav1.StateReadableCondition + " " + stateReadableReason(r.obj)
	}
	return r.destroyBlocked(bk)
}

// destroyBlocked returns why the deleting object's last destroy failed or
// could not start, read from bk, the pass's bookkeeping, or "" when
// neither: ApplyJobSucceeded is False, and status.lastRun names a destroy
// (a failed destroy Job; both are kept once it is pruned), or the reason
// is DestroyFailed (also set, without a Job, when the destroy cannot be
// rendered) or IdentityNotAllowed (the identity does not allow the
// namespace, or is gone). A retained Job's condition replaces the latter
// two on the next pass, so abandonStart catches them where the destroy
// stops as well.
func (r *reconciler) destroyBlocked(bk *Bookkeeping) string {
	c := bk.ApplyJob
	if c.Status != metav1.ConditionFalse {
		return ""
	}
	switch {
	case r.st.LastRun.Operation == infrav1.OperationDestroy:
		return "the last destroy failed: " + infrav1.ApplyJobSucceededCondition + " " + c.Reason
	case c.Reason == infrav1.DestroyFailedReason, c.Reason == infrav1.IdentityNotAllowedReason:
		return "the destroy cannot start: " + infrav1.ApplyJobSucceededCondition + " " + c.Reason
	}
	return ""
}

// abandonStart reports whether the abandon annotation releases a deleting
// object whose Job was about to start and cannot, with why saying what
// stops it, using ctx and the pass's bookkeeping bk. When it does, it
// returns true with the result and error from cleanup; otherwise false
// and a zero result.
func (r *reconciler) abandonStart(ctx context.Context, bk *Bookkeeping, why string) (bool, ctrl.Result, error) {
	if !r.deleting || !r.abandonRequested() {
		return false, ctrl.Result{}, nil
	}
	res, err := r.abandon(ctx, bk, "the destroy cannot start: "+why)
	return true, res, err
}

// abandon removes the finalizer of a deleting object without a destroy,
// using ctx and the pass's bookkeeping bk, with why the reason the
// InfrastructureAbandoned event gives. It returns the result and error
// from cleanup.
func (r *reconciler) abandon(ctx context.Context, bk *Bookkeeping, why string) (ctrl.Result, error) {
	r.abandonWhy = why
	return r.cleanup(ctx, bk, cleanupAbandoned)
}

// deletionHeld ends a pass of a deleting object whose state is lost or
// unreadable, using ctx and the pass's bookkeeping bk: an abandon
// annotation that is not the object's uid (abandoned released the uid
// already) is ignored, which StateReadable's message says (so the change
// emits its Warning once). It requeues at StateRequeue, and returns the
// result and error from finish.
func (r *reconciler) deletionHeld(ctx context.Context, bk *Bookkeeping) (ctrl.Result, error) {
	if v, ok := r.obj.GetAnnotations()[infrav1.AbandonInfrastructureAnnotation]; ok {
		if c := conditions.Get(r.obj, infrav1.StateReadableCondition); c != nil {
			c.Message += fmt.Sprintf(" %s=%q is ignored: it is not this object's uid.", infrav1.AbandonInfrastructureAnnotation, v)
			conditions.Set(r.obj, *c)
		}
	}
	klog.FromContext(ctx).V(LogFlow).Info("Deletion held on the state", "reason", stateReadableReason(r.obj))
	return r.finish(bk, nil, ctrl.Result{RequeueAfter: StateRequeue})
}

// stateReadableReason returns obj's StateReadable reason, "" when unset.
func stateReadableReason(obj Object) string {
	if c := conditions.Get(obj, infrav1.StateReadableCondition); c != nil {
		return c.Reason
	}
	return ""
}

// emitAbandoned records that the finalizer of the object was removed
// without a destroy because the abandon annotation names its uid, and
// why it could be (abandonWhy): the held state's StateReadable reason, a
// failed destroy, or what keeps the destroy from starting.
func (r *reconciler) emitAbandoned() {
	r.d.Emit(r.obj, corev1.EventTypeWarning, EventInfrastructureAbandoned, "Delete",
		"Removed finalizer %s without a destroy (%s): %s names this object's uid. "+
			"Whatever the module created keeps running and is no longer managed; the state backups go with the object",
		r.k.Finalizer(), r.abandonWhy, infrav1.AbandonInfrastructureAnnotation)
}
