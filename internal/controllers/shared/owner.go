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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// OwnerLookup describes how ResolveOwner finds the CAPI owner of a machine
// or a pool: T is the owner type (*clusterv1.Machine or
// *clusterv1.MachinePool).
type OwnerLookup[T client.Object] struct {
	// Noun names the owner kind in a lookup error ("Machine").
	Noun string
	// Get looks the owner up through its ownerRef: the owner and true when
	// found, false when there is no ownerRef of the kind, or an error
	// (a NotFound error means the ownerRef's target is gone).
	Get func(ctx context.Context) (T, bool, error)
	// Mismatch returns a non-empty message when the found owner does not
	// reference the object back (a forged or stale ownerRef).
	Mismatch func(owner T) string
	// WaitingReason and WaitingMessage are the gate returned when the object
	// has some other ownerRef but none of the kind yet.
	WaitingReason, WaitingMessage string
	// Assign records a valid owner on the result.
	Assign func(info *OwnerInfo, owner T)
}

// ResolveOwner resolves the owner of the object with metadata meta through
// lookup, then completes it with LookupCluster, using ctx and c for every
// read:
//
//   - an ownerRef whose owner is gone: HasOwnerRef and OwnerGone;
//   - an owner that does not reference the object back: HasOwnerRef and an
//     OwnerMismatch gate, no owner and no Cluster lookup;
//   - no ownerRef of the kind but another ownerRef: the waiting gate;
//   - no ownerRef at all: an empty result;
//   - a valid owner: HasOwnerRef and the owner assigned.
//
// It returns the resolved OwnerInfo, or an error from a lookup failure
// other than not-found.
func ResolveOwner[T client.Object](ctx context.Context, c client.Client, meta metav1.ObjectMeta, lookup OwnerLookup[T]) (OwnerInfo, error) {
	var owner OwnerInfo
	found, ok, err := lookup.Get(ctx)
	switch {
	case apierrors.IsNotFound(err):
		owner.HasOwnerRef, owner.OwnerGone = true, true
	case err != nil:
		return OwnerInfo{}, fmt.Errorf("get owner %s: %w", lookup.Noun, err)
	case ok:
		if msg := lookup.Mismatch(found); msg != "" {
			owner.HasOwnerRef = true
			owner.Gate = &Gate{Status: metav1.ConditionFalse, Reason: infrav1.OwnerMismatchReason, Message: msg}
			return owner, nil
		}
		owner.HasOwnerRef = true
		lookup.Assign(&owner, found)
	case len(meta.OwnerReferences) > 0:
		owner.Gate = &Gate{Status: metav1.ConditionFalse, Reason: lookup.WaitingReason, Message: lookup.WaitingMessage}
		return owner, nil
	default:
		return owner, nil
	}
	if err := LookupCluster(ctx, c, meta, &owner); err != nil {
		return OwnerInfo{}, err
	}
	return owner, nil
}
