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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
)

// TestJobFailedRetryNote: a failed Job's JobFailed or JobDeadlineExceeded
// note says when its op runs again, from the backoff DecideOp applies to
// the same failures; a Job bookkeeping did not read, an op without a
// counted failure, and a remediation at its cap say no time.
func TestJobFailedRetryNote(t *testing.T) {
	t.Parallel()
	failed := job("a2", jobs.OpApply, jobs.Failed, t0)
	remediation := job("a3", jobs.OpApply, jobs.Failed, t0)
	remediation.Annotations = map[string]string{RemediationAnnotation: "true"}
	view := func(n, remediations int) JobsView {
		return JobsView{
			Failures: map[jobs.Op]int{jobs.OpApply: n}, LastFailure: map[jobs.Op]time.Time{jobs.OpApply: t0},
			FailedLimit: 3, RemediationFailures: remediations,
		}
	}
	for _, tt := range []struct {
		name string
		c    *metav1.Condition
		bk   *Bookkeeping
		want string // note suffix; "" for none
	}{
		{
			name: "first failure", c: cond(infrav1.ApplyJobSucceededCondition, cFalse, infrav1.ApplyFailedReason, "Job a2: step apply failed"),
			bk:   &Bookkeeping{View: view(1, 0), byName: map[string]finished{"a2": {job: &failed}}},
			want: "; next attempt not before 2026-09-25T12:01:00Z (backoff 1m0s after 1 failure)",
		},
		{
			name: "second failure doubles", c: cond(infrav1.ApplyJobSucceededCondition, cFalse, infrav1.ApplyFailedReason, "Job a2"),
			bk:   &Bookkeeping{View: view(2, 0), byName: map[string]finished{"a2": {job: &failed}}},
			want: "; next attempt not before 2026-09-25T12:02:00Z (backoff 2m0s after 2 consecutive failures)",
		},
		{
			name: "deadline at the history limit", c: cond(infrav1.ApplyJobSucceededCondition, cFalse, infrav1.JobDeadlineExceededReason, "Job a2"),
			bk:   &Bookkeeping{View: view(3, 0), byName: map[string]finished{"a2": {job: &failed}}},
			want: "; next attempt not before 2026-09-25T12:10:00Z (backoff 10m0s after 3 consecutive failures)",
		},
		{
			name: "remediation at its cap", c: cond(infrav1.ApplyJobSucceededCondition, cFalse, infrav1.ApplyFailedReason, "Job a3"),
			bk:   &Bookkeeping{View: view(3, 3), byName: map[string]finished{"a3": {job: &remediation}}},
			want: "; 3 drift remediations failed, so none runs again until a drift check succeeds",
		},
		{name: "not read", c: cond(infrav1.ApplyJobSucceededCondition, cFalse, infrav1.ApplyFailedReason, "Job a2"), bk: &Bookkeeping{View: view(1, 0)}},
		{
			name: "no counted failure", c: cond(infrav1.ApplyJobSucceededCondition, cFalse, infrav1.ApplyFailedReason, "Job a2"),
			bk: &Bookkeeping{View: view(0, 0), byName: map[string]finished{"a2": {job: &failed}}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tr := jobOutcome(*tt.c, jobNamed(tt.c.Message), tt.bk)
			if tt.want == "" {
				if strings.Contains(tr.note, "next attempt") || strings.Contains(tr.note, "remediations failed") {
					t.Errorf("note = %q, want no retry time", tr.note)
				}
				return
			}
			if !strings.HasSuffix(tr.note, tt.want) {
				t.Errorf("note = %q, want suffix %q", tr.note, tt.want)
			}
		})
	}
}

// TestJobFailedRetryNoteOnce: a failed apply's JobFailed, with its retry
// time, is emitted once across reconciles.
func TestJobFailedRetryNoteOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	e.runner.jobs = append(e.runner.jobs, job("a", jobs.OpApply, jobs.Failed, t0))
	reconcileMachine(t, e, readyOwner)
	reconcileMachine(t, e, readyOwner)
	got := e.rec.only(EventJobFailed)
	if len(got) != 1 || !strings.Contains(got[0].note, "; next attempt not before 2026-09-25T12:01:00Z (backoff 1m0s after 1 failure)") {
		t.Fatalf("JobFailed = %+v, want one with the retry time (all: %v)", got, e.rec.reasons)
	}
}

// TestWarningTransitionsLogAtV0: a Warning transition is logged at V0
// with its condition's type, reason and message; a Normal one only at
// LogFlow.
func TestWarningTransitionsLogAtV0(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		prev, c   *metav1.Condition
		verbosity int
		want      []string // substrings of the logged line; nil for none
	}{
		{
			name: "state lost at V0", verbosity: 0,
			prev: cond(infrav1.StateReadableCondition, cTrue, infrav1.StateReadReason, ""),
			c:    cond(infrav1.StateReadableCondition, cFalse, infrav1.StateLostReason, "the state is gone"),
			want: []string{"Condition changed", `type="StateReadable"`, `reason="StateLost"`, `message="the state is gone"`, `event="StateLost"`},
		},
		{
			name: "Normal transition is quiet at V0", verbosity: 0,
			prev: cond(infrav1.StateReadableCondition, cFalse, infrav1.StateLostReason, "the state is gone"),
			c:    cond(infrav1.StateReadableCondition, cTrue, infrav1.StateReadReason, ""),
		},
		{
			name: "Normal transition at LogFlow", verbosity: LogFlow,
			prev: cond(infrav1.StateReadableCondition, cFalse, infrav1.StateLostReason, "the state is gone"),
			c:    cond(infrav1.StateReadableCondition, cTrue, infrav1.StateReadReason, ""),
			want: []string{"Condition changed", `reason="StateRead"`, `event="ConditionChanged"`},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, u := capture(t, tt.verbosity)
			obj := machine()
			conditions.Set(obj, *tt.c)
			before := map[string]metav1.Condition{tt.prev.Type: *tt.prev}
			emitConditions(Deps{Recorder: &fakeRecorder{}}, logger, "TerraformMachine", obj, before, []string{tt.c.Type}, nil)
			got := u.GetBuffer().String()
			if tt.want == nil {
				if got != "" {
					t.Errorf("logged %s, want nothing", got)
				}
				return
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("log %s lacks %s", got, w)
				}
			}
		})
	}
}
