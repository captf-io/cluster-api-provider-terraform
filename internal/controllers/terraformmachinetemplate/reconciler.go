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

package terraformmachinetemplate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/imageinspect"
)

// Retry bounds after a registry or auth failure.
const (
	RetryFloor   = 30 * time.Second
	RetryCeiling = 10 * time.Minute
)

// notInspected starts the CapacityResolved message of a template whose
// spec.capacity was applied while its image could not be inspected.
const notInspected = "image not inspected"

// Reconciler reconciles TerraformMachineTemplates. The manager's RBAC
// markers are in internal/controllers/rbac.go.
type Reconciler struct {
	Deps shared.Deps
}

// Reconcile resolves, using ctx, the capacity of the TerraformMachineTemplate
// named by req from its image, once per image reference (tags are not
// re-polled; a new image is a new template). It returns a Result requeuing
// with a backoff after a failed inspection, and an error only from a
// failed get or status patch.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	t := &infrav1.TerraformMachineTemplate{}
	if err := r.Deps.Client.Get(ctx, req.NamespacedName, t); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ctx = klog.NewContext(ctx, klog.LoggerWithValues(klog.FromContext(ctx), "TerraformMachineTemplate", klog.KObj(t)))
	if Resolved(t) {
		return ctrl.Result{}, nil
	}

	helper, err := patch.NewHelper(t, r.Deps.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("patch helper: %w", err)
	}
	defer func() {
		if err := helper.Patch(ctx, t, patch.WithOwnedConditions{Conditions: []string{infrav1.CapacityResolvedCondition, infrav1.VariablesValidCondition}}); err != nil {
			reterr = kerrors.NewAggregate([]error{reterr, fmt.Errorf("patch: %w", err)})
		}
	}()
	return r.resolve(ctx, t)
}

// Resolved reports whether t's status already describes its spec: the same
// image, the capacity from the same origin (spec.capacity as it stands, or
// the image), resolved or not declared, or a label invalid for that same
// image (the tag is not re-polled, so a retry cannot change it), and its
// variables checked against the image's schema (valid or invalid;
// Unknown, a source that is not there yet, is retried).
func Resolved(t *infrav1.TerraformMachineTemplate) bool {
	if t.Status.CapacitySource.Image != t.Spec.Template.Spec.Source.Image {
		return false
	}
	if len(t.Spec.Capacity) > 0 {
		if t.Status.CapacitySource.Source != infrav1.CapacitySourceSpec || !equality.Semantic.DeepEqual(t.Status.Capacity, t.Spec.Capacity) {
			return false
		}
	} else if t.Status.CapacitySource.Source != infrav1.CapacitySourceImage {
		return false
	}
	c := conditions.Get(t, infrav1.CapacityResolvedCondition)
	v := conditions.Get(t, infrav1.VariablesValidCondition)
	return c != nil && (c.Status == metav1.ConditionTrue || c.Reason == infrav1.CapacityLabelInvalidReason) &&
		v != nil && v.Status != metav1.ConditionUnknown
}

// resolve inspects t's spec image using ctx, and sets its status.capacity,
// status.nodeInfo and CapacityResolved condition from the result, emitting
// an event when the capacity changes or the inspection fails. It returns a
// Result requeuing with a backoff on a failed inspection, and an error
// only from resolving the image's pull Secrets.
func (r *Reconciler) resolve(ctx context.Context, t *infrav1.TerraformMachineTemplate) (ctrl.Result, error) {
	spec := t.Spec.Template.Spec
	// Only the template's own jobs.imagePullSecrets: a template has no
	// TerraformCluster, so the cluster's defaults.jobs cannot take part.
	var names []string
	if spec.Jobs != nil {
		for _, s := range spec.Jobs.ImagePullSecrets {
			names = append(names, s.Name)
		}
	}
	keychain, missing, err := imageinspect.PullSecretsKeychain(ctx, r.Deps.APIReader, t.Namespace, names)
	if err != nil {
		return ctrl.Result{}, err
	}

	cfg, err := r.Deps.Inspector.Config(ctx, spec.Source.Image, keychain, imageinspect.DefaultPlatform())
	if err != nil {
		msg := InspectFailure(spec.Source.Image, err)
		if len(missing) > 0 {
			msg += "; pull Secret(s) not found: " + strings.Join(missing, ", ")
		}
		// The raw error goes to the log only: registry responses can carry
		// arbitrary bodies that do not belong in a condition or an event.
		klog.FromContext(ctx).V(shared.LogFlow).Info("Image inspection failed", "image", spec.Source.Image, "error", err.Error())
		retry := failedRetry(t, r.Deps.Clock.Now())
		prev := conditions.Get(t, infrav1.CapacityResolvedCondition)
		firstFailure := prev == nil || (prev.Reason != infrav1.ImageInspectFailedReason && !strings.HasPrefix(prev.Message, notInspected))
		// Without the image there is no schema to check the variables
		// against, with the override or without.
		conditions.Set(t, metav1.Condition{
			Type: infrav1.VariablesValidCondition, Status: metav1.ConditionUnknown, Reason: infrav1.VariablesSchemaUnavailableReason, Message: msg,
		})
		if len(t.Spec.Capacity) > 0 {
			// The override does not depend on the registry: apply it, keep
			// the last known nodeInfo, and keep retrying for the image.
			t.Status.Capacity = t.Spec.Capacity.DeepCopy()
			t.Status.CapacitySource = infrav1.CapacitySource{Source: infrav1.CapacitySourceSpec}
			msg = notInspected + ": " + msg
			if firstFailure {
				r.Deps.Emit(t, corev1.EventTypeWarning, shared.EventImageInspectFailed, "Inspect", "%s", msg)
				r.Deps.Metrics.ImageInspectError(infrav1.ImageInspectFailedReason)
			}
			conditions.Set(t, metav1.Condition{
				Type: infrav1.CapacityResolvedCondition, Status: metav1.ConditionTrue, Reason: infrav1.CapacityResolvedReason, Message: msg,
			})
			return ctrl.Result{RequeueAfter: retry}, nil
		}
		if firstFailure {
			r.Deps.Emit(t, corev1.EventTypeWarning, shared.EventImageInspectFailed, "Inspect", "%s", msg)
			r.Deps.Metrics.ImageInspectError(infrav1.ImageInspectFailedReason)
		}
		conditions.Set(t, metav1.Condition{
			Type: infrav1.CapacityResolvedCondition, Status: metav1.ConditionFalse, Reason: infrav1.ImageInspectFailedReason, Message: msg,
		})
		return ctrl.Result{RequeueAfter: retry}, nil
	}

	// spec.capacity wins entirely: the image's capacity label is not read
	// (so an invalid one cannot fail the condition), while node-info still
	// comes from the image.
	imageLabels, source := cfg.Labels, infrav1.CapacitySourceImage
	if len(t.Spec.Capacity) > 0 {
		imageLabels = maps.Clone(cfg.Labels)
		delete(imageLabels, imageinspect.CapacityLabel)
		source = infrav1.CapacitySourceSpec
	}
	res := imageinspect.Resolve(imageLabels)
	if source == infrav1.CapacitySourceSpec {
		res.Capacity = t.Spec.Capacity.DeepCopy()
	}
	if !equality.Semantic.DeepEqual(t.Status.Capacity, res.Capacity) || !equality.Semantic.DeepEqual(t.Status.NodeInfo, res.NodeInfo) {
		r.Deps.Emit(t, corev1.EventTypeNormal, shared.EventCapacityResolved, "Inspect", "Capacity from %s: %s", capacityOrigin(source, spec.Source.Image), capacityNote(res.Capacity))
	}
	t.Status.Capacity, t.Status.NodeInfo = res.Capacity, res.NodeInfo
	t.Status.CapacitySource = infrav1.CapacitySource{Source: source, Image: spec.Source.Image}
	if prev := conditions.Get(t, infrav1.CapacityResolvedCondition); res.Condition.Reason == infrav1.CapacityLabelInvalidReason &&
		(prev == nil || prev.Reason != infrav1.CapacityLabelInvalidReason) {
		r.Deps.Metrics.ImageInspectError(infrav1.CapacityLabelInvalidReason)
	}
	conditions.Set(t, res.Condition)
	return r.validateVariables(ctx, t, cfg)
}

// validateVariables sets t's VariablesValid condition: the template's
// merged variables and variablesFrom against the variables schema in cfg,
// the image config just read (also remembered in the shared cache, so the
// admission webhook knows it). A source that is missing leaves it Unknown
// and requeues; an image without a usable schema checks nothing. ctx
// bounds the source reads. It returns a Result requeuing for a missing
// source, and an error only from reading a source.
func (r *Reconciler) validateVariables(ctx context.Context, t *infrav1.TerraformMachineTemplate, cfg *imageinspect.Config) (ctrl.Result, error) {
	spec := t.Spec.Template.Spec
	set := func(status metav1.ConditionStatus, reason, msg string) {
		conditions.Set(t, metav1.Condition{Type: infrav1.VariablesValidCondition, Status: status, Reason: reason, Message: msg})
	}
	schema, schemaErr := r.Deps.Schemas.Remember(spec.Source.Image, cfg)
	vars, gate, err := shared.ResolveVariables(ctx, r.Deps.APIReader, t.Namespace, contract.RoleMachine,
		shared.VariablesSpec{Inline: spec.Variables, From: spec.VariablesFrom})
	switch {
	case err != nil:
		return ctrl.Result{}, err
	case gate != nil && gate.Reason == infrav1.VariablesSourceNotFoundReason:
		set(metav1.ConditionUnknown, infrav1.VariablesSourcePendingReason, gate.Message)
		return ctrl.Result{RequeueAfter: RetryFloor}, nil
	case gate != nil:
		set(metav1.ConditionFalse, infrav1.VariablesRejectedReason, gate.Message)
	case schemaErr != nil || schema == nil:
		set(metav1.ConditionTrue, infrav1.VariablesSchemaNotDeclaredReason, "The image declares no usable "+imageinspect.VariablesSchemaLabel+"; the variables are not checked")
	default:
		if g := shared.VariablesSchemaGateOf(schema, vars); g != nil {
			set(metav1.ConditionFalse, infrav1.VariablesRejectedReason, g.Message)
		} else {
			set(metav1.ConditionTrue, infrav1.VariablesValidReason, "")
		}
	}
	return ctrl.Result{}, nil
}

// capacityOrigin returns where a capacity of the given source came from, for
// an event message: "spec.capacity", or "image <image>" for the image.
func capacityOrigin(source infrav1.CapacitySourceKind, image string) string {
	if source == infrav1.CapacitySourceSpec {
		return "spec.capacity"
	}
	return "image " + image
}

// capacityNote lists resource list c as "cpu=4, memory=16Gi", sorted by
// name; it returns "none declared" for an empty c.
func capacityNote(c corev1.ResourceList) string {
	if len(c) == 0 {
		return "none declared"
	}
	parts := make([]string, 0, len(c))
	for _, n := range slices.Sorted(maps.Keys(c)) {
		q := c[n]
		parts = append(parts, string(n)+"="+q.String())
	}
	return strings.Join(parts, ", ")
}

// InspectFailure returns the condition message for a failed inspection of
// image: the image name and a class of err's failure (unauthorized, not
// found, unreachable, invalid, other), never the registry's or the
// network's own error text.
func InspectFailure(image string, err error) string {
	var (
		terr *transport.Error
		bad  *name.ErrBadName
		nerr net.Error
	)
	switch {
	case errors.As(err, &terr) && (terr.StatusCode == http.StatusUnauthorized || terr.StatusCode == http.StatusForbidden):
		return "Reading image " + image + " failed: unauthorized; check the pull Secrets"
	case errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound:
		return "Reading image " + image + " failed: not found in the registry"
	case errors.As(err, &bad), errors.Is(err, imageinspect.ErrNoLinuxImage):
		return "Reading image " + image + " failed: invalid reference or image (no linux image)"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &nerr):
		return "Reading image " + image + " failed: registry unreachable"
	case errors.As(err, &terr):
		return fmt.Sprintf("Reading image %s failed: registry answered HTTP %d", image, terr.StatusCode)
	}
	return "Reading image " + image + " failed; the manager log has the details"
}

// failedRetry roughly doubles the retry while t's inspection keeps
// failing: it returns the time since the failures began, measured from
// now, clamped to [RetryFloor, RetryCeiling].
func failedRetry(t *infrav1.TerraformMachineTemplate, now time.Time) time.Duration {
	c := conditions.Get(t, infrav1.CapacityResolvedCondition)
	if c == nil || (c.Reason != infrav1.ImageInspectFailedReason && !strings.HasPrefix(c.Message, notInspected)) {
		return RetryFloor
	}
	return min(max(now.Sub(c.LastTransitionTime.Time), RetryFloor), RetryCeiling)
}

// SetupWithManager registers the controller with mgr, applying opts to the
// underlying controller: the template and its generation only (a fixed pull
// Secret is picked up by the retry; a spec.capacity change bumps the
// generation). It returns
// an error if the controller could not be built.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, opts controller.Options) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.TerraformMachineTemplate{}, builder.WithPredicates(
			predicates.ResourceHasFilterLabel(mgr.GetScheme(), mgr.GetLogger(), r.Deps.WatchFilter),
			predicate.GenerationChangedPredicate{},
		)).
		Named("terraformmachinetemplate").
		WithOptions(opts).
		Complete(r)
	if err != nil {
		return fmt.Errorf("terraformmachinetemplate: setup: %w", err)
	}
	return nil
}
