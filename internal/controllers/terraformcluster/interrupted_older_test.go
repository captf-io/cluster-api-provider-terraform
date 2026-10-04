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

package terraformcluster

import (
	"strings"
	"testing"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// TestInterruptedApplyAfterOlderBlock: an apply X blocked before a
// destructive plan is older than the approved apply A that disappeared
// after it, so X's block does not hide A: ApplyJobSucceeded says A
// disappeared and an apply is due, and the follow-up apply carries A's
// name.
func TestInterruptedApplyAfterOlderBlock(t *testing.T) {
	t.Parallel()
	e := newClusterEnv(t)
	e.setVersion("v1.37.0")
	x := e.reconcileStarts()
	h := x.Annotations[state.InputsHashAnnotation]
	e.block(x)
	e.reconcileNoApply()
	if c := e.applyCondition(); c.Reason != infrav1.DestructivePlanBlockedReason {
		t.Fatalf("ApplyJobSucceeded after the apply was blocked = %+v", c)
	}

	e.approve(h)
	a := e.reconcileStarts()
	e.deleteJob(a)
	r := e.reconcileStarts()
	if got := e.interrupted(); got != a.Name {
		t.Fatalf("interrupted apply after the approved Job %s vanished = %q", a.Name, got)
	}
	if r.Annotations[shared.AfterInterruptedApplyAnnotation] != a.Name {
		t.Errorf("follow-up apply annotations %v, want the interrupted Job %s", r.Annotations, a.Name)
	}
	if c := e.applyCondition(); c.Reason != infrav1.ApplyFailedReason || !strings.HasPrefix(c.Message, "Job "+a.Name+": disappeared while it ran") {
		t.Errorf("ApplyJobSucceeded while the follow-up apply is due = %+v, want Job %s disappeared", c, a.Name)
	}
}
