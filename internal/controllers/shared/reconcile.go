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
	"maps"
	"slices"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	captfconds "github.com/captf-io/cluster-api-provider-terraform/internal/conditions"
	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/rbac"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Fallback requeues; watches are the primary trigger.
const (
	// GateRequeue while the owner reference, dependencies, credentials or
	// deletion ordering are not ready.
	GateRequeue = 30 * time.Second
	// StateRequeue after an unreadable or lost state.
	StateRequeue = time.Minute
	// LagRequeue while the Job cache has not caught up with a Job the API
	// server has, or a live Job holds the run lease, where the reconcile
	// would otherwise clear block-move or drop the finalizer; after a
	// conflict removing a consumed annotation or dropping an owner of the
	// credential mirror; and after a Job start was deferred
	// (ErrStartDeferred).
	LagRequeue = 5 * time.Second
)

// OwnedConditions returns every condition type CAPTF writes, for the patch
// helper's conflict resolution.
func OwnedConditions() []string {
	return slices.Sorted(maps.Keys(infrav1.ConditionReasons()))
}

// Reconcile is the common reconcile flow for k, using ctx and the shared
// dependencies d. It patches the object once, in a deferred call, with the
// owned conditions and the observed generation; only block-move before a
// Job create, the removal of a consumed annotation (removeAnnotation) and
// the preamble's own writes are persisted earlier. It returns the
// controller-runtime result and any error from the reconcile.
func Reconcile(ctx context.Context, d Deps, k Kind) (ctrl.Result, error) {
	res, _, err := ReconcileWithOwner(ctx, d, k)
	return res, err
}

// ReconcileWithOwner is Reconcile for k using ctx and the shared
// dependencies d, also returning the owner lookup's result for callers
// that act on the owners afterwards (the machine's remediation
// annotation). It returns the controller-runtime result, the owner info,
// and any error from the reconcile.
func ReconcileWithOwner(ctx context.Context, d Deps, k Kind) (_ ctrl.Result, _ OwnerInfo, reterr error) {
	// The preamble persists Paused itself, before the snapshot below.
	var pausedBefore *metav1.Condition
	if c := conditions.Get(k.Object(), clusterv1.PausedCondition); c != nil {
		pausedBefore = c.DeepCopy()
	}
	pre, err := Preamble(ctx, d, k)
	if err == nil {
		emitPaused(d, k.Object(), pausedBefore)
	}
	if err != nil || pre.Stop {
		return pre.Result, pre.Owner, err
	}
	ctx = WithObjectLogger(ctx, pre.Owner)
	obj := k.Object()
	helper, err := patch.NewHelper(obj, d.Client)
	if err != nil {
		return ctrl.Result{}, pre.Owner, fmt.Errorf("patch helper: %w", err)
	}
	r := &reconciler{d: d, k: k, obj: obj, owner: pre.Owner, st: k.Status(), before: snapshot(obj), logger: klog.FromContext(ctx), isPaused: pre.Paused}
	defer func() {
		// The helper tolerates NotFound once the finalizer is gone.
		if err := helper.Patch(ctx, obj,
			patch.WithOwnedConditions{Conditions: OwnedConditions()},
			patch.WithStatusObservedGeneration{},
		); err != nil {
			reterr = kerrors.NewAggregate([]error{reterr, fmt.Errorf("patch: %w", err)})
			return
		}
		// Only now is what the finished Jobs said persisted.
		if r.bk != nil && !r.skipMark {
			if err := r.bk.MarkBookkept(ctx, d.Client); err != nil {
				klog.FromContext(ctx).Error(err, "Marking finished Jobs bookkept failed; the next reconcile reads them again")
			}
		}
		if r.finalizerDropped {
			if err := SweepRBAC(ctx, d, obj.GetNamespace()); err != nil {
				klog.FromContext(ctx).Error(err, "Runner RBAC sweep failed; the resync sweep retries")
			}
		}
	}()

	if err := r.setup(ctx); err != nil {
		return ctrl.Result{}, pre.Owner, err
	}
	var res ctrl.Result
	if pre.Paused {
		res, err = r.paused(ctx)
	} else {
		res, err = r.run(ctx)
	}
	if errors.Is(err, errAnnotationConflict) {
		klog.FromContext(ctx).V(LogFlow).Info("The object changed while removing a consumed annotation; deciding again", "reason", err.Error())
		return ctrl.Result{RequeueAfter: LagRequeue}, pre.Owner, nil
	}
	if err != nil {
		// controller-runtime ignores RequeueAfter next to an error.
		return ctrl.Result{}, pre.Owner, err
	}
	return res, pre.Owner, nil
}

// reconciler carries one reconcile's working set.
type reconciler struct {
	d     Deps
	k     Kind
	obj   Object
	owner OwnerInfo
	st    CommonStatus

	eff      EffectiveConfig
	suffix   string
	deleting bool
	// durable is the durable inputs Secret, read once in setup; nil before
	// the first apply. Bookkeeping's digest pin is applied to it in memory.
	durable *inputs.Durable
	// bk is this pass's bookkeeping, whose finished Jobs are marked
	// bookkept once the status patch succeeded.
	bk *Bookkeeping
	// chunks is the state Secrets' metadata as readState listed them, for
	// repairOwners; nil when the state was not read, or Adopt rewrote
	// their owner references this pass.
	chunks []metav1.ObjectMeta

	identityName     string
	identityAllowed  bool
	credsReady       bool
	serviceAccount   string
	finalizerDropped bool
	// abandonWhy is why the abandon annotation released the deletion, for
	// the InfrastructureAbandoned event; "" unless abandoned this pass.
	abandonWhy string
	// retainedFrom is the uid of the earlier object whose retained Secrets
	// this pass found (checkRetained); "" when none.
	retainedFrom string
	// adopted is true when this pass adopted them (adoptRetained).
	adopted bool
	// isPaused is true on the paused branch: no Job starts.
	isPaused bool
	// inputsBytes is the size of the inputs rendered this pass, else of the
	// durable inputs; 0 when neither exists (captf_inputs_bytes).
	inputsBytes int
	// before holds the event-watched conditions as the reconcile found them.
	before map[string]metav1.Condition
	// logger is the reconcile's logger, for finish, which has no context.
	logger klog.Logger
	// consumed are the annotations removeAnnotation removed this pass; the
	// object in memory keeps them, so the deferred patch leaves them alone.
	consumed map[string]bool
	// patchedRV is the resourceVersion the last removeAnnotation left; ""
	// before any.
	patchedRV string
	// skipMark keeps the finished Jobs from being marked bookkept after a
	// consumed annotation could not be removed.
	skipMark bool
	// plans are the object's TerraformPlans, newest first (loadPlans); nil
	// for a kind that keeps none.
	plans []infrav1.TerraformPlan
	// guard is how an ExportsGuard kind guards an apply of the inputs built
	// this pass (observeGuard); nil for other kinds, or when no inputs
	// were built.
	guard *Guard
	// decided is true once DecideOp's decision was taken this pass;
	// planWait is then ApplyJobSucceeded while an apply waits for the
	// approval of its TerraformPlan (waitCondition), nil otherwise. finish reports
	// it on every pass of the wait (planWaitCondition), also those that
	// start a refresh or drift Job.
	decided  bool
	planWait *metav1.Condition
}

// setup sets up the reconcile using ctx: first-visit conditions, Deleting,
// the effective config, the state suffix and the durable inputs. It
// returns any error from computing the state suffix or reading the
// durable inputs.
func (r *reconciler) setup(ctx context.Context) error {
	captfconds.SetInitial(r.obj, r.k.Kind())
	r.deleting = !r.obj.GetDeletionTimestamp().IsZero()
	if r.deleting {
		conditions.Set(r.obj, metav1.Condition{
			Type: clusterv1.DeletingCondition, Status: metav1.ConditionTrue, Reason: clusterv1.DeletingReason,
		})
	}
	r.eff = Resolve(r.k.Spec(), r.owner.InfraCluster, r.d.DriftDefault, r.k.Mutable())
	suffix, err := state.Suffix(r.obj.GetNamespace(), r.k.Kind(), r.obj.GetName())
	if err != nil {
		return err
	}
	r.suffix = suffix
	r.st.StateSecretSuffix = suffix
	return r.readDurable(ctx)
}

// readDurable reads the object's durable inputs Secret using ctx into
// r.durable, r.st.Source.ImageDigest and r.inputsBytes; it leaves
// r.durable nil, without error, when no such Secret exists yet, and
// otherwise returns any read error.
func (r *reconciler) readDurable(ctx context.Context) error {
	d, err := inputs.Read(ctx, r.d.Client, r.obj.GetNamespace(), kindShort(r.k), r.obj.GetName())
	switch {
	case errors.Is(err, inputs.ErrNotFound):
		r.durable = nil
	case err != nil:
		return err
	default:
		r.durable = d
		r.st.Source.ImageDigest = d.Meta.ImageDigest
		r.inputsBytes = d.Files.Size()
	}
	return nil
}

// bookkeep runs Bookkeep using ctx with the durable Secret read in setup,
// and records a digest it pinned in memory instead of reading the Secret
// again. It then reads the object's TerraformPlans, records the plans the
// finished Jobs made, and moves the plans through their phases. It
// returns the resulting Bookkeeping, also stored on r.bk, or any error
// from Bookkeep, from removing a consumed annotation, or from reading or
// writing a TerraformPlan.
func (r *reconciler) bookkeep(ctx context.Context) (*Bookkeeping, error) {
	bk, err := Bookkeep(ctx, r.d, r.k, r.eff, r.suffix, r.durable)
	if err != nil {
		return nil, err
	}
	r.bk = bk
	r.releaseLeases(ctx, bk)
	if bk.PinnedDigest != "" && r.durable != nil {
		r.durable.Meta.ImageDigest = bk.PinnedDigest
		r.st.Source.ImageDigest = bk.PinnedDigest
	}
	if bk.MarkedApplied && r.durable != nil {
		r.durable.Meta.Applied = true
	}
	if bk.ExportsRecorded && r.durable != nil {
		r.durable.AppliedClusterOutputs, r.durable.AppliedExportsHash = bk.AppliedExports, bk.AppliedExportsHash
		r.durable.Pending, r.durable.Partial = nil, nil
	}
	if bk.PendingSet != nil && r.durable != nil {
		r.durable.Pending = bk.PendingSet
	}
	if bk.PartialSet != nil && r.durable != nil {
		r.durable.Partial = bk.PartialSet
	}
	if bk.InterruptedCleared && r.durable != nil {
		r.durable.InterruptedApply = ""
	}
	if err := r.consumeRestore(ctx, bk); err != nil {
		return nil, err
	}
	// The plans' own transitions first: a plan whose approved apply found
	// it changed fails, before the new plan supersedes what is live.
	if err := r.loadPlans(ctx); err != nil {
		return nil, err
	}
	if err := r.syncPlans(ctx, bk); err != nil {
		return nil, err
	}
	if err := r.recordPlans(ctx, bk); err != nil {
		return nil, err
	}
	return bk, nil
}

// provisioned reports whether the object's status.initialization.provisioned
// field is set and true.
func (r *reconciler) provisioned() bool {
	p := r.st.Initialization.Provisioned
	return p != nil && *p
}

// phase returns the condition phase for the object's current provisioned
// state: AfterProvisioned once provisioned, otherwise BeforeProvisioned.
func (r *reconciler) phase() captfconds.Phase {
	if r.provisioned() {
		return captfconds.AfterProvisioned
	}
	return captfconds.BeforeProvisioned
}

// finish points status.pendingPlanRef at the live TerraformPlan as the pass
// leaves it, sets the object's apply condition from applyCond, else from the
// wait for a plan's approval that stands (planWaitCondition), else from
// bk's ApplyJob (applyJobCondition: the held condition while a change of
// the cluster's exports waits for approval), sets the Ready condition for
// the current phase, emits condition-transition events, and updates the
// per-object metrics gauges (or removes them when the finalizer was just
// dropped). It returns res unchanged along with any error from setting
// Ready.
func (r *reconciler) finish(bk *Bookkeeping, applyCond *metav1.Condition, res ctrl.Result) (ctrl.Result, error) {
	r.setPendingPlanRef()
	if applyCond == nil {
		applyCond = r.planWaitCondition(bk)
	}
	var c metav1.Condition
	switch {
	case applyCond != nil:
		c = *applyCond
	case bk != nil:
		c = r.applyJobCondition(bk)
	}
	if c.Type != "" {
		conditions.Set(r.obj, c)
	}
	if err := captfconds.SetReady(r.obj, r.k.Kind(), r.phase()); err != nil {
		return ctrl.Result{}, err
	}
	emitTransitions(r.d, r.logger, r.k.Kind(), r.obj, r.before, bk)
	// The per-object gauges go with the finalizer: a gone object must not
	// leave a series behind.
	if r.finalizerDropped {
		r.d.Metrics.DeleteObject(r.k.Kind(), r.obj.GetNamespace(), r.obj.GetName())
	} else {
		r.recordGauges(bk)
	}
	return res, nil
}

// paused is the branch run using ctx when the object is paused: Job
// bookkeeping, a stuck Job deleted, and block-move cleared once no Job
// runs, which is what clusterctl move waits for. No Job starts. It
// returns the result and error from finish.
func (r *reconciler) paused(ctx context.Context) (ctrl.Result, error) {
	if r.deleting {
		// A paused object never destroys: clusterctl move deletes its
		// source objects while the Cluster is paused.
		conditions.Set(r.obj, metav1.Condition{
			Type: clusterv1.DeletingCondition, Status: metav1.ConditionTrue, Reason: clusterv1.DeletingReason,
			Message: "Deletion waits until the object is unpaused (Cluster spec.paused or the " +
				"cluster.x-k8s.io/paused annotation); a paused object never runs a destroy",
		})
	}
	bk, err := r.bookkeep(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if bk.Active != nil {
		// A Job that can never start would otherwise hold block-move, and
		// with it clusterctl move, until its deadline.
		deleted, err := DeleteStuckJob(ctx, r.d, r.obj, bk.Active)
		if err != nil {
			return ctrl.Result{}, err
		}
		if deleted {
			// It never started: nothing vanished (recordVanishedApply).
			bk.Active, r.st.ActiveJob = nil, infrav1.ActiveJob{}
		}
	}
	if bk.Active != nil {
		r.recordActive(bk.Active)
		return r.finish(bk, nil, ctrl.Result{})
	}
	// Block-move stays until the API server confirms the Job is gone.
	lag, err := r.cacheLag(ctx, bk)
	if err != nil {
		return ctrl.Result{}, err
	}
	if lag {
		return r.finish(bk, nil, ctrl.Result{RequeueAfter: LagRequeue})
	}
	if err := r.recordVanishedApply(ctx, bk); err != nil {
		return ctrl.Result{}, err
	}
	ClearBlockMove(r.obj)
	r.st.ActiveJob = infrav1.ActiveJob{}
	return r.finish(bk, nil, ctrl.Result{})
}

// run is the unpaused path after setup, using ctx: credentials, Job
// bookkeeping, the active-Job check, state and inputs, the operation
// decision, and starting it. A deleting object prepares its credentials
// only for a Job it is about to start (deletionCredentials): dropping the
// finalizer without a Job (no state, abandoned) never waits on them. It
// returns the controller-runtime result and any error from that path.
func (r *reconciler) run(ctx context.Context) (ctrl.Result, error) {
	r.resolveIdentity()
	if !r.deleting {
		if err := r.credentials(ctx); err != nil {
			return ctrl.Result{}, err
		}
	}
	lastRefresh := r.st.LastRefresh
	bk, err := r.bookkeep(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	// A Job runs; state is not read.
	if bk.Active != nil {
		deleted, err := DeleteStuckJob(ctx, r.d, r.obj, bk.Active)
		if err != nil {
			return ctrl.Result{}, err
		}
		if deleted {
			// It never started: nothing vanished (recordVanishedApply).
			// DeleteStuckJob cleared status.activeJob on the API server
			// before the delete; the pass's conditions still follow.
			bk.Active, r.st.ActiveJob = nil, infrav1.ActiveJob{}
			return r.finish(bk, nil, ctrl.Result{RequeueAfter: time.Second})
		}
		klog.FromContext(withJob(ctx, bk.Active)).V(LogDebug).Info("Job running", "op", jobs.OpOf(bk.Active))
		SetBlockMove(r.obj)
		r.recordActive(bk.Active)
		if jobs.OpOf(bk.Active) == jobs.OpApply && !r.provisioned() {
			captfconds.SetInfrastructureHealthy(r.obj, nil, captfconds.HealthApplyStarted)
		}
		return r.finish(bk, nil, ctrl.Result{RequeueAfter: ActiveJobRequeue})
	}

	// No Job is active, unless the Job cache lags behind the one
	// status.activeJob names: then wait for it. Otherwise clear
	// block-move and the active-Job status.
	lag, err := r.cacheLag(ctx, bk)
	if err != nil {
		return ctrl.Result{}, err
	}
	if lag {
		return r.finish(bk, nil, ctrl.Result{RequeueAfter: LagRequeue})
	}
	if err := r.recordVanishedApply(ctx, bk); err != nil {
		return ctrl.Result{}, err
	}
	ClearBlockMove(r.obj)
	r.st.ActiveJob = infrav1.ActiveJob{}
	if r.deleting && bk.DestroySucceeded {
		return r.cleanup(ctx, bk, cleanupDestroyed)
	}
	if r.deleting {
		blocked, err := r.k.DeletionBlocked(ctx, r.owner)
		if err != nil {
			return ctrl.Result{}, err
		}
		if blocked {
			return r.finish(bk, nil, ctrl.Result{RequeueAfter: GateRequeue})
		}
	}
	view, stateErr := r.readState(ctx, bk, lastRefresh)
	if errors.Is(stateErr, errRetainedState) {
		// Before repairOwners: another object's retained Secrets are never
		// owned without spec.adoptRetainedState.
		return r.heldOnRetained(ctx, bk)
	}
	if stateErr != nil && !errors.Is(stateErr, errStateUnreadable) {
		return ctrl.Result{}, stateErr
	}
	// No Job is active, and cleanup has not started: the object's Secrets
	// can be re-owned (after a restore, or a chunk written without a ref).
	r.repairOwners(ctx, r.chunks)
	if stateErr == nil && bk.ForeignLock != "" {
		conditions.Set(r.obj, metav1.Condition{
			Type: infrav1.StateReadableCondition, Status: metav1.ConditionFalse, Reason: infrav1.StateLockedReason, Message: bk.ForeignLock,
		})
	}
	held := stateErr != nil
	// deletionPolicy Retain releases any deletion, held or not: decide
	// returns ActionRetain before a restore or a destroy.
	retain := r.deleting && r.eff.DeletionPolicy == infrav1.DeletionPolicyRetain
	// Before the restore and destroy decisions: neither may hide it.
	if r.deleting && !retain && r.abandonRequested() {
		if why := r.abandonCause(held, bk); why != "" {
			return r.abandon(ctx, bk, why)
		}
	}

	var in any
	var gate *Gate
	if !r.deleting && (r.k.Mutable() || !r.provisioned()) {
		if in, gate, err = r.buildInputs(ctx, bk, &view); err != nil {
			return ctrl.Result{}, err
		}
	}
	// A restore is what an unreadable or lost state waits for, so it is
	// looked for before giving up on one; a deletion waits for it too.
	backup, restore, err := r.restoreTarget(ctx, held && !retain)
	if err != nil {
		return ctrl.Result{}, err
	}
	if held && !restore && !retain {
		if r.deleting {
			return r.deletionHeld(ctx, bk)
		}
		return r.finish(bk, nil, ctrl.Result{RequeueAfter: StateRequeue})
	}

	di := r.decideInput(bk, view)
	di.Restore = restore
	di.StateHeld = r.deleting && held
	di.Retain = retain
	dec, waiting := decide(di)
	klog.FromContext(ctx).V(LogFlow).Info("Decided", "op", decisionOp(dec), "reason", dec.Reason, "requeueAfter", dec.RequeueAfter)
	r.d.Metrics.Decision(r.k.Kind(), decisionOp(dec), dec.Reason)
	if !r.deleting {
		if err := r.supersedeStale(ctx, dec, gate, view); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.prunePlans(ctx); err != nil {
			return ctrl.Result{}, err
		}
	}
	r.decided = true
	switch p := r.planNamed(dec.Plan); {
	case p == nil:
	case waiting == ReasonPlanAwaitingApproval:
		// Also while a refresh or drift Job runs meanwhile: the wait is
		// what the condition reports until it ends.
		c := r.waitCondition(bk, p)
		r.planWait = &c
	case waiting == ReasonDestructivePlanBlocked && bk.ApplyJob.Reason != infrav1.DestructivePlanBlockedReason:
		// No blocked Job reports the plan (they stayed behind on a
		// clusterctl move): the plan itself does.
		c := blockedWaitCondition(p)
		r.planWait = &c
	}

	switch dec.Action {
	case ActionDropFinalizer:
		return r.cleanup(ctx, bk, cleanupNoState)
	case ActionRetain:
		return r.cleanup(ctx, bk, cleanupRetained)
	case ActionJob:
		if r.deleting {
			r.deletionCredentials(ctx)
			if !r.credsReady {
				// Covers an identity that no longer allows the namespace
				// (or is gone) too: credsReady requires it.
				if ok, res, err := r.abandonStart(ctx, bk, "it waits for its credentials: "+r.credentialsBlocker()); ok {
					return res, err
				}
			}
		}
		if dec.Op == jobs.OpRestore {
			return r.startRestore(ctx, bk, dec, backup)
		}
		return r.startOp(ctx, bk, dec, view, in, gate)
	}
	return r.finish(bk, nil, ctrl.Result{RequeueAfter: dec.RequeueAfter})
}

// decideInput gathers what DecideOp reads from bk, the pass's bookkeeping,
// and view, the state as read this pass. It returns the assembled
// DecideInput.
func (r *reconciler) decideInput(bk *Bookkeeping, view StateView) DecideInput {
	var health time.Duration
	if r.provisioned() {
		health = r.eff.HealthCheckInterval
	}
	// Membership is refreshed provisioned or not: a pool's members join
	// before its health reads provisioned. Converging reads the object as
	// readState's ApplyOutputs left it this pass.
	var converging bool
	if m, ok := r.k.(MembershipObserver); ok && view.Exists {
		converging = view.Pending || m.MembershipConverging()
	}
	jv := bk.View
	jv.LastApplyFailed = r.lastApplyFailed(bk)
	return DecideInput{
		Deleting:       r.deleting,
		Mutable:        r.k.Mutable(),
		State:          view,
		Jobs:           jv,
		LastRefresh:    timePtr(r.st.LastRefresh),
		LastDriftCheck: timePtr(r.st.LastDriftCheck),
		DriftInterval:  r.eff.DriftInterval,
		Now:            r.d.Clock.Now(),

		RefreshAfterApply:  r.k.RefreshAfterApply(),
		LastApplySucceeded: lastApplySucceeded(bk),
		PendingRefreshes:   int(r.st.PendingRefreshes),
		Remediate:          r.k.Mutable() && r.eff.DriftAction == infrav1.DriftActionRemediate && conditions.IsTrue(r.obj, infrav1.DriftDetectedCondition),
		HealthInterval:     health,
		MembershipInterval: r.eff.MembershipRefreshInterval,
		Converging:         converging,
		UID:                string(r.obj.GetUID()),
		Created:            r.obj.GetCreationTimestamp().Time,
		ApprovalHash:       r.guardApprovalHash(),
		Unheld:             r.guard != nil && r.guard.Guarded && (r.guard.Partial || r.guard.Unrecorded),
		ManualApply:        r.manualApply(),
		Plan:               r.planView(),
	}
}

// lastApplyFailed reports whether an apply is due because one failed
// (JobsView.LastApplyFailed, from bk), because a failed apply may have
// left part of a change of the cluster's exports in the state
// (Guard.Partial), which a blocked apply since did not undo, or because
// an apply Job disappeared while it ran (interruptedApply), which no
// result tells about: an apply is then due until one succeeds, even of
// the state's own inputs. An apply started while it holds is marked so
// (AfterFailedApplyAnnotation).
func (r *reconciler) lastApplyFailed(bk *Bookkeeping) bool {
	return bk.View.LastApplyFailed || (r.guard != nil && r.guard.Partial) || r.interruptedApply() != ""
}

// startOp starts the decided operation using ctx, or reports why it cannot
// start. bk is the pass's bookkeeping, dec is DecideOp's decision, view is
// the state as read this pass, in is the built inputs (nil when none were
// built), and gate is the pending-dependency gate, non-nil when the
// operation must wait instead of starting.
func (r *reconciler) startOp(ctx context.Context, bk *Bookkeeping, dec Decision, view StateView, in any, gate *Gate) (ctrl.Result, error) {
	op := dec.Op
	if op == jobs.OpDestroy && !r.identityAllowed {
		c := metav1.Condition{
			Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionFalse, Reason: infrav1.IdentityNotAllowedReason,
			Message: "Destroy waits until the identity allows this namespace again",
		}
		return r.finish(bk, &c, ctrl.Result{RequeueAfter: GateRequeue})
	}
	if !r.credsReady {
		return r.waitForCredentials(bk, op)
	}
	if gate != nil {
		return r.finish(bk, nil, ctrl.Result{RequeueAfter: GateRequeue})
	}
	if (op == jobs.OpApply || op == jobs.OpPlan) && in == nil {
		// A provisioned immutable object builds no inputs: its state lacks
		// the inputs hash, and re-applying is impossible.
		conditions.Set(r.obj, metav1.Condition{
			Type: infrav1.StateReadableCondition, Status: metav1.ConditionFalse, Reason: infrav1.StateLostReason,
			Message: "The state carries no inputs hash although the object is provisioned; it cannot be re-applied. Restore a state backup with the " + infrav1.RestoreStateAnnotation + " annotation; see https://captf.io/docs/operator-guide/runbooks/state-restore.html",
		})
		return r.finish(bk, nil, ctrl.Result{RequeueAfter: StateRequeue})
	}

	if op != jobs.OpDestroy {
		// A teardown never wedges on a policy check; only other
		// operations are refused.
		if err := ValidateEffectiveJobPolicy(r.eff.Jobs); err != nil {
			c := metav1.Condition{
				Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionFalse, Reason: infrav1.JobPolicyInvalidReason,
				Message: fmt.Sprintf("The effective Job policy is invalid; no Job starts: %v", err),
			}
			return r.finish(bk, &c, ctrl.Result{RequeueAfter: RetryMax})
		}
	}

	req := JobRequest{
		Op:             op,
		Identity:       r.identityName,
		ServiceAccount: r.serviceAccount,
		Suffix:         r.suffix,
		ClusterName:    ClusterName(r.obj, r.owner),
		Attempt:        jobs.Attempt(bk.Jobs, op),
		ForceUnlockID:  bk.ForceUnlockID,
		Policy:         r.eff.Jobs,
		Source:         r.jobSource(op),
		InputsHash:     view.InputsHash,
		Why:            dec.Reason,
	}
	if r.durable != nil {
		req.PinnedDigest = r.durable.Meta.ImageDigest
	}

	files, ok, err := r.files(ctx, op, in)
	if errors.Is(err, render.ErrInputsTooLarge) {
		// Retrying cannot help until the inputs change, which re-triggers
		// the reconcile.
		c := metav1.Condition{
			Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionFalse, Reason: infrav1.InputsTooLargeReason,
			Message: fmt.Sprintf("The rendered inputs for %s are too large for a Secret; no Job starts. Shrink the inputs (bootstrap data, exports, variables)", op),
		}
		return r.finish(bk, &c, ctrl.Result{RequeueAfter: RetryMax})
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ok {
		return r.noFiles(ctx, bk, op)
	}
	req.Files = files
	switch op {
	case jobs.OpApply, jobs.OpPlan:
		if req.InputsHash, err = r.inputsHash(in); err != nil {
			return ctrl.Result{}, err
		}
		// A pool's apply is guarded only for a change of the cluster's
		// exports, under its approval hash.
		if op == jobs.OpApply && r.guard != nil {
			r.guardRequest(&req)
		}
		req.ExpectPlan = dec.ExpectPlan
		if op == jobs.OpApply {
			req.Plan, req.AllowDeletesHash = dec.Plan, dec.AllowDeletes
		}
		req.AfterFailedApply = op == jobs.OpApply && r.lastApplyFailed(bk)
		if op == jobs.OpApply {
			req.AfterInterruptedApply = r.interruptedApply()
		}
		if dec.Reason == "DriftRemediation" {
			// The inputs are unchanged, so without a tick the name would
			// be the retained first apply's.
			req.DriftTick = "remediate/" + stamp(r.st.LastDriftCheck)
			req.Remediation = op == jobs.OpApply
		}
	case jobs.OpRefresh, jobs.OpDrift:
		// The Job name embeds the stamp of the op's last success. It stays
		// the same across reconciles until the next success, so a retry
		// after a stale read (which computes the same attempt, the op's next
		// sequence number) finds the Job it created instead of starting a
		// second one; after a success the stamp moves and the name with it.
		last := r.st.LastDriftCheck
		if op == jobs.OpRefresh {
			last = r.st.LastRefresh
		}
		req.DriftTick = stamp(last)
	}

	// The leases name the Job about to be created, so they come after
	// everything that goes into its name, and before anything is written.
	wait, err := r.takeLeases(ctx, req, JobName(r.k, req))
	if err != nil {
		return ctrl.Result{}, err
	}
	if wait.reason != "" {
		return r.waitForLease(ctx, bk, op, wait)
	}
	logRendered(ctx, req.InputsHash, req.Files)
	job, err := StartJob(ctx, r.d, r.k, req)
	if errors.Is(err, ErrStartDeferred) {
		return r.startDeferred(ctx, bk, op, err)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	r.jobStarted(dec, req, view, job)
	if op == jobs.OpApply && !r.provisioned() {
		captfconds.SetInfrastructureHealthy(r.obj, nil, captfconds.HealthApplyStarted)
	}
	return r.finish(bk, nil, ctrl.Result{RequeueAfter: ActiveJobRequeue})
}

// startDeferred ends, logging with ctx, a pass whose start of op's Job
// StartJob deferred: err, which wraps ErrStartDeferred, says why (a pause
// read live, leases that could not be taken fresh again). No Job was
// created and its leases went back, so it is not a failure: bk, the pass's
// bookkeeping, is reported as on any pass that starts nothing, and the
// next pass, soon, decides again. It returns that requeue and any error
// from finish.
func (r *reconciler) startDeferred(ctx context.Context, bk *Bookkeeping, op jobs.Op, err error) (ctrl.Result, error) {
	klog.FromContext(ctx).V(LogFlow).Info("The Job start was deferred; deciding again shortly", "op", op, "reason", err.Error())
	return r.finish(bk, nil, ctrl.Result{RequeueAfter: LagRequeue})
}

// jobStarted emits what starting job for the decided operation dec means
// besides JobCreated, given the request req that started it and view, the
// state as read this pass. Under applyPolicy Manual the plan Job reports
// an input change (counted once per change).
func (r *reconciler) jobStarted(dec Decision, req JobRequest, view StateView, job *batchv1.Job) {
	switch {
	case dec.Reason == "InputsChanged" && req.Op == jobs.OpPlan:
		r.d.Metrics.InputsHashChanged(r.k.Kind())
		r.d.EmitRelated(r.obj, job, corev1.EventTypeNormal, EventInputsChanged, "Run",
			"The inputs changed (hash %s, state has %s); Job %s plans them for approval (applyPolicy Manual)", req.InputsHash, view.InputsHash, job.Name)
	case dec.Reason == "InputsChanged" && dec.ExpectPlan == "":
		r.d.Metrics.InputsHashChanged(r.k.Kind())
		r.d.EmitRelated(r.obj, job, corev1.EventTypeNormal, EventInputsChanged, "Run",
			"The inputs changed (hash %s, state has %s); Job %s applies them", req.InputsHash, view.InputsHash, job.Name)
	case dec.Reason == "DriftRemediation" && req.Op == jobs.OpApply:
		r.d.EmitRelated(r.obj, job, corev1.EventTypeNormal, EventDriftRemediationStarted, "Run",
			"Remediating drift: Job %s applies the current inputs", job.Name)
	}
}

// files renders what op runs, using ctx: the current inputs in for Apply,
// the durable inputs for Destroy, and checkFiles for Refresh and Drift. It
// returns the rendered files, ok false when there is nothing to run
// against, and any render error.
func (r *reconciler) files(ctx context.Context, op jobs.Op, in any) (render.Files, bool, error) {
	switch op {
	case jobs.OpApply, jobs.OpPlan:
		files, err := r.renderFiles(in)
		if err != nil {
			return render.Files{}, false, err
		}
		return files, true, nil
	case jobs.OpDestroy:
		return r.destroyFiles(ctx)
	}
	return r.checkFiles(in)
}

// noFiles reports, using ctx and the pass's bookkeeping bk, that op has
// nothing to run against: a destroy in ApplyJobSucceeded, a refresh or
// drift check in DriftJobSucceeded (DurableInputsMissing), since without
// it InfrastructureHealthy and DriftDetected keep their last readings. It
// returns the result and error from finish.
func (r *reconciler) noFiles(ctx context.Context, bk *Bookkeeping, op jobs.Op) (ctrl.Result, error) {
	if op == jobs.OpDestroy {
		if ok, res, err := r.abandonStart(ctx, bk, "the durable inputs Secret is missing"); ok {
			return res, err
		}
		c := metav1.Condition{
			Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionFalse, Reason: infrav1.DestroyFailedReason,
			Message: "The durable inputs Secret is missing, so destroy cannot be rendered; see https://captf.io/docs/operator-guide/runbooks/stuck-destroy.html",
		}
		return r.finish(bk, &c, ctrl.Result{RequeueAfter: RetryMax})
	}
	klog.FromContext(ctx).Info("No durable inputs to run against; skipping", "op", op)
	conditions.Set(r.obj, metav1.Condition{
		Type: infrav1.DriftJobSucceededCondition, Status: metav1.ConditionUnknown, Reason: infrav1.DurableInputsMissingReason,
		Message: fmt.Sprintf("The %s Job is due, but the durable inputs Secret is missing, so it cannot be rendered: no refresh or drift check runs, "+
			"and InfrastructureHealthy keeps its last reading. Restore the Secret; see %s", op, durableRunbook),
	})
	return r.finish(bk, nil, ctrl.Result{RequeueAfter: RetryMax})
}

// durableRunbook is the runbook section on a missing durable inputs
// Secret.
const durableRunbook = "https://captf.io/docs/operator-guide/runbooks/stuck-destroy.html#the-durable-inputs-secret-is-missing"

// jobSource returns the image a Job runs for op: the spec's for Apply and
// mutable kinds; for other operations of immutable kinds, the image
// pinned in the durable Secret, with the current pull policy.
func (r *reconciler) jobSource(op jobs.Op) infrav1.Source {
	src := r.k.Spec().Source
	if op != jobs.OpApply && !r.k.Mutable() && r.durable != nil && r.durable.Meta.Image != "" {
		src.Image = r.durable.Meta.Image
	}
	return src
}

// checkFiles is what Refresh and Drift run against: the current inputs in
// of a mutable kind, the pinned inputs of an immutable one. Neither writes
// the durable Secret; only Apply does. It returns the files to run
// against, ok false when there is nothing to run against, and any render
// error.
func (r *reconciler) checkFiles(in any) (render.Files, bool, error) {
	if r.k.Mutable() && in != nil {
		files, err := r.renderFiles(in)
		if err != nil {
			return render.Files{}, false, err
		}
		return files, true, nil
	}
	if r.durable != nil {
		return r.durable.Files, true, nil
	}
	return render.Files{}, false, nil
}

// destroyFiles renders destroy from the durable inputs; a mutable kind
// whose Secret is gone falls back, using ctx, to its current inputs when
// they build. It returns the files to run against, ok false when there is
// nothing to run against, and any build or render error.
func (r *reconciler) destroyFiles(ctx context.Context) (render.Files, bool, error) {
	if r.durable != nil {
		return r.durable.Files, true, nil
	}
	if !r.k.Mutable() {
		return render.Files{}, false, nil
	}
	in, gate, err := r.k.BuildInputs(ctx, r.owner, nil)
	if err != nil || gate != nil {
		return render.Files{}, false, err
	}
	files, err := r.renderFiles(in)
	if err != nil {
		return render.Files{}, false, err
	}
	return files, true, nil
}

// renderFiles renders in and records its size for captf_inputs_bytes,
// also when it is too large to run. It returns the rendered files, or an
// error wrapping any render failure.
func (r *reconciler) renderFiles(in any) (render.Files, error) {
	files, err := render.Root(r.k.Role(), in)
	var tooLarge *render.InputsTooLargeError
	switch {
	case errors.As(err, &tooLarge):
		r.inputsBytes = tooLarge.Bytes
	case err == nil:
		r.inputsBytes = files.Size()
	}
	if err != nil {
		return render.Files{}, fmt.Errorf("render: %w", err)
	}
	return files, nil
}

// inputsHash returns the hash of in, the built inputs, for the
// reconciler's role and spec image, or an error wrapping any hashing
// failure.
func (r *reconciler) inputsHash(in any) (string, error) {
	src := r.k.Spec().Source
	h, err := hash.Inputs(r.k.Role(), src.Image, in)
	if err != nil {
		return "", fmt.Errorf("inputs hash: %w", err)
	}
	return h, nil
}

// cleanupMode is why cleanup removes the finalizer.
type cleanupMode int

const (
	// cleanupDestroyed follows a successful destroy.
	cleanupDestroyed cleanupMode = iota
	// cleanupNoState is a deletion without state: nothing to destroy.
	cleanupNoState
	// cleanupAbandoned releases a deletion whose destroy cannot run
	// without one (AbandonInfrastructureAnnotation).
	cleanupAbandoned
	// cleanupRetained keeps the infrastructure and its state for a later
	// adoption (deletionPolicy Retain).
	cleanupRetained
	// cleanupReleased drops the finalizer of an object held on another
	// object's retained state, touching none of it (heldOnRetained).
	cleanupReleased
)

// cleanup removes the finalizer, using ctx and the pass's bookkeeping bk,
// after a successful destroy, on deletion without state, when a deletion
// is abandoned, with deletionPolicy Retain (Retain keeps the state), or
// for an object held on another object's retained state (release only);
// mode says which. A live Job holding the run lease, which the Job cache
// has not shown yet, defers it by LagRequeue, as does a credential mirror
// changed under the owner removal (errMirrorConflict). It returns the
// result and error from finish, or any other error from Cleanup, Retain
// or release.
func (r *reconciler) cleanup(ctx context.Context, bk *Bookkeeping, mode cleanupMode) (ctrl.Result, error) {
	holder, live, err := runLive(ctx, r.d, r.obj.GetNamespace(), r.suffix)
	if err != nil {
		return ctrl.Result{}, err
	}
	if live {
		klog.FromContext(ctx).V(LogFlow).Info("A live Job holds the run lease; the finalizer stays until it finishes", "holder", holder)
		return r.finish(bk, nil, ctrl.Result{RequeueAfter: LagRequeue})
	}
	var kept Retained
	switch mode {
	case cleanupRetained:
		kept, err = Retain(ctx, r.d, r.k, r.suffix, r.identityName)
	case cleanupReleased:
		err = release(ctx, r.d, r.k, r.identityName)
	default:
		err = Cleanup(ctx, r.d, r.k, r.suffix, r.identityName)
	}
	if errors.Is(err, errMirrorConflict) {
		// Cleanup logged the race; the next pass, with the finalizer
		// still on, runs it again from the mirror read.
		return r.finish(bk, nil, ctrl.Result{RequeueAfter: LagRequeue})
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	r.finalizerDropped = true
	klog.FromContext(ctx).Info("Removed the finalizer", "destroyed", mode == cleanupDestroyed, "abandoned", mode == cleanupAbandoned,
		"retained", mode == cleanupRetained, "heldOnRetained", mode == cleanupReleased)
	switch mode {
	case cleanupDestroyed:
		r.d.Emit(r.obj, corev1.EventTypeNormal, EventDestroyed, "Delete", "Infrastructure destroyed; state and inputs removed")
		r.d.Emit(r.obj, corev1.EventTypeNormal, EventFinalizerRemoved, "Delete", "Removed finalizer %s after the destroy", r.k.Finalizer())
	case cleanupAbandoned:
		r.emitAbandoned()
	case cleanupRetained:
		r.d.Emit(r.obj, corev1.EventTypeNormal, EventInfrastructureRetained, "Delete",
			"Removed finalizer %s without a destroy (deletionPolicy Retain): the infrastructure keeps running. "+
				"Kept %d state Secret(s), %d state backup Secret(s) and %d durable inputs Secret, without owner references and labeled %s=%s; "+
				"a %s of the same namespace and name adopts them with spec.adoptRetainedState: true",
			r.k.Finalizer(), kept.State, kept.Backups, kept.Inputs, state.RetainedFromUIDLabel, r.obj.GetUID(), r.k.Kind())
	case cleanupReleased:
		r.d.Emit(r.obj, corev1.EventTypeNormal, EventFinalizerRemoved, "Delete",
			"Removed finalizer %s without a destroy: this object never ran a Job, and the state it found was retained by an earlier object (%s=%s), "+
				"which is left untouched", r.k.Finalizer(), state.RetainedFromUIDLabel, r.retainedFrom)
	default:
		r.d.Emit(r.obj, corev1.EventTypeNormal, EventFinalizerRemoved, "Delete", "Removed finalizer %s: there was no state, so nothing to destroy", r.k.Finalizer())
	}
	return r.finish(bk, nil, ctrl.Result{})
}

// recordActive mirrors the running job into status.activeJob.
func (r *reconciler) recordActive(job *batchv1.Job) {
	attempt, _ := strconv.ParseInt(job.Labels[jobs.AttemptLabel], 10, 32)
	start := job.Status.StartTime
	if start == nil {
		start = &job.CreationTimestamp
	}
	r.st.ActiveJob = infrav1.ActiveJob{
		Name: job.Name, Operation: infrav1.Operation(jobs.OpOf(job)), Attempt: int32(attempt), StartTime: start,
	}
}

// resolveIdentity sets the name of the identity the object's Jobs run
// with. Immutable kinds use the identity pinned in their durable Secret:
// the apply that wrote it records the identity it ran with, and destroy,
// refresh and drift keep using it even when the cluster's
// defaults.identityRef changes.
func (r *reconciler) resolveIdentity() {
	r.identityName = r.eff.IdentityName
	if !r.k.Mutable() && r.durable != nil && r.durable.Meta.Identity != "" {
		r.identityName = r.durable.Meta.Identity
	}
}

// deletionCredentials runs credentials using ctx for the Job a deleting
// object is about to start. A failure leaves credsReady false, recorded
// in the credential conditions, instead of failing the reconcile: in a
// terminating namespace NamespaceLifecycle forbids creating the runner
// ServiceAccount, RoleBinding and mirror, and returning that error would
// only retry it with backoff. The Job then waits (waitForCredentials).
func (r *reconciler) deletionCredentials(ctx context.Context) {
	if err := r.credentials(ctx); err != nil {
		r.credsReady = false
		klog.FromContext(ctx).Info("The credentials for the deletion's Job are not ready; it waits", "err", err)
	}
}

// waitForCredentials ends a pass whose Job op waits for credentials that
// are not ready, with the pass's bookkeeping bk. While deleting, the
// Deleting condition's message says what the teardown waits for. It
// returns the result and error from finish.
func (r *reconciler) waitForCredentials(bk *Bookkeeping, op jobs.Op) (ctrl.Result, error) {
	if r.deleting {
		conditions.Set(r.obj, metav1.Condition{
			Type: clusterv1.DeletingCondition, Status: metav1.ConditionTrue, Reason: clusterv1.DeletingReason,
			Message: fmt.Sprintf("The %s Job waits for its credentials: %s", op, r.credentialsBlocker()),
		})
	}
	return r.finish(bk, nil, ctrl.Result{RequeueAfter: GateRequeue})
}

// credentialsBlocker returns a description of the first credential
// condition that is not True (the identity, the mirror, then the runner
// RBAC), or a generic one when all are.
func (r *reconciler) credentialsBlocker() string {
	for _, t := range []string{infrav1.IdentityAllowedCondition, infrav1.CredentialsMirroredCondition, infrav1.RunnerRBACReadyCondition} {
		if c := conditions.Get(r.obj, t); c != nil && c.Status != metav1.ConditionTrue {
			msg := fmt.Sprintf("%s is %s (%s)", t, c.Status, c.Reason)
			if c.Message != "" {
				msg += ": " + c.Message
			}
			return msg
		}
	}
	return "they are not ready"
}

// credentials handles identity, allowedNamespaces, the source Secret's
// ownerRef for move, the mirror and the runner RBAC, using ctx, for the
// identity resolveIdentity chose. Each failure is also recorded in the
// IdentityAllowed, CredentialsMirrored or RunnerRBACReady condition. It
// returns any error from those steps.
func (r *reconciler) credentials(ctx context.Context) error {
	mirrorOK, err := r.identity(ctx)
	if err != nil {
		return err
	}
	sa, reason, err := rbac.EnsureRunner(ctx, r.d.Client, r.obj.GetNamespace(), &r.eff.Jobs.ServiceAccountName)
	c := metav1.Condition{Type: infrav1.RunnerRBACReadyCondition, Status: metav1.ConditionFalse, Reason: reason}
	switch {
	case err != nil:
		c.Message = err.Error()
	case reason == infrav1.RBACReadyReason:
		c.Status = metav1.ConditionTrue
	default:
		c.Message = "ServiceAccount " + r.eff.Jobs.ServiceAccountName + " does not carry " + rbac.RunnerLabel + "=true"
	}
	conditions.Set(r.obj, c)
	if err != nil {
		return err
	}
	r.serviceAccount = sa
	r.credsReady = r.identityAllowed && mirrorOK && reason == infrav1.RBACReadyReason
	return nil
}

// identity sets IdentityAllowed and CredentialsMirrored using ctx and
// reports whether the mirror is in place.
func (r *reconciler) identity(ctx context.Context) (bool, error) {
	ns := r.obj.GetNamespace()
	set := func(status metav1.ConditionStatus, reason, msg string) {
		conditions.Set(r.obj, metav1.Condition{Type: infrav1.IdentityAllowedCondition, Status: status, Reason: reason, Message: msg})
	}
	pending := func(msg string) {
		conditions.Set(r.obj, metav1.Condition{
			Type: infrav1.CredentialsMirroredCondition, Status: metav1.ConditionUnknown, Reason: infrav1.MirrorPendingReason, Message: msg,
		})
	}
	if r.identityName == "" {
		set(metav1.ConditionFalse, infrav1.IdentityNotFoundReason, "No identityRef on the object or the cluster defaults")
		pending("No identity")
		return false, nil
	}
	id, err := identity.Get(ctx, r.d.APIReader, r.identityName)
	if apierrors.IsNotFound(err) {
		set(metav1.ConditionFalse, infrav1.IdentityNotFoundReason, "TerraformClusterIdentity "+r.identityName+" not found")
		pending("No identity")
		return false, nil
	}
	if err != nil {
		set(metav1.ConditionUnknown, infrav1.IdentityCheckFailedReason, err.Error())
		return false, err
	}
	ok, reason, err := identity.Allowed(ctx, r.d.APIReader, id, ns)
	if err != nil {
		set(metav1.ConditionUnknown, reason, err.Error())
		return false, err
	}
	if !ok {
		set(metav1.ConditionFalse, reason, "TerraformClusterIdentity "+r.identityName+" does not allow namespace "+ns)
		pending("Identity not allowed; mirror revoked")
		removed, err := identity.Revoke(ctx, r.d.Client, r.identityName, ns)
		if removed && err == nil {
			r.d.Emit(r.obj, corev1.EventTypeNormal, EventMirrorRemoved, "Credentials",
				"Removed credential mirror %s: TerraformClusterIdentity %s no longer allows namespace %s", identity.MirrorName(r.identityName), r.identityName, ns)
		}
		return false, err
	}
	src, err := identity.SourceSecret(ctx, r.d.APIReader, id)
	if errors.Is(err, identity.ErrSecretNotFound) {
		set(metav1.ConditionFalse, infrav1.SecretNotFoundReason, err.Error())
		pending("Identity Secret not found")
		return false, nil
	}
	if err != nil {
		set(metav1.ConditionUnknown, infrav1.IdentityCheckFailedReason, err.Error())
		return false, err
	}
	set(metav1.ConditionTrue, infrav1.IdentityAllowedReason, "")
	r.identityAllowed = true
	if err := identity.EnsureSourceOwnerRef(ctx, r.d.Client, id, src); err != nil {
		conditions.Set(r.obj, metav1.Condition{
			Type: infrav1.CredentialsMirroredCondition, Status: metav1.ConditionFalse, Reason: infrav1.MirrorFailedReason, Message: err.Error(),
		})
		return false, err
	}
	mirror := metav1.Condition{Type: infrav1.CredentialsMirroredCondition, Status: metav1.ConditionTrue, Reason: infrav1.MirroredReason}
	// Many objects of a namespace share one mirror, so a concurrent update
	// conflicts now and then; EnsureMirror re-reads it on each try.
	var res identity.MirrorResult
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var err error
		_, res, err = identity.EnsureMirror(ctx, r.d.Client, r.d.APIReader, id, ns, r.obj)
		return err
	})
	conflict := errors.Is(err, identity.ErrMirrorConflict)
	switch {
	case conflict:
		// Not returned, so not retried with backoff: it does not clear on
		// its own. Logged at V0 when first reported, not on every pass.
		name := identity.MirrorName(r.identityName)
		mirror.Status, mirror.Reason = metav1.ConditionFalse, infrav1.MirrorFailedReason
		mirror.Message = fmt.Sprintf("Secret %s/%s has the name of TerraformClusterIdentity %s's credential mirror but is not one (no %s=true label, "+
			"or its %s annotation names another identity); it is never overwritten. Rename or remove it",
			ns, name, r.identityName, identity.MirroredLabel, inputs.IdentityAnnotation)
		logger := klog.FromContext(ctx)
		if prev, ok := r.before[infrav1.CredentialsMirroredCondition]; ok && prev.Message == mirror.Message {
			logger = logger.V(LogFlow)
		}
		logger.Info("A Secret with the credential mirror's name is not a mirror of this identity; it is never overwritten, and no Job starts",
			"Secret", klog.KRef(ns, name), "identity", r.identityName)
	case err != nil:
		mirror.Status, mirror.Reason, mirror.Message = metav1.ConditionFalse, infrav1.MirrorFailedReason, err.Error()
	}
	if res.Created {
		r.d.Emit(r.obj, corev1.EventTypeNormal, EventMirrorCreated, "Credentials",
			"Created credential mirror %s of TerraformClusterIdentity %s in namespace %s", identity.MirrorName(r.identityName), r.identityName, ns)
	}
	if res.OwnerRepaired {
		klog.FromContext(ctx).V(LogFlow).Info("Repaired the owner references of this object's Secrets", "mirror", 1)
		r.d.Emit(r.obj, corev1.EventTypeNormal, EventOwnerReferencesRepaired, "Credentials",
			"Replaced the owner reference to an earlier UID of this object on credential mirror %s", identity.MirrorName(r.identityName))
	}
	conditions.Set(r.obj, mirror)
	if err != nil && !conflict {
		return false, err
	}
	return err == nil, nil
}

// errStateUnreadable marks a state that exists but cannot be read, or the
// state that is gone of an object that ever applied (everApplied).
var errStateUnreadable = errors.New("state unreadable")

// noState handles a missing state, using ctx: lost for an object that ever
// applied (everApplied: provisioned, or the durable Secret's marks of an
// apply, which survive clusterctl move, or a state backup); not yet there
// otherwise. It returns errStateUnreadable for a lost state, or any error
// from looking for a previous apply.
func (r *reconciler) noState(ctx context.Context) error {
	lost, err := everApplied(ctx, r.d, r.k, r.suffix, r.durable)
	if err != nil {
		return err
	}
	switch {
	case lost && r.deleting:
		r.lostOnDelete()
		return errStateUnreadable
	case lost:
		// Applying again would create a second set of resources next to
		// the live ones (and a machine has no inputs to apply): stop.
		conditions.Set(r.obj, metav1.Condition{
			Type: infrav1.StateReadableCondition, Status: metav1.ConditionFalse, Reason: infrav1.StateLostReason,
			Message: "The state Secret is missing although the object applied before; no Job runs until it is restored. " +
				"Restore a backup listed in status.stateBackups with the " + infrav1.RestoreStateAnnotation + " annotation; see https://captf.io/docs/operator-guide/runbooks/state-restore.html",
		})
		return errStateUnreadable
	}
	conditions.Set(r.obj, metav1.Condition{
		Type: infrav1.StateReadableCondition, Status: metav1.ConditionUnknown, Reason: infrav1.StateNotFoundReason, Message: "No state Secret yet",
	})
	if !conditions.Has(r.obj, infrav1.OutputsValidCondition) {
		conditions.Set(r.obj, metav1.Condition{
			Type: infrav1.OutputsValidCondition, Status: metav1.ConditionUnknown, Reason: infrav1.OutputsPendingReason, Message: "No state yet",
		})
	}
	if !conditions.Has(r.obj, infrav1.InfrastructureHealthyCondition) {
		captfconds.SetInfrastructureHealthy(r.obj, nil, captfconds.HealthNotStarted)
	}
	return nil
}

// unreadable sets StateReadable False with reason and the message of
// err, the read error, adding, while deleting, that the deletion is held.
// It logs err using ctx's logger: at V0 when the condition did not report
// it as the pass began, at the flow level on the passes after. It returns
// errStateUnreadable.
func (r *reconciler) unreadable(ctx context.Context, reason string, err error) error {
	msg := err.Error()
	if r.deleting {
		msg += ". " + r.heldNote()
	}
	logger := klog.FromContext(ctx)
	if prev, ok := r.before[infrav1.StateReadableCondition]; ok && prev.Reason == reason && prev.Message == msg {
		logger = logger.V(LogFlow)
	}
	logger.Info("The state cannot be read; no Job runs until it is fixed", "reason", reason, "deleting", r.deleting, "err", err)
	conditions.Set(r.obj, metav1.Condition{Type: infrav1.StateReadableCondition, Status: metav1.ConditionFalse, Reason: reason, Message: msg})
	return errStateUnreadable
}

// readState reads state after the active-Job check, using ctx and the
// pass's bookkeeping bk: read state, adopt it after a successful apply, map
// outputs, set StateReadable, OutputsValid, InfrastructureHealthy and
// provisioned, and count a health sample when a refresh or drift Job
// completed this pass (status.lastRefresh moved past prevRefresh, its
// value before bookkeeping) or when the apply's own reading stands in for
// the post-apply refresh (applySample). It returns a StateView of what was
// read, and any error from reading or adopting the state or mapping
// outputs.
func (r *reconciler) readState(ctx context.Context, bk *Bookkeeping, prevRefresh *metav1.Time) (StateView, error) {
	set := func(t string, status metav1.ConditionStatus, reason, msg string) {
		conditions.Set(r.obj, metav1.Condition{Type: t, Status: status, Reason: reason, Message: msg})
	}
	st, err := r.d.State.Read(ctx, r.obj.GetNamespace(), r.suffix)
	var chunks []metav1.ObjectMeta
	if err == nil {
		chunks = st.Metadata
	}
	// Before anything reads, adopts, backs up or maps that state.
	if err := r.checkRetained(ctx, chunks, err == nil); err != nil {
		return StateView{}, err
	}
	switch {
	case errors.Is(err, state.ErrNoState):
		return StateView{}, r.noState(ctx)
	case errors.Is(err, state.ErrStateEncrypted):
		return StateView{Exists: true}, r.unreadable(ctx, infrav1.StateEncryptedReason, err)
	case errors.Is(err, state.ErrStateInconsistent):
		return StateView{Exists: true}, r.unreadable(ctx, infrav1.StateInconsistentReason, err)
	case errors.Is(err, state.ErrStateCorrupt), errors.Is(err, state.ErrUnsupportedStateVersion):
		return StateView{Exists: true}, r.unreadable(ctx, infrav1.StateCorruptReason, err)
	case err != nil:
		return StateView{}, fmt.Errorf("read state: %w", err)
	}
	set(infrav1.StateReadableCondition, metav1.ConditionTrue, infrav1.StateReadReason, "")
	r.d.Metrics.SetState(r.k.Kind(), r.obj.GetNamespace(), r.obj.GetName(), st.ManagedResources, st.Bytes)
	prevSerial := r.st.ObservedStateSerial
	newSerial := prevSerial != st.Serial
	r.st.ObservedStateSerial = st.Serial
	klog.FromContext(ctx).V(LogDebug).Info("Read state", "stateSerial", st.Serial, "newSerial", newSerial, "inputsHash", st.InputsHash)

	// Adopt after a successful apply or restore: ownerRefs on every chunk
	// and the applied (or the backup's) hash on the base Secret. Secrets
	// are read live, so the decision below sees the hash. Otherwise the
	// chunks' owner references are checked by repairOwners.
	r.chunks = st.Metadata
	if src := adoptSource(bk); src != nil {
		if h := src.Annotations[state.InputsHashAnnotation]; h != "" && h != st.InputsHash {
			if err := state.Adopt(ctx, r.d.Client, r.obj, r.suffix, h); err != nil {
				return StateView{}, err
			}
			r.d.EmitRelated(r.obj, src, corev1.EventTypeNormal, EventStateAdopted, "Reconcile",
				"Adopted the state Job %s wrote, with inputs hash %s", src.Name, h)
			st.InputsHash = h
			r.chunks = nil
		}
	}
	// A state with an inputs hash was written by a successful apply or
	// restored from a backup of one (objects that applied before the marker
	// existed get it here too).
	if st.InputsHash != "" || r.provisioned() {
		marked, err := markApplied(ctx, r.d, r.k, r.durable)
		if err != nil {
			return StateView{}, err
		}
		if marked {
			r.durable.Meta.Applied = true
		}
	}
	if newSerial && !r.backupState(ctx, bk) {
		// A transient failure: observe the serial again next pass, so its
		// backup is retried (TakeBackup is idempotent by content).
		r.st.ObservedStateSerial = prevSerial
	}

	res, health, err := r.k.ApplyOutputs(ctx, r.owner, st, r.durable)
	if err != nil {
		return StateView{}, err
	}
	set(infrav1.OutputsValidCondition, res.Status(), res.Reason(), res.Message())
	if res.Status() == metav1.ConditionFalse {
		klog.FromContext(ctx).V(LogFlow).Info("The module's outputs violate the contract", "reason", res.Reason(), "message", res.Message())
	}
	// Before the first successful apply there is nothing to observe: keep
	// WaitingForProvisioning or Provisioning.
	if st.InputsHash != "" || r.provisioned() {
		captfconds.SetInfrastructureHealthy(r.obj, health, captfconds.HealthObserved)
	}
	if Provisioned(r.provisioned(), st.InputsHash != "", res.Valid(), health) && !r.provisioned() {
		r.st.Initialization.Provisioned = new(true)
		klog.FromContext(ctx).V(LogFlow).Info("Provisioned", "stateSerial", st.Serial)
		r.d.Emit(r.obj, corev1.EventTypeNormal, EventProvisioned, "Reconcile", "Infrastructure provisioned")
	}
	r.sample(bk, prevRefresh, res.Valid())
	ih := conditions.Get(r.obj, infrav1.InfrastructureHealthyCondition)
	return StateView{
		Exists:     true,
		InputsHash: st.InputsHash,
		Pending:    ih != nil && ih.Status == metav1.ConditionFalse && ih.Reason == infrav1.InstancePendingReason,
	}, nil
}

// lastApplySucceeded returns when bk's newest finished apply succeeded, or
// nil when there was none or it did not succeed.
func lastApplySucceeded(bk *Bookkeeping) *time.Time {
	if bk.LastApply == nil || !bk.LastApplySucceeded {
		return nil
	}
	t := jobs.FinishedAt(bk.LastApply)
	return &t
}

// stamp formats t, a status time, for a Job-name tick; it returns "" when
// t is unset.
func stamp(t *metav1.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// timePtr returns a *time.Time built from t's Time field, or nil when t is
// nil.
func timePtr(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	return &t.Time
}
