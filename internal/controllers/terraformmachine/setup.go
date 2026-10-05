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

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// ClusterToMachines maps a Cluster to its TerraformMachines, as CAPD's
// DevMachine controller does. util.ClusterToInfrastructureMapFunc would map
// it to the TerraformCluster instead. c and scheme are passed through to
// the underlying CAPI mapper. It returns the built handler.MapFunc, or an
// error if the mapper could not be built.
func ClusterToMachines(c client.Client, scheme *runtime.Scheme) (handler.MapFunc, error) {
	m, err := util.ClusterToTypedObjectsMapper(c, &infrav1.TerraformMachineList{}, scheme)
	if err != nil {
		return nil, fmt.Errorf("terraformmachine: cluster mapper: %w", err)
	}
	return m, nil
}

// SecretToMachines maps our Secrets, read through c, to machines: a
// machine's own state, inputs or mirror Secret, and the cluster's base
// state Secret, whose exports output is a machine input. It returns the
// combined handler.MapFunc.
func SecretToMachines(c client.Reader) handler.MapFunc {
	return shared.Merge(shared.SecretToOwner(state.KindTerraformMachine), shared.ClusterStateSecretToMachines(c))
}

// SetupWithManager registers the controller with mgr and its watches,
// using ctx to build them and applying opts to the underlying controller.
// The watch-filter predicate is only on For and the Machine and Cluster
// watches. It returns an error if the controller could not be built.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, opts controller.Options) error {
	logger := klog.FromContext(ctx)
	scheme, c := mgr.GetScheme(), mgr.GetClient()
	filter := predicates.ResourceHasFilterLabel(scheme, logger, r.Deps.WatchFilter)
	clusterToMachines, err := ClusterToMachines(c, scheme)
	if err != nil {
		return err
	}
	b := capicontrollerutil.NewControllerManagedBy(mgr, logger).
		For(&infrav1.TerraformMachine{}, builder.WithPredicates(filter)).
		Named("terraformmachine").
		WithOptions(opts).
		Watches(&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(util.MachineToInfrastructureMapFunc(infrav1.GroupVersion.WithKind(state.KindTerraformMachine))),
			filter).
		Watches(&clusterv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(clusterToMachines),
			predicates.ClusterPausedTransitionsOrInfrastructureProvisioned(scheme, logger), filter).
		Owns(&batchv1.Job{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(SecretToMachines(c)), shared.ManagedSecret()).
		Watches(&infrav1.TerraformClusterIdentity{}, handler.EnqueueRequestsFromMapFunc(shared.IdentityToMachines(c))).
		Watches(&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(shared.NamespaceToObjects(c, func() client.ObjectList { return &infrav1.TerraformMachineList{} })),
			shared.LabelsChanged())
	for _, src := range shared.VariablesSourceWatches(r.Deps, shared.VariablesSourceToMachines(c)) {
		b = b.WatchesRawSource(src)
	}
	if err := b.Complete(ctx, r); err != nil {
		return fmt.Errorf("terraformmachine: setup: %w", err)
	}
	return nil
}
