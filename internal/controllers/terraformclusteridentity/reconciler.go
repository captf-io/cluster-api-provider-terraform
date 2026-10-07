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

package terraformclusteridentity

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
)

// The manager's RBAC for this controller (identities get/list/watch,
// identities/status update/patch, secrets get/list/watch) is already in
// internal/controllers/rbac.go.
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=terraformclusteridentities,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=terraformclusteridentities/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// DefaultRequeueAfter is how often the source Secret is re-read when the
// caller sets no RequeueAfter.
const DefaultRequeueAfter = 5 * time.Minute

// NotReadyRequeueAfter is how often the source Secret is re-read while it
// is missing or lacks a required key, when RequeueAfter is not shorter:
// the Secret cannot be watched (see APIReader), and the identity's users
// start no Job until Ready is True.
const NotReadyRequeueAfter = 30 * time.Second

// SourceReadTimeout bounds the live read of an identity's source Secret.
const SourceReadTimeout = 15 * time.Second

// Reconciler sets a TerraformClusterIdentity's status.
type Reconciler struct {
	// Client reads identities, updates the source Secret's ownerRefs and
	// writes status.
	Client client.Client
	// Cache lists mirror Secrets as metadata (shared.MirrorIdentityIndex)
	// and the identity's users in their namespaces: the manager's cache
	// (mgr.GetCache()), so a mirror event costs no API request. The
	// default client cannot serve these lists: it reads Secrets live, even
	// as metadata (manager.UncachedObjects).
	Cache client.Reader
	// APIReader reads the source Secret. It must be uncached: the Secret is
	// unlabeled and usually outside the manager's cache scope.
	APIReader client.Reader
	// RequeueAfter re-reads the source Secret periodically. It cannot be
	// watched for the same reason it cannot be cached, so a Secret created or
	// deleted out of band is noticed within this period, or within
	// NotReadyRequeueAfter while Ready is False.
	RequeueAfter time.Duration
	// WatchFilter is the cluster.x-k8s.io/watch-filter label value.
	WatchFilter string
	// Recorder emits IdentitySecretFound and IdentitySecretNotFound when
	// Ready changes; nil emits nothing.
	Recorder events.EventRecorder
}

// SetupWithManager registers the controller with mgr, applying opts to the
// underlying controller: identities (spec changes) and the metadata of
// mirror Secrets, mapped to their identity through
// inputs.IdentityAnnotation. Every mirror event passes, ownerRef changes
// included: they change which namespaces use the identity, and the queue
// collapses a burst into one reconcile per identity. It returns an error
// if the controller could not be built.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, opts controller.Options) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.TerraformClusterIdentity{}, builder.WithPredicates(
			predicates.ResourceHasFilterLabel(mgr.GetScheme(), mgr.GetLogger(), r.WatchFilter),
			predicate.GenerationChangedPredicate{},
		)).
		WatchesMetadata(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(MirrorToIdentity),
			builder.WithPredicates(predicate.NewPredicateFuncs(isMirror))).
		Named("terraformclusteridentity").
		WithOptions(opts).
		Complete(r)
	if err != nil {
		return fmt.Errorf("terraformclusteridentity: setup: %w", err)
	}
	return nil
}

// isMirror reports whether o is a credential mirror (identity.MirroredLabel).
func isMirror(o client.Object) bool {
	return o.GetLabels()[identity.MirroredLabel] == "true" && o.GetAnnotations()[inputs.IdentityAnnotation] != ""
}

// MirrorToIdentity maps mirror Secret o to the identity it mirrors; it
// returns a single request for that identity, or nil if o is not a
// mirror.
func MirrorToIdentity(_ context.Context, o client.Object) []reconcile.Request {
	if !isMirror(o) {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: o.GetAnnotations()[inputs.IdentityAnnotation]}}}
}

// Reconcile sets Ready (SecretFound or SecretNotFound) and
// status.namespaces of the TerraformClusterIdentity named by req, using
// ctx for every call it makes. It returns a Result requeuing after
// RequeueAfter (or DefaultRequeueAfter), at most NotReadyRequeueAfter
// while Ready is False, and an error only from a failed read or status
// patch.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	id := &infrav1.TerraformClusterIdentity{}
	if err := r.Client.Get(ctx, req.NamespacedName, id); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !id.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	ctx = klog.NewContext(ctx, klog.LoggerWithValues(klog.FromContext(ctx), "TerraformClusterIdentity", klog.KObj(id)))
	ready := metav1.Condition{
		Type:               infrav1.ReadyCondition,
		Status:             metav1.ConditionTrue,
		Reason:             infrav1.SecretFoundReason,
		ObservedGeneration: id.Generation,
	}
	// A live read: bounded, so an API server that stalls holds a worker for
	// SourceReadTimeout at most, not until the manager's context ends.
	readCtx, cancel := context.WithTimeout(ctx, SourceReadTimeout)
	src, err := identity.SourceSecret(readCtx, r.APIReader, id)
	cancel()
	switch {
	case errors.Is(err, identity.ErrSecretNotFound):
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, infrav1.SecretNotFoundReason, err.Error()
	case err != nil:
		return ctrl.Result{}, err
	default:
		// Drop an ownerRef to the identity that earlier versions put on the
		// user's Secret (EnsureSourceOwnerRef does not add clusterctl's move
		// label: see its doc comment). This must not wait for an object
		// using the identity to reconcile: an identity nobody uses is
		// exactly the one the webhook lets be deleted, and the ref would
		// have the garbage collector delete the Secret with it.
		if err := identity.EnsureSourceOwnerRef(ctx, r.Client, id, src); err != nil {
			return ctrl.Result{}, err
		}
		if missing := identity.MissingKeys(id, src); len(missing) > 0 {
			ready.Status, ready.Reason = metav1.ConditionFalse, infrav1.CredentialsIncompleteReason
			ready.Message = fmt.Sprintf("Secret %s/%s lacks required key(s): %s", src.Namespace, src.Name, strings.Join(missing, ", "))
		}
	}
	namespaces, err := r.mirrorNamespaces(ctx, id.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	before := id.DeepCopy()
	meta.SetStatusCondition(&id.Status.Conditions, ready)
	id.Status.Namespaces = namespaces
	if !equalStatus(before.Status, id.Status) {
		if err := r.Client.Status().Patch(ctx, id, client.MergeFrom(before)); err != nil {
			if apierrors.IsNotFound(err) {
				klog.FromContext(ctx).V(shared.LogDebug).Info("Identity deleted before its status was patched; nothing to record")
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("terraformclusteridentity: patch status of %s: %w", id.Name, err)
		}
		r.emitReady(id, meta.FindStatusCondition(before.Status.Conditions, infrav1.ReadyCondition), ready)
	}
	after := r.RequeueAfter
	if after <= 0 {
		after = DefaultRequeueAfter
	}
	if ready.Status != metav1.ConditionTrue {
		after = min(after, NotReadyRequeueAfter)
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// emitReady emits one event on id when Ready changed status from prev (nil
// on a first reconcile) to ready: IdentitySecretFound (only after it was
// not found; a new identity whose Secret exists is quiet) or
// IdentitySecretNotFound. The message names the Secret, never its content.
func (r *Reconciler) emitReady(id *infrav1.TerraformClusterIdentity, prev *metav1.Condition, ready metav1.Condition) {
	if r.Recorder == nil || (prev != nil && prev.Status == ready.Status) {
		return
	}
	secret := id.Spec.SecretRef.Namespace + "/" + id.Spec.SecretRef.Name
	switch {
	case ready.Status == metav1.ConditionFalse && ready.Reason == infrav1.CredentialsIncompleteReason:
		r.Recorder.Eventf(id, nil, corev1.EventTypeWarning, shared.EventIdentitySecretNotFound, "Reconcile",
			"Credentials Secret %s is incomplete: %s; objects using this identity start no Job", secret, ready.Message)
	case ready.Status == metav1.ConditionFalse:
		r.Recorder.Eventf(id, nil, corev1.EventTypeWarning, shared.EventIdentitySecretNotFound, "Reconcile",
			"Credentials Secret %s not found; objects using this identity start no Job", secret)
	case prev != nil:
		r.Recorder.Eventf(id, nil, corev1.EventTypeNormal, shared.EventIdentitySecretFound, "Reconcile",
			"Credentials Secret %s found", secret)
	}
}

// mirrorNamespaces lists, sorted, the namespaces holding a mirror of the
// identity named name, using ctx for the reads. A namespace counts only when
// it holds the Secret named identity.MirrorName(name), labeled and annotated
// as a mirror of name, and an object in it uses the identity
// (identity.UsedInNamespace); the delete webhook refuses deletion on this
// list, so a Secret anyone can create must not be able to pin it. Both
// reads go through r.Cache: the mirrors' metadata by MirrorIdentityIndex,
// then the users of each mirror's namespace. It returns that namespace
// list, or an error from a read.
func (r *Reconciler) mirrorNamespaces(ctx context.Context, name string) ([]string, error) {
	secrets := &metav1.PartialObjectMetadataList{}
	secrets.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	if err := r.Cache.List(ctx, secrets, client.MatchingFields{shared.MirrorIdentityIndex: name}); err != nil {
		return nil, fmt.Errorf("terraformclusteridentity: list mirrors: %w", err)
	}
	mirrorName := identity.MirrorName(name)
	var out []string
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if s.Name != mirrorName || s.Annotations[inputs.IdentityAnnotation] != name || !s.DeletionTimestamp.IsZero() {
			continue
		}
		used, err := identity.UsedInNamespace(ctx, r.Cache, name, s.Namespace)
		if err != nil {
			return nil, fmt.Errorf("terraformclusteridentity: find users of %s in %s: %w", name, s.Namespace, err)
		}
		if used {
			out = append(out, s.Namespace)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// equalStatus reports whether statuses a and b are equal, ignoring
// condition transition times, which meta.SetStatusCondition keeps when the
// status does not change.
func equalStatus(a, b infrav1.TerraformClusterIdentityStatus) bool {
	if !slices.Equal(a.Namespaces, b.Namespaces) || len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		x, y := a.Conditions[i], b.Conditions[i]
		if x.Type != y.Type || x.Status != y.Status || x.Reason != y.Reason || x.Message != y.Message || x.ObservedGeneration != y.ObservedGeneration {
			return false
		}
	}
	return true
}
