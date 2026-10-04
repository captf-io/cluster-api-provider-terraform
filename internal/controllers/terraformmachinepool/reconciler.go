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

package terraformmachinepool

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Reconciler reconciles TerraformMachinePools. The manager's RBAC markers
// are in internal/controllers/rbac.go.
type Reconciler struct {
	Deps shared.Deps
}

// Reconcile runs the shared flow with the pool adapter for the
// TerraformMachinePool named by req, using ctx for every call it makes,
// then writes an autoscaled pool's observed replicas back to its
// MachinePool (SyncReplicas). It returns shared.ReconcileWithOwner's Result
// and error, a zero Result and SyncReplicas' error when that fails, or a
// zero Result and no error once the object is gone.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	tmp := &infrav1.TerraformMachinePool{}
	if err := r.Deps.Client.Get(ctx, req.NamespacedName, tmp); err != nil {
		if apierrors.IsNotFound(err) {
			// Gone without our cleanup: drop its gauges.
			r.Deps.Metrics.DeleteObject(state.KindTerraformMachinePool, req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ctx = klog.NewContext(ctx, klog.LoggerWithValues(klog.FromContext(ctx), "TerraformMachinePool", klog.KObj(tmp)))
	res, owner, err := shared.ReconcileWithOwner(ctx, r.Deps, newAdapter(r.Deps, tmp))
	if err != nil {
		return res, err
	}
	// After the shared flow, which has mapped this pass's replicas output
	// into status.replicas. SyncReplicas itself skips a paused or deleting
	// pool.
	if err := SyncReplicas(ctx, r.Deps, owner.MachinePool, tmp); err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}
