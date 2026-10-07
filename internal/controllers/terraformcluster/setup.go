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
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util"
	capicontrollerutil "sigs.k8s.io/cluster-api/util/controller"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// ClusterPredicates passes pause transitions and the Cluster changes the
// cluster inputs hash covers: controlPlaneInitialized, the topology
// version, the control-plane
// endpoint and spec.clusterNetwork. CAPD's ClusterPausedTransitions alone
// would leave those re-applies to the resync period. All are update
// predicates: a Cluster create reaches the TerraformCluster through its
// ownerRef update. scheme and logger are passed through to the underlying
// CAPI predicates. It returns the combined predicate.Funcs.
func ClusterPredicates(scheme *runtime.Scheme, logger klog.Logger) predicate.Funcs {
	return predicates.Any(scheme, logger,
		predicates.ClusterPausedTransitions(scheme, logger),
		predicates.ClusterControlPlaneInitialized(scheme, logger),
		predicates.ClusterTopologyVersionChanged(scheme, logger),
		shared.ClusterEndpointChanged(),
		shared.ClusterNetworkChanged(),
	)
}

// SetupWithManager registers the controller with mgr and its watches,
// using ctx to build them and applying opts to the underlying controller.
// The watch-filter predicate is only on For and the Cluster watch: Jobs,
// Secrets, identities and Namespaces never carry the label, and a global
// event filter would drop Job completions. Managed Secrets are watched as
// metadata only (manager.CacheOptions), and a credential mirror only for
// its deletion (shared.ManagedSecretEvents); identities only for spec
// changes (shared.IdentitySpecChanged). It returns an error if the
// controller could not be built.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, opts controller.Options) error {
	logger := klog.FromContext(ctx)
	scheme, c := mgr.GetScheme(), mgr.GetClient()
	filter := predicates.ResourceHasFilterLabel(scheme, logger, r.Deps.WatchFilter)
	b := capicontrollerutil.NewControllerManagedBy(mgr, logger).
		For(&infrav1.TerraformCluster{}, builder.WithPredicates(filter)).
		Named("terraformcluster").
		WithOptions(opts).
		Watches(&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(util.ClusterToInfrastructureMapFunc(ctx,
				infrav1.GroupVersion.WithKind(state.KindTerraformCluster), c, &infrav1.TerraformCluster{})),
			ClusterPredicates(scheme, logger), filter).
		Owns(&batchv1.Job{}).
		// An approval is a spec change; the controller's own phase labels
		// and status writes are not.
		Owns(&infrav1.TerraformPlan{}, predicate.GenerationChangedPredicate{}).
		WatchesMetadata(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(shared.SecretToOwner(state.KindTerraformCluster)), shared.ManagedSecretEvents()).
		Watches(&infrav1.TerraformClusterIdentity{}, handler.EnqueueRequestsFromMapFunc(shared.IdentityToClusters(c)),
			shared.IdentitySpecChanged()).
		Watches(&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(shared.NamespaceToObjects(c, func() client.ObjectList { return &infrav1.TerraformClusterList{} })),
			shared.LabelsChanged()).
		Watches(&infrav1.TerraformMachine{}, handler.EnqueueRequestsFromMapFunc(shared.MachineToClusters(c)), shared.DeletesOnly()).
		Watches(&infrav1.TerraformMachinePool{}, handler.EnqueueRequestsFromMapFunc(shared.MachineToClusters(c)), shared.DeletesOnly())
	for _, src := range shared.VariablesSourceWatches(r.Deps, shared.VariablesSourceToClusters(c)) {
		b = b.WatchesRawSource(src)
	}
	if err := b.Complete(ctx, r); err != nil {
		return fmt.Errorf("terraformcluster: setup: %w", err)
	}
	return nil
}
