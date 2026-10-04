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

// terraformClusterTemplateKind is the Kind name used in
// TerraformClusterTemplate rejections and log lines.
const terraformClusterTemplateKind = "TerraformClusterTemplate"

// missingIdentityWarning is returned for a template without an identityRef:
// a ClusterClass patch or the Cluster that uses it may still set one.
const missingIdentityWarning = "spec.template.spec sets no identityRef; " +
	"TerraformClusters created from this template are rejected unless a patch sets one"

// TerraformClusterTemplate validates TerraformClusterTemplates.
// +kubebuilder:object:generate=false
type TerraformClusterTemplate struct{}

// SetupWebhookWithManager registers the webhook with mgr. It returns an
// error when registration fails.
func (w *TerraformClusterTemplate) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &infrav1.TerraformClusterTemplate{}).
		WithValidator(w).
		Complete()
}

// +kubebuilder:webhook:verbs=create;update,path=/validate-infrastructure-cluster-x-k8s-io-v1alpha1-terraformclustertemplate,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=infrastructure.cluster.x-k8s.io,resources=terraformclustertemplates,versions=v1alpha1,name=validation.terraformclustertemplate.infrastructure.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1

var _ admission.Validator[*infrav1.TerraformClusterTemplate] = &TerraformClusterTemplate{}

// ValidateCreate validates obj's template metadata, source and jobs
// policies, and warns without an identity. It returns those warnings and an
// Invalid error listing every violation found, or a nil error when obj is
// valid. ctx supplies the logger and requesting user of the denial log.
func (*TerraformClusterTemplate) ValidateCreate(ctx context.Context, obj *infrav1.TerraformClusterTemplate) (admission.Warnings, error) {
	return clusterTemplateWarnings(obj), invalid(ctx, terraformClusterTemplateKind, obj.Name, validateClusterTemplate(obj, nil))
}

// ValidateUpdate applies the create rules to newObj and also rejects any
// change to spec.template.spec, compared against oldObj: Cluster API
// templates are immutable. ctx supplies the admission request
// skipImmutability inspects for a ClusterClass dry-run. It returns the same
// warnings as ValidateCreate and an Invalid error listing every violation
// found, or a nil error when the update is valid.
func (*TerraformClusterTemplate) ValidateUpdate(ctx context.Context, oldObj, newObj *infrav1.TerraformClusterTemplate) (admission.Warnings, error) {
	errs := validateClusterTemplate(newObj, priorSpec(&oldObj.Spec.Template.Spec, &newObj.Spec.Template.Spec, !newObj.DeletionTimestamp.IsZero()))
	skip, err := skipImmutability(ctx, newObj)
	if err != nil {
		return nil, err
	}
	if !skip && !equality.Semantic.DeepEqual(oldObj.Spec.Template.Spec, newObj.Spec.Template.Spec) {
		errs = append(errs, immutable(templateSpecPath, terraformClusterTemplateKind))
	}
	return clusterTemplateWarnings(newObj), invalid(ctx, terraformClusterTemplateKind, newObj.Name, errs)
}

// ValidateDelete allows every delete. It always returns no warnings and a
// nil error.
func (*TerraformClusterTemplate) ValidateDelete(_ context.Context, _ *infrav1.TerraformClusterTemplate) (admission.Warnings, error) {
	return nil, nil
}

// validateClusterTemplate validates obj's template metadata and spec; old
// is the stored template spec on an update, or nil on create. It returns
// the field errors found, or nil when obj is valid.
func validateClusterTemplate(obj *infrav1.TerraformClusterTemplate, old *infrav1.TerraformClusterSpec) field.ErrorList {
	errs := validateTemplateMetadata(obj.Spec.Template.ObjectMeta)
	return append(errs, validateClusterSpec(templateSpecPath, &obj.Spec.Template.Spec, old)...)
}

// clusterTemplateWarnings returns missingIdentityWarning when obj's
// template sets no identityRef, or nil otherwise.
func clusterTemplateWarnings(obj *infrav1.TerraformClusterTemplate) admission.Warnings {
	if obj.Spec.Template.Spec.IdentityRef.Name != "" {
		return nil
	}
	return admission.Warnings{missingIdentityWarning}
}
