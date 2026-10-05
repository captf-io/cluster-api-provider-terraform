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

package shared

import (
	"context"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// SecretToOwner maps a captf.io/managed Secret to the objects of kind that
// own it: every ownerRef of that kind (a credential mirror has one per
// object using it), else the backend's owner labels of a state Secret not
// adopted yet. The owner-name label is the name only for names of at most
// 63 characters (state.LabelValue); a longer name's pre-adoption state
// Secret does not map, and the Job watch still reports the Job's end.
func SecretToOwner(kind string) handler.MapFunc {
	return func(_ context.Context, o client.Object) []reconcile.Request {
		var reqs []reconcile.Request
		for _, ref := range o.GetOwnerReferences() {
			gv, err := schema.ParseGroupVersion(ref.APIVersion)
			if err == nil && gv.Group == infrav1.GroupVersion.Group && ref.Kind == kind {
				reqs = append(reqs, request(o.GetNamespace(), ref.Name))
			}
		}
		l := o.GetLabels()
		if len(reqs) == 0 && l[state.OwnerKindLabel] == kind && l[state.OwnerNameLabel] != "" {
			reqs = append(reqs, request(o.GetNamespace(), l[state.OwnerNameLabel]))
		}
		return reqs
	}
}

// ClusterStateSecretToMachines maps a TerraformCluster's base state Secret
// to the TerraformMachines of that cluster that are not provisioned yet, so
// a changed exports output reaches them.
// Provisioned machines are skipped: they never read the exports again, and
// waking every machine on each cluster state write costs thousands of
// reconciles in a large cluster. Chunk Secrets (-part-N) are ignored: they
// change together with the base Secret. It lists machines through the
// reader c and returns the MapFunc to register as a watch handler.
func ClusterStateSecretToMachines(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		cluster := clusterOfStateSecret(o)
		if cluster == "" {
			return nil
		}
		machines := &infrav1.TerraformMachineList{}
		if err := c.List(ctx, machines, client.InNamespace(o.GetNamespace()), client.MatchingLabels{clusterv1.ClusterNameLabel: cluster}); err != nil {
			klog.FromContext(ctx).Error(err, "List TerraformMachines of a cluster")
			return nil
		}
		// A provisioned machine never builds inputs again, so the cluster's
		// exports cannot change anything for it.
		var reqs []reconcile.Request
		for i := range machines.Items {
			m := &machines.Items[i]
			if p := m.Status.Initialization.Provisioned; p == nil || !*p {
				reqs = append(reqs, request(m.Namespace, m.Name))
			}
		}
		return reqs
	}
}

// ClusterStateSecretToPools maps a TerraformCluster's base state Secret to
// every TerraformMachinePool of that cluster, provisioned or not: pools are
// mutable and re-read the cluster's exports and failure_domains outputs on
// every reconcile. Chunk Secrets are ignored, as in
// ClusterStateSecretToMachines. It lists pools through the reader c and
// returns the MapFunc to register as a watch handler.
func ClusterStateSecretToPools(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		cluster := clusterOfStateSecret(o)
		if cluster == "" {
			return nil
		}
		return list(ctx, c, &infrav1.TerraformMachinePoolList{}, client.InNamespace(o.GetNamespace()),
			client.MatchingLabels{clusterv1.ClusterNameLabel: cluster})
	}
}

// clusterOfStateSecret returns the cluster-name label of o when o is a
// TerraformCluster's base state Secret; "" for anything else, chunk
// Secrets included.
func clusterOfStateSecret(o client.Object) string {
	l := o.GetLabels()
	if l[state.OwnerKindLabel] != state.KindTerraformCluster || o.GetName() != state.SecretName(l[state.BackendSuffixLabel]) {
		return ""
	}
	return l[clusterv1.ClusterNameLabel]
}

// IdentityToClusters maps a TerraformClusterIdentity to the TerraformClusters
// that reference it, directly or through defaults (IdentityIndex). It
// lists clusters through the reader c and returns the MapFunc to register
// as a watch handler.
func IdentityToClusters(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		return list(ctx, c, &infrav1.TerraformClusterList{}, client.MatchingFields{IdentityIndex: o.GetName()})
	}
}

// IdentityToMachines maps a TerraformClusterIdentity to the TerraformMachines
// that reference it, and to the machines without their own identityRef that
// fall back to it: their TerraformCluster's defaults.identityRef, else its
// spec.identityRef (identity.MachineFallbackName). It lists clusters and
// machines through the reader c and returns the MapFunc to register as a
// watch handler.
func IdentityToMachines(c client.Reader) handler.MapFunc {
	return identityToWorkloads(c, "TerraformMachines", func() *infrav1.TerraformMachineList { return &infrav1.TerraformMachineList{} },
		func(l *infrav1.TerraformMachineList) []reconcile.Request {
			var reqs []reconcile.Request
			for _, m := range l.Items {
				if m.Spec.IdentityRef.Name == "" {
					reqs = append(reqs, request(m.Namespace, m.Name))
				}
			}
			return reqs
		})
}

// IdentityToPools maps a TerraformClusterIdentity to the
// TerraformMachinePools that reference it, and to the pools without their
// own identityRef that fall back to it, as IdentityToMachines does for
// machines. It lists clusters and pools through the reader c and returns
// the MapFunc to register as a watch handler.
func IdentityToPools(c client.Reader) handler.MapFunc {
	return identityToWorkloads(c, "TerraformMachinePools", func() *infrav1.TerraformMachinePoolList { return &infrav1.TerraformMachinePoolList{} },
		func(l *infrav1.TerraformMachinePoolList) []reconcile.Request {
			var reqs []reconcile.Request
			for _, p := range l.Items {
				if p.Spec.IdentityRef.Name == "" {
					reqs = append(reqs, request(p.Namespace, p.Name))
				}
			}
			return reqs
		})
}

// identityToWorkloads is the shared body of IdentityToMachines and
// IdentityToPools. It returns the MapFunc that requests every object of the
// list type L that the identity index maps to the identity, plus those
// without an identityRef of their own in the clusters falling back to it.
// It lists through the reader c; kind is the plural kind name for the log
// message of a failed List, newList returns an empty L, and unset returns
// the requests for the items of a listed L that have no identityRef.
func identityToWorkloads[L client.ObjectList](c client.Reader, kind string, newList func() L, unset func(L) []reconcile.Request) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		reqs := list(ctx, c, newList(), client.MatchingFields{IdentityIndex: o.GetName()})
		for _, tc := range fallbackClusters(ctx, c, o.GetName()) {
			l := newList()
			if err := c.List(ctx, l, client.InNamespace(tc.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: tc.Labels[clusterv1.ClusterNameLabel]}); err != nil {
				klog.FromContext(ctx).Error(err, "List "+kind+" of a cluster")
				continue
			}
			reqs = append(reqs, unset(l)...)
		}
		return dedupe(reqs)
	}
}

// fallbackClusters lists, through the reader c using ctx, the
// TerraformClusters carrying a cluster-name label whose
// identity.MachineFallbackName is name: those whose machines and pools
// without their own identityRef use it. It returns those clusters; a List
// error is logged and returns none.
func fallbackClusters(ctx context.Context, c client.Reader, name string) []infrav1.TerraformCluster {
	clusters := &infrav1.TerraformClusterList{}
	if err := c.List(ctx, clusters, client.MatchingFields{IdentityIndex: name}); err != nil {
		klog.FromContext(ctx).Error(err, "List TerraformClusters by identity")
		return nil
	}
	return slices.DeleteFunc(clusters.Items, func(tc infrav1.TerraformCluster) bool {
		return identity.MachineFallbackName(&tc) != name || tc.Labels[clusterv1.ClusterNameLabel] == ""
	})
}

// variablesSourceKey returns the VariablesSourceIndex value of o, a
// labeled ConfigMap or Secret; "" for anything else.
func variablesSourceKey(o client.Object) string {
	switch o.(type) {
	case *corev1.ConfigMap:
		return kindConfigMap + "/" + o.GetName()
	case *corev1.Secret:
		return kindSecret + "/" + o.GetName()
	}
	return ""
}

// VariablesSourceToClusters maps a ConfigMap or Secret labeled
// captf.io/variables=true to the TerraformClusters in its namespace that
// name it in variablesFrom (VariablesSourceIndex). The variables are hashed,
// so a changed value re-applies. It lists clusters through the reader c
// and returns the MapFunc to register as a watch handler.
func VariablesSourceToClusters(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		key := variablesSourceKey(o)
		if key == "" {
			return nil
		}
		return list(ctx, c, &infrav1.TerraformClusterList{}, client.InNamespace(o.GetNamespace()),
			client.MatchingFields{VariablesSourceIndex: key})
	}
}

// VariablesSourceToMachines maps a labeled ConfigMap or Secret to the
// TerraformMachines in its namespace that name it and are not provisioned
// yet: a provisioned machine never reads its variables again. It lists
// machines through the reader c and returns the MapFunc to register as a
// watch handler.
func VariablesSourceToMachines(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		key := variablesSourceKey(o)
		if key == "" {
			return nil
		}
		machines := &infrav1.TerraformMachineList{}
		if err := c.List(ctx, machines, client.InNamespace(o.GetNamespace()), client.MatchingFields{VariablesSourceIndex: key}); err != nil {
			klog.FromContext(ctx).Error(err, "List TerraformMachines by variables source")
			return nil
		}
		var reqs []reconcile.Request
		for i := range machines.Items {
			m := &machines.Items[i]
			if p := m.Status.Initialization.Provisioned; p == nil || !*p {
				reqs = append(reqs, request(m.Namespace, m.Name))
			}
		}
		return reqs
	}
}

// VariablesSourceToPools maps a labeled ConfigMap or Secret to the
// TerraformMachinePools in its namespace that name it, provisioned or not:
// a pool is mutable, so a changed value re-applies. It lists pools through
// the reader c and returns the MapFunc to register as a watch handler.
func VariablesSourceToPools(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		key := variablesSourceKey(o)
		if key == "" {
			return nil
		}
		return list(ctx, c, &infrav1.TerraformMachinePoolList{}, client.InNamespace(o.GetNamespace()),
			client.MatchingFields{VariablesSourceIndex: key})
	}
}

// NamespaceToObjects maps a Namespace to every object of newList's kind in
// it, listed through the reader c: a label change may change what an
// allowedNamespaces selector admits. Namespace label changes are
// rare, so no attempt is made to pick only the objects whose identity uses
// a selector. It returns the MapFunc to register as a watch handler.
func NamespaceToObjects(c client.Reader, newList func() client.ObjectList) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		return list(ctx, c, newList(), client.InNamespace(o.GetName()))
	}
}

// MachineToClusters maps a TerraformMachine or TerraformMachinePool (any
// object with a cluster-name label) to the TerraformClusters of its
// cluster, so a cluster waiting on DeletionBlocked proceeds when its last
// machine or pool is gone. It lists clusters through the reader c and returns the
// MapFunc to register as a watch handler.
func MachineToClusters(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		cluster := o.GetLabels()[clusterv1.ClusterNameLabel]
		if cluster == "" {
			return nil
		}
		return list(ctx, c, &infrav1.TerraformClusterList{}, client.InNamespace(o.GetNamespace()),
			client.MatchingLabels{clusterv1.ClusterNameLabel: cluster})
	}
}

// Merge combines mappers, dropping duplicate requests. It returns the
// combined MapFunc.
func Merge(mappers ...handler.MapFunc) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		var reqs []reconcile.Request
		for _, m := range mappers {
			reqs = append(reqs, m(ctx, o)...)
		}
		return dedupe(reqs)
	}
}

// request returns a reconcile.Request for the object named name in
// namespace.
func request(namespace, name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}
}

// list returns a request per object listed into l through the reader c
// using ctx and opts; a List error is logged and maps to nothing (the
// resync period catches up).
func list(ctx context.Context, c client.Reader, l client.ObjectList, opts ...client.ListOption) []reconcile.Request {
	if err := c.List(ctx, l, opts...); err != nil {
		klog.FromContext(ctx).Error(err, "List for a watch mapping failed")
		return nil
	}
	objs, err := meta.ExtractList(l)
	if err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(objs))
	for _, obj := range objs {
		if m, err := meta.Accessor(obj); err == nil {
			reqs = append(reqs, request(m.GetNamespace(), m.GetName()))
		}
	}
	return reqs
}

// dedupe returns reqs sorted and with duplicate requests removed.
func dedupe(reqs []reconcile.Request) []reconcile.Request {
	slices.SortFunc(reqs, func(a, b reconcile.Request) int {
		if a.String() < b.String() {
			return -1
		}
		if a.String() > b.String() {
			return 1
		}
		return 0
	})
	return slices.Compact(reqs)
}
