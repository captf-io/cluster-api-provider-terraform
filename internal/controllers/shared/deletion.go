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
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
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
// object. The same holds while no state exists but an apply Job's outcome
// is unconfirmed (ApplyOutcomeUnknown): it may have created resources
// before any state was written. The hold ends with a restore (the destroy
// follows), once the state reads again, with the unconfirmed Job confirmed
// to have created nothing (ConfirmNoResourcesAnnotation), or with
// deletionPolicy Retain, which removes the
// finalizer and keeps the state, its backups and the durable inputs for a
// later adoption. Retain also releases a deletion whose destroy failed
// (also after a restore) or cannot start (it cannot be rendered, the
// identity does not allow the namespace, or its credentials cannot be
// prepared), so a destroy that can never succeed has an escape that loses
// nothing.

// everApplied reports, using ctx and the shared dependencies d, whether
// k's object ever applied or may have, so a missing state is not one that
// was never written: it applied (appliedBefore), or an apply Job whose
// outcome is unconfirmed is recorded on durable, the inputs records as
// read (nil when none). That Job ended without a result or vanished
// before any state was written, so it may have created resources: for
// every kind, a deletion is held rather than released, and no second
// first apply runs (ApplyOutcomeUnknown). suffix is the object's state
// suffix, which names its backups. It returns whether the object ever
// applied or may have, and any error listing the backups.
func everApplied(ctx context.Context, d Deps, k Kind, suffix string, durable *inputs.Durable) (bool, error) {
	if durable != nil && durable.InterruptedApply != "" {
		return true, nil
	}
	return appliedBefore(ctx, d, k, suffix, durable)
}

// appliedBefore reports, using ctx and the shared dependencies d, whether
// k's object applied before, so a missing state is a lost one:
// status.initialization.provisioned (which clusterctl move does not carry
// over), the applied marker on durable (the inputs records as read, nil
// when none; set at the first successful apply or restore, moved with the
// Secret and never cleared), an applied record on durable (only a
// successful apply writes one), or any state backup of suffix (a state
// existed; none are kept with --state-backups=0). Secrets are read live
// through d.Client. It returns any error listing the backups.
func appliedBefore(ctx context.Context, d Deps, k Kind, suffix string, durable *inputs.Durable) (bool, error) {
	if p := k.Status().Initialization.Provisioned; p != nil && *p {
		return true, nil
	}
	if durable != nil && (durable.AppliedMark || durable.Applied != nil) {
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
	conditions.Set(r.obj, metav1.Condition{
		Type: infrav1.StateReadableCondition, Status: metav1.ConditionFalse, Reason: infrav1.StateLostReason,
		Message: "The state Secret is missing although the object applied before. " + r.heldNote(),
	})
}

// outcomeUnknown sets StateReadable False/ApplyOutcomeUnknown for an
// object without state whose apply Job job ended without a result or
// disappeared, so it may have created resources no state records: no
// apply runs, and a deletion is held (heldNote). The message names the
// Job and the ways out: a restore, confirming the Job created nothing
// (ConfirmNoResourcesAnnotation), or, deleting, Retain.
func (r *reconciler) outcomeUnknown(job string) {
	msg := "No state exists, but apply Job " + job + " ended without a result or disappeared while it ran, " +
		"so it may have created resources that no state records. "
	if r.deleting {
		msg += "Once the infrastructure is checked and holds nothing it created, set " + infrav1.ConfirmNoResourcesAnnotation + "=" + job +
			" to drop the finalizer. " + r.heldNote()
	} else {
		msg += "No apply runs, as a new one would create a second set. Once the infrastructure is checked and holds nothing it created, set " +
			infrav1.ConfirmNoResourcesAnnotation + "=" + job + " to apply again; or restore a backup listed in status.stateBackups with the " +
			infrav1.RestoreStateAnnotation + " annotation; see https://captf.io/docs/operator-guide/runbooks/state-restore.html"
	}
	conditions.Set(r.obj, metav1.Condition{
		Type: infrav1.StateReadableCondition, Status: metav1.ConditionFalse, Reason: infrav1.ApplyOutcomeUnknownReason, Message: msg,
	})
}

// confirmNoResources consumes ConfirmNoResourcesAnnotation, using ctx,
// when it names the apply Job whose outcome is unconfirmed
// (interruptedApply): the operator checked that the Job created nothing,
// so the record is removed (inputs.ClearInterruptedApply), then the
// annotation (removeAnnotation), and a missing state reads as none yet in
// this same pass. An annotation naming another Job, or set while none is
// recorded, is left alone. It returns any error from removing either.
func (r *reconciler) confirmNoResources(ctx context.Context) error {
	v, job := strings.TrimSpace(r.annotation(infrav1.ConfirmNoResourcesAnnotation)), r.interruptedApply()
	if v == "" || job == "" {
		return nil
	}
	logger := klog.FromContext(ctx)
	if v != job {
		logger.Info("The confirmation that an apply Job created nothing names another Job; ignored",
			"annotation", infrav1.ConfirmNoResourcesAnnotation, "names", v, "unconfirmed", job)
		return nil
	}
	if err := inputs.ClearInterruptedApply(ctx, r.d.Client, r.obj); err != nil && !errors.Is(err, inputs.ErrNotFound) {
		return err
	}
	r.durable.InterruptedApply = ""
	if err := r.removeAnnotation(ctx, infrav1.ConfirmNoResourcesAnnotation); err != nil {
		return err
	}
	logger.Info("The operator confirmed that an apply Job whose outcome was unconfirmed created nothing; it no longer holds the object", "Job", job)
	return nil
}

// deletionHeld ends a pass of a deleting object whose state is lost or
// unreadable, using ctx and the pass's bookkeeping bk. It requeues at
// StateRequeue, and returns the result and error from finish.
func (r *reconciler) deletionHeld(ctx context.Context, bk *Bookkeeping) (ctrl.Result, error) {
	klog.FromContext(ctx).V(LogFlow).Info("Deletion held on the state", "reason", stateReadableReason(r.obj))
	return r.finish(bk, nil, ctrl.Result{RequeueAfter: StateRequeue})
}

// policyUnresolved ends a pass of a deleting object whose deletionPolicy
// is inherited but unknown (EffectiveConfig.DeletionPolicy ""): it sets no
// policy of its own and its TerraformCluster cannot be found, so neither a
// destroy nor a Retain may run, and Destroy is never assumed. The Deleting
// condition says so, with DeletionPolicyUnresolved, and the pass requeues
// at GateRequeue, logging with ctx and reporting the pass's bookkeeping
// bk. It returns the result and error from finish.
func (r *reconciler) policyUnresolved(ctx context.Context, bk *Bookkeeping) (ctrl.Result, error) {
	conditions.Set(r.obj, metav1.Condition{
		Type: clusterv1.DeletingCondition, Status: metav1.ConditionTrue, Reason: infrav1.DeletionPolicyUnresolvedReason,
		Message: "Deletion waits: this object sets no spec.deletionPolicy, and the TerraformCluster it inherits one from cannot be found, " +
			"so neither a destroy nor a Retain runs. Set spec.deletionPolicy on this object (Destroy or Retain) to proceed",
	})
	klog.FromContext(ctx).V(LogFlow).Info("Deletion held: the inherited deletionPolicy is unknown")
	return r.finish(bk, nil, ctrl.Result{RequeueAfter: GateRequeue})
}

// stateReadableReason returns obj's StateReadable reason, "" when unset.
func stateReadableReason(obj Object) string {
	if c := conditions.Get(obj, infrav1.StateReadableCondition); c != nil {
		return c.Reason
	}
	return ""
}
