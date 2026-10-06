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

package terraformmachinepool

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

// ClusterToPools maps a Cluster to its TerraformMachinePools, as
// terraformmachine.ClusterToMachines does for machines. c and scheme are
// passed through to the underlying CAPI mapper. It returns the built
// handler.MapFunc, or an error if the mapper could not be built.
func ClusterToPools(c client.Client, scheme *runtime.Scheme) (handler.MapFunc, error) {
	m, err := util.ClusterToTypedObjectsMapper(c, &infrav1.TerraformMachinePoolList{}, scheme)
	if err != nil {
		return nil, fmt.Errorf("terraformmachinepool: cluster mapper: %w", err)
	}
	return m, nil
}

// SecretToPools maps our Secrets, read through c, to pools: a pool's own
// state, inputs or mirror Secret, and the cluster's base state Secret,
// whose exports and failure_domains outputs are pool inputs. It returns
// the combined handler.MapFunc.
func SecretToPools(c client.Reader) handler.MapFunc {
	return shared.Merge(shared.SecretToOwner(state.KindTerraformMachinePool), shared.ClusterStateSecretToPools(c))
}

// SetupWithManager registers the controller with mgr and its watches,
// using ctx to build them and applying opts to the underlying controller.
// The watch-filter predicate is only on For and the MachinePool, Cluster
// and TerraformCluster watches. A controller write to the pool (spec.providerIDList,
// status) re-triggers a reconcile but no apply: inputs are compared by
// hash, and they hold none of the mapped outputs. The bootstrap data
// Secret is deliberately not watched yet (it is not captf.io/managed, so
// the cache does not hold it): a rotated token reaches the pool at its
// next reconcile, at the latest the membership refresh interval later. It
// returns an error if the controller could not be built.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, opts controller.Options) error {
	logger := klog.FromContext(ctx)
	scheme, c := mgr.GetScheme(), mgr.GetClient()
	filter := predicates.ResourceHasFilterLabel(scheme, logger, r.Deps.WatchFilter)
	clusterToPools, err := ClusterToPools(c, scheme)
	if err != nil {
		return err
	}
	b := capicontrollerutil.NewControllerManagedBy(mgr, logger).
		For(&infrav1.TerraformMachinePool{}, builder.WithPredicates(filter)).
		Named("terraformmachinepool").
		WithOptions(opts).
		Watches(&clusterv1.MachinePool{},
			handler.EnqueueRequestsFromMapFunc(util.MachinePoolToInfrastructureMapFunc(ctx, infrav1.GroupVersion.WithKind(state.KindTerraformMachinePool))),
			filter).
		Watches(&clusterv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(clusterToPools),
			predicates.ClusterPausedTransitionsOrInfrastructureProvisioned(scheme, logger), filter).
		Watches(&infrav1.TerraformCluster{},
			handler.EnqueueRequestsFromMapFunc(shared.TerraformClusterToObjects(c, func() client.ObjectList { return &infrav1.TerraformMachinePoolList{} })),
			shared.InheritedPolicyChanged(), filter).
		Owns(&batchv1.Job{}).
		// An approval is a spec change; the controller's own phase labels
		// and status writes are not.
		Owns(&infrav1.TerraformPlan{}, predicate.GenerationChangedPredicate{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(SecretToPools(c)), shared.ManagedSecret()).
		Watches(&infrav1.TerraformClusterIdentity{}, handler.EnqueueRequestsFromMapFunc(shared.IdentityToPools(c))).
		Watches(&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(shared.NamespaceToObjects(c, func() client.ObjectList { return &infrav1.TerraformMachinePoolList{} })),
			shared.LabelsChanged())
	for _, src := range shared.VariablesSourceWatches(r.Deps, shared.VariablesSourceToPools(c)) {
		b = b.WatchesRawSource(src)
	}
	if err := b.Complete(ctx, r); err != nil {
		return fmt.Errorf("terraformmachinepool: setup: %w", err)
	}
	return nil
}
