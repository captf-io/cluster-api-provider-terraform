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
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// terraformMachineKind is the Kind name used in TerraformMachine rejections
// and log lines.
const terraformMachineKind = "TerraformMachine"

// TerraformMachine validates TerraformMachines.
// +kubebuilder:object:generate=false
type TerraformMachine struct {
	// Schemas is the variables schemas known so far; nil checks no
	// variables against one.
	Schemas SchemaLookup

	// Reader lists the Machines that reference the object on delete. It
	// should be uncached (manager.GetAPIReader) so a Machine whose deletion
	// just started is seen.
	Reader client.Reader
	// ManagerUser is the username of the manager's ServiceAccount
	// (system:serviceaccount:<namespace>:<name>), the only caller allowed to
	// set spec.providerID on an existing TerraformMachine. Empty when the
	// identity is unknown, which refuses every such update.
	ManagerUser string
}

// SetupWebhookWithManager registers the webhook with mgr. It returns an
// error when registration fails.
func (w *TerraformMachine) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &infrav1.TerraformMachine{}).
		WithValidator(w).
		Complete()
}

// +kubebuilder:webhook:verbs=create;update,path=/validate-infrastructure-cluster-x-k8s-io-v1alpha1-terraformmachine,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=infrastructure.cluster.x-k8s.io,resources=terraformmachines,versions=v1alpha1,name=validation.terraformmachine.infrastructure.cluster.x-k8s.io,timeoutSeconds=10,sideEffects=None,admissionReviewVersions=v1
// +kubebuilder:webhook:verbs=delete,path=/validate-infrastructure-cluster-x-k8s-io-v1alpha1-terraformmachine,mutating=false,failurePolicy=ignore,matchPolicy=Equivalent,groups=infrastructure.cluster.x-k8s.io,resources=terraformmachines,versions=v1alpha1,name=delete.validation.terraformmachine.infrastructure.cluster.x-k8s.io,timeoutSeconds=10,sideEffects=None,admissionReviewVersions=v1

var _ admission.Validator[*infrav1.TerraformMachine] = &TerraformMachine{}

// ValidateCreate validates obj's source and jobs policy. It returns no
// warnings and an Invalid error listing every violation found, or a nil
// error when obj is valid. ctx supplies the logger and requesting user of
// the denial log.
func (w *TerraformMachine) ValidateCreate(ctx context.Context, obj *infrav1.TerraformMachine) (admission.Warnings, error) {
	specPath := field.NewPath("spec")
	errs := validateMachineSpec(specPath, &obj.Spec, nil)
	if len(errs) == 0 {
		errs = schemaErrors(ctx, w.Schemas, obj.Namespace, specPath, &obj.Spec.WorkspaceSpec, nil)
	}
	return nil, invalid(ctx, terraformMachineKind, obj.Name, errs)
}

// ValidateUpdate applies the create rules and rejects a change to source,
// identityRef, variables or variablesFrom, which define the machine, and
// any providerID change except setting it once, from empty to non-empty, by
// the manager's ServiceAccount (ManagerUser): the controller only writes it
// while empty, so a value set by anyone else would stay wrong for good or
// bind the machine to another Node. The CRD repeats the immutability and
// set-once rules in CEL (TerraformMachine in api/v1alpha1), so they hold when
// the webhook is down or its configuration is gone, but only this webhook
// knows who sets providerID. Create still accepts a providerID,
// because clusterctl move recreates objects with theirs. jobs, drift and remediation are operational policy and
// stay mutable, so a stuck machine's deadline or drift checks can be changed
// without rolling it. Metadata changes are always allowed: KCP SSA-syncs
// labels and annotations on every reconcile, and clusterctl move annotates
// before deleting. ctx supplies the requesting user; newObj is validated and
// compared against oldObj. It returns no warnings and an Invalid error listing every violation found,
// or a nil error when the update is valid.
func (w *TerraformMachine) ValidateUpdate(ctx context.Context, oldObj, newObj *infrav1.TerraformMachine) (admission.Warnings, error) {
	specPath := field.NewPath("spec")
	prior := priorSpec(&oldObj.Spec, &newObj.Spec, !newObj.DeletionTimestamp.IsZero())
	errs := validateMachineSpec(specPath, &newObj.Spec, prior)

	oldProviderID, newProviderID := oldObj.Spec.ProviderID, newObj.Spec.ProviderID
	if oldProviderID != "" && oldProviderID != newProviderID {
		errs = append(errs, field.Forbidden(specPath.Child("providerID"),
			"providerID can only be set once, from empty to non-empty"))
	}
	if oldProviderID == "" && newProviderID != "" {
		req, err := admission.RequestFromContext(ctx)
		if err != nil {
			return nil, apierrors.NewInternalError(fmt.Errorf("read admission request: %w", err))
		}
		if w.ManagerUser == "" || req.UserInfo.Username != w.ManagerUser {
			errs = append(errs, field.Forbidden(specPath.Child("providerID"),
				"only the provider's controller may set providerID on an existing TerraformMachine"))
		}
	}
	if !equality.Semantic.DeepEqual(oldObj.Spec.Source, newObj.Spec.Source) {
		errs = append(errs, immutable(specPath.Child("source"), terraformMachineKind))
	}
	if oldObj.Spec.IdentityRef != newObj.Spec.IdentityRef {
		errs = append(errs, immutable(specPath.Child("identityRef"), terraformMachineKind))
	}
	if !equalVariables(oldObj.Spec.Variables, newObj.Spec.Variables) {
		errs = append(errs, immutable(specPath.Child("variables"), terraformMachineKind))
	}
	if !equality.Semantic.DeepEqual(oldObj.Spec.VariablesFrom, newObj.Spec.VariablesFrom) {
		errs = append(errs, immutable(specPath.Child("variablesFrom"), terraformMachineKind))
	}
	if len(errs) == 0 {
		errs = schemaErrors(ctx, w.Schemas, newObj.Namespace, specPath, &newObj.Spec.WorkspaceSpec, &prior.WorkspaceSpec)
	}
	return nil, invalid(ctx, terraformMachineKind, newObj.Name, errs)
}

// validateMachineSpec checks what a TerraformMachine and a
// TerraformMachineTemplate have in common: the source, the jobs policy and
// the variables. specPath is rooted at spec's parent field; old is the
// stored spec on an update (the jobs policy is checked only when it changed
// from there), or nil on create. It returns the field errors found, or nil
// when spec is valid.
func validateMachineSpec(specPath *field.Path, spec, old *infrav1.TerraformMachineSpec) field.ErrorList {
	var oldWS *infrav1.WorkspaceSpec
	if old != nil {
		oldWS = &old.WorkspaceSpec
	}
	return validateWorkloadSpec(specPath, contract.RoleMachine, &spec.WorkspaceSpec, oldWS)
}

// ValidateDelete refuses a direct delete: it would skip drain and the
// pre-drain/pre-terminate hooks. Deletion is allowed for clusterctl move,
// or when no Machine that is not being deleted references obj.
//
// The Machines are found by listing the namespace and matching their
// spec.infrastructureRef, not through obj's ownerReferences or labels:
// anyone who may update obj can drop or change those, and clusterctl move
// rewrites ownerReference UIDs, so they cannot be pinned. An object nothing
// references (a stuck object whose Machine is gone) can always be deleted.
//
// It is a best-effort guardrail, not a security boundary: its webhook entry
// has failurePolicy=Ignore and a 10s timeout, so a delete goes through when
// the manager is down or slow, and a namespace or the provider can always be
// removed.
//
// clusterctl move is recognized by its delete-for-move annotation only while
// the owning Cluster (cluster.x-k8s.io/cluster-name label) has
// spec.paused=true, which clusterctl sets before it deletes anything: the
// annotation alone can be set by anyone who may update the object, and would
// otherwise skip drain and the hooks with one write. An annotated machine of
// an unpaused, missing or unlabeled Cluster gets the ordinary check. ctx
// bounds the Cluster and Machine reads this makes for obj. It returns no
// warnings and an error when the delete must go through the referencing
// Machine instead, or a nil error when the delete is allowed.
func (w *TerraformMachine) ValidateDelete(ctx context.Context, obj *infrav1.TerraformMachine) (admission.Warnings, error) {
	if _, ok := obj.GetAnnotations()[clusterctlv1.DeleteForMoveAnnotation]; ok {
		paused, err := w.clusterPaused(ctx, obj)
		if err != nil {
			// A non-status error would become a 403 Denied, which reads as policy.
			return nil, apierrors.NewInternalError(err)
		}
		if paused {
			return nil, nil
		}
	}
	machines := &clusterv1.MachineList{}
	if err := w.Reader.List(ctx, machines, client.InNamespace(obj.Namespace)); err != nil {
		// A non-status error would become a 403 Denied, which reads as policy.
		return nil, apierrors.NewInternalError(fmt.Errorf("list Machines in %s: %w", obj.Namespace, err))
	}
	for i := range machines.Items {
		m := &machines.Items[i]
		if m.DeletionTimestamp.IsZero() && referencesMachine(m, obj) {
			return nil, invalid(ctx, terraformMachineKind, obj.Name, field.ErrorList{field.Forbidden(field.NewPath("metadata"),
				fmt.Sprintf("delete the Machine %s instead; deleting the TerraformMachine directly skips drain and the lifecycle hooks", m.Name))})
		}
	}
	return nil, nil
}

// referencesMachine reports whether m's spec.infrastructureRef names obj by
// API group, kind and name.
func referencesMachine(m *clusterv1.Machine, obj *infrav1.TerraformMachine) bool {
	ref := m.Spec.InfrastructureRef
	return ref.APIGroup == infrav1.GroupVersion.Group && ref.Kind == terraformMachineKind && ref.Name == obj.Name
}

// clusterPaused reports whether the CAPI Cluster named by obj's cluster-name
// label exists and has spec.paused=true. ctx bounds the read. It also
// returns an error from a read failure other than not-found.
func (w *TerraformMachine) clusterPaused(ctx context.Context, obj *infrav1.TerraformMachine) (bool, error) {
	name := obj.GetLabels()[clusterv1.ClusterNameLabel]
	if name == "" {
		return false, nil
	}
	cluster := &clusterv1.Cluster{}
	if err := w.Reader.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: name}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get Cluster %s/%s: %w", obj.Namespace, name, err)
	}
	paused := cluster.Spec.Paused
	return paused != nil && *paused, nil
}
