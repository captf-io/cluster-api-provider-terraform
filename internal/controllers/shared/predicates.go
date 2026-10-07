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
	"maps"

	"k8s.io/apimachinery/pkg/api/equality"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// ClusterEndpointChanged passes a Cluster update whose
// spec.controlPlaneEndpoint changed: an endpoint a hosted control-plane
// provider sets later is a hashed cluster input. CAPD's Cluster predicate
// misses it. It returns the predicate to register on a watch.
func ClusterEndpointChanged() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, ok1 := e.ObjectOld.(*clusterv1.Cluster)
			n, ok2 := e.ObjectNew.(*clusterv1.Cluster)
			return ok1 && ok2 && o.Spec.ControlPlaneEndpoint != n.Spec.ControlPlaneEndpoint
		},
	}
}

// ClusterNetworkChanged passes Cluster updates that change
// spec.clusterNetwork, a hashed cluster input (cluster_network). It
// returns the predicate to register on a watch.
func ClusterNetworkChanged() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, ok1 := e.ObjectOld.(*clusterv1.Cluster)
			n, ok2 := e.ObjectNew.(*clusterv1.Cluster)
			return ok1 && ok2 && !equality.Semantic.DeepEqual(o.Spec.ClusterNetwork, n.Spec.ClusterNetwork)
		},
	}
}

// InheritedPolicyChanged passes TerraformCluster updates that change what
// its machines and pools inherit from it (Resolve): spec.defaults, or the
// cluster's own identityRef, jobs policy, drift policy or deletionPolicy,
// which they fall back to. It
// returns the predicate to register on a watch, with
// TerraformClusterToObjects.
func InheritedPolicyChanged() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, ok1 := e.ObjectOld.(*infrav1.TerraformCluster)
			n, ok2 := e.ObjectNew.(*infrav1.TerraformCluster)
			return ok1 && ok2 && !equality.Semantic.DeepEqual(inheritedPolicy(o), inheritedPolicy(n))
		},
	}
}

// inheritedPolicy returns the part of tc's spec its machines and pools
// inherit (Resolve), for InheritedPolicyChanged to compare.
func inheritedPolicy(tc *infrav1.TerraformCluster) []any {
	return []any{tc.Spec.Defaults, tc.Spec.IdentityRef, tc.Spec.Jobs, tc.Spec.Drift, tc.Spec.DeletionPolicy}
}

// ManagedSecretEvents passes the events of Secrets labeled
// captf.io/managed=true that an owner must react to: every event of a
// state, backup or inputs Secret, and only the deletion of a credential
// mirror (identity.MirroredLabel), which its users recreate. A mirror
// carries one ownerRef per object using the identity in its namespace, so
// passing its updates would wake every one of them each time one is added
// or removed. Its users read it live when they need it, so they need no
// other event. Updates of the other Secrets all pass: the watch sees only
// metadata, so a data change cannot be told from an ownerRef change. It
// returns the predicate to register on a watch.
func ManagedSecretEvents() predicate.Funcs {
	managed := func(o client.Object) bool { return o.GetLabels()[state.ManagedLabel] == "true" }
	mirror := func(o client.Object) bool { return o.GetLabels()[identity.MirroredLabel] == "true" }
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return managed(e.Object) && !mirror(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return managed(e.ObjectNew) && !mirror(e.ObjectNew) },
		GenericFunc: func(e event.GenericEvent) bool { return managed(e.Object) && !mirror(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return managed(e.Object) },
	}
}

// IdentitySpecChanged passes a TerraformClusterIdentity's creation and
// deletion, and its updates that change its generation, that is its spec.
// Its users read only the spec; its status changes (Ready,
// status.namespaces as mirrors come and go) would otherwise wake every
// object using it. It returns the predicate to register on a watch.
func IdentitySpecChanged() predicate.GenerationChangedPredicate {
	return predicate.GenerationChangedPredicate{}
}

// LabelsChanged passes updates whose labels changed and nothing else: the
// Namespace watch re-evaluates allowedNamespaces selectors. It returns
// the predicate to register on a watch.
func LabelsChanged() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return !maps.Equal(e.ObjectOld.GetLabels(), e.ObjectNew.GetLabels())
		},
	}
}

// DeletesOnly passes delete events only. It returns the predicate to
// register on a watch.
func DeletesOnly() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
	}
}
