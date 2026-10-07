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
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
)

// t0 is the fixed instant every test in this package treats as "now".
var t0 = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

// destructivePlan returns the live Destructive plan "d" of inputs, approved
// when ok.
func destructivePlan(inputs string, ok bool) *PlanView {
	return &PlanView{Name: "d", Reason: infrav1.PlanReasonDestructive, InputsHash: inputs, PlanHash: "p2:d", Approved: ok}
}

// exportsPlan returns the live ExportsChange plan "x" of the approval hash
// approval, approved when ok.
func exportsPlan(approval string, ok bool) *PlanView {
	return &PlanView{Name: "x", Reason: infrav1.PlanReasonExportsChange, InputsHash: approval, PlanHash: "p2:x", Approved: ok}
}

// at returns a pointer to t0 plus d.
func at(d time.Duration) *time.Time {
	t := t0.Add(d)
	return &t
}

// TestDecideOp runs DecideOp through its priority order (destroy, apply,
// refresh, drift), the backoffs on each, drift remediation, and destructive
// plan blocking and approval, table by table, and checks the returned
// Decision.
func TestDecideOp(t *testing.T) {
	t.Parallel()
	applied := StateView{Exists: true, InputsHash: "h1:a"}
	failedApply := func(n int, ago time.Duration) JobsView {
		return JobsView{
			Failures:    map[jobs.Op]int{jobs.OpApply: n},
			LastFailure: map[jobs.Op]time.Time{jobs.OpApply: t0.Add(-ago)},
			FailedLimit: 3,
		}
	}
	tests := []struct {
		name string
		in   DecideInput
		want Decision
	}{
		{"job active", DecideInput{Deleting: true, Jobs: JobsView{Active: true}},
			Decision{RequeueAfter: ActiveJobRequeue, Reason: "JobActive"}},
		{"deleting without state drops the finalizer", DecideInput{Deleting: true},
			Decision{Action: ActionDropFinalizer, Reason: "DeletingWithoutState"}},
		{"deleting with state destroys", DecideInput{Deleting: true, State: applied},
			Decision{Action: ActionJob, Op: jobs.OpDestroy, Reason: "Deleting"}},
		{"deleting with a lost state holds", DecideInput{Deleting: true, StateHeld: true},
			Decision{RequeueAfter: StateRequeue, Reason: ReasonDeletionHeld}},
		{"deleting with an unreadable state holds", DecideInput{Deleting: true, StateHeld: true, State: StateView{Exists: true}},
			Decision{RequeueAfter: StateRequeue, Reason: ReasonDeletionHeld}},
		{"a held deletion ignores the destroy backoff", DecideInput{Deleting: true, StateHeld: true, State: applied, Now: t0, Jobs: JobsView{
			Failures: map[jobs.Op]int{jobs.OpDestroy: 1}, LastFailure: map[jobs.Op]time.Time{jobs.OpDestroy: t0}}},
			Decision{RequeueAfter: StateRequeue, Reason: ReasonDeletionHeld}},
		{"deleting wins over a hash change", DecideInput{Deleting: true, Mutable: true, State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:b"}},
			Decision{Action: ActionJob, Op: jobs.OpDestroy, Reason: "Deleting"}},
		{"failed destroy waits for its backoff", DecideInput{Deleting: true, State: applied, Now: t0, Jobs: JobsView{
			Failures: map[jobs.Op]int{jobs.OpDestroy: 1}, LastFailure: map[jobs.Op]time.Time{jobs.OpDestroy: t0.Add(-20 * time.Second)}}},
			Decision{RequeueAfter: 40 * time.Second, Reason: "DeletingBackoff"}},
		{"no state applies", DecideInput{},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "NoState"}},
		{"state without hash on an immutable machine applies", DecideInput{State: StateView{Exists: true}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "StateWithoutInputsHash"}},
		{"state without hash after a failure backs off", DecideInput{State: StateView{Exists: true}, Now: t0, Jobs: failedApply(2, 30*time.Second)},
			Decision{RequeueAfter: 90 * time.Second, Reason: "StateWithoutInputsHashBackoff"}},
		{"backoff elapsed retries", DecideInput{State: StateView{Exists: true}, Now: t0, Jobs: failedApply(2, 3*time.Minute)},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "StateWithoutInputsHash"}},
		{"mutable with changed inputs applies", DecideInput{Mutable: true, State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:b"}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged"}},
		{"mutable, inputs not buildable: no apply", DecideInput{Mutable: true, State: applied},
			Decision{Reason: "UpToDate"}},
		{"immutable ignores a hash difference", DecideInput{State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:b"}},
			Decision{Reason: "UpToDate"}},
		{"pending health refreshes when never refreshed", DecideInput{State: StateView{Exists: true, InputsHash: "h1:a", Pending: true}, Now: t0},
			Decision{Action: ActionJob, Op: jobs.OpRefresh, Reason: "HealthPending"}},
		{"pending health waits 30s between refreshes", DecideInput{State: StateView{Exists: true, InputsHash: "h1:a", Pending: true}, Now: t0, LastRefresh: at(-10 * time.Second)},
			Decision{RequeueAfter: 20 * time.Second, Reason: "UpToDate"}},
		{"pending refresh deadline beats a later drift", DecideInput{State: StateView{Exists: true, InputsHash: "h1:a", Pending: true}, Now: t0,
			LastRefresh: at(-10 * time.Second), LastDriftCheck: at(-time.Minute), DriftInterval: 30 * time.Minute},
			Decision{RequeueAfter: 20 * time.Second, Reason: "UpToDate"}},
		{"drift never checked is due", DecideInput{State: applied, DriftInterval: 30 * time.Minute, Now: t0},
			Decision{Action: ActionJob, Op: jobs.OpDrift, Reason: "DriftDue"}},
		{"drift due after the interval", DecideInput{State: applied, DriftInterval: 30 * time.Minute, Now: t0, LastDriftCheck: at(-31 * time.Minute)},
			Decision{Action: ActionJob, Op: jobs.OpDrift, Reason: "DriftDue"}},
		{"drift not due requeues at the deadline", DecideInput{State: applied, DriftInterval: 30 * time.Minute, Now: t0, LastDriftCheck: at(-10 * time.Minute)},
			Decision{RequeueAfter: 20 * time.Minute, Reason: "UpToDate"}},
		{"drift interval 0 never checks", DecideInput{State: applied, Now: t0},
			Decision{Reason: "UpToDate"}},
		// The first drift check comes one interval after the successful
		// apply, not right after it (item 10).
		{"first drift waits an interval after the apply", DecideInput{State: applied, DriftInterval: 30 * time.Minute, Now: t0,
			LastApplySucceeded: at(-10 * time.Minute)},
			Decision{RequeueAfter: 20 * time.Minute, Reason: "UpToDate"}},
		{"first drift due an interval after the apply", DecideInput{State: applied, DriftInterval: 30 * time.Minute, Now: t0,
			LastApplySucceeded: at(-30 * time.Minute)},
			Decision{Action: ActionJob, Op: jobs.OpDrift, Reason: "DriftDue"}},
		{"without an apply, drift counts from creation", DecideInput{State: applied, DriftInterval: 30 * time.Minute, Now: t0,
			Created: t0.Add(-5 * time.Minute)},
			Decision{RequeueAfter: 25 * time.Minute, Reason: "UpToDate"}},
		{"drift deadline carries the object's jitter", DecideInput{State: applied, DriftInterval: 30 * time.Minute, Now: t0,
			LastDriftCheck: at(-10 * time.Minute), UID: "uid-1"},
			Decision{RequeueAfter: 20*time.Minute + Jitter("uid-1", 30*time.Minute), Reason: "UpToDate"}},
		// Health checks independent of drift (item 2).
		{"health check due refreshes with drift off", DecideInput{State: applied, Now: t0, HealthInterval: 5 * time.Minute,
			LastRefresh: at(-6 * time.Minute)},
			Decision{Action: ActionJob, Op: jobs.OpRefresh, Reason: "HealthCheckDue"}},
		{"health check not due requeues at its deadline", DecideInput{State: applied, Now: t0, HealthInterval: 5 * time.Minute,
			LastRefresh: at(-2 * time.Minute), DriftInterval: 30 * time.Minute, LastDriftCheck: at(-time.Minute)},
			Decision{RequeueAfter: 3 * time.Minute, Reason: "UpToDate"}},
		{"pending health keeps the 30s cadence over the health interval", DecideInput{State: StateView{Exists: true, InputsHash: "h1:a", Pending: true},
			Now: t0, HealthInterval: 5 * time.Minute, LastRefresh: at(-10 * time.Second)},
			Decision{RequeueAfter: 20 * time.Second, Reason: "UpToDate"}},
		// Newest apply failed: not converged, even with an equal hash (item 4).
		{"a failed apply is retried although the inputs equal the state's", DecideInput{Mutable: true, Now: t0,
			State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:a"},
			Jobs:  JobsView{LastApplyFailed: true, Failures: map[jobs.Op]int{jobs.OpApply: 1}, LastFailure: map[jobs.Op]time.Time{jobs.OpApply: t0.Add(-2 * time.Minute)}, FailedLimit: 3}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "LastApplyFailed"}},
		{"the retry of a failed apply waits for its backoff", DecideInput{Mutable: true, Now: t0,
			State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:a"},
			Jobs:  JobsView{LastApplyFailed: true, Failures: map[jobs.Op]int{jobs.OpApply: 2}, LastFailure: map[jobs.Op]time.Time{jobs.OpApply: t0.Add(-30 * time.Second)}, FailedLimit: 3}},
			Decision{RequeueAfter: 90 * time.Second, Reason: "LastApplyFailedBackoff"}},
		{"an immutable kind never re-applies after a failure", DecideInput{Now: t0, State: applied, Jobs: JobsView{LastApplyFailed: true}},
			Decision{Reason: "UpToDate"}},
		{"refresh right after a successful apply", DecideInput{State: applied, Now: t0, RefreshAfterApply: true, LastApplySucceeded: at(-time.Minute)},
			Decision{Action: ActionJob, Op: jobs.OpRefresh, Reason: "RefreshAfterApply"}},
		{"already refreshed since the apply", DecideInput{State: applied, Now: t0, RefreshAfterApply: true, LastApplySucceeded: at(-time.Minute), LastRefresh: at(-10 * time.Second)},
			Decision{Reason: "UpToDate"}},
		{"refresh after apply not requested", DecideInput{State: applied, Now: t0, LastApplySucceeded: at(-time.Minute)},
			Decision{Reason: "UpToDate"}},
		{"failed drift backs off", DecideInput{State: applied, DriftInterval: 30 * time.Minute, Now: t0, LastDriftCheck: at(-31 * time.Minute), Jobs: JobsView{
			Failures: map[jobs.Op]int{jobs.OpDrift: 1}, LastFailure: map[jobs.Op]time.Time{jobs.OpDrift: t0.Add(-10 * time.Second)}}},
			Decision{RequeueAfter: 50 * time.Second, Reason: "DriftDueBackoff"}},
		// Drift remediation.
		{"drift found with Remediate applies", DecideInput{Mutable: true, State: applied, Now: t0, Remediate: true, Jobs: JobsView{FailedLimit: 3}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "DriftRemediation"}},
		{"an inputs change outranks remediation", DecideInput{Mutable: true, State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:b"}, Now: t0, Remediate: true},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged"}},
		{"remediation outranks a pending refresh and a due drift", DecideInput{Mutable: true, State: StateView{Exists: true, InputsHash: "h1:a", Pending: true},
			Now: t0, DriftInterval: 30 * time.Minute, Remediate: true},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "DriftRemediation"}},
		{"a failed remediation backs off", DecideInput{Mutable: true, State: applied, Now: t0, Remediate: true, Jobs: JobsView{
			FailedLimit: 3, RemediationFailures: 1, Failures: map[jobs.Op]int{jobs.OpApply: 1}, LastFailure: map[jobs.Op]time.Time{jobs.OpApply: t0.Add(-15 * time.Second)}}},
			Decision{RequeueAfter: 45 * time.Second, Reason: "DriftRemediationBackoff"}},
		{"after FailedLimit remediations the drift stays pending until the next check", DecideInput{Mutable: true, State: applied, Now: t0,
			Remediate: true, DriftInterval: 30 * time.Minute, LastDriftCheck: at(-10 * time.Minute), Jobs: JobsView{
				FailedLimit: 3, RemediationFailures: 3, Failures: map[jobs.Op]int{jobs.OpApply: 3}, LastFailure: map[jobs.Op]time.Time{jobs.OpApply: t0.Add(-time.Minute)}}},
			Decision{RequeueAfter: 20 * time.Minute, Reason: "UpToDate"}},
		// A blocked destructive plan: an apply the runner stopped before a
		// plan that deletes or replaces resources, which waits for approval
		// as a TerraformPlan.
		{"a blocked input change waits for its plan's approval, not in a loop", DecideInput{Mutable: true, Now: t0, Plan: destructivePlan("h1:b", false),
			State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:b"}, Jobs: JobsView{BlockedHash: "h1:b"}},
			Decision{RequeueAfter: RetryMax, Reason: ReasonDestructivePlanBlocked, Plan: "d"}},
		{"the plan waits with no blocked Job left (after a move)", DecideInput{Mutable: true, Now: t0, Plan: destructivePlan("h1:b", false),
			State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:b"}},
			Decision{RequeueAfter: RetryMax, Reason: ReasonDestructivePlanBlocked, Plan: "d"}},
		{"the approved plan applies, expecting exactly it", DecideInput{Mutable: true, Now: t0, Plan: destructivePlan("h1:b", true),
			State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:b"}, Jobs: JobsView{BlockedHash: "h1:b"}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged", ExpectPlan: "p2:d", Plan: "d"}},
		{"new inputs after a block apply (guarded again by the runner)", DecideInput{Mutable: true, Now: t0, Plan: destructivePlan("h1:b", true),
			State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:c"}, Jobs: JobsView{BlockedHash: "h1:b"}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged"}},
		{"a blocked apply that left no plan runs again RetryMax after the block", DecideInput{Mutable: true, Now: t0,
			State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:b"}, Jobs: JobsView{BlockedHash: "h1:b", BlockedAt: t0.Add(-4 * time.Minute)}},
			Decision{RequeueAfter: RetryMax - 4*time.Minute, Reason: ReasonDestructivePlanBlocked}},
		{"and then runs", DecideInput{Mutable: true, Now: t0,
			State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:b"}, Jobs: JobsView{BlockedHash: "h1:b", BlockedAt: t0.Add(-RetryMax)}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged"}},
		{"a blocked remediation waits", DecideInput{Mutable: true, Now: t0, Remediate: true, Plan: destructivePlan("h1:a", false),
			State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:a"}, Jobs: JobsView{FailedLimit: 3, BlockedHash: "h1:a"}},
			Decision{RequeueAfter: RetryMax, Reason: ReasonDestructivePlanBlocked, Plan: "d"}},
		{"an approved remediation applies", DecideInput{Mutable: true, Now: t0, Remediate: true, Plan: destructivePlan("h1:a", true),
			State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:a"}, Jobs: JobsView{FailedLimit: 3, BlockedHash: "h1:a"}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "DriftRemediation", ExpectPlan: "p2:d", Plan: "d"}},
		{"a blocked remediation keeps the drift schedule", DecideInput{Mutable: true, Now: t0, Remediate: true, DriftInterval: 30 * time.Minute, LastDriftCheck: at(-31 * time.Minute),
			Plan: destructivePlan("h1:a", false), State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:a"}, Jobs: JobsView{FailedLimit: 3, BlockedHash: "h1:a"}},
			Decision{Action: ActionJob, Op: jobs.OpDrift, Reason: "DriftDue", Plan: "d"}},
		{"a blocked remediation requeues at a sooner check", DecideInput{Mutable: true, Now: t0, Remediate: true, DriftInterval: 30 * time.Minute, LastDriftCheck: at(-25 * time.Minute),
			Plan: destructivePlan("h1:a", false), State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:a"}, Jobs: JobsView{FailedLimit: 3, BlockedHash: "h1:a"}},
			Decision{RequeueAfter: 5 * time.Minute, Reason: ReasonDestructivePlanBlocked, Plan: "d"}},
		{"a blocked input change pauses a due drift check", DecideInput{Mutable: true, Now: t0, DriftInterval: 30 * time.Minute, LastDriftCheck: at(-31 * time.Minute),
			Plan: destructivePlan("h1:b", false), State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:b"}, Jobs: JobsView{BlockedHash: "h1:b"}},
			Decision{RequeueAfter: RetryMax, Reason: ReasonDestructivePlanBlocked, Plan: "d"}},
		{"a pool's plan is made for its approval hash, not the inputs hash", DecideInput{Mutable: true, Now: t0, ApprovalHash: "h2:approval", Plan: exportsPlan("h2:approval", false),
			State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:b"}, Jobs: JobsView{BlockedHash: "h1:b"}},
			Decision{RequeueAfter: RetryMax, Reason: ReasonDestructivePlanBlocked, Plan: "x"}},
		{"a pool's approved change allows its deletes under the approval hash", DecideInput{Mutable: true, Now: t0, ApprovalHash: "h2:approval", Plan: exportsPlan("h2:approval", true),
			State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:b"}, Jobs: JobsView{BlockedHash: "h1:b"}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged", AllowDeletes: "h2:approval", Plan: "x"}},
		{"a held pool applies past the pending plan", DecideInput{Mutable: true, Now: t0, Plan: exportsPlan("h2:approval", false),
			State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:held"}, Jobs: JobsView{BlockedHash: "h1:b", BlockedApproval: "h2:approval"}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged"}},
		{"a pool that cannot hold waits on its approval hash across a rotation", DecideInput{Mutable: true, Now: t0, ApprovalHash: "h2:approval", Unheld: true,
			Plan: exportsPlan("h2:approval", false), State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:rotated"},
			Jobs: JobsView{BlockedHash: "h1:b", BlockedApproval: "h2:approval"}},
			Decision{RequeueAfter: RetryMax, Reason: ReasonDestructivePlanBlocked, Plan: "x"}},
		{"a pool's new approval hash applies, guarded again", DecideInput{Mutable: true, Now: t0, ApprovalHash: "h2:rolled", Unheld: true,
			Plan: exportsPlan("h2:approval", false), State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:c"},
			Jobs: JobsView{BlockedHash: "h1:b", BlockedApproval: "h2:approval"}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged"}},
		{"a pool's approved plan applies across a rotation", DecideInput{Mutable: true, Now: t0, ApprovalHash: "h2:approval", Unheld: true,
			Plan: exportsPlan("h2:approval", true), State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:rotated"},
			Jobs: JobsView{BlockedHash: "h1:b", BlockedApproval: "h2:approval"}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged", AllowDeletes: "h2:approval", Plan: "x"}},
		{"a pool that can hold re-runs a blocked change after a rotation, to hold it again", DecideInput{Mutable: true, Now: t0, ApprovalHash: "h2:approval",
			State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:rotated"}, Jobs: JobsView{BlockedHash: "h1:b", BlockedApproval: "h2:approval"}},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged"}},
		{"a blocked apply without state checks nothing", DecideInput{Mutable: true, Now: t0, DriftInterval: 30 * time.Minute, Plan: destructivePlan("h1:b", false),
			State: StateView{CurrentHash: "h1:b"}, Jobs: JobsView{BlockedHash: "h1:b"}},
			Decision{RequeueAfter: RetryMax, Reason: ReasonDestructivePlanBlocked, Plan: "d"}},
		// Priority: destroy > apply > refresh > drift.
		{"destroy wins over a pending refresh", DecideInput{Deleting: true, State: StateView{Exists: true, InputsHash: "h1:a", Pending: true}, Now: t0},
			Decision{Action: ActionJob, Op: jobs.OpDestroy, Reason: "Deleting"}},
		{"apply wins over a pending refresh", DecideInput{Mutable: true, State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:b", Pending: true}, Now: t0},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged"}},
		{"pending refresh wins over a due drift", DecideInput{State: StateView{Exists: true, InputsHash: "h1:a", Pending: true}, Now: t0, DriftInterval: 30 * time.Minute},
			Decision{Action: ActionJob, Op: jobs.OpRefresh, Reason: "HealthPending"}},
		{"refresh after apply wins over a due drift", DecideInput{State: applied, Now: t0, DriftInterval: 30 * time.Minute,
			RefreshAfterApply: true, LastApplySucceeded: at(-time.Minute)},
			Decision{Action: ActionJob, Op: jobs.OpRefresh, Reason: "RefreshAfterApply"}},
		{"a sooner drift deadline beats the pending cadence", DecideInput{State: StateView{Exists: true, InputsHash: "h1:a", Pending: true}, Now: t0,
			LastRefresh: at(-10 * time.Second), LastDriftCheck: at(-(30*time.Minute - 5*time.Second)), DriftInterval: 30 * time.Minute},
			Decision{RequeueAfter: 5 * time.Second, Reason: "UpToDate"}},
		// Membership (TerraformMachinePool).
		{"converging refreshes once 30s passed", DecideInput{Mutable: true, State: applied, Now: t0, Converging: true, LastRefresh: at(-31 * time.Second)},
			Decision{Action: ActionJob, Op: jobs.OpRefresh, Reason: "MembershipConverging"}},
		{"converging never refreshed refreshes now", DecideInput{Mutable: true, State: applied, Now: t0, Converging: true},
			Decision{Action: ActionJob, Op: jobs.OpRefresh, Reason: "MembershipConverging"}},
		{"converging not due requeues at its deadline", DecideInput{Mutable: true, State: applied, Now: t0, Converging: true, LastRefresh: at(-10 * time.Second),
			MembershipInterval: time.Minute, DriftInterval: 30 * time.Minute, LastDriftCheck: at(-time.Minute)},
			Decision{RequeueAfter: 20 * time.Second, Reason: "UpToDate"}},
		{"converging carries the object's jitter", DecideInput{Mutable: true, State: applied, Now: t0, Converging: true, LastRefresh: at(-10 * time.Second), UID: "uid-1"},
			Decision{RequeueAfter: 20*time.Second + Jitter("uid-1", MembershipConvergingInterval), Reason: "UpToDate"}},
		{"converging replaces the doubled pending delay", DecideInput{Mutable: true, State: StateView{Exists: true, InputsHash: "h1:a", Pending: true}, Now: t0,
			Converging: true, PendingRefreshes: 5, LastRefresh: at(-31 * time.Second)},
			Decision{Action: ActionJob, Op: jobs.OpRefresh, Reason: "MembershipConverging"}},
		{"without converging the pending delay doubles", DecideInput{Mutable: true, State: StateView{Exists: true, InputsHash: "h1:a", Pending: true}, Now: t0,
			PendingRefreshes: 5, LastRefresh: at(-31 * time.Second)},
			Decision{RequeueAfter: 5*time.Minute - 31*time.Second, Reason: "UpToDate"}},
		{"refresh after apply outranks converging", DecideInput{Mutable: true, State: applied, Now: t0, Converging: true,
			RefreshAfterApply: true, LastApplySucceeded: at(-time.Minute), LastRefresh: at(-2 * time.Minute)},
			Decision{Action: ActionJob, Op: jobs.OpRefresh, Reason: "RefreshAfterApply"}},
		{"a failed refresh backs off while converging", DecideInput{Mutable: true, State: applied, Now: t0, Converging: true, LastRefresh: at(-time.Minute),
			Jobs: JobsView{Failures: map[jobs.Op]int{jobs.OpRefresh: 1}, LastFailure: map[jobs.Op]time.Time{jobs.OpRefresh: t0.Add(-20 * time.Second)}}},
			Decision{RequeueAfter: 40 * time.Second, Reason: "MembershipConvergingBackoff"}},
		{"membership refresh due", DecideInput{Mutable: true, State: applied, Now: t0, MembershipInterval: time.Minute, LastRefresh: at(-61 * time.Second)},
			Decision{Action: ActionJob, Op: jobs.OpRefresh, Reason: "MembershipRefreshDue"}},
		{"membership refresh not due requeues at its deadline", DecideInput{Mutable: true, State: applied, Now: t0, MembershipInterval: time.Minute,
			LastRefresh: at(-15 * time.Second), DriftInterval: 30 * time.Minute, LastDriftCheck: at(-time.Minute)},
			Decision{RequeueAfter: 45 * time.Second, Reason: "UpToDate"}},
		{"membership counts from the apply when never refreshed", DecideInput{Mutable: true, State: applied, Now: t0, MembershipInterval: time.Minute,
			LastApplySucceeded: at(-20 * time.Second)},
			Decision{RequeueAfter: 40 * time.Second, Reason: "UpToDate"}},
		{"membership counts from creation without an apply", DecideInput{Mutable: true, State: applied, Now: t0, MembershipInterval: time.Minute,
			Created: t0.Add(-2 * time.Minute)},
			Decision{Action: ActionJob, Op: jobs.OpRefresh, Reason: "MembershipRefreshDue"}},
		{"membership refresh outranks a due drift", DecideInput{Mutable: true, State: applied, Now: t0, MembershipInterval: time.Minute,
			LastRefresh: at(-2 * time.Minute), DriftInterval: 30 * time.Minute},
			Decision{Action: ActionJob, Op: jobs.OpRefresh, Reason: "MembershipRefreshDue"}},
		{"membership interval 0 is off", DecideInput{Mutable: true, State: applied, Now: t0, LastRefresh: at(-time.Hour)},
			Decision{Reason: "UpToDate"}},
		{"an input change outranks converging", DecideInput{Mutable: true, State: StateView{Exists: true, InputsHash: "h1:a", CurrentHash: "h1:b"}, Now: t0,
			Converging: true, MembershipInterval: time.Minute},
			Decision{Action: ActionJob, Op: jobs.OpApply, Reason: "InputsChanged"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := DecideOp(tt.in); got != tt.want {
				t.Errorf("DecideOp = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestPendingRefreshDelay: the pending refresh backs off 30s, 1m, 2m, 4m
// and stays at 5m, per consecutive pending sample.
func TestPendingRefreshDelay(t *testing.T) {
	t.Parallel()
	for n, want := range []time.Duration{
		30 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute,
	} {
		if got := PendingRefreshDelay(n); got != want {
			t.Errorf("PendingRefreshDelay(%d) = %s, want %s", n, got, want)
		}
	}
	if got := PendingRefreshDelay(1000); got != RefreshPendingCeiling {
		t.Errorf("PendingRefreshDelay(1000) = %s", got)
	}
}

// TestDecidePendingBackoff: DecideOp waits PendingRefreshDelay(n) plus the
// object's jitter (at most a tenth, as for drift) after the last refresh
// while health is pending.
func TestDecidePendingBackoff(t *testing.T) {
	t.Parallel()
	pending := StateView{Exists: true, InputsHash: "h1:a", Pending: true}
	at := func(d time.Duration) *time.Time { v := t0.Add(d); return &v }
	for n, delay := range []time.Duration{30 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		for _, uid := range []string{"", "uid-1", "uid-2"} {
			wait := delay + Jitter(uid, delay)
			if j := Jitter(uid, delay); j < 0 || j > delay/10 {
				t.Fatalf("jitter %s of %s exceeds a tenth", j, delay)
			}
			in := DecideInput{State: pending, Now: t0, UID: uid, PendingRefreshes: n, LastRefresh: at(-time.Second)}
			if got := DecideOp(in); got.Action != ActionNone || got.RequeueAfter != wait-time.Second {
				t.Errorf("n=%d uid=%q just refreshed: %+v, want requeue %s", n, uid, got, wait-time.Second)
			}
			in.LastRefresh = at(-wait)
			if got := DecideOp(in); got.Action != ActionJob || got.Op != jobs.OpRefresh || got.Reason != "HealthPending" {
				t.Errorf("n=%d uid=%q at the deadline: %+v, want a HealthPending refresh", n, uid, got)
			}
		}
	}
}

// TestRetryDelay proves RetryDelay doubles per failure count up to
// RetryMax, saturating once n reaches limit.
func TestRetryDelay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		n, limit int
		want     time.Duration
	}{
		{1, 3, time.Minute},
		{2, 3, 2 * time.Minute},
		{3, 3, RetryMax}, // saturated history
		{3, 10, 4 * time.Minute},
		{5, 10, RetryMax}, // 16m capped
		{4, 0, 8 * time.Minute},
	}
	for _, tt := range tests {
		if got := RetryDelay(tt.n, tt.limit); got != tt.want {
			t.Errorf("RetryDelay(%d, %d) = %s, want %s", tt.n, tt.limit, got, tt.want)
		}
	}
}

// TestProvisioned proves Provisioned is true only once an inputs hash
// exists, outputs are valid and health reads a definite (non-pending,
// non-null) state, and stays true once latched regardless of prev's other
// inputs.
func TestProvisioned(t *testing.T) {
	t.Parallel()
	running := &contract.Health{State: contract.HealthRunning, Healthy: true}
	tests := []struct {
		name                string
		prev, hash, outputs bool
		health              *contract.Health
		want                bool
	}{
		{"all conditions hold", false, true, true, running, true},
		{"unhealthy but not pending still counts", false, true, true, &contract.Health{State: contract.HealthStopped}, true},
		{"no inputs hash (apply never completed)", false, false, true, running, false},
		{"outputs invalid", false, true, false, running, false},
		{"health pending", false, true, true, &contract.Health{State: contract.HealthPending}, false},
		{"health null", false, true, true, nil, false},
		{"latched: stays true whatever state says", true, false, false, nil, true},
	}
	for _, tt := range tests {
		if got := Provisioned(tt.prev, tt.hash, tt.outputs, tt.health); got != tt.want {
			t.Errorf("%s: Provisioned = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestResolveMachine proves Resolve inherits identity, Jobs and drift from
// the cluster's spec.defaults for a machine, returns copies rather than
// aliasing the cluster's policies, lets a machine's own JobPolicy and drift
// win over the defaults, falls back to the cluster's own identity when
// defaults.identityRef is unset, and returns built-in defaults with no
// cluster.
func TestResolveMachine(t *testing.T) {
	t.Parallel()
	cluster := &infrav1.TerraformCluster{Spec: infrav1.TerraformClusterSpec{
		WorkspaceSpec: infrav1.WorkspaceSpec{
			IdentityRef: infrav1.IdentityReference{Name: "cluster-own"},
		},
		Defaults: &infrav1.TerraformClusterDefaults{
			IdentityRef: infrav1.IdentityReference{Name: "from-defaults"},
			Jobs: &infrav1.JobPolicy{
				ServiceAccountName: "deployer",
				ImagePullSecrets:   []corev1.LocalObjectReference{{Name: "regcred"}},
				Resources:          &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}},
			},
			Drift: &infrav1.MachineDriftPolicy{IntervalSeconds: new(int32(60))},
		},
	}}

	// Everything inherited.
	e := Resolve(SpecView{InheritsDefaults: true}, cluster, 30*time.Minute, false)
	if e.IdentityName != "from-defaults" || e.Jobs.ServiceAccountName != "deployer" || e.Jobs.Resources == nil ||
		e.DriftInterval != time.Minute || e.DriftAction != infrav1.DriftActionReport {
		t.Errorf("inherited = %+v", e)
	}
	// Returned policies are copies.
	e.Jobs.ImagePullSecrets[0].Name = "mutated"
	if cluster.Spec.Defaults.Jobs.ImagePullSecrets[0].Name != "regcred" {
		t.Error("Resolve returned a shared JobPolicy")
	}

	// A machine adding one env var keeps the cluster's ServiceAccount,
	// pull secrets and resources.
	own := Resolve(SpecView{
		InheritsDefaults: true,
		WorkspaceSpec: infrav1.WorkspaceSpec{
			IdentityRef: infrav1.IdentityReference{Name: "own"},
			Jobs:        &infrav1.JobPolicy{Env: []corev1.EnvVar{{Name: "X", Value: "1"}}},
		},
		MachineDrift: &infrav1.MachineDriftPolicy{IntervalSeconds: new(int32(0))},
	}, cluster, 30*time.Minute, false)
	if own.IdentityName != "own" || own.Jobs.ServiceAccountName != "deployer" || len(own.Jobs.ImagePullSecrets) != 1 ||
		own.Jobs.Resources == nil || len(own.Jobs.Env) != 1 {
		t.Errorf("own jobs = %+v", own.Jobs)
	}
	if own.DriftInterval != 0 || own.DriftAction != infrav1.DriftActionReport {
		t.Errorf("own drift: interval %s action %s, want 0 (disabled) and Report", own.DriftInterval, own.DriftAction)
	}

	// Without defaults.identityRef a machine falls back to the cluster's own.
	noDefaultID := cluster.DeepCopy()
	noDefaultID.Spec.Defaults.IdentityRef = infrav1.IdentityReference{}
	if got := Resolve(SpecView{InheritsDefaults: true}, noDefaultID, 0, false).IdentityName; got != "cluster-own" {
		t.Errorf("fallback identity = %q, want cluster-own", got)
	}

	// No cluster yet: built-in defaults.
	def := Resolve(SpecView{InheritsDefaults: true}, nil, 30*time.Minute, false)
	if def.IdentityName != "" || def.DriftInterval != 30*time.Minute || def.DriftAction != infrav1.DriftActionReport || !reflect.DeepEqual(def.Jobs, infrav1.JobPolicy{}) {
		t.Errorf("defaults = %+v", def)
	}
}

// TestResolvePool proves Resolve, for a TerraformMachinePool (PoolDrift
// set), inherits identity, jobs and the drift interval from the cluster's
// spec.defaults like a machine, lets its own interval win, never takes an
// inherited 0 as disabled, keeps a Remediate action (a pool is mutable),
// and defaults its membership refresh interval to 60s; machines and
// clusters get none.
func TestResolvePool(t *testing.T) {
	t.Parallel()
	cluster := func(interval *int32) *infrav1.TerraformCluster {
		return &infrav1.TerraformCluster{Spec: infrav1.TerraformClusterSpec{
			Defaults: &infrav1.TerraformClusterDefaults{
				IdentityRef: infrav1.IdentityReference{Name: "from-defaults"},
				Jobs:        &infrav1.JobPolicy{ServiceAccountName: "deployer"},
				Drift:       &infrav1.MachineDriftPolicy{IntervalSeconds: interval},
			},
		}}
	}
	pool := func(d infrav1.MachinePoolDriftPolicy, membership time.Duration) SpecView {
		return SpecView{InheritsDefaults: true, PoolDrift: &d, MembershipRefreshInterval: membership}
	}
	tests := []struct {
		name       string
		spec       SpecView
		cluster    *infrav1.TerraformCluster
		mutable    bool
		interval   time.Duration
		action     infrav1.DriftAction
		membership time.Duration
	}{
		{"inherits the defaults' interval", pool(infrav1.MachinePoolDriftPolicy{}, 0), cluster(new(int32(600))), true,
			10 * time.Minute, infrav1.DriftActionReport, DefaultMembershipRefreshInterval},
		{"own interval wins", pool(infrav1.MachinePoolDriftPolicy{IntervalSeconds: 120}, 0), cluster(new(int32(600))), true,
			2 * time.Minute, infrav1.DriftActionReport, DefaultMembershipRefreshInterval},
		{"inherited 0 does not disable a pool's drift", pool(infrav1.MachinePoolDriftPolicy{}, 0), cluster(new(int32(0))), true,
			30 * time.Minute, infrav1.DriftActionReport, DefaultMembershipRefreshInterval},
		{"no cluster: the manager default", pool(infrav1.MachinePoolDriftPolicy{}, 0), nil, true,
			30 * time.Minute, infrav1.DriftActionReport, DefaultMembershipRefreshInterval},
		{"Remediate is kept", pool(infrav1.MachinePoolDriftPolicy{Action: infrav1.DriftActionRemediate}, 0), cluster(nil), true,
			30 * time.Minute, infrav1.DriftActionRemediate, DefaultMembershipRefreshInterval},
		{"own membership interval wins", pool(infrav1.MachinePoolDriftPolicy{}, 15*time.Second), cluster(nil), true,
			30 * time.Minute, infrav1.DriftActionReport, 15 * time.Second},
		{"a machine has no membership interval", SpecView{InheritsDefaults: true}, cluster(new(int32(600))), false,
			10 * time.Minute, infrav1.DriftActionReport, 0},
		{"a cluster has no membership interval", SpecView{Drift: &infrav1.DriftPolicy{Action: infrav1.DriftActionRemediate}}, cluster(nil), true,
			30 * time.Minute, infrav1.DriftActionRemediate, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := Resolve(tt.spec, tt.cluster, 30*time.Minute, tt.mutable)
			if e.DriftInterval != tt.interval || e.DriftAction != tt.action || e.MembershipRefreshInterval != tt.membership {
				t.Errorf("drift %s %s, membership %s; want %s %s, %s",
					e.DriftInterval, e.DriftAction, e.MembershipRefreshInterval, tt.interval, tt.action, tt.membership)
			}
			if tt.spec.InheritsDefaults && tt.cluster != nil && (e.IdentityName != "from-defaults" || e.Jobs.ServiceAccountName != "deployer") {
				t.Errorf("identity %q, jobs %+v: not inherited", e.IdentityName, e.Jobs)
			}
		})
	}
}

// TestResolveCluster proves Resolve never lets a TerraformCluster inherit
// its own spec.defaults (those are for machines), defaults DriftAction to
// Report, honors DriftActionRemediate only for a mutable kind, and resolves
// no identity for a cluster without its own identityRef.
func TestResolveCluster(t *testing.T) {
	t.Parallel()
	cluster := &infrav1.TerraformCluster{Spec: infrav1.TerraformClusterSpec{
		WorkspaceSpec: infrav1.WorkspaceSpec{
			IdentityRef: infrav1.IdentityReference{Name: "own"},
		},
		Defaults: &infrav1.TerraformClusterDefaults{
			IdentityRef: infrav1.IdentityReference{Name: "for-machines"},
			Jobs:        &infrav1.JobPolicy{ServiceAccountName: "machines-sa"},
			Drift:       &infrav1.MachineDriftPolicy{IntervalSeconds: new(int32(60))},
		},
	}}
	// spec.defaults are for machines only: a cluster without its own jobs
	// and drift gets the built-ins, and its own identity.
	e := Resolve(SpecView{WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: cluster.Spec.IdentityRef}}, cluster, 30*time.Minute, true)
	if e.IdentityName != "own" || e.Jobs.ServiceAccountName != "" || e.DriftInterval != 30*time.Minute {
		t.Errorf("cluster inherited its own defaults: %+v", e)
	}
	// The default action is Report: nothing is applied unasked.
	if e.DriftAction != infrav1.DriftActionReport {
		t.Errorf("default drift action = %s, want Report", e.DriftAction)
	}
	// Remediate only when asked, and only for mutable kinds.
	rem := SpecView{
		WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: cluster.Spec.IdentityRef},
		Drift:         &infrav1.DriftPolicy{IntervalSeconds: new(int32(120)), Action: infrav1.DriftActionRemediate},
	}
	if got := Resolve(rem, cluster, 0, true); got.DriftAction != infrav1.DriftActionRemediate || got.DriftInterval != 2*time.Minute {
		t.Errorf("remediate = %+v", got)
	}
	if got := Resolve(rem, cluster, 0, false); got.DriftAction != infrav1.DriftActionReport {
		t.Errorf("immutable kind action = %s, want Report", got.DriftAction)
	}
	// No identity of its own: none, not the machines' default.
	if got := Resolve(SpecView{}, cluster, 0, true).IdentityName; got != "" {
		t.Errorf("cluster without identityRef resolved %q", got)
	}
}

// TestMergeJobPolicy proves MergeJobPolicy takes own's scalars and structs
// over defaults' when own sets them (including an explicit zero value),
// unions ImagePullSecrets and Env by name (own's entries first, own's value
// winning on a shared name), falls back to defaults or an empty JobPolicy
// when own is nil or unset, and returns a value that shares no memory with
// either input.
func TestMergeJobPolicy(t *testing.T) {
	t.Parallel()
	sc := func(uid int64) *corev1.SecurityContext { return &corev1.SecurityContext{RunAsUser: new(uid)} }
	full := infrav1.JobPolicy{
		SuccessfulJobsHistoryLimit: new(int32(1)),
		FailedJobsHistoryLimit:     new(int32(2)),
		ActiveDeadlineSeconds:      600,
		ServiceAccountName:         "defaults-sa",
		LockTimeoutSeconds:         new(int32(60)),
		ImagePullSecrets:           []corev1.LocalObjectReference{{Name: "shared"}, {Name: "d-only"}},
		Resources:                  &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
		Env:                        []corev1.EnvVar{{Name: "A", Value: "defaults"}, {Name: "B", Value: "defaults"}},
		SecurityContext:            sc(1),
		PodSecurityContext:         &corev1.PodSecurityContext{FSGroup: new(int64(1))},
	}
	tests := []struct {
		name          string
		own, defaults *infrav1.JobPolicy
		want          infrav1.JobPolicy
	}{
		{name: "both nil", want: infrav1.JobPolicy{}},
		{name: "only defaults", defaults: &full, want: full},
		{name: "only own", own: &full, want: full},
		{
			name:     "zero values set by the machine win",
			own:      &infrav1.JobPolicy{SuccessfulJobsHistoryLimit: new(int32(0)), LockTimeoutSeconds: new(int32(0))},
			defaults: &full,
			want: func() infrav1.JobPolicy {
				w := *full.DeepCopy()
				w.SuccessfulJobsHistoryLimit, w.LockTimeoutSeconds = new(int32(0)), new(int32(0))
				return w
			}(),
		},
		{
			name: "every field set on both: scalars and structs from the machine, lists merged",
			own: &infrav1.JobPolicy{
				SuccessfulJobsHistoryLimit: new(int32(9)),
				FailedJobsHistoryLimit:     new(int32(8)),
				ActiveDeadlineSeconds:      7,
				ServiceAccountName:         "own-sa",
				LockTimeoutSeconds:         new(int32(6)),
				ImagePullSecrets:           []corev1.LocalObjectReference{{Name: "own-only"}, {Name: "shared"}},
				Resources:                  &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
				Env:                        []corev1.EnvVar{{Name: "B", Value: "own"}, {Name: "C", Value: "own"}},
				SecurityContext:            sc(2),
				PodSecurityContext:         &corev1.PodSecurityContext{RunAsNonRoot: new(true)},
			},
			defaults: &full,
			want: infrav1.JobPolicy{
				SuccessfulJobsHistoryLimit: new(int32(9)),
				FailedJobsHistoryLimit:     new(int32(8)),
				ActiveDeadlineSeconds:      7,
				ServiceAccountName:         "own-sa",
				LockTimeoutSeconds:         new(int32(6)),
				// Union, the machine's first, deduplicated by name.
				ImagePullSecrets: []corev1.LocalObjectReference{{Name: "own-only"}, {Name: "shared"}, {Name: "d-only"}},
				// Replaced as a whole: no requests from the defaults.
				Resources: &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
				// By name, the machine wins on B, the defaults add A.
				Env:                []corev1.EnvVar{{Name: "B", Value: "own"}, {Name: "C", Value: "own"}, {Name: "A", Value: "defaults"}},
				SecurityContext:    sc(2),
				PodSecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: new(true)},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := MergeJobPolicy(tt.own, tt.defaults)
			if !apiequality.Semantic.DeepEqual(got, tt.want) {
				t.Errorf("MergeJobPolicy =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}

	// The result shares no memory with its inputs.
	own := full.DeepCopy()
	got := MergeJobPolicy(own, &full)
	*got.LockTimeoutSeconds = 1
	got.Env[0].Value = "x"
	got.ImagePullSecrets[0].Name = "x"
	*got.SecurityContext.RunAsUser = 42
	if *own.LockTimeoutSeconds != 60 || own.Env[0].Value != "defaults" || own.ImagePullSecrets[0].Name != "shared" || *own.SecurityContext.RunAsUser != 1 {
		t.Errorf("MergeJobPolicy aliases its input: %+v", own)
	}
}

// TestMergeMachineDriftPolicy proves MergeMachineDriftPolicy returns an
// unset policy for nil, nil, falls back to the default's IntervalSeconds
// when own leaves it unset, keeps own's explicit zero (disabled), and
// returns a value that shares no memory with its default input.
func TestMergeMachineDriftPolicy(t *testing.T) {
	t.Parallel()
	d := &infrav1.MachineDriftPolicy{IntervalSeconds: new(int32(60))}
	if got := MergeMachineDriftPolicy(nil, nil); got.IntervalSeconds != nil {
		t.Errorf("nil, nil = %+v", got)
	}
	if got := MergeMachineDriftPolicy(&infrav1.MachineDriftPolicy{}, d); *got.IntervalSeconds != 60 {
		t.Errorf("unset own = %+v, want the default's 60", got)
	}
	if got := MergeMachineDriftPolicy(&infrav1.MachineDriftPolicy{IntervalSeconds: new(int32(0))}, d); *got.IntervalSeconds != 0 {
		t.Errorf("own 0 = %+v, want 0 (disabled)", got)
	}
	got := MergeMachineDriftPolicy(nil, d)
	*got.IntervalSeconds = 1
	if *d.IntervalSeconds != 60 {
		t.Error("MergeMachineDriftPolicy aliases its input")
	}
}

// TestBlockMove proves SetBlockMove and ClearBlockMove are idempotent,
// HasBlockMove reports the annotation's presence, clearing it leaves other
// annotations alone, and SetBlockMove works on an object with nil
// annotations.
func TestBlockMove(t *testing.T) {
	t.Parallel()
	obj := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"keep": "me"}}}
	if HasBlockMove(obj) || ClearBlockMove(obj) {
		t.Fatal("fresh object has block-move")
	}
	if !SetBlockMove(obj) || SetBlockMove(obj) || !HasBlockMove(obj) {
		t.Fatal("SetBlockMove not idempotent")
	}
	if !ClearBlockMove(obj) || HasBlockMove(obj) || obj.Annotations["keep"] != "me" {
		t.Fatalf("ClearBlockMove: %v", obj.Annotations)
	}
	bare := &infrav1.TerraformMachine{}
	if !SetBlockMove(bare) || !HasBlockMove(bare) {
		t.Error("SetBlockMove on nil annotations")
	}
}
