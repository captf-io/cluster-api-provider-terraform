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

package webhooks

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// planFixture returns a TerraformPlan in phase, approved by approvedBy when
// that is not empty.
func planFixture(phase infrav1.PlanPhase, approvedBy string) *infrav1.TerraformPlan {
	p := &infrav1.TerraformPlan{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo-0123456789", Namespace: "ns",
			Labels: map[string]string{infrav1.PlanPhaseLabel: string(phase)},
		},
		Spec: infrav1.TerraformPlanSpec{
			TargetRef:  infrav1.PlanTargetRef{Kind: infrav1.PlanTargetCluster, Name: "demo"},
			PlanHash:   "p2:abc",
			InputsHash: "h2:def",
			Reason:     infrav1.PlanReasonDestructive,
			Summary:    infrav1.PlanSummary{Replace: new(int32(1))},
		},
	}
	if approvedBy != "" {
		p.Spec.Approved, p.Spec.ApprovedBy = new(true), approvedBy
	}
	return p
}

// TestTerraformPlanApproval proves an approval needs approvedBy to be the
// requesting user and a live plan, can neither be withdrawn nor changed, and
// that every other spec field is immutable.
func TestTerraformPlanApproval(t *testing.T) {
	t.Parallel()
	w := &TerraformPlan{ManagerUser: testManagerUser}
	tests := []struct {
		name    string
		old     *infrav1.TerraformPlan
		mutate  func(*infrav1.TerraformPlan)
		user    string
		invalid string
	}{
		{name: "approve", old: planFixture(infrav1.PlanPhasePending, ""), user: "alice",
			mutate: func(p *infrav1.TerraformPlan) { p.Spec.Approved, p.Spec.ApprovedBy = new(true), "alice" }},
		{name: "approve an approved-phase plan", old: planFixture(infrav1.PlanPhaseApproved, ""), user: "alice",
			mutate: func(p *infrav1.TerraformPlan) { p.Spec.Approved, p.Spec.ApprovedBy = new(true), "alice" }},
		{name: "approve a plan without a phase label", old: planFixture("", ""), user: "alice",
			mutate: func(p *infrav1.TerraformPlan) { p.Spec.Approved, p.Spec.ApprovedBy = new(true), "alice" }},
		{name: "approvedBy of another user", old: planFixture(infrav1.PlanPhasePending, ""), user: "mallory",
			mutate:  func(p *infrav1.TerraformPlan) { p.Spec.Approved, p.Spec.ApprovedBy = new(true), "alice" },
			invalid: "spec.approvedBy: Forbidden"},
		{name: "approve without approvedBy", old: planFixture(infrav1.PlanPhasePending, ""), user: "alice",
			mutate:  func(p *infrav1.TerraformPlan) { p.Spec.Approved = new(true) },
			invalid: "spec.approvedBy: Required"},
		{name: "approvedBy without approved", old: planFixture(infrav1.PlanPhasePending, ""), user: "alice",
			mutate:  func(p *infrav1.TerraformPlan) { p.Spec.ApprovedBy = "alice" },
			invalid: "spec.approvedBy: Forbidden"},
		{name: "approve an applied plan", old: planFixture(infrav1.PlanPhaseApplied, ""), user: "alice",
			mutate:  func(p *infrav1.TerraformPlan) { p.Spec.Approved, p.Spec.ApprovedBy = new(true), "alice" },
			invalid: "can no longer be approved"},
		{name: "approve a superseded plan", old: planFixture(infrav1.PlanPhaseSuperseded, ""), user: "alice",
			mutate:  func(p *infrav1.TerraformPlan) { p.Spec.Approved, p.Spec.ApprovedBy = new(true), "alice" },
			invalid: "can no longer be approved"},
		{name: "approve a failed plan", old: planFixture(infrav1.PlanPhaseFailed, ""), user: "alice",
			mutate:  func(p *infrav1.TerraformPlan) { p.Spec.Approved, p.Spec.ApprovedBy = new(true), "alice" },
			invalid: "can no longer be approved"},
		{name: "withdraw", old: planFixture(infrav1.PlanPhaseApproved, "alice"), user: "alice",
			mutate:  func(p *infrav1.TerraformPlan) { p.Spec.Approved, p.Spec.ApprovedBy = new(false), "" },
			invalid: "spec.approved: Forbidden"},
		{name: "re-attribute", old: planFixture(infrav1.PlanPhaseApproved, "alice"), user: "bob",
			mutate:  func(p *infrav1.TerraformPlan) { p.Spec.ApprovedBy = "bob" },
			invalid: "spec.approved: Forbidden"},
		{name: "an approved plan updated again", old: planFixture(infrav1.PlanPhaseApproved, "alice"), user: "bob",
			mutate: func(p *infrav1.TerraformPlan) { p.Annotations = map[string]string{"x": "y"} }},
		{name: "planHash", old: planFixture(infrav1.PlanPhasePending, ""), user: "alice",
			mutate: func(p *infrav1.TerraformPlan) { p.Spec.PlanHash = "p2:other" }, invalid: "spec.planHash: Forbidden"},
		{name: "inputsHash", old: planFixture(infrav1.PlanPhasePending, ""), user: "alice",
			mutate: func(p *infrav1.TerraformPlan) { p.Spec.InputsHash = "h2:other" }, invalid: "spec.inputsHash: Forbidden"},
		{name: "reason", old: planFixture(infrav1.PlanPhasePending, ""), user: "alice",
			mutate: func(p *infrav1.TerraformPlan) { p.Spec.Reason = infrav1.PlanReasonManual }, invalid: "spec.reason: Forbidden"},
		{name: "targetRef", old: planFixture(infrav1.PlanPhasePending, ""), user: "alice",
			mutate: func(p *infrav1.TerraformPlan) { p.Spec.TargetRef.Name = "other" }, invalid: "spec.targetRef: Forbidden"},
		{name: "summary", old: planFixture(infrav1.PlanPhasePending, ""), user: "alice",
			mutate: func(p *infrav1.TerraformPlan) { p.Spec.Summary.Replace = new(int32(0)) }, invalid: "spec.summary: Forbidden"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			updated := tt.old.DeepCopy()
			tt.mutate(updated)
			_, err := w.ValidateUpdate(userContext(tt.user), tt.old, updated)
			if tt.invalid == "" {
				wantInvalid(t, err, false)
				return
			}
			wantInvalid(t, err, true, tt.invalid)
		})
	}
}

// TestTerraformPlanPhaseLabel proves only the manager changes the
// captf.io/plan-phase label, and that a manager of unknown identity refuses
// everyone.
func TestTerraformPlanPhaseLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		w       *TerraformPlan
		user    string
		invalid bool
	}{
		{name: "manager", w: &TerraformPlan{ManagerUser: testManagerUser}, user: testManagerUser},
		{name: "another service account", w: &TerraformPlan{ManagerUser: testManagerUser}, user: "system:serviceaccount:ns:other", invalid: true},
		{name: "human", w: &TerraformPlan{ManagerUser: testManagerUser}, user: "alice", invalid: true},
		{name: "manager identity unknown", w: &TerraformPlan{}, user: "", invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			old := planFixture(infrav1.PlanPhasePending, "")
			updated := old.DeepCopy()
			updated.Labels[infrav1.PlanPhaseLabel] = string(infrav1.PlanPhaseApplied)
			_, err := tt.w.ValidateUpdate(userContext(tt.user), old, updated)
			wantInvalid(t, err, tt.invalid, "plan phase")
		})
	}
	t.Run("removing the label", func(t *testing.T) {
		t.Parallel()
		old := planFixture(infrav1.PlanPhaseApplied, "")
		updated := old.DeepCopy()
		delete(updated.Labels, infrav1.PlanPhaseLabel)
		_, err := (&TerraformPlan{ManagerUser: testManagerUser}).ValidateUpdate(userContext("alice"), old, updated)
		wantInvalid(t, err, true, "plan phase")
	})
	t.Run("other labels stay open", func(t *testing.T) {
		t.Parallel()
		old := planFixture(infrav1.PlanPhasePending, "")
		updated := old.DeepCopy()
		updated.Labels["team"] = "a"
		_, err := (&TerraformPlan{ManagerUser: testManagerUser}).ValidateUpdate(userContext("alice"), old, updated)
		wantInvalid(t, err, false)
	})
}

// TestTerraformPlanCreate proves create accepts an unapproved plan and an
// approval by the creator, and rejects an approval without an approver.
func TestTerraformPlanCreate(t *testing.T) {
	t.Parallel()
	w := &TerraformPlan{ManagerUser: testManagerUser}
	_, err := w.ValidateCreate(userContext(testManagerUser), planFixture(infrav1.PlanPhasePending, ""))
	wantInvalid(t, err, false)
	p := planFixture(infrav1.PlanPhaseApproved, "alice")
	p.Spec.ApprovedBy = ""
	_, err = w.ValidateCreate(userContext("alice"), p)
	wantInvalid(t, err, true, "spec.approvedBy: Required")
}

// TestTerraformPlanCreateForgedApproval proves a creator other than the
// manager cannot name someone else as approver of a live plan, nor set a
// phase only the controller owns, while an approval by themselves, a move
// of a finished plan and the manager's own creates pass.
func TestTerraformPlanCreateForgedApproval(t *testing.T) {
	t.Parallel()
	w := &TerraformPlan{ManagerUser: testManagerUser}
	const mover = "system:serviceaccount:capi:mover"
	unlabelled := func(approvedBy string) *infrav1.TerraformPlan {
		p := planFixture("", approvedBy)
		p.Labels = nil
		return p
	}
	tests := []struct {
		name    string
		user    string
		plan    *infrav1.TerraformPlan
		invalid string
	}{
		{name: "approved by another user", user: "mallory", plan: planFixture(infrav1.PlanPhasePending, "alice"),
			invalid: "spec.approvedBy: Forbidden"},
		{name: "approved by another user, no label", user: "mallory", plan: unlabelled("alice"),
			invalid: "spec.approvedBy: Forbidden"},
		{name: "approved by another user, Approved label", user: mover, plan: planFixture(infrav1.PlanPhaseApproved, "alice"),
			invalid: "spec.approvedBy: Forbidden"},
		{name: "approved by self", user: "alice", plan: planFixture(infrav1.PlanPhaseApproved, "alice")},
		{name: "approved by self, no label", user: "alice", plan: unlabelled("alice")},
		{name: "unapproved Pending", user: "mallory", plan: planFixture(infrav1.PlanPhasePending, "")},
		{name: "unapproved with Approved label", user: "mallory", plan: planFixture(infrav1.PlanPhaseApproved, ""),
			invalid: "metadata.labels[captf.io/plan-phase]: Forbidden"},
		{name: "move of an applied plan", user: mover, plan: planFixture(infrav1.PlanPhaseApplied, "alice")},
		{name: "move of a superseded plan", user: mover, plan: planFixture(infrav1.PlanPhaseSuperseded, "alice")},
		{name: "move of a failed plan", user: mover, plan: planFixture(infrav1.PlanPhaseFailed, "alice")},
		{name: "manager approved", user: testManagerUser, plan: planFixture(infrav1.PlanPhaseApproved, "alice")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := w.ValidateCreate(userContext(tt.user), tt.plan)
			if tt.invalid == "" {
				wantInvalid(t, err, false)
				return
			}
			wantInvalid(t, err, true, tt.invalid)
		})
	}
	t.Run("unknown manager", func(t *testing.T) {
		t.Parallel()
		_, err := (&TerraformPlan{}).ValidateCreate(userContext(testManagerUser), planFixture(infrav1.PlanPhaseApproved, "alice"))
		wantInvalid(t, err, true, "spec.approvedBy: Forbidden")
	})
}

// TestTerraformPlanNoAdmissionRequest proves an update outside the webhook
// server, which carries no requesting user, is an InternalError.
func TestTerraformPlanNoAdmissionRequest(t *testing.T) {
	t.Parallel()
	old := planFixture(infrav1.PlanPhasePending, "")
	_, err := (&TerraformPlan{}).ValidateUpdate(context.Background(), old, old.DeepCopy())
	if !apierrors.IsInternalError(err) {
		t.Fatalf("error = %v, want InternalError", err)
	}
	if _, err := (&TerraformPlan{}).ValidateCreate(context.Background(), old); !apierrors.IsInternalError(err) {
		t.Errorf("ValidateCreate error = %v, want InternalError", err)
	}
	if _, err := (&TerraformPlan{}).ValidateDelete(context.Background(), old); err != nil {
		t.Errorf("ValidateDelete = %v", err)
	}
}
