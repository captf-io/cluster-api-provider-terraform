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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
	"github.com/captf-io/cluster-api-provider-terraform/internal/strutil"
)

// Event reasons. Events go through the manager's events.k8s.io/v1 recorder
// (mgr.GetEventRecorder), once per transition or occurrence, never on
// every reconcile.
// Messages never carry credentials, tfvars, output values or raw stderr,
// and are capped at MaxEventNote bytes. The runner's own progress events
// are internal/runner.DocumentedEvents.
const (
	// EventJobCreated: a Job was started (op, attempt, image, why).
	EventJobCreated = "JobCreated"
	// EventJobSucceeded: an apply, destroy, refresh or drift Job succeeded.
	EventJobSucceeded = "JobSucceeded"
	// EventJobFailed: a Job failed, or an apply or destroy could not start
	// (ApplyJobSucceeded False without a Job). A failed Job's note says
	// when its op may run again (the retry backoff).
	EventJobFailed = "JobFailed"
	// EventJobInterrupted: a Job was stopped from outside (a drain,
	// eviction or deletion); it is retried without backoff.
	EventJobInterrupted = "JobInterrupted"
	// EventJobDeadlineExceeded: a Job hit activeDeadlineSeconds; the note
	// says when its op may run again, as JobFailed's does.
	EventJobDeadlineExceeded = "JobDeadlineExceeded"
	// EventStuckJobDeleted: a Job that could never start (its per-run
	// Secret is missing) was deleted to be started again.
	EventStuckJobDeleted = "StuckJobDeleted"
	// EventImagePullFallback: a destroy, refresh, drift or restore Job
	// could not pull its image (the pinned digest, say, garbage collected
	// by the registry) past PullFailureGrace, so it was deleted and the
	// operation starts again on the next image it may run.
	EventImagePullFallback = "ImagePullFallback"
	// EventWaitingForRunLease: an operation waits because another live Job
	// holds the object's run lease.
	EventWaitingForRunLease = "WaitingForRunLease"
	// EventWaitingForJobSlot: an operation waits because the manager's or
	// the cluster's active Jobs reached their limit.
	EventWaitingForJobSlot = "WaitingForJobSlot"
	// EventWaitingForClusterOperation: a machine's apply or destroy waits
	// for its TerraformCluster's apply or destroy.
	EventWaitingForClusterOperation = "WaitingForClusterOperation"
	// EventWaitingForMachineOperations: a TerraformCluster's apply or
	// destroy waits for its machines' applies and destroys in flight.
	EventWaitingForMachineOperations = "WaitingForMachineOperations"
	// EventDestructivePlanBlocked: a TerraformCluster apply, or a
	// TerraformMachinePool apply of a change of the cluster's exports,
	// stopped before a plan that deletes or replaces resources, and the
	// plan waits for approval as a TerraformPlan (named, with the approve
	// command); once per blocked Job, in place of JobFailed. The pool then
	// keeps applying with the exports of its last successful apply until
	// the change is approved, unless an earlier apply may have left a
	// change of them partly applied: it then waits for the approval, as a
	// cluster does.
	EventDestructivePlanBlocked = "DestructivePlanBlocked"
	// EventPlanReady: a plan Job planned a TerraformCluster's change under
	// applyPolicy Manual, and its TerraformPlan waits for approval (counts,
	// the plan, the approve command), or the plan changes nothing and needs
	// none; once per plan.
	EventPlanReady = "PlanReady"
	// EventPlanApproved: a TerraformPlan of the object was approved (names
	// the approver).
	EventPlanApproved = "PlanApproved"
	// EventPlanApplied: the apply of an approved TerraformPlan succeeded;
	// the plan is Applied.
	EventPlanApplied = "PlanApplied"
	// EventPlanChanged: an approved apply planned other changes and stopped
	// before applying them; its TerraformPlan is Failed. Once per such Job.
	EventPlanChanged = "PlanChanged"
	// EventPlanSuperseded: a TerraformPlan of the object was superseded,
	// by a newer plan or because it no longer applies; a Warning when it
	// was approved, since its approval applied nothing.
	EventPlanSuperseded = "PlanSuperseded"

	// EventDeletionStarted: the first reconcile with a deletionTimestamp.
	EventDeletionStarted = "DeletionStarted"
	// EventDestroyed: the destroy succeeded and cleanup ran.
	EventDestroyed = "Destroyed"
	// EventFinalizerRemoved: the finalizer was removed; the object goes.
	EventFinalizerRemoved = "FinalizerRemoved"
	// EventInfrastructureRetained: a deletion with deletionPolicy Retain
	// removed the finalizer without a destroy and kept the state, its
	// backups and the durable inputs, labeled captf.io/retained-from-uid,
	// for a later object of the same name to adopt.
	EventInfrastructureRetained = "InfrastructureRetained"
	// EventRetainedStateFound: the object found state another object of
	// its kind, namespace and name retained (StateReadable
	// False/RetainedStateFound); nothing runs until it is adopted.
	EventRetainedStateFound = "RetainedStateFound"
	// EventRetainedStateAdopted: with spec.adoptRetainedState, the object
	// removed captf.io/retained-from-uid from the retained Secrets it found
	// and now manages that infrastructure.
	EventRetainedStateAdopted = "RetainedStateAdopted"
	// EventPaused: the Paused condition changed to True; reconciliation
	// stops starting Jobs.
	EventPaused = "Paused"
	// EventResumed: the Paused condition changed to False after being True;
	// reconciliation resumes.
	EventResumed = "Resumed"
	// EventProvisioned: provisioned latched true.
	EventProvisioned = "Provisioned"
	// EventProviderIDSet: a TerraformMachine's or TerraformMachinePool's
	// spec.providerID was written (a pool's may change).
	EventProviderIDSet = "ProviderIDSet"
	// EventControlPlaneEndpointSet: a TerraformCluster's
	// spec.controlPlaneEndpoint was written from the module output.
	EventControlPlaneEndpointSet = "ControlPlaneEndpointSet"
	// EventFailureDomainsChanged: a TerraformCluster's
	// status.failureDomains changed.
	EventFailureDomainsChanged = "FailureDomainsChanged"
	// EventExportsNotPublished: a TerraformCluster's exports output is
	// larger than the publish limit, so status.exports is empty; machines
	// and pools still read the output from the state.
	EventExportsNotPublished = "ExportsNotPublished"

	// EventInputsChanged: the inputs hash differs from the state's and an
	// apply of the new inputs starts.
	EventInputsChanged = "InputsChanged"
	// EventDigestPinned: a successful apply's image digest was recorded
	// with its inputs on the applied inputs Secret, a new one or one that
	// differs from the previous apply's.
	EventDigestPinned = "DigestPinned"
	// EventDigestUnknown: no digest could be pinned, or an operation runs
	// the spec reference for lack of one.
	EventDigestUnknown = "DigestUnknown"
	// EventAppliedInputsUnknown: an apply succeeded, but neither its
	// per-run Secret nor the attempt record holds its inputs any more, so
	// the applied record still holds an older apply's.
	EventAppliedInputsUnknown = "AppliedInputsUnknown"
	// EventDestroyInputsMismatch: a destroy renders inputs whose hash is
	// not the one the state records: those of an apply that failed after
	// it may have changed resources, or, with no record of the state's
	// hash, the last ones applied.
	EventDestroyInputsMismatch = "DestroyInputsMismatch"
	// EventForceUnlocked: a stale state lock was force-unlocked.
	EventForceUnlocked = "ForceUnlocked"
	// EventStateAdopted: the state written by a successful apply was
	// adopted with its new inputs hash.
	EventStateAdopted = "StateAdopted"
	// EventStateLost: a provisioned object's state is gone or carries no
	// inputs hash (StateReadable False/StateLost), or an apply whose
	// outcome is unconfirmed may have left resources no state records
	// (StateReadable False/ApplyOutcomeUnknown).
	EventStateLost = "StateLost"
	// EventStateLocked: the state lock is held by something else
	// (StateReadable False/StateLocked).
	EventStateLocked = "StateLocked"
	// EventStateUnreadable: the state could not be read.
	EventStateUnreadable = "StateUnreadable"
	// EventStateBackedUp: a new state serial was copied into a backup
	// (once per backup).
	EventStateBackedUp = "StateBackedUp"
	// EventStateRestored: a restore Job pushed a backup into the backend
	// and the captf.io/restore-state annotation was removed.
	EventStateRestored = "StateRestored"
	// EventStateRestoreFailed: a restore Job failed; it is not retried for
	// the same serial.
	EventStateRestoreFailed = "StateRestoreFailed"
	// EventOutputsInvalid: the module's outputs broke the contract.
	EventOutputsInvalid = "OutputsInvalid"

	// EventDriftDetected: a drift check found a difference.
	EventDriftDetected = "DriftDetected"
	// EventDriftResolved: DriftDetected went from True to False.
	EventDriftResolved = "DriftResolved"
	// EventDriftRemediationStarted: an apply remediating drift started.
	EventDriftRemediationStarted = "DriftRemediationStarted"
	// EventInstanceHealthy: InfrastructureHealthy became True.
	EventInstanceHealthy = "InstanceHealthy"
	// EventInstanceUnhealthy: InfrastructureHealthy became False for an
	// unhealthy, degraded, stopped or terminated instance.
	EventInstanceUnhealthy = "InstanceUnhealthy"
	// EventRemediationRequested: the owner Machine was annotated with
	// cluster.x-k8s.io/remediate-machine.
	EventRemediationRequested = "RemediationRequested"
	// EventRemediationWithdrawn: the instance read Healthy again and the
	// annotation CAPTF set was removed from the owner Machine.
	EventRemediationWithdrawn = "RemediationWithdrawn"

	// EventIdentityNotAllowed: the identity does not allow the namespace.
	EventIdentityNotAllowed = "IdentityNotAllowed"
	// EventIdentitySecretFound: a TerraformClusterIdentity's credentials
	// Secret appeared.
	EventIdentitySecretFound = "IdentitySecretFound"
	// EventIdentitySecretNotFound: a TerraformClusterIdentity's credentials
	// Secret went missing, or lacks a key listed in requiredKeys.
	EventIdentitySecretNotFound = "IdentitySecretNotFound"
	// EventMirrorCreated: the credential mirror of the namespace was
	// created on behalf of this object.
	EventMirrorCreated = "MirrorCreated"
	// EventMirrorRemoved: the credential mirror of the namespace was
	// deleted on behalf of this object.
	EventMirrorRemoved = "MirrorRemoved"
	// EventOwnerReferencesRepaired: Secrets of this object (state,
	// state backups, durable inputs, plan key, or its entry in the
	// credential mirror) had no owner reference to it, or one to an
	// earlier UID, as a management-cluster restore leaves them; they are
	// owned by the object again.
	EventOwnerReferencesRepaired = "OwnerReferencesRepaired"

	// EventCapacityResolved: a TerraformMachineTemplate's capacity or
	// nodeInfo changed from its image labels.
	EventCapacityResolved = "CapacityResolved"
	// EventImageInspectFailed: the registry could not be read for capacity.
	EventImageInspectFailed = "ImageInspectFailed"

	// EventConditionChanged: any other owned condition changed status or
	// reason; Normal into its good or an informational state, Warning into
	// its bad state.
	EventConditionChanged = "ConditionChanged"

	// EventReplicasWrittenBack: an autoscaled pool's observed replicas
	// output was patched onto MachinePool.spec.replicas ("X → Y").
	EventReplicasWrittenBack = "ReplicasWrittenBack"
	// EventReplicasManagedExternally: valid autoscaler annotations but a
	// foreign replicas-managed-by owner; spec.replicas is not written back.
	EventReplicasManagedExternally = "ReplicasManagedExternally"
	// EventExternallyManagedReleased: a deleting object that carries the
	// managed-by annotation lost only this provider's finalizer; its
	// state and infrastructure are left to the external manager.
	EventExternallyManagedReleased = "ExternallyManagedReleased"
)

// DocumentedEvents returns every event reason the manager emits;
// TestEventsEmitted checks that each is emitted somewhere.
func DocumentedEvents() []string {
	return []string{
		EventJobCreated, EventJobSucceeded, EventJobFailed, EventJobInterrupted, EventJobDeadlineExceeded,
		EventStuckJobDeleted, EventImagePullFallback, EventDestructivePlanBlocked,
		EventPlanReady, EventPlanApproved, EventPlanApplied, EventPlanChanged, EventPlanSuperseded,
		EventWaitingForRunLease, EventWaitingForClusterOperation, EventWaitingForMachineOperations, EventWaitingForJobSlot,
		EventDeletionStarted, EventDestroyed, EventFinalizerRemoved,
		EventInfrastructureRetained, EventRetainedStateFound, EventRetainedStateAdopted, EventPaused, EventResumed, EventProvisioned,
		EventProviderIDSet, EventControlPlaneEndpointSet, EventFailureDomainsChanged, EventExportsNotPublished,
		EventInputsChanged, EventDigestPinned, EventDigestUnknown, EventAppliedInputsUnknown, EventDestroyInputsMismatch, EventForceUnlocked, EventStateAdopted,
		EventStateLost, EventStateLocked, EventStateUnreadable, EventOutputsInvalid,
		EventStateBackedUp, EventStateRestored, EventStateRestoreFailed,
		EventDriftDetected, EventDriftResolved, EventDriftRemediationStarted, EventInstanceHealthy, EventInstanceUnhealthy,
		EventRemediationRequested, EventRemediationWithdrawn, EventReplicasWrittenBack, EventReplicasManagedExternally,
		EventExternallyManagedReleased,
		EventIdentityNotAllowed, EventIdentitySecretFound, EventIdentitySecretNotFound, EventMirrorCreated, EventMirrorRemoved,
		EventOwnerReferencesRepaired, EventCapacityResolved, EventImageInspectFailed, EventConditionChanged,
	}
}

// MaxEventNote bounds an event's note, well below the recorder's 1 KiB.
const MaxEventNote = 512

// Emit records an event about obj when a recorder is configured, of
// eventType (Normal or Warning) and reason, with action naming what CAPTF
// was doing and note (formatted with args, in the style of fmt.Sprintf) as
// its message.
func (d Deps) Emit(obj Object, eventType, reason, action, note string, args ...any) {
	d.EmitRelated(obj, nil, eventType, reason, action, note, args...)
}

// EmitRelated is Emit about obj with a related object (a Job), of
// eventType (Normal or Warning) and reason, with action naming what CAPTF
// was doing and note (formatted with args, in the style of fmt.Sprintf) as
// its message.
func (d Deps) EmitRelated(obj Object, related runtime.Object, eventType, reason, action, note string, args ...any) {
	if d.Recorder == nil {
		return
	}
	d.Recorder.Eventf(obj, related, eventType, reason, action, "%s", capNote(fmt.Sprintf(note, args...)))
}

// capNote returns s cut to MaxEventNote bytes on a rune boundary.
func capNote(s string) string {
	return strutil.Truncate(s, MaxEventNote)
}

// snapshot returns every owned condition of obj as the reconcile found it,
// by condition type.
func snapshot(obj Object) map[string]metav1.Condition {
	out := map[string]metav1.Condition{}
	for _, t := range OwnedConditions() {
		if c := conditions.Get(obj, t); c != nil {
			out[t] = *c
		}
	}
	return out
}

// transition is the event one condition change emits.
type transition struct {
	eventType, reason, note string
}

// informationalReasons are condition reasons of an expected, passing state
// (waiting, in progress, not yet known): a change into one is Normal,
// whatever the status. DeletionBlocked's DependentsExist is not one: a
// TerraformCluster deletion held by its machines and pools waits for
// someone to delete them, so it is a Warning.
var informationalReasons = sets.New(
	infrav1.ReadyUnknownReason,
	infrav1.WaitingForOwnerReason,
	infrav1.WaitingForOwnerMachineReason,
	infrav1.WaitingForOwnerMachinePoolReason,
	infrav1.WaitingForClusterInfrastructureReason,
	infrav1.WaitingForClusterExportsReason,
	infrav1.WaitingForBootstrapDataReason,
	infrav1.MirrorPendingReason,
	infrav1.NoApplyYetReason,
	infrav1.StateNotFoundReason,
	infrav1.OutputsPendingReason,
	infrav1.ProvisioningReason,
	infrav1.InstancePendingReason,
	infrav1.WaitingForProvisioningReason,
	infrav1.DriftNotCheckedReason,
	infrav1.DriftJobRunningReason,
	infrav1.NotBlockedReason,
	infrav1.WaitingForRunLeaseReason,
	infrav1.WaitingForClusterOperationReason,
	infrav1.WaitingForMachineOperationsReason,
	infrav1.WaitingForJobSlotReason,
	infrav1.PlanAwaitingApprovalReason,
	infrav1.PlanChangedReason,
	infrav1.AutoscalingDisabledReason,
	clusterv1.NotDeletingReason,
	clusterv1.DeletingReason,
)

// badUnknownReasons are Unknown reasons that report a problem.
var badUnknownReasons = sets.New(
	infrav1.IdentityCheckFailedReason,
	infrav1.HealthUnknownReason,
	infrav1.DurableInputsMissingReason,
)

// unhealthyReasons are the InfrastructureHealthy False reasons of an
// instance that is not working (InstanceUnhealthy).
var unhealthyReasons = sets.New(
	infrav1.InstanceUnhealthyReason,
	infrav1.InstanceDegradedReason,
	infrav1.InstanceStoppedReason,
	infrav1.InstanceTerminatedReason,
)

// negativePolarity are the condition types whose True is the bad state.
var negativePolarity = sets.New(
	infrav1.DriftDetectedCondition,
	infrav1.DeletionBlockedCondition,
	clusterv1.DeletingCondition,
)

// bad reports whether c is its type's bad state (a Warning), given prev,
// the condition before (nil when it was not set). Ready is only bad when
// it leaves True: before provisioning it is False or Unknown by design.
func bad(prev *metav1.Condition, c metav1.Condition) bool {
	if c.Type == infrav1.ReadyCondition && c.Reason != infrav1.SecretNotFoundReason && c.Reason != infrav1.CredentialsIncompleteReason {
		return c.Status != metav1.ConditionTrue && prev != nil && prev.Status == metav1.ConditionTrue
	}
	if informationalReasons.Has(c.Reason) {
		return false
	}
	switch c.Status {
	case metav1.ConditionUnknown:
		return badUnknownReasons.Has(c.Reason)
	case metav1.ConditionTrue:
		return negativePolarity.Has(c.Type)
	default:
		return !negativePolarity.Has(c.Type)
	}
}

// conditionNote returns c formatted as
// "<Type>: <Status>/<Reason>[: <message>]".
func conditionNote(c metav1.Condition) string {
	s := fmt.Sprintf("%s: %s/%s", c.Type, c.Status, c.Reason)
	if c.Message != "" {
		s += ": " + c.Message
	}
	return s
}

// jobNamed returns the Job the condition message msg is about, from its
// "Job <name>[: …]" form, or "" when msg does not name one.
func jobNamed(msg string) string {
	rest, ok := strings.CutPrefix(msg, "Job ")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, ":")
	if name == "" || strings.ContainsAny(name, " ") {
		return ""
	}
	return name
}

// transitionFor decides the event of one owned condition, c, against its
// state before (nil when unset): at most one event per change, none when
// nothing changed.
//
//   - A status or reason change emits, Normal or Warning by polarity (bad);
//     a condition that first appears emits only in its bad state, so a new
//     object's first-visit conditions are quiet.
//   - Job outcomes (ApplyJobSucceeded and DriftJobSucceeded naming a Job)
//     emit once per Job: JobSucceeded, JobFailed, JobInterrupted,
//     JobDeadlineExceeded or DestructivePlanBlocked. A running drift Job
//     (DriftJobRunning) is covered by JobCreated.
//   - StateReadable, OutputsValid and IdentityAllowed in their bad state
//     emit again when the reason or message changes (another failure).
//   - Specific reasons replace ConditionChanged: DeletionStarted,
//     DriftDetected (once per drift finding), DriftResolved,
//     InstanceHealthy/InstanceUnhealthy, StateLost, StateLocked,
//     StateUnreadable, OutputsInvalid, IdentityNotAllowed.
//
// Paused is not handled here: the preamble changes it before the snapshot
// (emitPaused). prev is the condition before (nil when unset), and bk (nil
// when bookkeeping did not run) enriches Job outcomes. It returns the
// event to emit and whether one applies.
func transitionFor(prev *metav1.Condition, c metav1.Condition, bk *Bookkeeping) (transition, bool) {
	changed := prev == nil || prev.Status != c.Status || prev.Reason != c.Reason
	isBad := bad(prev, c)
	warn := func(reason string) (transition, bool) {
		return transition{corev1.EventTypeWarning, reason, conditionNote(c)}, true
	}
	switch c.Type {
	case clusterv1.PausedCondition:
		return transition{}, false
	case infrav1.InputsAppliedCondition:
		// The apply Job's own outcome, the plan wait and InputsChanged
		// already emit; this one only summarizes them for status readers.
		return transition{}, false
	case infrav1.RestoreJobSucceededCondition:
		if reason, ok := leaseWaitEvents[c.Reason]; ok {
			if prev == nil || prev.Reason != c.Reason {
				return transition{corev1.EventTypeNormal, reason, conditionNote(c)}, true
			}
			return transition{}, false
		}
		if jobNamed(c.Message) != "" {
			// StateRestored and StateRestoreFailed are emitted once per
			// restore Job when bookkeeping counts it.
			return transition{}, false
		}
	case infrav1.ApplyJobSucceededCondition, infrav1.DriftJobSucceededCondition:
		if reason, ok := leaseWaitEvents[c.Reason]; ok {
			// Once per wait, not on every requeue while it lasts.
			if prev == nil || prev.Reason != c.Reason {
				return transition{corev1.EventTypeNormal, reason, conditionNote(c)}, true
			}
			return transition{}, false
		}
		if planWaitReasons.Has(c.Reason) {
			// PlanReady and PlanChanged are emitted once per Job that made
			// the plan, when bookkeeping counts it.
			return transition{}, false
		}
		if prev != nil && (leaseWaitEvents[prev.Reason] != "" || planWaitReasons.Has(prev.Reason)) {
			return afterLeaseWait(c, bk)
		}
		if c.Reason == infrav1.DriftJobRunningReason {
			return transition{}, false
		}
		if name := jobNamed(c.Message); name != "" {
			if c.Reason == infrav1.DestructivePlanBlockedReason {
				// A blocked Job is reported once, when bookkeeping first
				// reads it: a pool's held condition names the same Job
				// again after a held apply's failure and success, and its
				// message follows the approval hash of the current inputs.
				// The bookkept mark is patched after the status, so a
				// pass whose Job list predates the patch reads the Job as
				// new again; the condition already naming it then tells.
				if f, ok := bk.finishedJob(name); !ok || f.bookkept || (!changed && jobNamed(prev.Message) == name) {
					return transition{}, false
				}
				return jobOutcome(c, name, bk), true
			}
			if !changed && prev.Message == c.Message {
				return transition{}, false
			}
			return jobOutcome(c, name, bk), true
		}
		if c.Reason == infrav1.DestructivePlanBlockedReason {
			// A plan's wait without a blocked Job (after clusterctl move):
			// the plan was announced when it was made.
			return transition{}, false
		}
		if c.Type == infrav1.ApplyJobSucceededCondition && c.Status == metav1.ConditionFalse &&
			(changed || prev.Message != c.Message) {
			// An apply or destroy that could not start (identity, inputs
			// size, missing durable inputs).
			return warn(EventJobFailed)
		}
	case clusterv1.DeletingCondition:
		if c.Status == metav1.ConditionTrue && (prev == nil || prev.Status != metav1.ConditionTrue) {
			return transition{corev1.EventTypeNormal, EventDeletionStarted, "Deletion started: " + conditionNote(c)}, true
		}
		return transition{}, false
	case infrav1.DriftDetectedCondition:
		switch {
		case c.Status == metav1.ConditionTrue:
			if prev != nil && prev.Status == c.Status && sameEvent(*prev, c) {
				return transition{}, false
			}
			return warn(EventDriftDetected)
		case c.Status == metav1.ConditionFalse && prev != nil && prev.Status == metav1.ConditionTrue:
			return transition{corev1.EventTypeNormal, EventDriftResolved, conditionNote(c)}, true
		}
	case infrav1.InfrastructureHealthyCondition:
		switch {
		case !changed || (prev == nil && !isBad):
			return transition{}, false
		case c.Status == metav1.ConditionTrue:
			return transition{corev1.EventTypeNormal, EventInstanceHealthy, conditionNote(c)}, true
		case c.Status == metav1.ConditionFalse && unhealthyReasons.Has(c.Reason):
			return warn(EventInstanceUnhealthy)
		}
	case infrav1.StateReadableCondition, infrav1.OutputsValidCondition, infrav1.IdentityAllowedCondition:
		if c.Status == metav1.ConditionFalse {
			if prev != nil && prev.Status == c.Status && sameEvent(*prev, c) {
				return transition{}, false
			}
			return warn(badStateReason(c))
		}
	}
	if !changed || (prev == nil && !isBad) {
		return transition{}, false
	}
	if isBad {
		return warn(EventConditionChanged)
	}
	return transition{corev1.EventTypeNormal, EventConditionChanged, conditionNote(c)}, true
}

// leaseWaitEvents are the Job condition reasons of an operation waiting for
// a run lease, with the event each wait emits once.
var leaseWaitEvents = map[string]string{
	infrav1.WaitingForRunLeaseReason:          EventWaitingForRunLease,
	infrav1.WaitingForClusterOperationReason:  EventWaitingForClusterOperation,
	infrav1.WaitingForMachineOperationsReason: EventWaitingForMachineOperations,
	infrav1.WaitingForJobSlotReason:           EventWaitingForJobSlot,
}

// planWaitReasons are the ApplyJobSucceeded reasons of an apply waiting for
// the approval of its plan (applyPolicy Manual).
var planWaitReasons = sets.New(
	infrav1.PlanAwaitingApprovalReason,
	infrav1.PlanChangedReason,
)

// leaseWaitMetrics are the captf_lease_waits_total reasons of the waits.
var leaseWaitMetrics = map[string]string{
	infrav1.WaitingForRunLeaseReason:          metrics.LeaseWaitRunLease,
	infrav1.WaitingForClusterOperationReason:  metrics.LeaseWaitClusterOperation,
	infrav1.WaitingForMachineOperationsReason: metrics.LeaseWaitMachineOperations,
	infrav1.WaitingForJobSlotReason:           metrics.LeaseWaitJobSlot,
}

// afterLeaseWait is the event of c, a Job condition that leaves a lease
// wait (or a wait for a plan's approval). It goes back to what the newest
// finished Job says, which names a Job whose outcome was reported when it
// finished: only a Job that finished this pass, found in bk, or an apply
// or destroy that could not start, emits. It returns the event to emit
// and whether one applies.
func afterLeaseWait(c metav1.Condition, bk *Bookkeeping) (transition, bool) {
	name := jobNamed(c.Message)
	switch {
	case name != "":
		if f, ok := bk.finishedJob(name); ok && !f.bookkept {
			return jobOutcome(c, name, bk), true
		}
	case c.Type == infrav1.ApplyJobSucceededCondition && c.Status == metav1.ConditionFalse:
		return transition{corev1.EventTypeWarning, EventJobFailed, conditionNote(c)}, true
	}
	return transition{}, false
}

// badStateReason returns the event reason of c, a StateReadable,
// OutputsValid or IdentityAllowed condition entering False.
func badStateReason(c metav1.Condition) string {
	switch {
	case c.Type == infrav1.OutputsValidCondition:
		return EventOutputsInvalid
	case c.Type == infrav1.IdentityAllowedCondition:
		return EventIdentityNotAllowed
	case c.Reason == infrav1.StateLostReason, c.Reason == infrav1.ApplyOutcomeUnknownReason:
		return EventStateLost
	case c.Reason == infrav1.StateLockedReason:
		return EventStateLocked
	case c.Reason == infrav1.RetainedStateFoundReason:
		return EventRetainedStateFound
	}
	return EventStateUnreadable
}

// jobOutcome is the event of a finished Job named by an ApplyJobSucceeded
// or DriftJobSucceeded condition c, whose name is name, enriched from what
// bk, this pass's bookkeeping, read of it. It returns the event to emit.
func jobOutcome(c metav1.Condition, name string, bk *Bookkeeping) transition {
	f, known := bk.finishedJob(name)
	op := ""
	if known {
		op = string(jobs.OpOf(f.job)) + " "
	}
	switch {
	case c.Status == metav1.ConditionTrue:
		note := fmt.Sprintf("%sJob %s succeeded", op, name)
		if known {
			note += jobDuration(f)
			if f.result != nil && f.result.Changes != nil {
				ch := f.result.Changes
				note += fmt.Sprintf("; resources: %d added, %d changed, %d destroyed", ch.Add, ch.Change, ch.Destroy)
				if ch.Import > 0 {
					note += fmt.Sprintf(", %d imported", ch.Import)
				}
			}
		}
		return transition{corev1.EventTypeNormal, EventJobSucceeded, note}
	case c.Reason == infrav1.DestructivePlanBlockedReason:
		note := fmt.Sprintf("Job %s stopped before a plan that deletes or replaces resources; nothing was applied", name)
		if known && f.job.Annotations[ApprovalHashAnnotation] != "" {
			note += ". The pool guarded the apply for the cluster's exports; " + infrav1.ApplyJobSucceededCondition +
				" says what the plan is for and what the pool applies until it is approved"
		}
		if plan := bk.madePlans[name]; known && plan != "" {
			note += fmt.Sprintf(". TerraformPlan %s waits for approval: %s", plan, planApproveCommand(plan, f.job.Namespace))
		}
		return transition{corev1.EventTypeWarning, EventDestructivePlanBlocked, note}
	case c.Reason == infrav1.JobDeadlineExceededReason || c.Reason == infrav1.DriftJobDeadlineExceededReason:
		return transition{corev1.EventTypeWarning, EventJobDeadlineExceeded,
			fmt.Sprintf("%sJob %s exceeded activeDeadlineSeconds: %s%s", op, name, conditionNote(c), retryNote(f, known, bk))}
	case known && f.interrupted:
		return transition{corev1.EventTypeWarning, EventJobInterrupted,
			fmt.Sprintf("%sJob %s was interrupted (drain, eviction or deletion)%s; it is retried without backoff", op, name, jobDuration(f))}
	}
	return transition{corev1.EventTypeWarning, EventJobFailed, conditionNote(c) + retryNote(f, known, bk)}
}

// retryNote returns when the op of f, a failed Job bk read (known false
// when it did not), runs again: "; next attempt not before <time>
// (backoff <delay> after <n> failures)", from the same
// failure count, last failure time and history limit DecideOp backs off
// with (JobsView, RetryDelay), so it does not depend on when the event is
// emitted. A failed drift remediation at the remediation cap says it
// waits for a drift check instead. It returns "" when bk counts no
// failure of the op (a Job that changed nothing, or one bk did not read).
func retryNote(f finished, known bool, bk *Bookkeeping) string {
	if !known {
		return ""
	}
	op := jobs.OpOf(f.job)
	n, last := bk.View.Failures[op], bk.View.LastFailure[op]
	if n == 0 || last.IsZero() {
		return ""
	}
	if op == jobs.OpApply && f.job.Annotations[RemediationAnnotation] == "true" && bk.View.RemediationFailures >= max(bk.View.FailedLimit, 1) {
		return fmt.Sprintf("; %d drift remediations failed, so none runs again until a drift check succeeds", bk.View.RemediationFailures)
	}
	delay, failures := RetryDelay(n, bk.View.FailedLimit), "1 failure"
	if n > 1 {
		failures = fmt.Sprintf("%d consecutive failures", n)
	}
	return fmt.Sprintf("; next attempt not before %s (backoff %s after %s)", last.Add(delay).UTC().Format(time.RFC3339), delay, failures)
}

// jobDuration returns " in <duration>" from f, the finished Job's start to
// its finish, or "" when either is unknown.
func jobDuration(f finished) string {
	start := f.job.CreationTimestamp.Time
	if f.job.Status.StartTime != nil {
		start = f.job.Status.StartTime.Time
	}
	end := jobs.FinishedAt(f.job)
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return ""
	}
	return " in " + end.Sub(start).Round(time.Second).String()
}

// sameEvent reports whether prev and c, two conditions of the same type
// and status, describe the same event. DriftDetected is the same drift
// check while its drift summary is unchanged: the DriftPending ↔
// DriftRemediating flips of a remediation (and their messages) are not a
// new finding.
func sameEvent(prev, c metav1.Condition) bool {
	if c.Type == infrav1.DriftDetectedCondition {
		return driftSummaryMessage(prev.Message) == driftSummaryMessage(c.Message)
	}
	return prev.Reason == c.Reason && prev.Message == c.Message
}

// metricTransitions are the conditions whose entry into False (or a new
// reason while False) is counted by recordTransition.
var metricTransitions = sets.New(
	infrav1.StateReadableCondition,
	infrav1.OutputsValidCondition,
	infrav1.IdentityAllowedCondition,
)

// emitTransitions emits, through d, the events of every owned condition of
// obj, a kind, that changed since before (transitionFor), except Paused,
// logging each with logger, and counts the metric transitions. bk (nil when
// bookkeeping did not run) enriches Job outcomes.
func emitTransitions(d Deps, logger klog.Logger, kind string, obj Object, before map[string]metav1.Condition, bk *Bookkeeping) {
	emitConditions(d, logger, kind, obj, before, OwnedConditions(), bk)
}

// emitConditions is emitTransitions for obj, a kind, restricted to the
// condition types given, emitting through d and logging with logger, with
// before the prior condition snapshot and bk this pass's bookkeeping (nil
// when it did not run).
func emitConditions(d Deps, logger klog.Logger, kind string, obj Object, before map[string]metav1.Condition, types []string, bk *Bookkeeping) {
	for _, t := range types {
		c := conditions.Get(obj, t)
		if c == nil {
			continue
		}
		var prev *metav1.Condition
		if p, ok := before[t]; ok {
			prev = &p
		}
		if tr, ok := transitionFor(prev, *c, bk); ok {
			logTransition(logger, *c, tr)
			d.Emit(obj, tr.eventType, tr.reason, "Reconcile", "%s", tr.note)
		}
		// Metrics count entering the bad state or a new reason in it; a
		// changed message alone (a detail) is not a new refusal or error.
		if metricTransitions.Has(t) && c.Status == metav1.ConditionFalse &&
			(prev == nil || prev.Status != c.Status || prev.Reason != c.Reason) {
			recordTransition(d, kind, c)
		}
		// A wait counts once, when it starts (or its reason changes).
		if reason, ok := leaseWaitMetrics[c.Reason]; ok && (prev == nil || prev.Reason != c.Reason) {
			d.Metrics.LeaseWait(kind, reason)
		}
	}
}

// logTransition logs, with logger, the change of c, an owned condition,
// that emits tr. A Warning (a failure, a refusal, a lost state, drift) is
// logged at V0 with the condition's message, so it is seen without
// raising the verbosity; a Normal one at LogFlow.
func logTransition(logger klog.Logger, c metav1.Condition, tr transition) {
	if tr.eventType == corev1.EventTypeWarning {
		logger.Info("Condition changed", "type", c.Type, "status", c.Status, "reason", c.Reason, "message", c.Message, "event", tr.reason)
		return
	}
	logger.V(LogFlow).Info("Condition changed", "type", c.Type, "status", c.Status, "reason", c.Reason, "event", tr.reason)
}

// emitPaused emits, through d, Paused or Resumed for obj when the Paused
// condition changed from before (nil when unset). A first visit that
// finds the object not paused emits nothing.
func emitPaused(d Deps, obj Object, before *metav1.Condition) {
	c := conditions.Get(obj, clusterv1.PausedCondition)
	if c == nil || (before != nil && before.Status == c.Status) {
		return
	}
	switch {
	case c.Status == metav1.ConditionTrue:
		d.Emit(obj, corev1.EventTypeNormal, EventPaused, "Reconcile", "Reconciliation paused: %s; no Job starts", c.Reason)
	case before != nil && before.Status == metav1.ConditionTrue:
		d.Emit(obj, corev1.EventTypeNormal, EventResumed, "Reconcile", "Reconciliation resumed")
	}
}
