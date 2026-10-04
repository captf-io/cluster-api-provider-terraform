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

package webhooks

import (
	"context"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// terraformMachinePoolTemplateKind is the Kind name used in
// TerraformMachinePoolTemplate rejections and log lines.
const terraformMachinePoolTemplateKind = "TerraformMachinePoolTemplate"

// TerraformMachinePoolTemplate validates TerraformMachinePoolTemplates.
// +kubebuilder:object:generate=false
type TerraformMachinePoolTemplate struct{}

// SetupWebhookWithManager registers the webhook with mgr. It returns an
// error when registration fails.
func (w *TerraformMachinePoolTemplate) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &infrav1.TerraformMachinePoolTemplate{}).
		WithValidator(w).
		Complete()
}

// +kubebuilder:webhook:verbs=create;update,path=/validate-infrastructure-cluster-x-k8s-io-v1alpha1-terraformmachinepooltemplate,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=infrastructure.cluster.x-k8s.io,resources=terraformmachinepooltemplates,versions=v1alpha1,name=validation.terraformmachinepooltemplate.infrastructure.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1

var _ admission.Validator[*infrav1.TerraformMachinePoolTemplate] = &TerraformMachinePoolTemplate{}

// ValidateCreate validates obj's template metadata, source and jobs
// policy. It returns no warnings and an Invalid error listing every
// violation found, or a nil error when obj is valid. ctx supplies the
// logger and requesting user of the denial log.
func (*TerraformMachinePoolTemplate) ValidateCreate(ctx context.Context, obj *infrav1.TerraformMachinePoolTemplate) (admission.Warnings, error) {
	return nil, invalid(ctx, terraformMachinePoolTemplateKind, obj.Name, validatePoolTemplate(obj, nil))
}

// ValidateUpdate applies the create rules to newObj and also rejects any
// change to spec.template.spec, compared against oldObj: like every
// Cluster API template, this one is immutable, even though the
// TerraformMachinePool it stamps out is itself mutable; a new template
// rolls a new pool through MachinePool's own mechanism. ctx supplies the
// admission request skipImmutability inspects for a ClusterClass dry-run.
// It returns no warnings and an Invalid error listing every violation
// found, or a nil error when the update is valid.
func (*TerraformMachinePoolTemplate) ValidateUpdate(ctx context.Context, oldObj, newObj *infrav1.TerraformMachinePoolTemplate) (admission.Warnings, error) {
	errs := validatePoolTemplate(newObj, priorSpec(&oldObj.Spec.Template.Spec, &newObj.Spec.Template.Spec, !newObj.DeletionTimestamp.IsZero()))
	skip, err := skipImmutability(ctx, newObj)
	if err != nil {
		return nil, err
	}
	if !skip && !equality.Semantic.DeepEqual(oldObj.Spec.Template.Spec, newObj.Spec.Template.Spec) {
		errs = append(errs, immutable(templateSpecPath, terraformMachinePoolTemplateKind))
	}
	return nil, invalid(ctx, terraformMachinePoolTemplateKind, newObj.Name, errs)
}

// ValidateDelete allows every delete. It always returns no warnings and a
// nil error.
func (*TerraformMachinePoolTemplate) ValidateDelete(_ context.Context, _ *infrav1.TerraformMachinePoolTemplate) (admission.Warnings, error) {
	return nil, nil
}

// validatePoolTemplate validates obj's template metadata and spec; old is
// the stored template spec on an update, or nil on create. It returns the
// field errors found, or nil when obj is valid.
func validatePoolTemplate(obj *infrav1.TerraformMachinePoolTemplate, old *infrav1.TerraformMachinePoolSpec) field.ErrorList {
	errs := validateTemplateMetadata(obj.Spec.Template.ObjectMeta)
	return append(errs, validatePoolSpec(templateSpecPath, &obj.Spec.Template.Spec, old)...)
}
