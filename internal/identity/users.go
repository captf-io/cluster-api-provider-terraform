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

package identity

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// ClusterIndex finds the TerraformCluster a TerraformMachine or
// TerraformMachinePool inherits its defaults from: the TerraformCluster of
// its namespace carrying the same cluster.x-k8s.io/cluster-name label. It is what the controllers'
// identity mappers use, and it needs no CAPI Cluster read.
type ClusterIndex map[client.ObjectKey]*infrav1.TerraformCluster

// IndexClusters indexes clusters by namespace and cluster-name label. Clusters
// without the label are left out. When several share a label, the one with
// the lowest name wins, so the choice does not depend on list order. It
// returns the resulting ClusterIndex.
func IndexClusters(clusters []infrav1.TerraformCluster) ClusterIndex {
	ix := ClusterIndex{}
	sorted := slices.Clone(clusters)
	slices.SortFunc(sorted, func(a, b infrav1.TerraformCluster) int { return strings.Compare(a.Name, b.Name) })
	for i := range sorted {
		tc := &sorted[i]
		name := tc.Labels[clusterv1.ClusterNameLabel]
		if name == "" {
			continue
		}
		key := client.ObjectKey{Namespace: tc.Namespace, Name: name}
		if _, ok := ix[key]; !ok {
			ix[key] = tc
		}
	}
	return ix
}

// For returns the TerraformCluster of m, a TerraformMachine or
// TerraformMachinePool, or nil when m has no cluster-name label or no
// TerraformCluster carries it.
func (ix ClusterIndex) For(m client.Object) *infrav1.TerraformCluster {
	name := m.GetLabels()[clusterv1.ClusterNameLabel]
	if name == "" {
		return nil
	}
	return ix[client.ObjectKey{Namespace: m.GetNamespace(), Name: name}]
}

// FirstUser returns one TerraformCluster, TerraformMachine or
// TerraformMachinePool, in any namespace, that uses the identity name: a
// cluster through its identityRef, a machine or pool through its own
// identityRef or, without one, its cluster's fallback (EffectiveName). It
// returns nil when nothing uses it. Objects
// being deleted count: their destroy still needs the credentials. reader
// should be uncached, so objects outside the manager's cache scope count too.
// ctx bounds the List calls this makes. It also returns an error from a
// failed list. See Users for how a machine's or pool's cluster is found.
func FirstUser(ctx context.Context, reader client.Reader, name string) (client.Object, error) {
	users, err := findUsers(ctx, reader, name, true)
	if err != nil || len(users) == 0 {
		return nil, err
	}
	return users[0], nil
}

// UsedInNamespace reports whether a TerraformCluster, TerraformMachine or
// TerraformMachinePool in namespace ns uses the identity name, as Users
// defines use, reading through reader with ctx. Every List is scoped to
// ns, so reader may be the manager's cache: a machine's or pool's
// TerraformCluster and CAPI Cluster are in its own namespace. It returns
// an error from a failed list.
func UsedInNamespace(ctx context.Context, reader client.Reader, name, ns string) (bool, error) {
	users, err := findUsers(ctx, reader, name, true, client.InNamespace(ns))
	return len(users) > 0, err
}

// Users returns every TerraformCluster, TerraformMachine and
// TerraformMachinePool that uses the identity name, as FirstUser defines
// use, bounded by ctx and read through reader. A machine or pool counts
// when either of two ways of finding its TerraformCluster yields name: the
// CAPI Cluster's spec.infrastructureRef, which is how the controllers find
// it, or the TerraformCluster carrying the same cluster-name label
// (ClusterIndex). Counting both means a stray or unlabeled TerraformCluster
// cannot hide a user. The first way is skipped when CAPI Clusters cannot
// be read at all (their CRD is not installed or registered). It returns an
// error from a failed list.
func Users(ctx context.Context, reader client.Reader, name string) ([]client.Object, error) {
	return findUsers(ctx, reader, name, false)
}

// findUsers implements FirstUser (firstOnly), Users and UsedInNamespace,
// listing through reader with ctx and opts and looking for users of the
// identity name. With firstOnly it stops at the first. It returns the
// users found, or an error from a failed list.
func findUsers(ctx context.Context, reader client.Reader, name string, firstOnly bool, opts ...client.ListOption) ([]client.Object, error) {
	var out []client.Object
	clusters := &infrav1.TerraformClusterList{}
	if err := reader.List(ctx, clusters, opts...); err != nil {
		return nil, fmt.Errorf("identity: list TerraformClusters: %w", err)
	}
	for i := range clusters.Items {
		if clusters.Items[i].Spec.IdentityRef.ClusterIdentityName() == name {
			out = append(out, &clusters.Items[i])
			if firstOnly {
				return out, nil
			}
		}
	}
	machines := &infrav1.TerraformMachineList{}
	if err := reader.List(ctx, machines, opts...); err != nil {
		return nil, fmt.Errorf("identity: list TerraformMachines: %w", err)
	}
	pools := &infrav1.TerraformMachinePoolList{}
	if err := reader.List(ctx, pools, opts...); err != nil {
		return nil, fmt.Errorf("identity: list TerraformMachinePools: %w", err)
	}
	ix := IndexClusters(clusters.Items)
	byRef, err := indexByInfrastructureRef(ctx, reader, clusters.Items, opts...)
	if err != nil {
		return nil, err
	}
	uses := func(m client.Object, own infrav1.IdentityReference) bool {
		if got, _ := EffectiveName(own, ix.For(m)); got == name {
			return true
		}
		if byRef == nil {
			return false
		}
		got, _ := EffectiveName(own, byRef.For(m))
		return got == name
	}
	for i := range machines.Items {
		if m := &machines.Items[i]; uses(m, m.Spec.IdentityRef) {
			out = append(out, m)
			if firstOnly {
				return out, nil
			}
		}
	}
	for i := range pools.Items {
		if p := &pools.Items[i]; uses(p, p.Spec.IdentityRef) {
			out = append(out, p)
			if firstOnly {
				return out, nil
			}
		}
	}
	return out, nil
}

// indexByInfrastructureRef maps each CAPI Cluster, by namespace and name, to
// the TerraformCluster in tcs that its spec.infrastructureRef names,
// reading Clusters through reader with ctx and opts. It returns a ClusterIndex keyed
// like IndexClusters, nil when CAPI Clusters cannot be listed at all (CRD
// absent or type unregistered), and an error from any other failed list.
func indexByInfrastructureRef(ctx context.Context, reader client.Reader, tcs []infrav1.TerraformCluster, opts ...client.ListOption) (ClusterIndex, error) {
	list := &clusterv1.ClusterList{}
	if err := reader.List(ctx, list, opts...); err != nil {
		if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("identity: list Clusters: %w", err)
	}
	byName := make(map[client.ObjectKey]*infrav1.TerraformCluster, len(tcs))
	for i := range tcs {
		byName[client.ObjectKeyFromObject(&tcs[i])] = &tcs[i]
	}
	ix := ClusterIndex{}
	for i := range list.Items {
		c := &list.Items[i]
		ref := c.Spec.InfrastructureRef
		if ref.Name == "" || ref.APIGroup != infrav1.GroupVersion.Group || ref.Kind != "TerraformCluster" {
			continue
		}
		if tc := byName[client.ObjectKey{Namespace: c.Namespace, Name: ref.Name}]; tc != nil {
			ix[client.ObjectKeyFromObject(c)] = tc
		}
	}
	return ix, nil
}
