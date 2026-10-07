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
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/finalizers"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/paused"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// PreambleResult is the outcome of Preamble's stages: externally-managed,
// owner lookup, finalizer and pause.
type PreambleResult struct {
	// Owner is the owner lookup's result.
	Owner OwnerInfo
	// Paused is true when the object or its Cluster is paused: the caller
	// runs only the paused branch (bookkeeping, block-move).
	Paused bool
	// Stop ends the reconcile with Result.
	Stop   bool
	Result ctrl.Result
}

// Preamble runs the following stages, in this fixed order:
//
//  0. deleting with foreground propagation → drop the foregroundDeletion
//     finalizer (convertForeground) and carry on;
//  1. externally managed → stop before any write; deleting, keep the
//     state as Retain does and drop the finalizer once no Job runs
//     (releaseExternallyManaged);
//  2. owner lookup: no ownerRef of the expected kind and not deleting →
//     DependenciesReady=Unknown and requeue; no ownerRef and deleting with
//     no state, no Job, no live run lease, no dependents blocking the
//     deletion and no sign of a previous apply → drop the finalizer (with
//     any of them, continue: the delete reconcile waits, holds or
//     destroys); an ownerRef whose
//     target is gone → continue, deletion still destroys; an owner gate
//     (for example ClusterNotTerraform, or OwnerMismatch when the
//     ownerRef's target does not reference this object back) → set
//     DependenciesReady and stop, but only while not deleting: a gate
//     while deleting falls through here exactly like an ownerRef whose
//     target is gone, so destroy still runs from the durable inputs
//     rather than getting stuck behind a forged or stale ownerRef;
//  3. finalizer → stop when just added;
//  4. pause → report it; the caller then runs only the paused branch.
//
// It runs using ctx and the shared dependencies d, for k's object, and
// returns the PreambleResult (Stop true when the caller must return now)
// and any error from the steps.
func Preamble(ctx context.Context, d Deps, k Kind) (PreambleResult, error) {
	obj := k.Object()
	if err := convertForeground(ctx, d, obj); err != nil {
		return conflictRequeue(err)
	}
	if annotations.IsExternallyManaged(obj) {
		if obj.GetDeletionTimestamp().IsZero() {
			klog.FromContext(ctx).V(LogDebug).Info("Object is externally managed, skipping")
			return PreambleResult{Stop: true}, nil
		}
		return releaseExternallyManaged(ctx, d, k)
	}

	owner, err := k.Owner(ctx)
	if err != nil {
		return PreambleResult{}, fmt.Errorf("owner lookup: %w", err)
	}
	deleting := !obj.GetDeletionTimestamp().IsZero()

	if !owner.HasOwnerRef {
		if !deleting {
			gate := owner.Gate
			if gate == nil {
				gate = &Gate{Status: metav1.ConditionUnknown, Reason: infrav1.WaitingForOwnerReason, Message: "Waiting for the owner reference"}
			}
			if err := patchGate(ctx, d, k, gate); err != nil {
				return PreambleResult{}, err
			}
			// The ownerRef update normally triggers the reconcile first.
			return PreambleResult{Owner: owner, Stop: true, Result: ctrl.Result{RequeueAfter: GateRequeue}}, nil
		}
		release, err := ownerlessRelease(ctx, d, k, owner)
		if err != nil {
			return PreambleResult{}, err
		}
		if release {
			removed, err := dropFinalizer(ctx, d.Client, obj, k.Finalizer())
			if removed && err == nil {
				klog.FromContext(ctx).Info("Removed the finalizer",
					"finalizer", k.Finalizer(), "reason", "no owner, no state and no running Job")
				d.Emit(obj, corev1.EventTypeNormal, EventFinalizerRemoved, "Delete",
					"Removed finalizer %s: no owner, no state and no running Job, so nothing to destroy", k.Finalizer())
			}
			if err != nil {
				return conflictRequeue(err)
			}
			return PreambleResult{Owner: owner, Stop: true}, nil
		}
	} else if owner.Gate != nil && !deleting {
		if err := patchGate(ctx, d, k, owner.Gate); err != nil {
			return PreambleResult{}, err
		}
		return PreambleResult{Owner: owner, Stop: true}, nil
	}

	if !deleting {
		added, err := finalizers.EnsureFinalizer(ctx, d.Client, obj, k.Finalizer())
		if err != nil {
			return PreambleResult{}, fmt.Errorf("add finalizer: %w", err)
		}
		if added {
			return PreambleResult{Owner: owner, Stop: true}, nil
		}
	}

	isPaused, requeue, err := paused.EnsurePausedCondition(ctx, d.Client, owner.Cluster, obj)
	if err != nil {
		return PreambleResult{}, fmt.Errorf("paused condition: %w", err)
	}
	if requeue && !isPaused {
		return PreambleResult{Owner: owner, Stop: true}, nil
	}
	return PreambleResult{Owner: owner, Paused: isPaused}, nil
}

// patchGate persists DependenciesReady from gate and, until the object is
// provisioned, a Ready condition that says why it stops here: False with
// NotReadyReason when gate is False, else Unknown with ReadyUnknownReason,
// carrying the gate's reason and message. A provisioned object keeps its
// Ready, which Cluster API mirrors.
// The DependenciesReady and Ready transitions it makes emit their events
// here: this path never reaches the reconcile's own emitTransitions. It
// patches k's object using ctx and emits through d. It returns any patch
// error.
func patchGate(ctx context.Context, d Deps, k Kind, gate *Gate) error {
	obj := k.Object()
	helper, err := patch.NewHelper(obj, d.Client)
	if err != nil {
		return fmt.Errorf("patch helper: %w", err)
	}
	types := []string{infrav1.DependenciesReadyCondition, infrav1.ReadyCondition}
	before := map[string]metav1.Condition{}
	for _, t := range types {
		if c := conditions.Get(obj, t); c != nil {
			before[t] = *c
		}
	}
	conditions.Set(obj, metav1.Condition{
		Type: infrav1.DependenciesReadyCondition, Status: gate.Status, Reason: gate.Reason, Message: gate.Message,
	})
	owned := []string{infrav1.DependenciesReadyCondition}
	if p := k.Status().Initialization.Provisioned; p == nil || !*p {
		ready := metav1.Condition{
			Type: infrav1.ReadyCondition, Status: metav1.ConditionUnknown, Reason: infrav1.ReadyUnknownReason,
			Message: gate.Reason + ": " + gate.Message,
		}
		if gate.Status == metav1.ConditionFalse {
			ready.Status, ready.Reason = metav1.ConditionFalse, infrav1.NotReadyReason
		}
		conditions.Set(obj, ready)
		owned = append(owned, infrav1.ReadyCondition)
	}
	if err := helper.Patch(ctx, obj, patch.WithOwnedConditions{Conditions: owned}); err != nil {
		return fmt.Errorf("patch %s: %w", infrav1.DependenciesReadyCondition, err)
	}
	emitConditions(d, klog.FromContext(ctx), k.Kind(), obj, before, types, nil)
	return nil
}

// conflictRequeue turns a Conflict from an optimistic-lock patch into a
// stop that requeues shortly, and returns any other err unchanged.
func conflictRequeue(err error) (PreambleResult, error) {
	if apierrors.IsConflict(err) {
		return PreambleResult{Stop: true, Result: ctrl.Result{RequeueAfter: time.Second}}, nil
	}
	return PreambleResult{}, err
}

// releaseExternallyManaged lets k's deleting object, which carries the
// managed-by annotation, go without a destroy, using ctx and the shared
// dependencies d: a deleting object that still carries our finalizer would
// otherwise hang forever. The infrastructure is left to the external
// manager, and so is its state: while a Job of the object runs (one
// started before the annotation was set) it waits, then it keeps the
// state Secrets, the backups and the inputs records exactly as
// deletionPolicy Retain does (Retain: unowned, so the garbage collector
// does not take them with the object, and labeled with its uid), deletes
// the state lock, releases the object and persists the finalizer removal
// with an optimistic-lock patch. It returns the PreambleResult (always
// Stop) and any error; a conflict requeues.
func releaseExternallyManaged(ctx context.Context, d Deps, k Kind) (PreambleResult, error) {
	obj := k.Object()
	if !controllerutil.ContainsFinalizer(obj, k.Finalizer()) {
		return PreambleResult{Stop: true}, nil
	}
	suffix, err := state.Suffix(obj.GetNamespace(), k.Kind(), obj.GetName())
	if err != nil {
		return PreambleResult{}, err
	}
	switch running, err := jobRunning(ctx, d, k, suffix); {
	case err != nil:
		return PreambleResult{}, err
	case running:
		klog.FromContext(ctx).V(LogFlow).Info("Externally managed and deleting; waiting for the running Job before releasing")
		return PreambleResult{Stop: true, Result: ctrl.Result{RequeueAfter: GateRequeue}}, nil
	}
	before, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return PreambleResult{}, fmt.Errorf("copy %T", obj)
	}
	// The credential mirror is not resolved here: the garbage collector
	// drops this object's reference from it, or the mirror with its last
	// user.
	kept, err := Retain(ctx, d, k, suffix, "")
	if err != nil {
		return conflictRequeue(err)
	}
	if err := client.IgnoreNotFound(d.Client.Patch(ctx, obj, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))); err != nil {
		return conflictRequeue(fmt.Errorf("remove finalizer: %w", err))
	}
	klog.FromContext(ctx).Info("Removed the finalizer", "finalizer", k.Finalizer(), "reason", "externally managed")
	d.Emit(obj, corev1.EventTypeNormal, EventExternallyManagedReleased, "Delete",
		"Removed finalizer %s: the object is externally managed, so its infrastructure is left to the external manager; "+
			"its state (%d Secrets), %d state backup Secrets and %d inputs Secrets are kept, labeled %s=%s",
		k.Finalizer(), kept.State, kept.Backups, kept.Inputs, state.RetainedFromUIDLabel, obj.GetUID())
	return PreambleResult{Stop: true}, nil
}

// convertForeground turns a foreground deletion of obj into a background
// one, using ctx and the shared dependencies d: it removes the
// foregroundDeletion finalizer the API server adds for
// propagationPolicy Foreground (kubectl delete --cascade=foreground, Argo
// CD's default prune, a Cluster deleted that way). While that finalizer is
// on obj, the garbage collector deletes every dependent of it at once,
// whatever obj's own finalizer: the Job running the destroy, the state
// Secrets and the inputs records. Without it the dependents are collected
// only once obj is gone, after the destroy, as with background
// propagation. Whatever the collector deleted before this pass keeps its
// data under state.ProtectionFinalizer. It returns any patch error.
func convertForeground(ctx context.Context, d Deps, obj Object) error {
	if obj.GetDeletionTimestamp().IsZero() || !controllerutil.ContainsFinalizer(obj, metav1.FinalizerDeleteDependents) {
		return nil
	}
	removed, err := dropFinalizer(ctx, d.Client, obj, metav1.FinalizerDeleteDependents)
	if err != nil || !removed {
		return err
	}
	klog.FromContext(ctx).Info("Converted a foreground deletion to background", "finalizer", metav1.FinalizerDeleteDependents)
	d.Emit(obj, corev1.EventTypeNormal, EventForegroundDeletionConverted, "Delete",
		"Removed finalizer %s: a foreground deletion would garbage-collect this object's Jobs and state before its destroy; they go with the object instead",
		metav1.FinalizerDeleteDependents)
	return nil
}

// dropFinalizer removes the finalizer with a merge patch under an
// optimistic lock, so a finalizer another controller added since the
// read is never dropped, using ctx and the client c; removed is false
// when obj did not carry it. It returns removed and any patch error,
// which wraps the API Conflict when obj is stale.
func dropFinalizer(ctx context.Context, c client.Client, obj Object, finalizer string) (removed bool, _ error) {
	before, ok := obj.DeepCopyObject().(client.Object)
	if !ok || !controllerutil.RemoveFinalizer(obj, finalizer) {
		return false, nil
	}
	if err := client.IgnoreNotFound(c.Patch(ctx, obj, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))); err != nil {
		return false, fmt.Errorf("remove finalizer: %w", err)
	}
	return true, nil
}

// ownerlessRelease reports, using ctx and the shared dependencies d,
// whether k's deleting object without an owner reference may drop its
// finalizer here, with owner the owner lookup's result: nothing runs or
// is stored (hasStateOrJob), its deletion is not blocked
// (Kind.DeletionBlocked), and it never applied (everApplied). Otherwise
// the full delete reconcile takes over, which waits for the dependents or
// holds on the lost state. It returns any error from those checks.
func ownerlessRelease(ctx context.Context, d Deps, k Kind, owner OwnerInfo) (bool, error) {
	obj := k.Object()
	suffix, err := state.Suffix(obj.GetNamespace(), k.Kind(), obj.GetName())
	if err != nil {
		return false, err
	}
	busy, err := hasStateOrJob(ctx, d, k, suffix)
	if err != nil || busy {
		return false, err
	}
	// DeletionBlocked sets its condition on the object in memory. The
	// reconcile that follows takes its patch base and event snapshot after
	// the preamble, and evaluates it again; set here, the transition would
	// be in both and never be persisted or reported.
	saved := slices.Clone(obj.GetConditions())
	blocked, err := k.DeletionBlocked(ctx, owner)
	obj.SetConditions(saved)
	if err != nil || blocked {
		return false, err
	}
	durable, err := inputs.Read(ctx, d.Client, obj.GetNamespace(), kindShort(k), obj.GetName())
	if err != nil && !errors.Is(err, inputs.ErrNotFound) {
		return false, err
	}
	applied, err := everApplied(ctx, d, k, suffix, durable)
	return !applied && err == nil, err
}

// hasStateOrJob reports, using ctx and the shared dependencies d, whether
// k's object, of state suffix suffix, has a state Secret, an active Job
// (counting the one status.activeJob names while the Job cache lags behind
// it) or a live run lease. A state read error other than "no state"
// counts as state: the finalizer is never dropped while state may
// describe live resources.
func hasStateOrJob(ctx context.Context, d Deps, k Kind, suffix string) (bool, error) {
	obj := k.Object()
	if _, err := d.State.Read(ctx, obj.GetNamespace(), suffix); !errors.Is(err, state.ErrNoState) {
		if err != nil {
			klog.FromContext(ctx).Info("State read failed; treating the object as having state", "err", err)
		}
		return true, nil
	}
	return jobRunning(ctx, d, k, suffix)
}

// jobRunning reports, using ctx and the shared dependencies d, whether k's
// object, of state suffix suffix, has an active Job (counting the one
// status.activeJob names while the Job cache lags behind it) or a live run
// lease. It returns any error from those reads.
func jobRunning(ctx context.Context, d Deps, k Kind, suffix string) (bool, error) {
	obj := k.Object()
	list, err := d.Jobs.List(ctx, obj, k.Kind())
	if err != nil {
		return false, fmt.Errorf("list jobs: %w", err)
	}
	if _, active := jobs.Active(list); active {
		return true, nil
	}
	if lag, err := activeJobLagging(ctx, d, obj, k.Status().ActiveJob.Name, list); err != nil || lag {
		return lag, err
	}
	_, live, err := runLive(ctx, d, obj.GetNamespace(), suffix)
	return live, err
}
