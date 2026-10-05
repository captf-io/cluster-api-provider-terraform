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
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Reconciler reconciles TerraformClusters. The manager's RBAC markers are in
// internal/controllers/rbac.go.
type Reconciler struct {
	Deps shared.Deps
}

// Reconcile runs the shared flow with the cluster adapter for the
// TerraformCluster named by req, using ctx for every call it makes. It
// returns shared.Reconcile's Result and error, or a zero Result and no
// error once the object is gone.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	tc := &infrav1.TerraformCluster{}
	if err := r.Deps.Client.Get(ctx, req.NamespacedName, tc); err != nil {
		if apierrors.IsNotFound(err) {
			// Gone without our cleanup (e.g. deleted while the manager was
			// down after the finalizer went): drop its gauges.
			r.Deps.Metrics.DeleteObject(state.KindTerraformCluster, req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ctx = klog.NewContext(ctx, klog.LoggerWithValues(klog.FromContext(ctx), "TerraformCluster", klog.KObj(tc)))
	return shared.Reconcile(ctx, r.Deps, newAdapter(r.Deps, tc))
}
