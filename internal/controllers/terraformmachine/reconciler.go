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

package terraformmachine

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Reconciler reconciles TerraformMachines. The manager's RBAC markers are in
// internal/controllers/rbac.go.
type Reconciler struct {
	Deps shared.Deps
}

// Reconcile runs the shared flow with the machine adapter for the
// TerraformMachine named by req, using ctx for every call it makes. It
// returns shared.Reconcile's Result and error, or a zero Result and no
// error once the object is gone.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	tm := &infrav1.TerraformMachine{}
	if err := r.Deps.Client.Get(ctx, req.NamespacedName, tm); err != nil {
		if apierrors.IsNotFound(err) {
			// Gone without our cleanup: drop its gauges.
			r.Deps.Metrics.DeleteObject(state.KindTerraformMachine, req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ctx = klog.NewContext(ctx, klog.LoggerWithValues(klog.FromContext(ctx), "TerraformMachine", klog.KObj(tm)))
	res, owner, err := shared.ReconcileWithOwner(ctx, r.Deps, newAdapter(r.Deps, tm))
	if annotations.IsExternallyManaged(tm) {
		return res, err
	}
	// The shared flow may have patched InfrastructureHealthy to Healthy
	// before failing, and a paused object touches no Machine except to
	// take back a request that no longer applies: withdrawing is always
	// safe, setting never happens on these paths.
	if err != nil || conditions.IsTrue(tm, clusterv1.PausedCondition) {
		if werr := WithdrawRemediation(ctx, r.Deps, owner.Machine, tm); werr != nil {
			klog.FromContext(ctx).Error(werr, "Withdrawing the remediation request failed; the next reconcile retries")
		}
		return res, err
	}
	// After the shared flow, which has read the health and counted the
	// sample, the remediation annotation follows the health.
	if err := SyncRemediation(ctx, r.Deps, owner.Machine, tm, owner.InfraCluster); err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}

// WithdrawRemediation is the withdraw-only half of SyncRemediation: it
// removes the remediation annotations this controller set on machine,
// through d's client using ctx, once tm's instance is Healthy again and
// the Machine is not being deleted. It never sets them, and never removes
// an annotation someone else set. A nil machine is ignored. It returns an
// error only from a failed patch.
func WithdrawRemediation(ctx context.Context, d shared.Deps, machine *clusterv1.Machine, tm *infrav1.TerraformMachine) error {
	if machine == nil {
		return nil
	}
	if _, annotated := machine.Annotations[clusterv1.RemediateMachineAnnotation]; annotated &&
		machine.Annotations[RequestedByAnnotation] != "" && Recovered(tm) && machine.DeletionTimestamp.IsZero() {
		return patchRemediation(ctx, d, machine, tm, "")
	}
	return nil
}
