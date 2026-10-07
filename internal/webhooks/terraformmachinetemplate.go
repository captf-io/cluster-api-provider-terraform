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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// terraformMachineTemplateKind is the Kind name used in
// TerraformMachineTemplate rejections and log lines.
const terraformMachineTemplateKind = "TerraformMachineTemplate"

// TerraformMachineTemplate validates TerraformMachineTemplates.
// +kubebuilder:object:generate=false
type TerraformMachineTemplate struct {
	// Schemas is the variables schemas known so far; nil checks no
	// variables against one.
	Schemas SchemaLookup
}

// SetupWebhookWithManager registers the webhook with mgr. It returns an
// error when registration fails.
func (w *TerraformMachineTemplate) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &infrav1.TerraformMachineTemplate{}).
		WithValidator(w).
		Complete()
}

// +kubebuilder:webhook:verbs=create;update,path=/validate-infrastructure-cluster-x-k8s-io-v1alpha1-terraformmachinetemplate,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=infrastructure.cluster.x-k8s.io,resources=terraformmachinetemplates,versions=v1alpha1,name=validation.terraformmachinetemplate.infrastructure.cluster.x-k8s.io,timeoutSeconds=10,sideEffects=None,admissionReviewVersions=v1

var _ admission.Validator[*infrav1.TerraformMachineTemplate] = &TerraformMachineTemplate{}

// ValidateCreate validates obj's template metadata, source and jobs policy.
// It returns no warnings and an Invalid error listing every violation
// found, or a nil error when obj is valid. ctx supplies the logger and
// requesting user of the denial log.
func (w *TerraformMachineTemplate) ValidateCreate(ctx context.Context, obj *infrav1.TerraformMachineTemplate) (admission.Warnings, error) {
	errs := validateMachineTemplate(obj, nil)
	if len(errs) == 0 {
		errs = schemaErrors(ctx, w.Schemas, obj.Namespace, templateSpecPath, &obj.Spec.Template.Spec.WorkspaceSpec, nil)
	}
	return nil, invalid(ctx, terraformMachineTemplateKind, obj.Name, errs)
}

// ValidateUpdate applies the create rules to newObj and also rejects any
// change to spec.template.spec, compared against oldObj: Cluster API
// templates are immutable, even where the TerraformMachine itself is not
// (jobs, drift, remediation); a new template rolls the machines. The CRD
// carries no CEL rule for this, unlike TerraformMachine: CAPI's ClusterClass
// topology controller dry-runs an update that changes the template spec to
// find out whether it can be applied, and only this webhook can skip the
// check for such a request (skipImmutability); a CEL transition rule would
// reject it. ctx supplies the admission request skipImmutability inspects for a
// ClusterClass dry-run. It returns no warnings and an Invalid error listing
// every violation found, or a nil error when the update is valid.
func (w *TerraformMachineTemplate) ValidateUpdate(ctx context.Context, oldObj, newObj *infrav1.TerraformMachineTemplate) (admission.Warnings, error) {
	prior := priorSpec(&oldObj.Spec.Template.Spec, &newObj.Spec.Template.Spec, !newObj.DeletionTimestamp.IsZero())
	errs := validateMachineTemplate(newObj, prior)
	skip, err := skipImmutability(ctx, newObj)
	if err != nil {
		return nil, err
	}
	if !skip && !equality.Semantic.DeepEqual(oldObj.Spec.Template.Spec, newObj.Spec.Template.Spec) {
		errs = append(errs, immutable(templateSpecPath, terraformMachineTemplateKind))
	}
	if len(errs) == 0 {
		errs = schemaErrors(ctx, w.Schemas, newObj.Namespace, templateSpecPath, &newObj.Spec.Template.Spec.WorkspaceSpec, &prior.WorkspaceSpec)
	}
	return nil, invalid(ctx, terraformMachineTemplateKind, newObj.Name, errs)
}

// ValidateDelete allows every delete. It always returns no warnings and a
// nil error.
func (*TerraformMachineTemplate) ValidateDelete(_ context.Context, _ *infrav1.TerraformMachineTemplate) (admission.Warnings, error) {
	return nil, nil
}

// validateMachineTemplate validates obj's template metadata and spec; old
// is the stored template spec on an update, or nil on create. It returns
// the field errors found, or nil when obj is valid.
func validateMachineTemplate(obj *infrav1.TerraformMachineTemplate, old *infrav1.TerraformMachineSpec) field.ErrorList {
	errs := validateTemplateMetadata(obj.Spec.Template.ObjectMeta)
	// A providerID in a template would stamp every machine cloned from it
	// with one instance identity, and the controller never overwrites it.
	if obj.Spec.Template.Spec.ProviderID != "" {
		errs = append(errs, field.Forbidden(templateSpecPath.Child("providerID"),
			"providerID is assigned per machine by the controller and must be empty in a template"))
	}
	return append(errs, validateMachineSpec(templateSpecPath, &obj.Spec.Template.Spec, old)...)
}
