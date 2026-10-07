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
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// terraformPlanKind is the Kind name used in TerraformPlan rejections and
// log lines.
const terraformPlanKind = "TerraformPlan"

// TerraformPlan validates TerraformPlans: who may approve one, and that
// nothing else about it changes.
// +kubebuilder:object:generate=false
type TerraformPlan struct {
	// ManagerUser is the username of the manager's ServiceAccount
	// (system:serviceaccount:<namespace>:<name>), the only caller allowed to
	// change the captf.io/plan-phase label. Empty when the identity is
	// unknown, which refuses every such change.
	ManagerUser string
}

// SetupWebhookWithManager registers the webhook with mgr. It returns an
// error when registration fails.
func (w *TerraformPlan) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &infrav1.TerraformPlan{}).
		WithValidator(w).
		Complete()
}

// +kubebuilder:webhook:verbs=create;update,path=/validate-infrastructure-cluster-x-k8s-io-v1alpha1-terraformplan,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=infrastructure.cluster.x-k8s.io,resources=terraformplans,versions=v1alpha1,name=validation.terraformplan.infrastructure.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1

var _ admission.Validator[*infrav1.TerraformPlan] = &TerraformPlan{}

// ValidateCreate checks that obj names an approver exactly when it is
// approved. It accepts approved: true, whoever the creator is and whatever
// approvedBy says: clusterctl move creates the plans again as the mover, so
// the right to create a TerraformPlan is the right to approve one, and RBAC
// on terraformplans is the access control. ctx supplies the logger and
// requesting user of the denial log. It returns no warnings and an Invalid
// error listing every violation found, or a nil error when obj is valid.
func (*TerraformPlan) ValidateCreate(ctx context.Context, obj *infrav1.TerraformPlan) (admission.Warnings, error) {
	return nil, invalid(ctx, terraformPlanKind, obj.Name, validateApprover(field.NewPath("spec"), &obj.Spec))
}

// ValidateUpdate rejects a change to any spec field but approved and
// approvedBy, which the controller wrote when it created the plan; an
// approval that is withdrawn, re-attributed or given once the plan is in a
// terminal phase (the captf.io/plan-phase label of the stored object); an
// approvedBy that is not the username of the request that approves; and a
// change to the captf.io/plan-phase label by anyone but the manager
// (ManagerUser). ctx supplies the requesting user; newObj is compared
// against oldObj. It returns no warnings and an Invalid error listing every
// violation found, or a nil error when the update is valid; an update
// without an admission request is an InternalError.
func (w *TerraformPlan) ValidateUpdate(ctx context.Context, oldObj, newObj *infrav1.TerraformPlan) (admission.Warnings, error) {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("read admission request: %w", err))
	}
	user := req.UserInfo.Username
	specPath := field.NewPath("spec")
	errs := validateApprover(specPath, &newObj.Spec)

	o, n := &oldObj.Spec, &newObj.Spec
	for _, f := range []struct {
		name string
		same bool
	}{
		{"targetRef", o.TargetRef == n.TargetRef},
		{"planHash", o.PlanHash == n.PlanHash},
		{"inputsHash", o.InputsHash == n.InputsHash},
		{"reason", o.Reason == n.Reason},
		{"summary", equality.Semantic.DeepEqual(o.Summary, n.Summary)},
	} {
		if !f.same {
			errs = append(errs, immutable(specPath.Child(f.name), terraformPlanKind))
		}
	}

	wasApproved, isApproved := approved(o), approved(n)
	switch {
	case wasApproved && (!isApproved || o.ApprovedBy != n.ApprovedBy):
		errs = append(errs, field.Forbidden(specPath.Child("approved"), "an approval can be neither withdrawn nor changed"))
	case !wasApproved && isApproved:
		if phase := infrav1.PlanPhase(oldObj.Labels[infrav1.PlanPhaseLabel]); phase.Terminal() {
			errs = append(errs, field.Forbidden(specPath.Child("approved"),
				fmt.Sprintf("the plan is %s and can no longer be approved", phase)))
		}
		if n.ApprovedBy != user {
			errs = append(errs, field.Forbidden(specPath.Child("approvedBy"),
				"approvedBy must be the username of the user that approves"))
		}
	}

	if oldObj.Labels[infrav1.PlanPhaseLabel] != newObj.Labels[infrav1.PlanPhaseLabel] && (w.ManagerUser == "" || user != w.ManagerUser) {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "labels").Key(infrav1.PlanPhaseLabel),
			"only the provider's controller may change the plan phase"))
	}
	return nil, invalid(ctx, terraformPlanKind, newObj.Name, errs)
}

// ValidateDelete allows every delete. It always returns no warnings and a
// nil error; the webhook is not registered for delete.
func (*TerraformPlan) ValidateDelete(context.Context, *infrav1.TerraformPlan) (admission.Warnings, error) {
	return nil, nil
}

// approved reports whether s approves the plan.
func approved(s *infrav1.TerraformPlanSpec) bool {
	return s.Approved != nil && *s.Approved
}

// validateApprover checks that s names an approver exactly when it is
// approved; specPath is the path of s. It returns the violations found.
func validateApprover(specPath *field.Path, s *infrav1.TerraformPlanSpec) field.ErrorList {
	switch {
	case approved(s) && s.ApprovedBy == "":
		return field.ErrorList{field.Required(specPath.Child("approvedBy"), "approvedBy is required when approved is true")}
	case !approved(s) && s.ApprovedBy != "":
		return field.ErrorList{field.Forbidden(specPath.Child("approvedBy"), "approvedBy can only be set when approved is true")}
	}
	return nil
}
