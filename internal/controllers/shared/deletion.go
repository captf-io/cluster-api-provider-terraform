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
// object. The hold ends with a restore (the destroy follows), once the
// state reads again, or with deletionPolicy Retain, which removes the
// finalizer and keeps the state, its backups and the durable inputs for a
// later adoption. Retain also releases a deletion whose destroy failed
// (also after a restore) or cannot start (it cannot be rendered, the
// identity does not allow the namespace, or its credentials cannot be
// prepared), so a destroy that can never succeed has an escape that loses
// nothing.

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
// while its deletion is held: how to restore, and how to retain.
func (r *reconciler) heldNote() string {
	return "Deletion is held: no destroy runs and the finalizer stays (keeping the state backups). " +
		"Restore a backup listed in status.stateBackups with the " + infrav1.RestoreStateAnnotation + " annotation, and the destroy follows; " +
		"or set " + retainHint + "; see https://captf.io/docs/operator-guide/runbooks/state-restore.html"
}

// retainHint says, for a condition message, how to remove the finalizer of
// a deletion whose destroy cannot run without losing the way back to the
// infrastructure.
const retainHint = "spec.deletionPolicy: Retain to remove the finalizer without a destroy, " +
	"leaving the infrastructure running and keeping its state, backups and durable inputs for a later adoption"

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

// deletionHeld ends a pass of a deleting object whose state is lost or
// unreadable, using ctx and the pass's bookkeeping bk. It requeues at
// StateRequeue, and returns the result and error from finish.
func (r *reconciler) deletionHeld(ctx context.Context, bk *Bookkeeping) (ctrl.Result, error) {
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
