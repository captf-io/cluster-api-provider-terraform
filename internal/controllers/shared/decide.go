/*
Copyright 2026 The cluster-api-provider-terraform Authors.

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
	"cmp"
	"hash/fnv"
	"strings"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
)

// Retry timing.
const (
	// RefreshPendingInterval is the first Refresh delay while health is
	// pending; PendingRefreshDelay doubles it per consecutive pending
	// reading. It is not a field.
	RefreshPendingInterval = 30 * time.Second
	// RefreshPendingCeiling caps the pending Refresh delay.
	RefreshPendingCeiling = 5 * time.Minute
	// RetryBase is the first retry delay after a failed Job.
	RetryBase = time.Minute
	// RetryMax caps the retry delay.
	RetryMax = 10 * time.Minute
	// ActiveJobRequeue is the fallback requeue while a Job runs; the Job
	// watch normally triggers the next reconcile first.
	ActiveJobRequeue = time.Minute
	// MembershipConvergingInterval is the Refresh delay while a
	// MembershipObserver's membership converges (or its health is pending):
	// fixed, not doubled, because members join on the provider's schedule.
	MembershipConvergingInterval = 30 * time.Second
)

// Action is what DecideOp asks for.
type Action int

const (
	// ActionNone starts nothing; requeue after RequeueAfter when non-zero.
	ActionNone Action = iota
	// ActionJob runs Op.
	ActionJob
	// ActionDropFinalizer removes the finalizer without a Job: deleting,
	// with no state and no Job, and the deletion not held (StateHeld).
	ActionDropFinalizer
)

// Decision is DecideOp's result.
type Decision struct {
	Action       Action
	Op           jobs.Op
	RequeueAfter time.Duration
	// Reason explains the decision for logs.
	Reason string
	// ExpectPlan is the approved plan hash an apply under applyPolicy
	// Manual must plan again before it applies (--expect-plan); "" for
	// every other decision.
	ExpectPlan string
	// PlanFlow is true for every decision of a gated apply (applyPolicy
	// Manual): its plan Job, the wait for the approval, the approved apply,
	// or their backoff. status.plan is kept only while it is.
	PlanFlow bool
}

// PlanView is what DecideOp needs from status.plan.
type PlanView struct {
	// InputsHash is the hash of the inputs the plan was made for.
	InputsHash string
	// PlanHash is the plan's fingerprint (runner.PlanHash).
	PlanHash string
}

// StateView is what DecideOp needs from state.
type StateView struct {
	// Exists is true when a state Secret exists.
	Exists bool
	// InputsHash is the hash recorded by the last successful apply; "" when
	// none.
	InputsHash string
	// CurrentHash is the hash of the current inputs of a mutable kind; ""
	// when they cannot be built (gated) or for immutable kinds.
	CurrentHash string
	// Pending is InfrastructureHealthy=False/InstancePending.
	Pending bool
}

// JobsView is what DecideOp needs from the object's Jobs.
type JobsView struct {
	// Active is true while a Job runs.
	Active bool
	// Failures counts, per op, the failed Jobs retained since the op last
	// succeeded (newest first, stopping at a success).
	Failures map[jobs.Op]int
	// LastFailure is, per op, when the newest failed Job finished.
	LastFailure map[jobs.Op]time.Time
	// FailedLimit is the failed-Jobs history limit; Failures cannot exceed
	// it, so reaching it means "at least that many".
	FailedLimit int
	// RemediationFailures counts the failed apply Jobs since the last
	// successful drift check (newest first, stopping at an apply success).
	RemediationFailures int
	// LastApplyFailed is true when the newest finished apply Job failed and
	// was not a drift remediation: reality may differ from the state's
	// inputs hash even when the current inputs equal it. A blocked apply
	// (or one whose plan changed) changed nothing and does not set it, nor
	// clear it when it started while it was set
	// (AfterFailedApplyAnnotation): it undid nothing of that failure, so
	// the apply then waits for its approval instead of being forgotten. A
	// pool's reconcile also sets it while a change of the cluster's exports
	// may be partly applied (Guard.Partial): a blocked apply since does not
	// undo that part. A mutable kind's reconcile sets it, too, while an
	// apply Job that disappeared while it ran is recorded
	// (inputs.Durable.InterruptedApply): it may have changed reality as a
	// failed apply may, and left no result to tell.
	LastApplyFailed bool
	// BlockedHash is the inputs hash of the newest finished apply Job when
	// the runner blocked it before a destructive plan; "" otherwise.
	BlockedHash string
	// BlockedApproval is that blocked Job's approval hash when it is a
	// pool apply guarded for a change of the cluster's exports
	// (ApprovalHashAnnotation); "" otherwise.
	BlockedApproval string
}

// DecideInput is everything DecideOp reads.
type DecideInput struct {
	Deleting       bool
	Mutable        bool
	State          StateView
	Jobs           JobsView
	LastRefresh    *time.Time
	LastDriftCheck *time.Time
	DriftInterval  time.Duration
	Now            time.Time
	// RefreshAfterApply and LastApplySucceeded ask for a Refresh once after
	// each successful apply (finished at LastApplySucceeded), unless
	// LastRefresh already reached it (readState advances it when the
	// apply's own outputs gave a definite health reading).
	RefreshAfterApply  bool
	LastApplySucceeded *time.Time
	// PendingRefreshes is status.pendingRefreshes: the consecutive samples
	// that read pending, which space the pending Refresh
	// (PendingRefreshDelay).
	PendingRefreshes int
	// Remediate asks for a remediation apply: a mutable kind with drift
	// action Remediate whose last drift check found changes.
	Remediate bool
	// HealthInterval, when non-zero, refreshes a provisioned object at that
	// cadence to sample its health, independent of drift
	// (remediation.healthCheckIntervalSeconds).
	HealthInterval time.Duration
	// MembershipInterval, when non-zero, refreshes the object at that
	// cadence to pick up group membership changes, provisioned or not
	// (a TerraformMachinePool's membershipRefreshIntervalSeconds).
	MembershipInterval time.Duration
	// Converging is true while a MembershipObserver's membership converges
	// or its health is pending: it refreshes every
	// MembershipConvergingInterval, in place of the doubling pending delay.
	Converging bool
	// UID seeds the deterministic jitter of the drift, health and
	// membership deadlines.
	UID string
	// Created is the object's creation time: the drift base when neither a
	// drift check nor a successful apply is known (after clusterctl move).
	Created time.Time
	// ApprovedHash is the object's captf.io/approve-destructive-plan
	// annotation: the hash approved for a destructive plan.
	ApprovedHash string
	// ApprovalHash is the hash an approval of the current inputs must
	// name when it is not their inputs hash: a pool apply guarded for a
	// change of the cluster's exports (Guard.ApprovalHash). "" means
	// State.CurrentHash, as for a cluster.
	ApprovalHash string
	// Unheld is true when a pool's guarded apply cannot fall back to the
	// exports of its last successful apply (Guard.Partial,
	// Guard.Unrecorded): a blocked apply then waits on its approval hash
	// (Jobs.BlockedApproval), not its inputs hash.
	Unheld bool
	// Restore is true when captf.io/restore-state names an existing backup
	// whose serial no restore Job was consumed for yet (restoreTarget).
	Restore bool
	// StateHeld is true while a deleting object's state is lost (it applied
	// before) or unreadable: no destroy can run against it, and dropping
	// the finalizer would orphan the infrastructure, so the deletion waits
	// for a restore (which Restore then starts) or for the infrastructure
	// to be abandoned.
	StateHeld bool
	// ManualApply is applyPolicy Manual: every apply but the first (no
	// state) runs only once the plan of its inputs is approved.
	ManualApply bool
	// Plan is status.plan; nil when no plan is recorded.
	Plan *PlanView
	// ApprovedPlan is the object's captf.io/approve-plan annotation: the
	// plan hash approved for an apply.
	ApprovedPlan string
}

// ReasonRestoreRequested is the decision reason of a restore.
const ReasonRestoreRequested = "RestoreRequested"

// ReasonDestructivePlanBlocked is the decision reason while an apply waits
// for an approval of its destructive plan.
const ReasonDestructivePlanBlocked = "DestructivePlanBlocked"

// ReasonPlanAwaitingApproval is the decision reason while an apply under
// applyPolicy Manual waits for the approval of its plan.
const ReasonPlanAwaitingApproval = "PlanAwaitingApproval"

// ReasonDeletionHeld is the decision reason while a deletion waits on a
// lost or unreadable state (StateHeld).
const ReasonDeletionHeld = "DeletionHeld"

// DecideOp picks the next operation, in this order: deleting with its
// state held (lost or unreadable) → Restore when one is requested, else
// wait; deleting → Destroy, or drop the finalizer with no state; a
// requested restore of an existing backup → Restore (whatever the state);
// no state → Apply; state without
// an inputs hash → Apply, for every kind; mutable and hash changed →
// Apply; mutable and the newest apply failed → Apply again, even when the
// current inputs equal the state's hash (the failed apply may have
// changed reality); drift found with action Remediate → Apply, until
// FailedLimit remediations failed since the last drift check; a
// successful apply not yet followed by a refresh, when the kind asks for
// it → Refresh; membership converging → Refresh every
// MembershipConvergingInterval; else health pending → Refresh after
// PendingRefreshDelay (30s doubling to at most 5m); membership refresh due
// → Refresh; health check due → Refresh; drift due → Drift; else requeue
// at the next deadline. A failed op is retried only after its backoff.
//
// An apply whose newest attempt was blocked before a destructive plan does
// not start again for the same inputs hash until ApprovedHash names it (or
// ApprovalHash, a pool's): it waits (at most RetryMax,
// DestructivePlanBlocked) for new inputs or the approval, which re-trigger
// the reconcile. A blocked remediation (the state's own inputs) keeps the
// refresh, health and drift schedule; a blocked input change pauses them,
// as a backoff does. A pool whose change of the cluster's exports was
// blocked does not wait: its inputs render the held exports (Guard.Held),
// whose applies, refreshes and drift checks run as usual.
//
// Under applyPolicy Manual (ManualApply) every apply but the first (no
// state: nothing to break yet) is gated instead (gatedApply): a plan Job
// plans the current inputs, the apply waits (PlanAwaitingApproval, at most
// RetryMax) until ApprovedPlan names the plan's hash, and then runs with
// ExpectPlan. A plan without changes needs no approval. The wait keeps or
// pauses the schedule as a blocked apply does, and the destructive guard's
// block does not apply: the plan's approval covers its deletes.
//
// It returns the Decision for the reconciler to act on.
func DecideOp(in DecideInput) Decision {
	dec, _ := decide(in)
	return dec
}

// decide is DecideOp for in. It returns the Decision, and the reason an
// apply waits for an approval meanwhile (ReasonPlanAwaitingApproval or
// ReasonDestructivePlanBlocked), "" when none does: a refresh or drift Job
// that runs while the wait keeps the schedule carries its own decision
// reason, and the reconciler must go on reporting the wait.
func decide(in DecideInput) (Decision, string) {
	if in.Jobs.Active {
		return Decision{RequeueAfter: ActiveJobRequeue, Reason: "JobActive"}, ""
	}
	if in.Restore && (!in.Deleting || in.StateHeld) {
		// Before everything but a deletion that can proceed: it is what the
		// operator asked for, and an apply or check against the state being
		// replaced would be wasted, or worse. A held deletion destroys once
		// the restored state reads. No backoff: a failed restore is not
		// retried for the same serial at all.
		return Decision{Action: ActionJob, Op: jobs.OpRestore, Reason: ReasonRestoreRequested}, ""
	}
	var apply string
	switch {
	case in.Deleting && in.StateHeld:
		return Decision{RequeueAfter: StateRequeue, Reason: ReasonDeletionHeld}, ""
	case in.Deleting && !in.State.Exists:
		return Decision{Action: ActionDropFinalizer, Reason: "DeletingWithoutState"}, ""
	case in.Deleting:
		return in.job(jobs.OpDestroy, "Deleting"), ""
	case !in.State.Exists:
		apply = "NoState"
	case in.State.InputsHash == "":
		apply = "StateWithoutInputsHash"
	case in.Mutable && in.State.CurrentHash != "" && in.State.CurrentHash != in.State.InputsHash:
		apply = "InputsChanged"
	case in.Mutable && in.Jobs.LastApplyFailed:
		apply = "LastApplyFailed"
	case in.Remediate && in.Jobs.RemediationFailures < max(in.Jobs.FailedLimit, 1):
		// After FailedLimit failed remediations the drift stays pending
		// until the next successful drift check resets the count.
		apply = "DriftRemediation"
	}
	// waiting is the reason an apply waits for an approval; "" when none
	// does.
	var waiting string
	switch {
	case apply != "" && apply != "NoState" && in.ManualApply:
		dec, wait := in.gatedApply(apply)
		if !wait {
			return dec, ""
		}
		waiting = ReasonPlanAwaitingApproval
	case apply != "" && in.applyBlocked():
		waiting = ReasonDestructivePlanBlocked
	case apply != "":
		return in.job(jobs.OpApply, apply), ""
	}
	if waiting != "" && (!in.State.Exists || in.State.CurrentHash != in.State.InputsHash) {
		// A waiting input change pauses checks like a backoff: refresh and
		// drift render the current, unapplied inputs, and would report the
		// waiting change as drift (and refresh its outputs into state).
		return Decision{RequeueAfter: RetryMax, Reason: waiting, PlanFlow: waiting == ReasonPlanAwaitingApproval}, waiting
	}
	// A remediation waiting for its plan's approval keeps the schedule, and
	// its plan: every decision meanwhile is part of the plan flow.
	dec := in.schedule(waiting)
	dec.PlanFlow = waiting == ReasonPlanAwaitingApproval
	return dec, waiting
}

// schedule is DecideOp once no apply starts: the refresh, membership,
// health and drift schedule, else a requeue at the next deadline (at most
// RetryMax while an apply waits for an approval, reason waiting). While
// Converging, the fixed converging refresh replaces the doubling pending
// one. It returns the resulting Decision.
func (in DecideInput) schedule(waiting string) Decision {
	if in.RefreshAfterApply && in.LastApplySucceeded != nil &&
		(in.LastRefresh == nil || in.LastRefresh.Before(*in.LastApplySucceeded)) {
		return in.job(jobs.OpRefresh, "RefreshAfterApply")
	}

	var next time.Duration
	sooner := func(due time.Duration) {
		if next == 0 || due < next {
			next = due
		}
	}
	switch {
	case in.Converging:
		iv := MembershipConvergingInterval
		due := dueIn(in.LastRefresh, iv+Jitter(in.UID, iv), in.Now)
		if due <= 0 {
			return in.job(jobs.OpRefresh, "MembershipConverging")
		}
		sooner(due)
	case in.State.Pending:
		delay := PendingRefreshDelay(in.PendingRefreshes)
		due := dueIn(in.LastRefresh, delay+Jitter(in.UID, delay), in.Now)
		if due <= 0 {
			return in.job(jobs.OpRefresh, "HealthPending")
		}
		sooner(due)
	}
	if iv := in.MembershipInterval; iv > 0 {
		due := dueIn(in.base(in.LastRefresh), iv+Jitter(in.UID, iv), in.Now)
		if due <= 0 {
			return in.job(jobs.OpRefresh, "MembershipRefreshDue")
		}
		sooner(due)
	}
	if in.HealthInterval > 0 {
		due := dueIn(in.base(in.LastRefresh), in.HealthInterval+Jitter(in.UID, in.HealthInterval), in.Now)
		if due <= 0 {
			return in.job(jobs.OpRefresh, "HealthCheckDue")
		}
		sooner(due)
	}
	if in.DriftInterval > 0 {
		due := dueIn(in.base(in.LastDriftCheck), in.DriftInterval+Jitter(in.UID, in.DriftInterval), in.Now)
		if due <= 0 {
			return in.job(jobs.OpDrift, "DriftDue")
		}
		sooner(due)
	}
	if waiting != "" {
		sooner(RetryMax)
		return Decision{RequeueAfter: next, Reason: waiting}
	}
	return Decision{RequeueAfter: next, Reason: "UpToDate"}
}

// gatedApply decides an apply (reason apply) under applyPolicy Manual: a
// plan Job while status.plan is not of the current inputs, or carries a
// plan hash of another fingerprint version than runner.PlanHashPrefix (a
// plan recorded before an upgrade: the runner cannot reproduce its hash,
// and an old empty-plan hash would wait for the approval of a plan that
// changes nothing), the approved apply once ApprovedPlan names its hash
// (or it changes nothing), else wait (true). Plan Jobs and the approved
// apply back off like any op. It returns the Decision and whether the
// caller must wait instead.
func (in DecideInput) gatedApply(apply string) (Decision, bool) {
	p := in.Plan
	var dec Decision
	switch {
	case p == nil || in.State.CurrentHash == "" || p.InputsHash != in.State.CurrentHash ||
		!strings.HasPrefix(p.PlanHash, runner.PlanHashPrefix):
		dec = in.job(jobs.OpPlan, apply)
	case p.PlanHash == runner.EmptyPlanHash || p.PlanHash == in.ApprovedPlan:
		dec = in.job(jobs.OpApply, apply)
		if dec.Action == ActionJob {
			dec.ExpectPlan = p.PlanHash
		}
	default:
		return Decision{}, true
	}
	dec.PlanFlow = true
	return dec, false
}

// applyBlocked reports whether the apply would render the inputs hash whose
// newest apply was blocked before a destructive plan, without an approval
// of it (ApprovalHash, else that inputs hash). Only mutable kinds compute
// the current hash. The cluster's applies are guarded, and a pool's that
// render a change of the cluster's exports; a pool whose change is blocked
// renders the held exports instead, so its current hash moves off the
// blocked one and it keeps applying. A pool that cannot hold them
// (Unheld) waits like a cluster, for as long as its approval hash is the
// blocked one's: a bootstrap rotation moves the inputs hash, not what the
// approval names, and re-running the blocked plan for it would only block
// again.
func (in DecideInput) applyBlocked() bool {
	if a := in.Jobs.BlockedApproval; in.Unheld && a != "" && in.ApprovalHash != "" {
		return a == in.ApprovalHash && in.ApprovedHash != a
	}
	b := in.Jobs.BlockedHash
	return b != "" && in.State.CurrentHash == b && in.ApprovedHash != cmp.Or(in.ApprovalHash, b)
}

// base is what a periodic check's interval counts from: its own last run,
// else the last successful apply (the first check comes one interval after
// provisioning, not right after it), else the object's creation. A zero
// creation time makes the check due at once. It returns that base time.
func (in DecideInput) base(last *time.Time) *time.Time {
	switch {
	case last != nil:
		return last
	case in.LastApplySucceeded != nil:
		return in.LastApplySucceeded
	}
	return &in.Created
}

// PendingRefreshDelay returns the Refresh delay while health is pending,
// after n consecutive samples read pending: RefreshPendingInterval for n
// ≤ 1, then doubling (1m, 2m, 4m) up to RefreshPendingCeiling. DecideOp
// adds the object's Jitter, as for drift.
func PendingRefreshDelay(n int) time.Duration {
	d := RefreshPendingInterval
	for i := 1; i < n && d < RefreshPendingCeiling; i++ {
		d *= 2
	}
	return min(d, RefreshPendingCeiling)
}

// Jitter returns a deterministic delay of up to a tenth of interval
// derived from uid, so objects created together (a MachineDeployment's
// machines, or everything after clusterctl move) do not check in
// lockstep. It is 0 for an empty uid.
func Jitter(uid string, interval time.Duration) time.Duration {
	spread := interval / 10
	if uid == "" || spread <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(uid))                      // hash.Hash never returns an error
	return time.Duration(h.Sum64() % uint64(spread)) // #nosec G115 -- remainder is < spread, which is a positive int64
}

// job returns ActionJob for op with the given reason, unless op is in its
// retry backoff, in which case it returns a backoff requeue instead.
func (in DecideInput) job(op jobs.Op, reason string) Decision {
	if wait := in.backoffRemaining(op); wait > 0 {
		return Decision{RequeueAfter: wait, Reason: reason + "Backoff"}
	}
	return Decision{Action: ActionJob, Op: op, Reason: reason}
}

// backoffRemaining returns how long op must still wait after its last
// failure: min(RetryBase·2^(n-1), RetryMax) for n consecutive failures,
// and RetryMax once n reaches the failed-Jobs history limit (pruning caps
// n there).
func (in DecideInput) backoffRemaining(op jobs.Op) time.Duration {
	n := in.Jobs.Failures[op]
	if n == 0 {
		return 0
	}
	return max(0, in.Jobs.LastFailure[op].Add(RetryDelay(n, in.Jobs.FailedLimit)).Sub(in.Now))
}

// RetryDelay returns the delay after n consecutive failures, capped at
// RetryMax once n reaches failedLimit.
func RetryDelay(n, failedLimit int) time.Duration {
	if failedLimit > 0 && n >= failedLimit {
		return RetryMax
	}
	d := RetryBase
	for i := 1; i < n && d < RetryMax; i++ {
		d *= 2
	}
	return min(d, RetryMax)
}

// dueIn returns how long until last+interval; ≤ 0 when due, and due now
// when never run.
func dueIn(last *time.Time, interval time.Duration, now time.Time) time.Duration {
	if last == nil {
		return 0
	}
	return last.Add(interval).Sub(now)
}
