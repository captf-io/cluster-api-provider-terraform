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

package terraformcluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/outputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/ownership"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Finalizer is the TerraformCluster finalizer.
const Finalizer = "terraformcluster.infrastructure.cluster.x-k8s.io"

// adapter implements shared.Kind for one TerraformCluster.
type adapter struct {
	d   shared.Deps
	obj *infrav1.TerraformCluster
}

var _ shared.Kind = &adapter{}

// newAdapter returns an adapter wrapping obj, using d for every call that
// needs a client, recorder or the other shared dependencies.
func newAdapter(d shared.Deps, obj *infrav1.TerraformCluster) *adapter {
	return &adapter{d: d, obj: obj}
}

// Object returns the wrapped TerraformCluster as a shared.Object.
func (a *adapter) Object() shared.Object { return a.obj }

// Kind returns state.KindTerraformCluster.
func (a *adapter) Kind() string { return state.KindTerraformCluster }

// Role returns contract.RoleCluster.
func (a *adapter) Role() contract.Role { return contract.RoleCluster }

// Finalizer returns Finalizer.
func (a *adapter) Finalizer() string { return Finalizer }

// Mutable reports true: a spec or input change re-applies.
func (a *adapter) Mutable() bool { return true }

// RefreshAfterApply reports false: the cluster's outputs are read after its
// apply, and nothing waits on a faster refresh.
func (a *adapter) RefreshAfterApply() bool { return false }

// Spec returns the wrapped TerraformCluster's spec as a shared.SpecView.
func (a *adapter) Spec() shared.SpecView {
	s := a.obj.Spec
	return shared.SpecView{WorkspaceSpec: s.WorkspaceSpec, Drift: s.Drift, ApplyPolicy: s.ApplyPolicy, MaxActiveJobs: s.MaxActiveJobs}
}

// Status returns the wrapped TerraformCluster's status as a
// shared.CommonStatus.
func (a *adapter) Status() shared.CommonStatus {
	return shared.CommonStatus{WorkspaceStatus: &a.obj.Status.WorkspaceStatus, PendingPlanRef: &a.obj.Status.PendingPlanRef}
}

// Owner looks up the owning Cluster (util.GetOwnerCluster), using ctx for
// the read. A dangling ownerRef is "owner gone", never "no owner":
// deletion still destroys. A Cluster ownerRef whose Cluster does not
// reference this TerraformCluster back (ownership.ClusterOwnerMismatch: wrong or
// missing spec.infrastructureRef, a UID mismatch, or a disagreeing
// cluster-name label) is a forged or stale ownerRef: OwnerMismatch, treated
// as no valid owner so no Job runs and nothing is written to the named
// Cluster; deletion still destroys from the durable inputs, exactly as for
// an owner that is gone. The TerraformCluster is its own source of
// defaults. It returns the resolved shared.OwnerInfo, or an error from a
// lookup failure other than not-found.
func (a *adapter) Owner(ctx context.Context) (shared.OwnerInfo, error) {
	owner := shared.OwnerInfo{InfraCluster: a.obj}
	cluster, err := util.GetOwnerCluster(ctx, a.d.Client, a.obj.ObjectMeta)
	switch {
	case apierrors.IsNotFound(err):
		owner.HasOwnerRef, owner.OwnerGone = true, true
	case err != nil:
		return shared.OwnerInfo{}, fmt.Errorf("get owner Cluster: %w", err)
	case cluster != nil:
		if msg := ownership.ClusterOwnerMismatch(cluster, a.obj, a.obj.Labels[clusterv1.ClusterNameLabel]); msg != "" {
			owner.HasOwnerRef = true
			owner.Gate = &shared.Gate{Status: metav1.ConditionFalse, Reason: infrav1.OwnerMismatchReason, Message: msg}
			return owner, nil
		}
		owner.HasOwnerRef, owner.Cluster = true, cluster
	}
	return owner, nil
}

// BuildInputs builds the cluster inputs from owner (the resolved owning
// Cluster) and a.obj, using ctx to resolve variables, and recording the
// endpoint source user before the first apply once no gate remains. durable (nil before the
// first apply) holds the latched control_plane_initialized. The user
// variables are resolved on every build: they are hashed, so a changed
// source re-applies. It returns the built inputs, or a non-nil Gate (with
// nil inputs) while owner is not yet resolved, or an error from resolving
// variables.
func (a *adapter) BuildInputs(ctx context.Context, owner shared.OwnerInfo, durable *inputs.Durable) (any, *shared.Gate, error) {
	if owner.Cluster == nil {
		if owner.OwnerGone {
			return nil, &shared.Gate{Status: metav1.ConditionFalse, Reason: infrav1.OwnerNotFoundReason, Message: "The owner Cluster is gone"}, nil
		}
		return nil, &shared.Gate{Status: metav1.ConditionUnknown, Reason: infrav1.WaitingForOwnerReason, Message: "Waiting for the owner Cluster"}, nil
	}
	source := a.obj.Annotations[EndpointSourceAnnotation]
	ep, setSource := EndpointInput(source, durable == nil,
		owner.Cluster.Spec.ControlPlaneEndpoint, a.obj.Spec.ControlPlaneEndpoint)
	if durable == nil && source == EndpointSourceUser && ep == nil {
		// The user endpoint is gone before any apply stored it: the
		// module owns the endpoint again.
		delete(a.obj.Annotations, EndpointSourceAnnotation)
	}
	vars, gate, err := shared.ResolveVariables(ctx, a.d.APIReader, a.obj.Namespace, contract.RoleCluster,
		shared.VariablesSpec{Inline: a.obj.Spec.Variables, From: a.obj.Spec.VariablesFrom})
	if err != nil || gate != nil {
		return nil, gate, err
	}
	// Recorded only now, once every gate has passed and the inputs are
	// final, so a gated pass leaves no provenance behind.
	if setSource != "" {
		a.setSource(setSource)
	}
	in := ClusterInputs(owner.Cluster, a.obj, inputs.LastControlPlaneInitialized(durable.LastAttempt()), ep)
	in.Variables = vars
	return in, nil, nil
}

// ApplyOutputs maps st's decoded cluster outputs onto a.obj:
// spec.controlPlaneEndpoint under the provenance rule (using durable's
// latched last-rendered-null flag to decide whether the module owns the
// endpoint), status.failureDomains, and EndpointAvailable (using owner, the
// resolved owning Cluster, and a.obj's own already-latched
// status.initialization.provisioned, to decide provisioned state). It
// returns the decode Result, the decoded health (nil if the health output
// has a problem), and always a nil error.
func (a *adapter) ApplyOutputs(_ context.Context, owner shared.OwnerInfo, st *state.State, durable *inputs.Durable) (outputs.Result, *contract.Health, error) {
	out, res := outputs.DecodeCluster(st)
	if ep := ModuleEndpoint(a.obj.Annotations[EndpointSourceAnnotation], inputs.LastControlPlaneEndpointNull(durable.AppliedOrAttempt()),
		out.ControlPlaneEndpoint, a.obj.Spec.ControlPlaneEndpoint); ep != nil {
		a.obj.Spec.ControlPlaneEndpoint = ep
		a.setSource(EndpointSourceModule)
		a.d.Emit(a.obj, corev1.EventTypeNormal, shared.EventControlPlaneEndpointSet, "Reconcile",
			"spec.controlPlaneEndpoint set to %s:%d from the module output", ep.Host, ep.Port)
	}
	// A violation keeps the previous list; null or [] clears it.
	if !res.Concerns("failure_domains") {
		fds := outputs.FailureDomains(out.FailureDomains)
		if !equality.Semantic.DeepEqual(fds, a.obj.Status.FailureDomains) {
			a.d.Emit(a.obj, corev1.EventTypeNormal, shared.EventFailureDomainsChanged, "Reconcile",
				"status.failureDomains: %s", failureDomainNames(fds))
		}
		a.obj.Status.FailureDomains = fds
	}
	if !res.Concerns("exports") {
		a.publishExports(out.Exports)
	}
	var health *contract.Health
	if !res.Concerns("health") {
		health = &out.Health
	}
	prev := a.obj.Status.Initialization.Provisioned
	a.setEndpointAvailable(owner, shared.Provisioned(prev != nil && *prev, st.InputsHash != "", res.Valid(), health), out.ControlPlaneEndpoint)
	return res, health, nil
}

// publishExports copies exports, the decoded exports output, into
// status.exports in compact form. Over infrav1.MaxPublishedExportsBytes it
// clears the field and warns, once per transition: when the field held a
// value, or when this is the first pass over a new generation (the
// reconciler's patch helper records status.observedGeneration after the
// pass); an unchanged oversize output on later passes stays quiet.
func (a *adapter) publishExports(exports json.RawMessage) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, exports); err != nil {
		return // DecodeCluster validated it; keep the previous value
	}
	cur := a.obj.Status.Exports.Raw
	if buf.Len() > infrav1.MaxPublishedExportsBytes {
		if len(cur) > 0 || a.obj.Status.ObservedGeneration != a.obj.Generation {
			a.d.Emit(a.obj, corev1.EventTypeWarning, shared.EventExportsNotPublished, "Reconcile",
				"status.exports not published: the exports output is %d bytes, over the %d-byte limit; machines and pools still read it from the state",
				buf.Len(), infrav1.MaxPublishedExportsBytes)
		}
		a.obj.Status.Exports = runtime.RawExtension{}
		return
	}
	if !bytes.Equal(cur, buf.Bytes()) {
		a.obj.Status.Exports = runtime.RawExtension{Raw: buf.Bytes()}
	}
}

// failureDomainNames lists fds' names for an event; it returns that list
// joined with ", " and prefixed with the count, or "none" for an empty
// fds.
func failureDomainNames(fds []clusterv1.FailureDomain) string {
	if len(fds) == 0 {
		return "none"
	}
	names := make([]string, 0, len(fds))
	for _, fd := range fds {
		names = append(names, fd.Name)
	}
	return fmt.Sprintf("%d: %s", len(fds), strings.Join(names, ", "))
}

// setEndpointAvailable sets EndpointAvailable on a.obj from owner (the
// resolved owning Cluster), provisioned and output (the decoded module
// endpoint, if valid): True with any valid endpoint; False/
// WaitingForEndpoint once provisioned without one; unset before that (the
// type has no Unknown reason).
func (a *adapter) setEndpointAvailable(owner shared.OwnerInfo, provisioned bool, output *contract.Endpoint) {
	valid := (a.obj.Spec.ControlPlaneEndpoint != nil && a.obj.Spec.ControlPlaneEndpoint.IsValid()) ||
		(output != nil && output.Host != "" && output.Port != 0) ||
		(owner.Cluster != nil && owner.Cluster.Spec.ControlPlaneEndpoint.IsValid())
	c := metav1.Condition{Type: infrav1.EndpointAvailableCondition}
	switch {
	case valid:
		c.Status, c.Reason = metav1.ConditionTrue, infrav1.EndpointAvailableReason
	case provisioned:
		c.Status, c.Reason = metav1.ConditionFalse, infrav1.WaitingForEndpointReason
		c.Message = "Provisioned, but neither the module's control_plane_endpoint output nor Cluster.spec.controlPlaneEndpoint is set"
	default:
		return
	}
	conditions.Set(a.obj, c)
}

// setSource records source as a.obj's endpoint provenance annotation.
func (a *adapter) setSource(source string) {
	if a.obj.Annotations == nil {
		a.obj.Annotations = map[string]string{}
	}
	a.obj.Annotations[EndpointSourceAnnotation] = source
}
