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

	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// terraformMachinePoolKind is the Kind name used in TerraformMachinePool
// rejections and log lines.
const terraformMachinePoolKind = "TerraformMachinePool"

// TerraformMachinePool validates TerraformMachinePools.
// +kubebuilder:object:generate=false
type TerraformMachinePool struct {
	// Schemas is the variables schemas known so far; nil checks no
	// variables against one.
	Schemas SchemaLookup
}

// SetupWebhookWithManager registers the webhook with mgr. It returns an
// error when registration fails.
func (w *TerraformMachinePool) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &infrav1.TerraformMachinePool{}).
		WithValidator(w).
		Complete()
}

// +kubebuilder:webhook:verbs=create;update,path=/validate-infrastructure-cluster-x-k8s-io-v1alpha1-terraformmachinepool,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=infrastructure.cluster.x-k8s.io,resources=terraformmachinepools,versions=v1alpha1,name=validation.terraformmachinepool.infrastructure.cluster.x-k8s.io,timeoutSeconds=10,sideEffects=None,admissionReviewVersions=v1

var _ admission.Validator[*infrav1.TerraformMachinePool] = &TerraformMachinePool{}

// ValidateCreate validates obj's source, jobs policy and variables. It
// returns no warnings and an Invalid error listing every violation found,
// or a nil error when obj is valid. ctx supplies the logger and requesting
// user of the denial log.
func (w *TerraformMachinePool) ValidateCreate(ctx context.Context, obj *infrav1.TerraformMachinePool) (admission.Warnings, error) {
	specPath := field.NewPath("spec")
	errs := validatePoolSpec(specPath, &obj.Spec, nil)
	if len(errs) == 0 {
		errs = schemaErrors(w.Schemas, specPath, &obj.Spec.WorkspaceSpec, nil)
	}
	return nil, invalid(ctx, terraformMachinePoolKind, obj.Name, errs)
}

// ValidateUpdate applies the create rules to newObj. Unlike a
// TerraformMachine, no field is immutable: providerID and providerIDList
// are written by the controller from the module's outputs and change
// whenever the group's membership does, and source, identityRef,
// variables, variablesFrom, jobs, drift and membershipRefreshIntervalSeconds
// are all operational policy or re-applied inputs for a mutable pool
// (https://captf.io/docs/module-author/contract/v1alpha1/machinepool.html "Lifecycle"). The jobs policy is checked only
// when it differs from oldObj's, and never on a deleting object, so a
// policy stored before a rule existed cannot block the controller's
// patches or a finalizer removal. It returns no
// warnings and an Invalid error listing every violation found, or a nil
// error when the update is valid. ctx supplies the logger and requesting
// user of the denial log.
func (w *TerraformMachinePool) ValidateUpdate(ctx context.Context, oldObj, newObj *infrav1.TerraformMachinePool) (admission.Warnings, error) {
	prior := priorSpec(&oldObj.Spec, &newObj.Spec, !newObj.DeletionTimestamp.IsZero())
	specPath := field.NewPath("spec")
	errs := validatePoolSpec(specPath, &newObj.Spec, prior)
	if len(errs) == 0 {
		errs = schemaErrors(w.Schemas, specPath, &newObj.Spec.WorkspaceSpec, &prior.WorkspaceSpec)
	}
	return nil, invalid(ctx, terraformMachinePoolKind, newObj.Name, errs)
}

// ValidateDelete allows every delete. It always returns no warnings and a
// nil error.
func (*TerraformMachinePool) ValidateDelete(_ context.Context, _ *infrav1.TerraformMachinePool) (admission.Warnings, error) {
	return nil, nil
}

// validatePoolSpec checks what a TerraformMachinePool and a
// TerraformMachinePoolTemplate have in common: the source, the jobs policy
// and the variables, validated for the machinepool role. specPath is
// rooted at spec's parent field; old is the stored spec on an update (the
// jobs policy is checked only when it changed from there), or nil on
// create. It returns the field errors found, or nil when spec is valid.
func validatePoolSpec(specPath *field.Path, spec, old *infrav1.TerraformMachinePoolSpec) field.ErrorList {
	var oldWS *infrav1.WorkspaceSpec
	if old != nil {
		oldWS = &old.WorkspaceSpec
	}
	return validateWorkloadSpec(specPath, contract.RoleMachinePool, &spec.WorkspaceSpec, oldWS)
}
