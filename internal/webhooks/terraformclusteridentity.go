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
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
)

// terraformClusterIdentityKind is the Kind name used in
// TerraformClusterIdentity rejections and log lines.
const terraformClusterIdentityKind = "TerraformClusterIdentity"

// TerraformClusterIdentity validates TerraformClusterIdentities.
// +kubebuilder:object:generate=false
type TerraformClusterIdentity struct {
	// Client creates the SubjectAccessReview that checks the requester may
	// read the referenced Secret.
	Client client.Client
	// Reader lists TerraformClusters and TerraformMachines on delete. It
	// should be uncached (manager.GetAPIReader), so objects outside the
	// manager's cache scope keep an identity too.
	Reader client.Reader
}

// SetupWebhookWithManager registers the webhook with mgr. It returns an
// error when registration fails.
func (w *TerraformClusterIdentity) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &infrav1.TerraformClusterIdentity{}).
		WithValidator(w).
		Complete()
}

// +kubebuilder:webhook:verbs=create;update;delete,path=/validate-infrastructure-cluster-x-k8s-io-v1alpha1-terraformclusteridentity,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=infrastructure.cluster.x-k8s.io,resources=terraformclusteridentities,versions=v1alpha1,name=validation.terraformclusteridentity.infrastructure.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1

var _ admission.Validator[*infrav1.TerraformClusterIdentity] = &TerraformClusterIdentity{}

// ValidateCreate validates obj's Secret reference and allowed namespaces,
// and checks, bounded by ctx, that the requester may get the referenced
// Secret. It returns no warnings and an error when obj is invalid or the
// requester may not read the Secret, or a nil error when the create is
// allowed.
func (w *TerraformClusterIdentity) ValidateCreate(ctx context.Context, obj *infrav1.TerraformClusterIdentity) (admission.Warnings, error) {
	if errs := validateIdentitySpec(&obj.Spec); len(errs) > 0 {
		return nil, invalid(ctx, terraformClusterIdentityKind, obj.Name, errs)
	}
	return nil, w.authorizeSecretRead(ctx, obj)
}

// ValidateUpdate applies the create rules to newObj, bounded by ctx and
// compared against oldObj. The Secret read check runs when secretRef or
// allowedNamespaces changes: the first chooses a Secret, and the second
// chooses where the manager mirrors it, so widening it would hand a
// requester who cannot read the Secret a copy in a namespace they control.
// An unrelated update (labels, annotations) skips the check. It returns no warnings and an error when the
// update is invalid or unauthorized, or a nil error when it is allowed.
func (w *TerraformClusterIdentity) ValidateUpdate(ctx context.Context, oldObj, newObj *infrav1.TerraformClusterIdentity) (admission.Warnings, error) {
	if errs := validateIdentitySpec(&newObj.Spec); len(errs) > 0 {
		return nil, invalid(ctx, terraformClusterIdentityKind, newObj.Name, errs)
	}
	if oldObj.Spec.SecretRef == newObj.Spec.SecretRef &&
		apiequality.Semantic.DeepEqual(oldObj.Spec.AllowedNamespaces, newObj.Spec.AllowedNamespaces) {
		return nil, nil
	}
	return nil, w.authorizeSecretRead(ctx, newObj)
}

// ValidateDelete refuses to delete an identity that is still in use: a
// TerraformCluster or TerraformMachine uses it (directly or through the
// cluster fallback), or status.namespaces still lists a mirror of its
// Secret. Deleting it would leave those objects unable to run, and destroy,
// with the credentials that created their resources. ctx bounds the reads
// this makes to find obj's users. It returns no warnings and an error when
// obj is still in use or its credentials are still mirrored somewhere, or
// a nil error when the delete is allowed.
func (w *TerraformClusterIdentity) ValidateDelete(ctx context.Context, obj *infrav1.TerraformClusterIdentity) (admission.Warnings, error) {
	user, err := identity.FirstUser(ctx, w.Reader, obj.Name)
	if err != nil {
		// A non-status error would become a 403 Denied, which reads as policy.
		return nil, apierrors.NewInternalError(err)
	}
	if user != nil {
		return nil, invalid(ctx, terraformClusterIdentityKind, obj.Name, field.ErrorList{field.Forbidden(field.NewPath("metadata"),
			fmt.Sprintf("the identity is in use by %s %s/%s; delete it or point it at another identity first",
				userKind(user), user.GetNamespace(), user.GetName()))})
	}
	if ns := obj.Status.Namespaces; len(ns) > 0 {
		return nil, invalid(ctx, terraformClusterIdentityKind, obj.Name, field.ErrorList{field.Forbidden(field.NewPath("status", "namespaces"),
			fmt.Sprintf("the identity's credentials are still mirrored into namespace(s) %s; wait for the objects there to be deleted",
				strings.Join(ns, ", ")))})
	}
	return nil, nil
}

// userKind names the kind of o, an object FirstUser returned; typed list
// items carry no TypeMeta, so the kind is inferred from o's Go type. It
// returns "TerraformMachine", "TerraformMachinePool" or "TerraformCluster".
func userKind(o client.Object) string {
	switch o.(type) {
	case *infrav1.TerraformMachine:
		return "TerraformMachine"
	case *infrav1.TerraformMachinePool:
		return "TerraformMachinePool"
	}
	return "TerraformCluster"
}

// authorizeSecretRead runs a SubjectAccessReview for the requesting user to
// get the referenced Secret. Without it anyone who may create identities, a
// "manage Terraform infrastructure" role, could point one at any Secret of
// any namespace and have the manager mirror it into a namespace they read.
// ctx bounds the SubjectAccessReview create and supplies the requester's
// identity, and obj names the Secret to check. It returns nil when the
// requester may get that Secret, or an error otherwise: an Invalid error
// when they may not, or an InternalError when the check itself fails.
func (w *TerraformClusterIdentity) authorizeSecretRead(ctx context.Context, obj *infrav1.TerraformClusterIdentity) error {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return apierrors.NewInternalError(fmt.Errorf("read admission request: %w", err))
	}
	ref := obj.Spec.SecretRef
	u := req.UserInfo
	extra := make(map[string]authorizationv1.ExtraValue, len(u.Extra))
	for k, v := range u.Extra {
		extra[k] = authorizationv1.ExtraValue(v)
	}
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:   u.Username,
		UID:    u.UID,
		Groups: u.Groups,
		Extra:  extra,
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: ref.Namespace,
			Verb:      "get",
			Resource:  "secrets",
			Name:      ref.Name,
		},
	}}
	if err := w.Client.Create(ctx, sar); err != nil {
		return apierrors.NewInternalError(fmt.Errorf("create SubjectAccessReview: %w", err))
	}
	if sar.Status.Allowed {
		return nil
	}
	klog.FromContext(ctx).V(1).Info("SubjectAccessReview denied a Secret reference",
		"user", u.Username, "secretNamespace", ref.Namespace, "secretName", ref.Name, "reason", sar.Status.Reason)
	return invalid(ctx, terraformClusterIdentityKind, obj.Name, field.ErrorList{field.Forbidden(field.NewPath("spec", "secretRef"),
		fmt.Sprintf("user %q may not get Secret %s/%s; an identity may only reference a Secret its author can read",
			u.Username, ref.Namespace, ref.Name))})
}

// validateIdentitySpec checks spec's Secret reference and, when set, its
// allowedNamespaces list or selector. It returns the field errors found, or
// nil when spec is valid.
func validateIdentitySpec(spec *infrav1.TerraformClusterIdentitySpec) field.ErrorList {
	var errs field.ErrorList
	refPath := field.NewPath("spec", "secretRef")
	if spec.SecretRef.Name == "" {
		errs = append(errs, field.Required(refPath.Child("name"), "the credentials Secret name is required"))
	}
	if spec.SecretRef.Namespace == "" {
		errs = append(errs, field.Required(refPath.Child("namespace"), "the credentials Secret namespace is required"))
	}
	if spec.AllowedNamespaces == nil {
		return errs
	}
	nsPath := field.NewPath("spec", "allowedNamespaces")
	if len(spec.AllowedNamespaces.List) == 0 && spec.AllowedNamespaces.Selector == nil {
		// {} used to mean "every namespace": the most permissive value looked
		// like the least. It is now rejected rather than reinterpreted.
		errs = append(errs, field.Invalid(nsPath, "{}",
			"allowedNamespaces {} is ambiguous and rejected: set list, or write selector: {} to allow every namespace"))
	}
	for i, ns := range spec.AllowedNamespaces.List {
		for _, msg := range validation.IsDNS1123Label(ns) {
			errs = append(errs, field.Invalid(nsPath.Child("list").Index(i), ns, msg))
		}
	}
	if sel := spec.AllowedNamespaces.Selector; sel != nil {
		if _, err := metav1.LabelSelectorAsSelector(sel); err != nil {
			errs = append(errs, field.Invalid(nsPath.Child("selector"), sel, err.Error()))
		}
	}
	return errs
}
