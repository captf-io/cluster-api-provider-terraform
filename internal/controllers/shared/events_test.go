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
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// cond returns a Condition of type t, status s, reason and message msg.
func cond(t string, s metav1.ConditionStatus, reason, msg string) *metav1.Condition {
	return &metav1.Condition{Type: t, Status: s, Reason: reason, Message: msg}
}

// cTrue, cFalse and cUnknown are the three metav1.ConditionStatus values;
// normal and warning are the two Kubernetes event types.
const (
	cTrue    = metav1.ConditionTrue
	cFalse   = metav1.ConditionFalse
	cUnknown = metav1.ConditionUnknown
	normal   = corev1.EventTypeNormal
	warning  = corev1.EventTypeWarning
)

// TestTransitionFor: the event of each kind of condition change, its type
// (polarity), and none for an unchanged condition or a quiet first visit.
func TestTransitionFor(t *testing.T) {
	t.Parallel()
	ih, ready, dd := infrav1.InfrastructureHealthyCondition, infrav1.ReadyCondition, infrav1.DriftDetectedCondition
	sr, aj, dj := infrav1.StateReadableCondition, infrav1.ApplyJobSucceededCondition, infrav1.DriftJobSucceededCondition
	del := clusterv1.DeletingCondition
	// blockedNow has the blocked Jobs a2 and a3 newly finished this pass;
	// blockedBefore has a2 bookkept already.
	blockedNow := &Bookkeeping{byName: map[string]finished{}}
	for _, name := range []string{"a2", "a3"} {
		j := job(name, jobs.OpApply, jobs.Failed, t0)
		blockedNow.byName[name] = finished{job: &j, blocked: true}
	}
	a2 := job("a2", jobs.OpApply, jobs.Failed, t0)
	blockedBefore := &Bookkeeping{byName: map[string]finished{"a2": {job: &a2, blocked: true, bookkept: true}}}
	cases := []struct {
		name       string
		prev, c    *metav1.Condition
		bk         *Bookkeeping
		reason     string // "" = no event
		eventType  string
		notePrefix string
	}{
		{name: "unchanged Ready", prev: cond(ready, cTrue, infrav1.ReadyReason, ""), c: cond(ready, cTrue, infrav1.ReadyReason, "")},
		{name: "Ready becomes True", prev: cond(ready, cUnknown, infrav1.ReadyUnknownReason, "x"), c: cond(ready, cTrue, infrav1.ReadyReason, ""),
			reason: EventConditionChanged, eventType: normal, notePrefix: "Ready: True/Ready"},
		{name: "Ready leaves True", prev: cond(ready, cTrue, infrav1.ReadyReason, ""), c: cond(ready, cFalse, infrav1.NotReadyReason, "InstanceStopped"),
			reason: EventConditionChanged, eventType: warning, notePrefix: "Ready: False/NotReady: InstanceStopped"},
		{name: "Ready False before provisioning is informational", prev: cond(ready, cUnknown, infrav1.ReadyUnknownReason, ""), c: cond(ready, cFalse, infrav1.NotReadyReason, ""),
			reason: EventConditionChanged, eventType: normal},
		{name: "first visit Ready False is quiet", c: cond(ready, cFalse, infrav1.NotReadyReason, "")},
		{name: "a message change alone is quiet", prev: cond(ready, cFalse, infrav1.NotReadyReason, "a"), c: cond(ready, cFalse, infrav1.NotReadyReason, "b")},

		{name: "healthy", prev: cond(ih, cFalse, infrav1.ProvisioningReason, ""), c: cond(ih, cTrue, infrav1.HealthyReason, ""),
			reason: EventInstanceHealthy, eventType: normal},
		{name: "unhealthy", prev: cond(ih, cTrue, infrav1.HealthyReason, ""), c: cond(ih, cFalse, infrav1.InstanceUnhealthyReason, ""),
			reason: EventInstanceUnhealthy, eventType: warning},
		{name: "terminated", prev: cond(ih, cFalse, infrav1.InstanceUnhealthyReason, ""), c: cond(ih, cFalse, infrav1.InstanceTerminatedReason, ""),
			reason: EventInstanceUnhealthy, eventType: warning},
		{name: "pending is informational", prev: cond(ih, cTrue, infrav1.HealthyReason, ""), c: cond(ih, cFalse, infrav1.InstancePendingReason, ""),
			reason: EventConditionChanged, eventType: normal},
		{name: "health unknown warns", prev: cond(ih, cTrue, infrav1.HealthyReason, ""), c: cond(ih, cUnknown, infrav1.HealthUnknownReason, ""),
			reason: EventConditionChanged, eventType: warning},
		{name: "provisioning on first visit is quiet", c: cond(ih, cFalse, infrav1.ProvisioningReason, "")},
		{name: "healthy unchanged", prev: cond(ih, cTrue, infrav1.HealthyReason, ""), c: cond(ih, cTrue, infrav1.HealthyReason, "")},

		{name: "drift found", prev: cond(dd, cFalse, infrav1.NoDriftReason, ""), c: cond(dd, cTrue, infrav1.DriftReportedReason, "Job d1: 1 to change"),
			reason: EventDriftDetected, eventType: warning},
		{name: "drift resolved", prev: cond(dd, cTrue, infrav1.DriftPendingReason, "Job d1"), c: cond(dd, cFalse, infrav1.NoDriftReason, "Job d2"),
			reason: EventDriftResolved, eventType: normal},
		{name: "first check without drift", prev: cond(dd, cUnknown, infrav1.DriftNotCheckedReason, ""), c: cond(dd, cFalse, infrav1.NoDriftReason, "Job d1"),
			reason: EventConditionChanged, eventType: normal},
		{name: "remediating is the same finding", prev: cond(dd, cTrue, infrav1.DriftPendingReason, "Job d1: x"),
			c: cond(dd, cTrue, infrav1.DriftRemediatingReason, "Job d1: x"+remediationMarker+"a applies the current inputs")},

		{name: "state lost", prev: cond(sr, cTrue, infrav1.StateReadReason, ""), c: cond(sr, cFalse, infrav1.StateLostReason, "gone"),
			reason: EventStateLost, eventType: warning},
		{name: "state locked", prev: cond(sr, cTrue, infrav1.StateReadReason, ""), c: cond(sr, cFalse, infrav1.StateLockedReason, "held"),
			reason: EventStateLocked, eventType: warning},
		{name: "state corrupt", prev: cond(sr, cTrue, infrav1.StateReadReason, ""), c: cond(sr, cFalse, infrav1.StateCorruptReason, "bad"),
			reason: EventStateUnreadable, eventType: warning},
		{name: "state lost again, same message", prev: cond(sr, cFalse, infrav1.StateLostReason, "gone"), c: cond(sr, cFalse, infrav1.StateLostReason, "gone")},
		{name: "state readable again", prev: cond(sr, cFalse, infrav1.StateLockedReason, "held"), c: cond(sr, cTrue, infrav1.StateReadReason, ""),
			reason: EventConditionChanged, eventType: normal},
		{name: "no state yet is quiet", c: cond(sr, cUnknown, infrav1.StateNotFoundReason, "")},
		{name: "outputs invalid", prev: cond(infrav1.OutputsValidCondition, cTrue, infrav1.OutputsValidReason, ""),
			c: cond(infrav1.OutputsValidCondition, cFalse, infrav1.OutputsMissingReason, "x"), reason: EventOutputsInvalid, eventType: warning},
		{name: "identity not allowed", c: cond(infrav1.IdentityAllowedCondition, cFalse, infrav1.NamespaceNotAllowedReason, "x"),
			reason: EventIdentityNotAllowed, eventType: warning},

		{name: "deletion started", prev: cond(del, cFalse, clusterv1.NotDeletingReason, ""), c: cond(del, cTrue, clusterv1.DeletingReason, ""),
			reason: EventDeletionStarted, eventType: normal},
		{name: "deleted before the first visit", c: cond(del, cTrue, clusterv1.DeletingReason, ""), reason: EventDeletionStarted, eventType: normal},
		{name: "still deleting", prev: cond(del, cTrue, clusterv1.DeletingReason, ""), c: cond(del, cTrue, clusterv1.DeletingReason, "")},
		{name: "not deleting on first visit", c: cond(del, cFalse, clusterv1.NotDeletingReason, "")},

		{name: "drift Job running is JobCreated's", prev: cond(dj, cTrue, infrav1.DriftCheckedReason, "Job d1"), c: cond(dj, cUnknown, infrav1.DriftJobRunningReason, "Job d2")},
		{name: "drift Job succeeded", prev: cond(dj, cUnknown, infrav1.DriftJobRunningReason, "Job d2"), c: cond(dj, cTrue, infrav1.DriftCheckedReason, "Job d2"),
			reason: EventJobSucceeded, eventType: normal, notePrefix: "Job d2 succeeded"},
		{name: "another drift Job succeeded", prev: cond(dj, cTrue, infrav1.DriftCheckedReason, "Job d1"), c: cond(dj, cTrue, infrav1.DriftCheckedReason, "Job d2"),
			reason: EventJobSucceeded, eventType: normal},
		{name: "the same drift Job", prev: cond(dj, cTrue, infrav1.DriftCheckedReason, "Job d2"), c: cond(dj, cTrue, infrav1.DriftCheckedReason, "Job d2")},
		{name: "checks stalled on missing inputs warn", prev: cond(dj, cTrue, infrav1.DriftCheckedReason, "Job d1"), c: cond(dj, cUnknown, infrav1.DurableInputsMissingReason, "gone"),
			reason: EventConditionChanged, eventType: warning, notePrefix: "DriftJobSucceeded: Unknown/DurableInputsMissing: gone"},
		{name: "checks stalled on missing inputs again is quiet", prev: cond(dj, cUnknown, infrav1.DurableInputsMissingReason, "gone"), c: cond(dj, cUnknown, infrav1.DurableInputsMissingReason, "gone")},
		{name: "drift Job deadline", prev: cond(dj, cTrue, infrav1.DriftCheckedReason, "Job d1"), c: cond(dj, cFalse, infrav1.DriftJobDeadlineExceededReason, "Job d2"),
			reason: EventJobDeadlineExceeded, eventType: warning},
		{name: "drift Job failed", prev: cond(dj, cTrue, infrav1.DriftCheckedReason, "Job d1"), c: cond(dj, cFalse, infrav1.DriftJobFailedReason, "Job d2: step plan failed"),
			reason: EventJobFailed, eventType: warning},
		{name: "apply Job deadline", c: cond(aj, cFalse, infrav1.JobDeadlineExceededReason, "Job a1"), reason: EventJobDeadlineExceeded, eventType: warning},
		{name: "apply without a Job", prev: cond(aj, cUnknown, infrav1.NoApplyYetReason, ""), c: cond(aj, cFalse, infrav1.IdentityNotAllowedReason, "waits"),
			reason: EventJobFailed, eventType: warning},
		{name: "apply without a Job, unchanged", prev: cond(aj, cFalse, infrav1.IdentityNotAllowedReason, "waits"), c: cond(aj, cFalse, infrav1.IdentityNotAllowedReason, "waits")},
		{name: "no apply yet is quiet", c: cond(aj, cUnknown, infrav1.NoApplyYetReason, "")},
		{name: "apply blocked", prev: cond(aj, cTrue, infrav1.ApplySucceededReason, "Job a1"), c: cond(aj, cFalse, infrav1.DestructivePlanBlockedReason, "Job a2: x. approve hash h2:a"),
			bk: blockedNow, reason: EventDestructivePlanBlocked, eventType: warning, notePrefix: "Job a2 stopped before a plan"},
		{name: "the same blocked Job with a new approval hash is reported once", prev: cond(aj, cFalse, infrav1.DestructivePlanBlockedReason, "Job a2: x. approve hash h2:a"),
			c: cond(aj, cFalse, infrav1.DestructivePlanBlockedReason, "Job a2: x. approve hash h2:b"), bk: blockedBefore},
		{name: "another blocked Job is reported", prev: cond(aj, cFalse, infrav1.DestructivePlanBlockedReason, "Job a2: x"),
			c: cond(aj, cFalse, infrav1.DestructivePlanBlockedReason, "Job a3: y"), bk: blockedNow, reason: EventDestructivePlanBlocked, eventType: warning},
		{name: "a bookkept blocked Job is not reported again after another outcome", prev: cond(aj, cFalse, infrav1.ApplyFailedReason, "Job a4"),
			c: cond(aj, cFalse, infrav1.DestructivePlanBlockedReason, "Job a2: x"), bk: blockedBefore},
		{name: "a cluster's blocked Job read again before its bookkept mark is seen is not reported again",
			prev: cond(aj, cFalse, infrav1.DestructivePlanBlockedReason, "Job a2: x. approve inputs hash h1:a"),
			c:    cond(aj, cFalse, infrav1.DestructivePlanBlockedReason, "Job a2: x. approve inputs hash h1:a"), bk: blockedNow},
		{name: "a pool's blocked Job read again before its bookkept mark is seen is not reported again",
			prev: cond(aj, cFalse, infrav1.DestructivePlanBlockedReason, "Job a2: x. approve hash h2:a"),
			c:    cond(aj, cFalse, infrav1.DestructivePlanBlockedReason, "Job a2: x. approve hash h2:b"), bk: blockedNow},
		{name: "a blocked Job bookkeeping did not read is not reported", prev: cond(aj, cTrue, infrav1.ApplySucceededReason, "Job a1"),
			c: cond(aj, cFalse, infrav1.DestructivePlanBlockedReason, "Job a2: x")},

		{name: "runner RBAC refused", prev: cond(infrav1.RunnerRBACReadyCondition, cTrue, infrav1.RBACReadyReason, ""),
			c: cond(infrav1.RunnerRBACReadyCondition, cFalse, infrav1.ServiceAccountNotOptedInReason, "x"), reason: EventConditionChanged, eventType: warning},
		{name: "mirror pending on first visit", c: cond(infrav1.CredentialsMirroredCondition, cUnknown, infrav1.MirrorPendingReason, "")},
		{name: "deletion waits for machines", prev: cond(infrav1.DeletionBlockedCondition, cFalse, infrav1.NotBlockedReason, ""),
			c: cond(infrav1.DeletionBlockedCondition, cTrue, infrav1.DependentsExistReason, "2 machines"), reason: EventConditionChanged, eventType: warning},
		{name: "deletion still waits for fewer machines", prev: cond(infrav1.DeletionBlockedCondition, cTrue, infrav1.DependentsExistReason, "2 machines"),
			c: cond(infrav1.DeletionBlockedCondition, cTrue, infrav1.DependentsExistReason, "1 machine")},
		{name: "deletion no longer waits", prev: cond(infrav1.DeletionBlockedCondition, cTrue, infrav1.DependentsExistReason, "1 machine"),
			c: cond(infrav1.DeletionBlockedCondition, cFalse, infrav1.NotBlockedReason, ""), reason: EventConditionChanged, eventType: normal},
		{name: "endpoint missing", prev: cond(infrav1.EndpointAvailableCondition, cTrue, infrav1.EndpointAvailableReason, ""),
			c: cond(infrav1.EndpointAvailableCondition, cFalse, infrav1.WaitingForEndpointReason, ""), reason: EventConditionChanged, eventType: warning},
		{name: "Paused is emitPaused's", prev: cond(clusterv1.PausedCondition, cFalse, clusterv1.NotPausedReason, ""), c: cond(clusterv1.PausedCondition, cTrue, clusterv1.PausedReason, "")},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tr, ok := transitionFor(tt.prev, *tt.c, tt.bk)
			if tt.reason == "" {
				if ok {
					t.Errorf("event %+v, want none", tr)
				}
				return
			}
			if !ok || tr.reason != tt.reason || tr.eventType != tt.eventType {
				t.Errorf("event = %+v (%v), want %s %s", tr, ok, tt.eventType, tt.reason)
			}
			if !strings.HasPrefix(tr.note, tt.notePrefix) {
				t.Errorf("note = %q, want prefix %q", tr.note, tt.notePrefix)
			}
		})
	}
}

// TestBlockedPoolApplyNote: the Warning of a pool apply guarded for the
// cluster's exports names the TerraformPlan its plan became and the
// command that approves it, and does not claim the plan is for the
// exports: after a change was partly applied, every apply is guarded, a
// rotation's or version roll's too.
func TestBlockedPoolApplyNote(t *testing.T) {
	t.Parallel()
	j := job("a2", jobs.OpApply, jobs.Failed, t0)
	j.Annotations = map[string]string{ApprovalHashAnnotation: "h2:a", ClusterOutputsHashAnnotation: "h2:e"}
	bk := &Bookkeeping{byName: map[string]finished{"a2": {job: &j, blocked: true}}, madePlans: map[string]string{"a2": "p1-0123456789"}}
	tr := jobOutcome(*cond(infrav1.ApplyJobSucceededCondition, metav1.ConditionFalse, infrav1.DestructivePlanBlockedReason, "Job a2: x"), "a2", bk)
	if !strings.HasSuffix(tr.note, "TerraformPlan p1-0123456789 waits for approval: "+planApproveCommand("p1-0123456789", testNS)) ||
		strings.Contains(tr.note, "The plan is for the cluster's exports") {
		t.Errorf("note = %q", tr.note)
	}
}

// TestCapNote proves Emit truncates an event note to about MaxEventNote
// runes.
func TestCapNote(t *testing.T) {
	t.Parallel()
	rec := &fakeRecorder{}
	Deps{Recorder: rec}.Emit(machine(), normal, EventConditionChanged, "Reconcile", "%s", strings.Repeat("é", MaxEventNote))
	if n := len(rec.events[0].note); n > MaxEventNote || n < MaxEventNote-1 {
		t.Errorf("note length = %d, want about %d", n, MaxEventNote)
	}
}

// changesResult and interruptedResult are encoded runner.Result JSON: a
// successful apply with one add and two changes, and an apply interrupted
// mid-step.
const (
	changesResult     = `{"version":1,"op":"apply","image":{"ref":"x"},"runtime":{"command":[],"version":""},"steps":[],"drift":null,"error":null,"changes":{"add":1,"change":2,"destroy":0}}`
	interruptedResult = `{"version":1,"op":"apply","image":{"ref":"x"},"runtime":{"command":[],"version":""},"steps":[{"name":"apply","exit":1,"seconds":3}],"drift":null,"error":{"kind":"interrupted","step":"apply","tail":"stopped"}}`
)

// reconcileMachine reconciles e's stored machine with inputs, as owner,
// using t for setup, with each mut applied to the fakeKind before the
// reconcile.
func reconcileMachine(t *testing.T, e *env, owner OwnerInfo, mut ...func(*fakeKind)) {
	t.Helper()
	k := e.kindFor(t, owner)
	k.in = machineIn()
	for _, f := range mut {
		f(k)
	}
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
}

// TestJobOutcomeEvents: each finished Job emits its outcome once, and a
// reconcile that finds nothing new emits nothing.
func TestJobOutcomeEvents(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		job     func() batchv1.Job
		result  string
		reason  string
		typ     string
		note    string
		notNote string
	}{
		{
			name: "succeeded", reason: EventJobSucceeded, typ: normal, result: changesResult,
			job: func() batchv1.Job {
				j := job("a", jobs.OpApply, jobs.Succeeded, t0)
				j.Status.StartTime = &metav1.Time{Time: t0.Add(-90 * time.Second)}
				return j
			},
			note: "apply Job a succeeded in 1m30s; resources: 1 added, 2 changed, 0 destroyed",
		},
		{
			name: "interrupted", reason: EventJobInterrupted, typ: warning, result: interruptedResult,
			job:  func() batchv1.Job { return job("a", jobs.OpApply, jobs.Failed, t0) },
			note: "apply Job a was interrupted", notNote: "stopped",
		},
		{
			name: "deadline", reason: EventJobDeadlineExceeded, typ: warning,
			job: func() batchv1.Job {
				j := job("a", jobs.OpApply, jobs.Failed, t0)
				j.Status.Conditions[0].Reason = batchv1.JobReasonDeadlineExceeded
				return j
			},
			note: "apply Job a exceeded activeDeadlineSeconds",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, world(machine(withFinalizer, notPaused))...)
			e.runner.jobs = append(e.runner.jobs, tt.job())
			if tt.result != "" {
				e.runner.pods["a"] = []corev1.Pod{*podWith(tt.result, "")}
			}
			reconcileMachine(t, e, readyOwner)
			reconcileMachine(t, e, readyOwner)
			got := e.rec.only(tt.reason)
			if len(got) != 1 || got[0].eventType != tt.typ || !strings.HasPrefix(got[0].note, tt.note) {
				t.Fatalf("%s events = %+v, want one %s with %q (all: %v)", tt.reason, got, tt.typ, tt.note, e.rec.reasons)
			}
			if tt.notNote != "" && strings.Contains(got[0].note, tt.notNote) {
				t.Errorf("note %q carries %q", got[0].note, tt.notNote)
			}
			if tt.reason != EventJobFailed && e.rec.count(EventJobFailed) != 0 {
				t.Errorf("also JobFailed: %v", e.rec.reasons)
			}
		})
	}
}

// TestLifecycleEvents: deletion, finalizer, pause, stuck Job, inputs,
// adoption and mirror events fire once for their transition.
func TestLifecycleEvents(t *testing.T) {
	t.Parallel()
	t.Run("deletion starts the destroy once", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused))...)
		if err := inputs.Write(t.Context(), e.c, machine(), renderMachine(t), inputs.Meta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
			t.Fatal(err)
		}
		e.state.st = &state.State{InputsHash: "h1:x"}
		reconcileMachine(t, e, readyOwner)
		reconcileMachine(t, e, readyOwner)
		if n := e.rec.count(EventDeletionStarted); n != 1 {
			t.Errorf("DeletionStarted = %d, want 1 (%v)", n, e.rec.reasons)
		}
		created := e.rec.only(EventJobCreated)
		if len(created) != 1 || !strings.Contains(created[0].note, "destroy Job") || !strings.Contains(created[0].note, "being deleted") {
			t.Errorf("JobCreated = %+v", created)
		}
		if j, ok := created[0].related.(*batchv1.Job); !ok || j.Name != e.runner.created[0] {
			t.Errorf("JobCreated related = %#v", created[0].related)
		}
	})
	t.Run("destroy then cleanup removes the finalizer", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused))...)
		e.state.st = &state.State{InputsHash: "h1:x"}
		e.runner.jobs = append(e.runner.jobs, job("d", jobs.OpDestroy, jobs.Succeeded, t0))
		// The mirror the destroy ran with; the cleanup prepares no
		// credentials of its own.
		id, err := identity.Get(t.Context(), e.c, testIdentity)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := identity.EnsureMirror(t.Context(), e.c, e.c, id, testNS, e.get(t)); err != nil {
			t.Fatal(err)
		}
		reconcileMachine(t, e, readyOwner)
		// The object was the mirror's only user: cleanup deletes it.
		if e.rec.count(EventDestroyed) != 1 || e.rec.count(EventFinalizerRemoved) != 1 || e.rec.count(EventMirrorRemoved) != 1 || e.rec.count(EventMirrorCreated) != 0 {
			t.Errorf("events = %v", e.rec.reasons)
		}
	})
	t.Run("runner events flags follow --runner-events", func(t *testing.T) {
		t.Parallel()
		for _, on := range []bool{true, false} {
			e := newEnv(t, world(machine(withFinalizer, notPaused))...)
			e.d.RunnerEvents = on
			reconcileMachine(t, e, readyOwner)
			if len(e.runner.jobs) != 1 {
				t.Fatalf("jobs = %v", e.runner.created)
			}
			args := sourceArgs(&e.runner.jobs[0])
			obj := "--event-object=" + infrav1.GroupVersion.String() + "/TerraformMachine/" + testNS + "/" + testName + "/m1-uid"
			jobName := "--job-name=" + e.runner.jobs[0].Name
			if got := slices.Contains(args, obj) && slices.Contains(args, jobName); got != on {
				t.Errorf("--runner-events=%v: args %v", on, args)
			}
			if !on && slices.ContainsFunc(args, func(a string) bool {
				return strings.HasPrefix(a, "--event-object") || strings.HasPrefix(a, "--job-name")
			}) {
				t.Errorf("--runner-events=false passes event flags: %v", args)
			}
		}
	})
	t.Run("deletion without state removes the finalizer", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused))...)
		reconcileMachine(t, e, readyOwner)
		if e.rec.count(EventDestroyed) != 0 || e.rec.count(EventFinalizerRemoved) != 1 {
			t.Errorf("events = %v", e.rec.reasons)
		}
	})
	t.Run("pause and resume", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(withFinalizer, notPaused))...)
		paused := OwnerInfo{HasOwnerRef: true, Cluster: cluster(true)}
		reconcileMachine(t, e, paused)
		reconcileMachine(t, e, paused)
		if n, m := e.rec.count(EventPaused), e.rec.count(EventResumed); n != 1 || m != 0 {
			t.Errorf("after pausing: %d Paused, %d Resumed (%v)", n, m, e.rec.reasons)
		}
		reconcileMachine(t, e, readyOwner)
		reconcileMachine(t, e, readyOwner)
		if n, m := e.rec.count(EventPaused), e.rec.count(EventResumed); n != 1 || m != 1 {
			t.Errorf("after resuming: %d Paused, %d Resumed (%v)", n, m, e.rec.reasons)
		}
	})
	t.Run("a first visit that is not paused is quiet", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(withFinalizer))...)
		reconcileMachine(t, e, readyOwner)
		if e.rec.count(EventPaused)+e.rec.count(EventResumed) != 0 {
			t.Errorf("events = %v", e.rec.reasons)
		}
	})
	t.Run("stuck Job deleted", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(withFinalizer, notPaused))...)
		e.runner.jobs = append(e.runner.jobs, job("stuck", jobs.OpApply, jobs.Running, t0.Add(-time.Minute)))
		reconcileMachine(t, e, readyOwner)
		if got := e.rec.only(EventStuckJobDeleted); len(got) != 1 || got[0].eventType != warning || !strings.Contains(got[0].note, "stuck") {
			t.Errorf("StuckJobDeleted = %+v", got)
		}
	})
	t.Run("inputs changed", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(withFinalizer, notPaused))...)
		e.state.st = &state.State{InputsHash: "h1:old"}
		mutable := func(k *fakeKind) { k.mutable = true }
		reconcileMachine(t, e, readyOwner, mutable)
		reconcileMachine(t, e, readyOwner, mutable) // the Job runs: nothing new
		if got := e.rec.only(EventInputsChanged); len(got) != 1 || !strings.Contains(got[0].note, "h1:old") {
			t.Errorf("InputsChanged = %+v", got)
		}
		if n := e.rec.count(EventJobCreated); n != 1 {
			t.Errorf("JobCreated = %d", n)
		}
	})
	t.Run("state adopted once", func(t *testing.T) {
		t.Parallel()
		suffix, err := state.Suffix(testNS, state.KindTerraformMachine, testName)
		if err != nil {
			t.Fatal(err)
		}
		base := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: state.SecretName(suffix), Labels: map[string]string{
			state.BackendStateLabel: "true", state.BackendSuffixLabel: suffix, state.BackendWorkspaceLabel: state.Workspace,
		}}}
		// The apply started, so InfrastructureHealthy is Provisioning.
		provisioning := func(m *infrav1.TerraformMachine) {
			m.Status.Conditions = append(m.Status.Conditions, *cond(infrav1.InfrastructureHealthyCondition, cFalse, infrav1.ProvisioningReason, ""))
		}
		e := newEnv(t, world(machine(withFinalizer, notPaused, provisioning), base)...)
		applied := job("a", jobs.OpApply, jobs.Succeeded, t0)
		applied.Annotations = map[string]string{state.InputsHashAnnotation: "h1:applied"}
		e.runner.jobs = append(e.runner.jobs, applied)
		e.state.st = &state.State{Serial: 7}
		healthy := func(k *fakeKind) { k.health = &contract.Health{State: contract.HealthRunning, Healthy: true} }
		reconcileMachine(t, e, readyOwner, healthy)
		reconcileMachine(t, e, readyOwner, healthy)
		if got := e.rec.only(EventStateAdopted); len(got) != 1 || !strings.Contains(got[0].note, "h1:applied") {
			t.Errorf("StateAdopted = %+v", got)
		}
		for _, r := range []string{EventProvisioned, EventInstanceHealthy, EventJobSucceeded} {
			if n := e.rec.count(r); n != 1 {
				t.Errorf("%s = %d, want 1 (%v)", r, n, e.rec.reasons)
			}
		}
	})
	t.Run("mirror created once, removed when the identity stops allowing", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(withFinalizer, notPaused))...)
		reconcileMachine(t, e, readyOwner)
		reconcileMachine(t, e, readyOwner)
		if n := e.rec.count(EventMirrorCreated); n != 1 {
			t.Errorf("MirrorCreated = %d (%v)", n, e.rec.reasons)
		}
		id := &infrav1.TerraformClusterIdentity{}
		if err := e.c.Get(t.Context(), client.ObjectKey{Name: testIdentity}, id); err != nil {
			t.Fatal(err)
		}
		id.Spec.AllowedNamespaces = nil
		if err := e.c.Update(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		// Credentials are only looked at while no Job runs.
		for i := range e.runner.jobs {
			e.runner.jobs[i].Status.Conditions = []batchv1.JobCondition{{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(t0),
			}}
		}
		reconcileMachine(t, e, readyOwner)
		reconcileMachine(t, e, readyOwner)
		if n, m := e.rec.count(EventMirrorRemoved), e.rec.count(EventIdentityNotAllowed); n != 1 || m != 1 {
			t.Errorf("MirrorRemoved = %d, IdentityNotAllowed = %d (%v)", n, m, e.rec.reasons)
		}
	})
	t.Run("an unchanged reconcile emits nothing", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(withFinalizer, notPaused))...)
		e.runner.jobs = append(e.runner.jobs, job("running", jobs.OpApply, jobs.Running, t0))
		e.runner.pods["running"] = []corev1.Pod{{Status: corev1.PodStatus{Phase: corev1.PodRunning}}}
		reconcileMachine(t, e, readyOwner)
		n := len(e.rec.reasons)
		for range 3 {
			reconcileMachine(t, e, readyOwner)
		}
		if len(e.rec.reasons) != n {
			t.Errorf("repeated reconciles emitted %v", e.rec.reasons[n:])
		}
	})
}
