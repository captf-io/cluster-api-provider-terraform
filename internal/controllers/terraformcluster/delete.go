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

package terraformcluster

import (
	"context"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
)

// DeletionBlocked holds the destroy while any TerraformMachine or
// TerraformMachinePool of the cluster exists in the namespace, using ctx for
// the lists. The Lists are uncached and unfiltered, so machines and pools of
// another --watch-filter instance count too. The cluster name is a.obj's
// cluster-name label, else owner's Cluster's name; with neither nothing can
// reference the cluster. The DependentsExist message counts and names
// them (dependents). It returns whether deletion is blocked, or an error
// from a list.
func (a *adapter) DeletionBlocked(ctx context.Context, owner shared.OwnerInfo) (bool, error) {
	name := a.obj.Labels[clusterv1.ClusterNameLabel]
	if name == "" && owner.Cluster != nil {
		name = owner.Cluster.Name
	}
	if name != "" {
		opts := []client.ListOption{client.InNamespace(a.obj.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: name}}
		machines := &infrav1.TerraformMachineList{}
		if err := a.d.APIReader.List(ctx, machines, opts...); err != nil {
			return false, fmt.Errorf("list TerraformMachines: %w", err)
		}
		pools := &infrav1.TerraformMachinePoolList{}
		if err := a.d.APIReader.List(ctx, pools, opts...); err != nil {
			return false, fmt.Errorf("list TerraformMachinePools: %w", err)
		}
		var parts []string
		if len(machines.Items) > 0 {
			names := make([]string, 0, len(machines.Items))
			for i := range machines.Items {
				names = append(names, machines.Items[i].Name)
			}
			parts = append(parts, dependents("TerraformMachine", names))
		}
		if len(pools.Items) > 0 {
			names := make([]string, 0, len(pools.Items))
			for i := range pools.Items {
				names = append(names, pools.Items[i].Name)
			}
			parts = append(parts, dependents("TerraformMachinePool", names))
		}
		if len(parts) > 0 {
			conditions.Set(a.obj, metav1.Condition{
				Type: infrav1.DeletionBlockedCondition, Status: metav1.ConditionTrue, Reason: infrav1.DependentsExistReason,
				Message: fmt.Sprintf("%s of cluster %s still exist", strings.Join(parts, " and "), name),
			})
			return true, nil
		}
	}
	conditions.Set(a.obj, metav1.Condition{
		Type: infrav1.DeletionBlockedCondition, Status: metav1.ConditionFalse, Reason: infrav1.NotBlockedReason,
	})
	return false, nil
}

// maxNamedDependents caps the names of each kind a DeletionBlocked
// message lists; the rest are counted.
const maxNamedDependents = 3

// dependents returns "<n> <kind>(s) (<names>)" for names, the names of
// kind's objects that block the deletion, which it sorts so the message
// does not change with the List order: the first maxNamedDependents, then
// "and <rest> more".
func dependents(kind string, names []string) string {
	slices.Sort(names)
	shown := strings.Join(names[:min(len(names), maxNamedDependents)], ", ")
	if rest := len(names) - maxNamedDependents; rest > 0 {
		shown += fmt.Sprintf(" and %d more", rest)
	}
	return fmt.Sprintf("%d %s(s) (%s)", len(names), kind, shown)
}
