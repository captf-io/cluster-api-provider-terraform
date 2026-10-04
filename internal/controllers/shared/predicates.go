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

package shared

import (
	"maps"

	"k8s.io/apimachinery/pkg/api/equality"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

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

// ManagedSecret passes Secrets labeled captf.io/managed=true: state, durable
// inputs and credential mirrors. It returns the predicate to register on
// a watch.
func ManagedSecret() predicate.Funcs {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[state.ManagedLabel] == "true"
	})
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
