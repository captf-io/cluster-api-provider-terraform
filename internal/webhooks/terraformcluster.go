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
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// terraformClusterKind is the Kind name used in TerraformCluster rejections
// and log lines.
const terraformClusterKind = "TerraformCluster"

// TerraformCluster validates TerraformClusters.
// +kubebuilder:object:generate=false
type TerraformCluster struct{}

// SetupWebhookWithManager registers the webhook with mgr. It returns an
// error when registration fails.
func (w *TerraformCluster) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &infrav1.TerraformCluster{}).
		WithValidator(w).
		Complete()
}

// +kubebuilder:webhook:verbs=create;update,path=/validate-infrastructure-cluster-x-k8s-io-v1alpha1-terraformcluster,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=infrastructure.cluster.x-k8s.io,resources=terraformclusters,versions=v1alpha1,name=validation.terraformcluster.infrastructure.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1

var _ admission.Validator[*infrav1.TerraformCluster] = &TerraformCluster{}

// ValidateCreate requires obj to set an identity and a valid source and
// jobs policies. It returns no warnings and an Invalid error listing every
// violation found, or a nil error when obj is valid. ctx supplies the
// logger and requesting user of the denial log.
func (*TerraformCluster) ValidateCreate(ctx context.Context, obj *infrav1.TerraformCluster) (admission.Warnings, error) {
	specPath := field.NewPath("spec")
	errs := validateCluster(specPath, &obj.Spec, nil)
	errs = append(errs, validateEndpointComplete(specPath.Child("controlPlaneEndpoint"), obj.Spec.ControlPlaneEndpoint)...)
	return nil, invalid(ctx, terraformClusterKind, obj.Name, errs)
}

// ValidateUpdate applies the create rules to newObj, comparing against
// oldObj. Only controlPlaneEndpoint is immutable, once it is valid (has a
// host and a port); a half-set endpoint stored before the create rule
// existed stays completable, and unrelated updates to it are not rejected.
// A new image is a new module version, applied against the existing state. It
// returns no warnings and an Invalid error listing every violation found,
// or a nil error when the update is valid. ctx supplies the logger and
// requesting user of the denial log.
func (*TerraformCluster) ValidateUpdate(ctx context.Context, oldObj, newObj *infrav1.TerraformCluster) (admission.Warnings, error) {
	specPath := field.NewPath("spec")
	errs := validateCluster(specPath, &newObj.Spec, priorSpec(&oldObj.Spec, &newObj.Spec, !newObj.DeletionTimestamp.IsZero()))
	epPath := specPath.Child("controlPlaneEndpoint")
	old, cur := oldObj.Spec.ControlPlaneEndpoint, newObj.Spec.ControlPlaneEndpoint
	if old != nil && old.IsValid() {
		if cur == nil || *cur != *old {
			errs = append(errs, field.Forbidden(epPath,
				"controlPlaneEndpoint is immutable once it has a host and a port: every Machine and kubeconfig of the cluster points at it"))
		}
	} else if cur != nil && (old == nil || *cur != *old) {
		errs = append(errs, validateEndpointComplete(epPath, cur)...)
	}
	return nil, invalid(ctx, terraformClusterKind, newObj.Name, errs)
}

// ValidateDelete allows every delete. It always returns no warnings and a
// nil error.
func (*TerraformCluster) ValidateDelete(_ context.Context, _ *infrav1.TerraformCluster) (admission.Warnings, error) {
	return nil, nil
}

// validateEndpointComplete requires ep to be unset or to have both a host
// and a port: a half-set endpoint is unusable and leaves the controller
// unable to complete it. epPath is the endpoint's field path. It returns
// the field errors found, or nil when ep is acceptable.
func validateEndpointComplete(epPath *field.Path, ep *clusterv1.APIEndpoint) field.ErrorList {
	if ep == nil || (ep.Host == "") == (ep.Port == 0) {
		return nil
	}
	return field.ErrorList{field.Invalid(epPath, *ep, "controlPlaneEndpoint must set both host and port, or neither")}
}

// validateCluster is validateClusterSpec plus the cluster's own identity,
// which a TerraformClusterTemplate may leave to a ClusterClass patch.
// specPath is rooted at spec's parent field; old is the stored spec on an
// update, or nil on create. It returns the field errors found, or nil when
// spec is valid.
func validateCluster(specPath *field.Path, spec, old *infrav1.TerraformClusterSpec) field.ErrorList {
	errs := validateClusterSpec(specPath, spec, old)
	if spec.IdentityRef.Name == "" {
		errs = append(errs, field.Required(specPath.Child("identityRef"),
			"an identity is mandatory: set spec.identityRef (spec.defaults.identityRef only applies to machines)"))
	}
	return errs
}

// validateClusterSpec checks what a TerraformCluster and a
// TerraformClusterTemplate have in common: the source, every jobs policy
// and the variables. specPath is rooted at spec's parent field; old is the
// stored spec on an update (a jobs policy is checked only when it changed
// from there), or nil on create. It returns the field errors found, or nil
// when spec is valid.
func validateClusterSpec(specPath *field.Path, spec, old *infrav1.TerraformClusterSpec) field.ErrorList {
	var oldJobs, oldDefaultJobs *infrav1.JobPolicy
	if old != nil {
		oldJobs = old.Jobs
		if old.Defaults != nil {
			oldDefaultJobs = old.Defaults.Jobs
		}
	}
	errs := validateSource(specPath.Child("source"), &spec.Source)
	errs = append(errs, validateJobPolicyChange(specPath.Child("jobs"), oldJobs, spec.Jobs)...)
	errs = append(errs, validateVariables(specPath, contract.RoleCluster, spec.Variables, spec.VariablesFrom)...)
	if spec.Defaults != nil {
		errs = append(errs, validateJobPolicyChange(specPath.Child("defaults", "jobs"), oldDefaultJobs, spec.Defaults.Jobs)...)
	}
	return errs
}
