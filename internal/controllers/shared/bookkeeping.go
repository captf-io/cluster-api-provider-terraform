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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/locks"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
	"github.com/captf-io/cluster-api-provider-terraform/internal/strutil"
)

// defaultFailedLimit is JobPolicy.failedJobsHistoryLimit's default.
const defaultFailedLimit = 3

// Job annotations the controller writes.
const (
	// BookkeptAnnotation marks a finished Job whose per-run Secret is gone
	// and whose completion was counted: later reconciles neither list its
	// pods nor delete its Secret again. Its result lives on in the
	// conditions and status.lastRun it set.
	BookkeptAnnotation = "captf.io/bookkept"
	// InterruptedAnnotation marks a bookkept Job the runner reported as
	// interrupted (SIGTERM: a drain, eviction, deletion or deadline), so it
	// keeps counting toward no retry backoff once its pod is not read again.
	InterruptedAnnotation = "captf.io/interrupted"
	// RemediationAnnotation marks an apply Job that remediates drift. A
	// failed one stays under the remediation cap instead of being retried as
	// an unconverged apply.
	RemediationAnnotation = "captf.io/drift-remediation"
	// BlockedAnnotation marks a bookkept apply Job the runner blocked
	// before a destructive plan (RunErrorKindBlocked), so DecideOp keeps
	// waiting for an approval once its pod is not read again.
	BlockedAnnotation = "captf.io/destructive-plan-blocked"
	// PlanChangedAnnotation marks a bookkept apply Job the runner stopped
	// because its plan was not the approved one (RunErrorKindPlanChanged),
	// so it keeps counting toward no backoff once its pod is not read
	// again.
	PlanChangedAnnotation = "captf.io/plan-changed"
	// PlanUnreadableAnnotation marks a bookkept plan Job that succeeded but
	// whose plan could not be read (no result: the pod was gone). It counts
	// as a failure, so the next plan Job backs off instead of looping.
	PlanUnreadableAnnotation = "captf.io/plan-unreadable"
	// ApprovedPlanAnnotation records, on an apply Job under applyPolicy
	// Manual, the plan hash it had to plan again (--expect-plan).
	ApprovedPlanAnnotation = "captf.io/approved-plan"
	// ApprovalHashAnnotation records, on a TerraformMachinePool apply Job
	// guarded for a change of the cluster's exports, the hash its approval
	// must name (hash.Approval: the inputs hash without bootstrap_data).
	ApprovalHashAnnotation = "captf.io/approval-hash"
	// ClusterOutputsHashAnnotation records, on a TerraformMachinePool apply
	// Job, hash.Exports of the cluster exports it renders: once it
	// succeeded they are the pool's applied exports, once it was blocked
	// the change that waits for approval.
	ClusterOutputsHashAnnotation = "captf.io/cluster-outputs-hash"
	// HeldClusterOutputsAnnotation marks a TerraformMachinePool apply Job
	// that rendered the exports of the last successful apply while a change
	// of them waited for approval: its success applies nothing of that
	// change.
	HeldClusterOutputsAnnotation = "captf.io/held-cluster-outputs"
	// AfterFailedApplyAnnotation marks an apply Job started while an apply
	// was due because the newest one failed, or a pool's change may be
	// partly applied, or an apply Job disappeared while it ran
	// (JobsView.LastApplyFailed as DecideOp read it). Blocked, or stopped for a changed plan, it changed
	// nothing and undid nothing of that failure, so bookkeeping keeps
	// LastApplyFailed set past it: the failed Job itself may be pruned by
	// then (jobs.Prune keeps only the newest failure at a history limit of
	// 1).
	AfterFailedApplyAnnotation = "captf.io/after-failed-apply"
	// AfterInterruptedApplyAnnotation names, on an apply Job started while
	// an apply Job that disappeared kept an apply due
	// (inputs.InterruptedApplyAnnotation), that Job: its success clears
	// the record, and its outcome is newer than the disappearance. A
	// success that does not carry it started before, and clears nothing.
	AfterInterruptedApplyAnnotation = "captf.io/after-interrupted-apply"
)

// Bookkeeping is the result of Bookkeep.
type Bookkeeping struct {
	// Jobs are the object's Jobs as listed, before pruning; the next Job's
	// attempt is derived from them.
	Jobs []batchv1.Job
	// Active is the running Job, if any.
	Active *batchv1.Job
	// View feeds DecideOp.
	View JobsView
	// LastApply is the newest finished apply Job, and whether it succeeded.
	LastApply          *batchv1.Job
	LastApplySucceeded bool
	// LastApplyBlocked is true when the newest finished apply Job was
	// blocked before a destructive plan.
	LastApplyBlocked bool
	// LastApplyPlanChanged is true when the newest finished apply Job
	// stopped because its plan was not the approved one.
	LastApplyPlanChanged bool
	// priorApply is, when LastApply was blocked, the newest finished apply
	// Job older than it that was neither blocked nor stopped for a changed
	// plan: the newest that may have changed anything, whose outcome
	// stands once the blocked change is withdrawn. nil when LastApply was
	// not blocked, or no such Job is retained.
	priorApply *finished
	// DestroySucceeded is true when the newest finished destroy Job
	// succeeded: cleanup may run.
	DestroySucceeded bool
	// ForceUnlockID is a stale lock the next Job must force-unlock.
	ForceUnlockID string
	// ForeignLock describes a state lock held by something other than this
	// object's runner (a workstation, another tool); "" when none.
	ForeignLock string
	// PinnedDigest is the digest bookkeeping pinned on the durable Secret
	// this pass; "" when it pinned nothing.
	PinnedDigest string
	// MarkedApplied is true when bookkeeping set the applied marker
	// (inputs.MarkApplied) on the durable Secret this pass.
	MarkedApplied bool
	// ExportsRecorded is true when bookkeeping recorded a pool apply's
	// cluster exports as applied this pass (inputs.RecordClusterOutputs),
	// which also dropped a pending change; AppliedExports is what it
	// recorded, nil when they did not fit, and AppliedExportsHash their
	// hash, recorded either way.
	ExportsRecorded    bool
	AppliedExports     []byte
	AppliedExportsHash string
	// PendingSet is the pending change bookkeeping recorded this pass for
	// a newly blocked pool apply (inputs.SetPending); nil when none.
	PendingSet *inputs.Pending
	// PartialSet is the partly applied change bookkeeping recorded this
	// pass for a newly failed guarded pool apply (inputs.SetPartial); nil
	// when none.
	PartialSet *inputs.Partial
	// InterruptedCleared is true when bookkeeping removed the record of an
	// apply Job that disappeared (inputs.ClearInterruptedApply) this pass,
	// for a newly finished successful apply started after it.
	InterruptedCleared bool
	// ApplyJob is the ApplyJobSucceeded condition the newest finished apply
	// or destroy Job implies. Reconcile sets it, unless a pending destroy is
	// blocked by the identity (IdentityNotAllowed): each condition is set
	// once per reconcile.
	ApplyJob metav1.Condition
	// CurrentHash is the inputs hash of a mutable kind's current inputs
	// (StateView.CurrentHash), once the reconcile built them this pass;
	// "" when it built none (a gate, a deletion, a paused object) or the
	// kind is immutable. Bookkeep does not set it: it runs before the
	// inputs are built. A cluster's apply blocked before a destructive
	// plan of other inputs is withdrawn by it (liftedCondition).
	CurrentHash string
	// lastRestore is the newest finished restore Job; nil when none.
	lastRestore *finished
	// lastPlan is the newest finished Job that made a plan for review (a
	// plan Job, or an apply whose approved plan changed) after the newest
	// successful apply; nil when none.
	lastPlan *finished
	// newestJob names the newest finished Job, the source a state backup
	// taken this pass records; "" when none.
	newestJob string
	// unmarked are the finished Jobs read this pass, for MarkBookkept.
	unmarked []finished
	// byName are the finished Jobs by name, for the Job outcome events.
	byName map[string]finished
}

// finishedJob returns the finished Job name as bookkeeping read it; bk may
// be nil.
func (bk *Bookkeeping) finishedJob(name string) (finished, bool) {
	if bk == nil {
		return finished{}, false
	}
	f, ok := bk.byName[name]
	return f, ok
}

// counted returns the names of the finished Jobs whose completion this pass
// counted (their per-run Secret was deleted now), which happens once per
// Job: the point where their run and cluster leases are released.
func (bk *Bookkeeping) counted() []string {
	var names []string
	for name, f := range bk.byName {
		if f.counted {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// finished is a finished Job with its newest pod and parsed result. A
// bookkept Job has neither: its pods are not listed again.
type finished struct {
	job      *batchv1.Job
	ok       bool
	pod      *corev1.Pod
	result   *jobs.Result
	bookkept bool
	// interrupted is true when the runner reported the step was stopped
	// from outside (SIGTERM: a drain, eviction, Job deletion or deadline):
	// not a module failure, so it counts toward no backoff. It is kept on
	// the Job (InterruptedAnnotation) because a bookkept Job's result is
	// not read again.
	interrupted bool
	// blocked is true when the runner stopped an apply before a plan that
	// deletes or replaces resources, for want of an approval
	// (RunErrorKindBlocked): nothing changed, so it counts toward no
	// backoff, and the apply waits for an approval or new inputs. It is
	// kept on the Job (BlockedAnnotation) like interrupted.
	blocked bool
	// planChanged is true when the runner stopped an approved apply because
	// its plan was not the approved one (RunErrorKindPlanChanged): nothing
	// changed, so it counts toward no backoff, and the new plan waits for
	// its approval. It is kept on the Job (PlanChangedAnnotation).
	planChanged bool
	// planUnreadable is true for a plan Job that succeeded without a
	// readable plan: it counts as failed (PlanUnreadableAnnotation).
	planUnreadable bool
	// counted is true when this pass deleted the per-run Secret, which
	// happens once per Job: its completion is counted now.
	counted bool
}

// Bookkeep processes k's finished Jobs, using ctx and the shared
// dependencies d: it lists the object's Jobs, pins the image digest
// of a newly finished successful apply, deletes the per-run Secret of
// every finished Job not yet bookkept and records it for
// MarkBookkept, sets ApplyJobSucceeded, DriftJobSucceeded and
// status.lastRun from the newest finished Jobs, prunes per eff's history
// limits, finds the active Job and, when none runs, checks the state lock
// named by suffix for a stale or foreign holder. Pods are read
// through the Runner, which lists them through the API reader. durable is
// the durable inputs Secret as read this reconcile, or nil. It returns the
// resulting Bookkeeping, or an error from listing, deleting or pruning
// Jobs or checking the lock.
func Bookkeep(ctx context.Context, d Deps, k Kind, eff EffectiveConfig, suffix string, durable *inputs.Durable) (*Bookkeeping, error) {
	obj := k.Object()
	list, err := d.Jobs.List(ctx, obj, k.Kind())
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	bk := &Bookkeeping{Jobs: list, View: JobsView{
		Failures:    map[jobs.Op]int{},
		LastFailure: map[jobs.Op]time.Time{},
		FailedLimit: defaultFailedLimit,
	}}
	if eff.Jobs.FailedJobsHistoryLimit != nil {
		// jobs.Prune keeps the newest unresolved failure of each op even
		// at 0, so a limit of 0 counts, and backs off, like 1.
		bk.View.FailedLimit = max(int(*eff.Jobs.FailedJobsHistoryLimit), 1)
	}

	done, err := collectFinished(ctx, d, list)
	if err != nil {
		return nil, err
	}
	bk.byName = make(map[string]finished, len(done))
	for _, f := range done {
		if !f.bookkept {
			bk.unmarked = append(bk.unmarked, f)
		}
		bk.byName[f.job.Name] = f
	}
	// Newest first.
	slices.SortFunc(done, func(a, b finished) int {
		return cmp.Or(jobs.FinishedAt(b.job).Compare(jobs.FinishedAt(a.job)), cmp.Compare(b.job.Name, a.job.Name))
	})
	for i := range done {
		if done[i].counted {
			recordFinished(d, k.Kind(), done[i], retryNumber(done, i))
		}
	}

	bk.countFailures(done)
	if err := bk.applyDestroy(ctx, d, k, done, durable); err != nil {
		return nil, err
	}
	setDriftJob(obj, done)
	bk.restores(d, k, done)
	bk.plans(d, k, done)
	if len(done) > 0 {
		bk.newestJob = done[0].job.Name
	}
	setDriftResults(obj, k.Status(), eff.DriftAction, done, bk.LastApply, bk.LastApplySucceeded, bk.LastApplyBlocked)
	bk.View.RemediationFailures = remediationFailures(done, k.Status().LastDriftCheck)
	if len(done) > 0 && (!done[0].bookkept || k.Status().LastRun.Job != done[0].job.Name) {
		setLastRun(k.Status(), done[0])
	}

	if err := jobs.Prune(ctx, d.Jobs, list, eff.Jobs.SuccessfulJobsHistoryLimit, eff.Jobs.FailedJobsHistoryLimit); err != nil {
		return nil, fmt.Errorf("prune jobs: %w", err)
	}
	if active, ok := jobs.Active(list); ok {
		bk.Active = active
		if jobs.OpOf(active) == jobs.OpDrift {
			setDriftRunning(obj, active.Name)
		}
		setDriftRemediating(obj, eff.DriftAction, active)
		return bk, nil
	}
	return bk, bk.checkLock(ctx, d, k, suffix)
}

// collectFinished reads, using ctx and the shared dependencies d, each
// finished Job of list's newest pod and result and deletes its per-run
// Secret; a bookkept Job costs no API call. It returns the finished Jobs
// found, or any error reading pods or deleting a Secret.
func collectFinished(ctx context.Context, d Deps, list []batchv1.Job) ([]finished, error) {
	var done []finished
	for i := range list {
		job := &list[i]
		outcome := jobs.OutcomeOf(job)
		if outcome == jobs.Running {
			continue
		}
		f := finished{
			job: job, ok: outcome == jobs.Succeeded,
			bookkept:    job.Annotations[BookkeptAnnotation] == "true",
			interrupted: job.Annotations[InterruptedAnnotation] == "true" && !jobs.DeadlineExceeded(job),
			blocked:     outcome != jobs.Succeeded && job.Annotations[BlockedAnnotation] == "true",
			planChanged: outcome != jobs.Succeeded && job.Annotations[PlanChangedAnnotation] == "true",
		}
		if job.Annotations[PlanUnreadableAnnotation] == "true" {
			f.ok, f.planUnreadable = false, true
		}
		if f.bookkept {
			done = append(done, f)
			continue
		}
		pods, err := d.Jobs.Pods(ctx, job)
		if err != nil {
			return nil, fmt.Errorf("pods of job %s: %w", job.Name, err)
		}
		if len(pods) > 0 {
			f.pod = newestPod(pods)
			if r, err := jobs.ParseResult(f.pod); err == nil {
				f.result = r
				// A Job the deadline killed reports interrupted too, but
				// it ran out of time: that is a failure and counts.
				f.interrupted = !f.ok && !jobs.DeadlineExceeded(job) && r.Error != nil && r.Error.Kind == string(infrav1.RunErrorKindInterrupted)
				f.blocked = !f.ok && r.Error != nil && r.Error.Kind == string(infrav1.RunErrorKindBlocked) && jobs.OpOf(job) == jobs.OpApply
				f.planChanged = !f.ok && r.Error != nil && r.Error.Kind == string(infrav1.RunErrorKindPlanChanged) && jobs.OpOf(job) == jobs.OpApply
			}
		}
		if f.ok && jobs.OpOf(job) == jobs.OpPlan && (f.result == nil || f.result.Plan == nil) {
			// Without its plan there is nothing to approve; as a success it
			// would only start the next plan Job at once.
			f.ok, f.planUnreadable = false, true
		}
		if f.counted, err = inputs.DeleteRun(ctx, d.Client, job); err != nil {
			return nil, err
		}
		done = append(done, f)
	}
	return done, nil
}

// MarkBookkept patches BookkeptAnnotation onto every finished Job this pass
// read, using ctx and the client c. The reconcile calls it only after the
// status patch succeeded: what a Job's pod said lives on only in the
// conditions and status.lastRun that patch wrote, so a failed patch leaves
// the Jobs to be read again. A Job that is already gone needs no mark. It
// returns a joined error of any patch failures.
func (bk *Bookkeeping) MarkBookkept(ctx context.Context, c client.Client) error {
	var errs []error
	for _, f := range bk.unmarked {
		job := f.job
		before := job.DeepCopy()
		metav1.SetMetaDataAnnotation(&job.ObjectMeta, BookkeptAnnotation, "true")
		if f.interrupted && !jobs.DeadlineExceeded(job) {
			metav1.SetMetaDataAnnotation(&job.ObjectMeta, InterruptedAnnotation, "true")
		}
		if f.blocked {
			metav1.SetMetaDataAnnotation(&job.ObjectMeta, BlockedAnnotation, "true")
		}
		if f.planChanged {
			metav1.SetMetaDataAnnotation(&job.ObjectMeta, PlanChangedAnnotation, "true")
		}
		if f.planUnreadable {
			metav1.SetMetaDataAnnotation(&job.ObjectMeta, PlanUnreadableAnnotation, "true")
		}
		err := c.Patch(ctx, job, client.MergeFrom(before))
		switch {
		case apierrors.IsNotFound(err):
			klog.FromContext(ctx).V(LogFlow).Info("The finished Job is already gone; nothing to mark bookkept", "Job", klog.KObj(job))
		case err != nil:
			errs = append(errs, fmt.Errorf("mark job %s bookkept: %w", job.Name, err))
		}
	}
	return kerrors.NewAggregate(errs)
}

// retryNumber returns the retry number of done[i] (newest first): 1 plus
// the failed Jobs of its op that finished before it since that op's
// previous success. It is what captf_job_attempts records.
func retryNumber(done []finished, i int) int {
	op := jobs.OpOf(done[i].job)
	n := 1
	for _, f := range done[i+1:] {
		if jobs.OpOf(f.job) != op {
			continue
		}
		if f.ok {
			break
		}
		if !f.blocked && !f.planChanged {
			n++
		}
	}
	return n
}

// checkLock looks, using ctx and the shared dependencies d, for a stale
// lock of k's object's own runner (force unlocked by the next Job) and for
// a lock someone else holds (reported as StateLocked), on the state named
// by suffix, and logs either. It returns any error reading the lock.
func (bk *Bookkeeping) checkLock(ctx context.Context, d Deps, k Kind, suffix string) error {
	obj := k.Object()
	ours := func(pod string) bool { return jobs.OwnsPod(kindShort(k), obj.GetName(), pod) }
	lock, err := locks.Check(ctx, d.APIReader, obj.GetNamespace(), suffix, ours)
	if err != nil {
		return fmt.Errorf("check state lock: %w", err)
	}
	if lock.Stale() {
		bk.ForceUnlockID = lock.LockID
		klog.FromContext(ctx).V(LogFlow).Info("The state lock is held by a runner pod of this object that is gone; the next Job force-unlocks it",
			"lockID", lock.LockID, "holderPod", lock.Holder)
	}
	if lock.Held && lock.Holder == "" {
		bk.ForeignLock = foreignLock(ctx, d.APIReader, obj.GetNamespace(), suffix, lock.LockID)
	}
	return nil
}

// foreignLock describes, using ctx and the reader c, a lock identified by
// lockID whose holder is not one of the object's runner pods, from the
// Lease named by namespace and suffix's lock info. locks.Check does not
// return the holder's Who, so the Lease is read again; this happens only
// while such a lock is held. Every Job fails on it, so it is logged at V0
// with its holder, and an error reading the Lease is logged too: the
// description then lacks the holder. It returns the description to
// report.
func foreignLock(ctx context.Context, c client.Reader, namespace, suffix, lockID string) string {
	logger := klog.FromContext(ctx)
	const held = "The state lock is held by something other than this object's runner; every Job waits for it and fails"
	msg := "The state lock " + lockID + " is held by something other than this object's runner"
	lease := &coordinationv1.Lease{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: state.LeaseName(suffix)}, lease); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(LogFlow).Info("The state lock's Lease is gone; the lock was released meanwhile", "lockID", lockID)
		} else {
			logger.Error(err, "Could not read the state lock's Lease to name the holder of a foreign lock", "lockID", lockID)
		}
		logger.Info(held, "lockID", lockID)
		return msg
	}
	var info locks.Info
	if raw := lease.Annotations[locks.LockInfoAnnotation]; raw == "" || json.Unmarshal([]byte(raw), &info) != nil || info.Who == "" {
		logger.Info(held, "lockID", lockID)
		return msg
	}
	logger.Info(held, "lockID", lockID, "holder", info.Who, "operation", info.Operation)
	msg = "The state lock " + lockID + " is held by " + info.Who
	if info.Operation != "" {
		msg += " (" + info.Operation + ")"
	}
	if !info.Created.IsZero() {
		msg += " since " + info.Created.UTC().Format(time.RFC3339)
	}
	return msg + "; every Job waits lockTimeoutSeconds for it and fails. Release it with force-unlock once that holder is gone; see the stale-lock runbook"
}

// countFailures fills View from done: per op, the failed Jobs since its
// last success.
func (bk *Bookkeeping) countFailures(done []finished) {
	stopped := map[jobs.Op]bool{}
	for _, f := range done {
		op := jobs.OpOf(f.job)
		if stopped[op] {
			continue
		}
		if f.ok {
			stopped[op] = true
			continue
		}
		if (f.interrupted && !jobs.DeadlineExceeded(f.job)) || f.blocked || f.planChanged {
			// Stopped from outside, or before changing anything: no
			// backoff. A blocked apply, or one whose plan changed, waits
			// for an approval in DecideOp instead. A deadline kill is not
			// stopped from outside: a step that always hangs must reach
			// the retry limit, so it counts even when marked interrupted.
			continue
		}
		if bk.View.Failures[op] == 0 {
			bk.View.LastFailure[op] = jobs.FinishedAt(f.job)
		}
		bk.View.Failures[op]++
	}
}

// applyDestroy sets ApplyJobSucceeded from the newest of done, k's
// finished apply or destroy Jobs, or from the newest plan Job when it
// failed after them (the apply it stands for cannot run), and pins,
// using ctx and the shared dependencies d, the digest of a newly finished
// successful apply, comparing it against durable, the durable inputs
// Secret as read this reconcile (nil when none). It returns any error
// from pinning the digest.
func (bk *Bookkeeping) applyDestroy(ctx context.Context, d Deps, k Kind, done []finished, durable *inputs.Durable) error {
	obj := k.Object()
	var newest *finished
	pinned, planSeen := false, false
	for i := range done {
		f := &done[i]
		op := jobs.OpOf(f.job)
		if op == jobs.OpPlan && !planSeen {
			planSeen = true
			if !f.ok && newest == nil {
				newest = f
			}
			continue
		}
		if op != jobs.OpApply && op != jobs.OpDestroy {
			continue
		}
		if newest == nil {
			newest = f
			bk.DestroySucceeded = op == jobs.OpDestroy && f.ok
		}
		if op == jobs.OpApply && bk.LastApply == nil {
			bk.LastApply, bk.LastApplySucceeded, bk.LastApplyBlocked = f.job, f.ok, f.blocked
			bk.LastApplyPlanChanged = f.planChanged
			// A blocked apply, or one whose plan changed, changed nothing:
			// reality still matches what it matched before it, the state's
			// inputs hash unless an apply had failed then.
			bk.View.LastApplyFailed = !f.ok && !f.blocked && !f.planChanged && f.job.Annotations[RemediationAnnotation] != "true"
			if (f.blocked || f.planChanged) && f.job.Annotations[AfterFailedApplyAnnotation] == "true" {
				bk.View.LastApplyFailed = true
			}
			if f.blocked {
				bk.View.BlockedHash = f.job.Annotations[state.InputsHashAnnotation]
				bk.View.BlockedApproval = f.job.Annotations[ApprovalHashAnnotation]
				if !f.bookkept {
					if err := bk.recordPending(ctx, d, k, f, durable); err != nil {
						return err
					}
				}
			}
			if !f.ok && !f.blocked && !f.planChanged && !f.bookkept {
				if err := bk.recordPartial(ctx, d, k, f, durable); err != nil {
					return err
				}
			}
		}
		if op == jobs.OpApply && bk.LastApplyBlocked && f.job != bk.LastApply && bk.priorApply == nil && !f.blocked && !f.planChanged {
			bk.priorApply = f
		}
		if op == jobs.OpApply && f.ok && !pinned {
			pinned = true
			// A bookkept apply was pinned when it finished; pinning it again
			// could put an old image's digest back after an image change.
			if !f.bookkept {
				digest, err := pinDigest(ctx, d, k, f, durable)
				if err != nil {
					return err
				}
				bk.PinnedDigest = digest
				if bk.MarkedApplied, err = markApplied(ctx, d, k, durable); err != nil {
					return err
				}
				if err := bk.recordExports(ctx, d, k, f, durable); err != nil {
					return err
				}
				if err := bk.clearInterrupted(ctx, d, k, f, durable); err != nil {
					return err
				}
			}
		}
	}
	prev := conditions.Get(obj, infrav1.ApplyJobSucceededCondition)
	switch {
	case newest != nil && newest.bookkept && prev != nil && namesJob(prev.Message, newest.job.Name):
		// Its pod was read when it finished; the condition says so already.
		bk.ApplyJob = *prev
	case newest != nil:
		bk.ApplyJob = applyDestroyCondition(*newest, k.Kind(), obj)
	case prev != nil && leaseWaitEvents[prev.Reason] == "":
		// No retained Job: keep what was reported (it is never Unknown once
		// an apply completed). A lease wait is over once bookkeeping runs
		// again: the reconcile sets it anew if it still waits.
		bk.ApplyJob = *prev
	default:
		bk.ApplyJob = metav1.Condition{
			Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionUnknown,
			Reason: infrav1.NoApplyYetReason, Message: "No apply has completed yet",
		}
	}
	return nil
}

// namesJob reports whether the condition message msg is about the Job
// name: it is "Job <name>", optionally followed by ": …".
func namesJob(msg, name string) bool {
	rest, ok := strings.CutPrefix(msg, "Job "+name)
	return ok && (rest == "" || strings.HasPrefix(rest, ":"))
}

// applyDestroyCondition maps f, a finished apply or destroy Job of obj, a
// kind. A pull failure also ends on the deadline, so it is checked first.
// It returns the ApplyJobSucceeded condition to set.
func applyDestroyCondition(f finished, kind string, obj client.Object) metav1.Condition {
	destroy := jobs.OpOf(f.job) == jobs.OpDestroy
	c := metav1.Condition{Type: infrav1.ApplyJobSucceededCondition, Message: "Job " + f.job.Name}
	switch {
	case f.blocked:
		c.Status, c.Reason = metav1.ConditionFalse, infrav1.DestructivePlanBlockedReason
		c.Message += ": " + blockedMessage(f, kind, obj)
		return c
	case f.planChanged:
		if p, ok := previewOf(&f); ok {
			return planCondition(p, true, kind, obj)
		}
		c.Status, c.Reason = metav1.ConditionUnknown, infrav1.PlanChangedReason
		c.Message += ": the plan changed since it was approved, so nothing was applied; approve the new plan in status.plan with the " +
			infrav1.ApprovePlanAnnotation + " annotation"
		return c
	case f.ok && destroy:
		c.Status, c.Reason = metav1.ConditionTrue, infrav1.DestroySucceededReason
	case f.ok:
		c.Status, c.Reason = metav1.ConditionTrue, infrav1.ApplySucceededReason
	case f.pod != nil && jobs.PullFailed(f.pod):
		c.Status, c.Reason = metav1.ConditionFalse, infrav1.ImagePullFailedReason
	case jobs.DeadlineExceeded(f.job):
		c.Status, c.Reason = metav1.ConditionFalse, infrav1.JobDeadlineExceededReason
	case f.result != nil && f.result.Error != nil && f.result.Error.Kind == string(infrav1.RunErrorKindImageLayout):
		c.Status, c.Reason = metav1.ConditionFalse, infrav1.ImageInvalidReason
	case destroy:
		c.Status, c.Reason = metav1.ConditionFalse, infrav1.DestroyFailedReason
	default:
		c.Status, c.Reason = metav1.ConditionFalse, infrav1.ApplyFailedReason
	}
	if !f.ok && f.result != nil && f.result.Error != nil && f.result.Error.Step != nil {
		c.Message += ": step " + *f.result.Error.Step + " failed"
	}
	return c
}

// blockedMessage explains f, a blocked apply of a kind on obj: what the
// plan would delete or replace (the runner's summary: addresses and
// actions only, never values), and the command that approves exactly this
// Job's hash (approvalHashOf). A pool's blocked apply is reported by
// heldCondition instead, once its change is pending. A bookkept Job's
// summary is gone; its condition was set when it finished. It returns the
// message to report.
func blockedMessage(f finished, kind string, obj client.Object) string {
	summary := "the plan deletes or replaces resources (the Job's log lists them)"
	if f.result != nil && f.result.Error != nil && f.result.Error.Tail != "" {
		summary = f.result.Error.Tail
	}
	h := approvalHashOf(f.job)
	return fmt.Sprintf("%s. Nothing was applied, and no apply of these inputs runs until they are approved. "+
		"To apply it, approve inputs hash %s: kubectl annotate %s %s -n %s %s=%s --overwrite",
		summary, h, strings.ToLower(kind), obj.GetName(), obj.GetNamespace(), infrav1.ApproveDestructivePlanAnnotation, h)
}

// setDriftJob sets obj's DriftJobSucceeded from the newest finished drift
// or refresh Job in done, and DriftNotChecked before any.
func setDriftJob(obj Object, done []finished) {
	for _, f := range done {
		if op := jobs.OpOf(f.job); op != jobs.OpDrift && op != jobs.OpRefresh {
			continue
		}
		c := metav1.Condition{Type: infrav1.DriftJobSucceededCondition, Message: "Job " + f.job.Name}
		switch {
		case f.ok:
			c.Status, c.Reason = metav1.ConditionTrue, infrav1.DriftCheckedReason
		case jobs.DeadlineExceeded(f.job):
			c.Status, c.Reason = metav1.ConditionFalse, infrav1.DriftJobDeadlineExceededReason
		default:
			c.Status, c.Reason = metav1.ConditionFalse, infrav1.DriftJobFailedReason
		}
		conditions.Set(obj, c)
		return
	}
	if c := conditions.Get(obj, infrav1.DriftJobSucceededCondition); c == nil || leaseWaitEvents[c.Reason] != "" {
		conditions.Set(obj, metav1.Condition{
			Type: infrav1.DriftJobSucceededCondition, Status: metav1.ConditionUnknown,
			Reason: infrav1.DriftNotCheckedReason, Message: "No drift check has run yet",
		})
	}
}

// pinDigest records, using ctx and the shared dependencies d, the digest
// f, a successful apply Job of k, ran, and returns it, or "" when nothing
// was pinned. Immutable kinds keep the first pin; mutable kinds re-pin
// after every apply. A Job that ran another image than durable, the
// durable Secret, records (the image changed since, and inputs.Write
// cleared the pin) pins nothing: its digest belongs to the old image. It
// returns the pinned digest, or any error pinning it.
func pinDigest(ctx context.Context, d Deps, k Kind, f *finished, durable *inputs.Durable) (string, error) {
	logger := klog.FromContext(ctx)
	if durable != nil && durable.Meta.Image != "" {
		if img := sourceImage(f.job); img != "" && img != durable.Meta.Image {
			logger.V(LogFlow).Info("Not pinning the digest of an apply that ran another image", "Job", klog.KObj(f.job), "image", img)
			return "", nil
		}
	}
	if f.pod != nil {
		if digest, ok := jobs.ImageDigest(f.pod); ok {
			pinned, err := inputs.PinDigest(ctx, d.Client, k.Object(), digest, k.Mutable())
			if err != nil && !errors.Is(err, inputs.ErrNotFound) {
				return "", err
			}
			if !pinned {
				return "", nil
			}
			logger.V(LogFlow).Info("Pinned the image digest", "Job", klog.KObj(f.job), "digest", digest)
			d.EmitRelated(k.Object(), f.job, corev1.EventTypeNormal, EventDigestPinned, "Pin", "Job %s ran %s; later operations run this digest", f.job.Name, digest)
			return digest, nil
		}
	}
	// The pod is gone or reports no digest: keep the previous pin, and say
	// so while nothing is pinned yet.
	if durable != nil && durable.Meta.ImageDigest == "" {
		d.EmitRelated(k.Object(), f.job, corev1.EventTypeWarning, EventDigestUnknown, "Pin",
			"Job %s succeeded but its image digest is unavailable; other operations run the spec image", f.job.Name)
	}
	logger.V(LogFlow).Info("Image digest unavailable", "Job", f.job.Name)
	return "", nil
}

// markApplied records on k's durable Secret, using ctx and the shared
// dependencies d, that k's object applied or had a state restored, so a
// missing state reads as lost from then on (everApplied), also after
// clusterctl move. durable is the Secret as read this reconcile; one that
// already carries the marker, or none (nothing to mark), costs no call. It
// reports whether it set the marker, or returns any error setting it.
func markApplied(ctx context.Context, d Deps, k Kind, durable *inputs.Durable) (bool, error) {
	if durable == nil || durable.Meta.Applied {
		return false, nil
	}
	err := inputs.MarkApplied(ctx, d.Client, k.Object())
	switch {
	case errors.Is(err, inputs.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// sourceImage returns the image of job's source container, "" when
// absent.
func sourceImage(job *batchv1.Job) string {
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.Name == jobs.SourceContainer {
			return c.Image
		}
	}
	return ""
}

// MaxRunSummary is the byte limit of status.lastRun.error.summary.
const MaxRunSummary = 512

// runSummary returns the runner's failure summary s cut to MaxRunSummary
// bytes on a rune boundary, so the status update never fails CRD
// validation.
func runSummary(s string) string {
	return strutil.Truncate(s, MaxRunSummary)
}

// setLastRun copies f, the newest finished Job's result, into st's
// LastRun and Source fields.
func setLastRun(st CommonStatus, f finished) {
	run := infrav1.LastRun{Job: f.job.Name, Operation: infrav1.Operation(jobs.OpOf(f.job))}
	if r := f.result; r != nil {
		for _, s := range r.Steps {
			exit := int32(s.Exit) // #nosec G115 -- a process exit status, 0-255 (or -1)
			ms := int64(s.Seconds * 1000)
			run.Steps = append(run.Steps, infrav1.RunStep{Name: s.Name, ExitCode: &exit, DurationMilliseconds: &ms})
		}
		if r.Error != nil {
			run.Error = infrav1.RunError{Kind: infrav1.RunErrorKind(r.Error.Kind), Summary: runSummary(r.Error.Tail)}
			if r.Error.Step != nil {
				run.Error.Step = *r.Error.Step
			}
		}
		if r.Image.Ref != "" {
			st.Source.Image = r.Image.Ref
		}
		run.Drift = driftSummary(r.Drift)
		if r.Runtime.Version != "" {
			st.Source.RuntimeVersion = r.Runtime.Version
		}
	}
	st.LastRun = run
}

// newestPod returns the pod of pods with the latest creation timestamp.
func newestPod(pods []corev1.Pod) *corev1.Pod {
	newest := &pods[0]
	for i := range pods[1:] {
		p := &pods[i+1]
		if p.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = p
		}
	}
	return newest
}

// kindShort returns the short kind of k (c, m or mp).
func kindShort(k Kind) string {
	s, _ := state.KindShort(k.Kind())
	return s
}
