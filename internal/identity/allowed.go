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

package identity

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// Reason is an IdentityAllowed condition reason.
type Reason = string

// Allowed reports whether id may be used from namespace, with the
// IdentityAllowed reason:
//
//   - nil allowedNamespaces allows no namespace;
//   - an empty object ({}) is rejected by admission, and allows nothing;
//   - otherwise list and selector are ORed;
//   - an empty selector ({}) matches every namespace, even one that does
//     not exist (yet);
//   - an empty list or an invalid selector matches nothing.
//
// The Namespace's labels are read through reader, which callers wire to the
// uncached API reader so a label change is seen at once. ctx bounds that
// read. A Namespace that no longer exists is not allowed. A read error
// returns IdentityCheckFailed.
func Allowed(ctx context.Context, reader client.Reader, id *infrav1.TerraformClusterIdentity, namespace string) (bool, Reason, error) {
	an := id.Spec.AllowedNamespaces
	switch {
	case an == nil, an.List == nil && an.Selector == nil:
		// {} is rejected by admission; should one exist, it allows nothing.
		return false, infrav1.NamespaceNotAllowedReason, nil
	case slices.Contains(an.List, namespace):
		return true, infrav1.IdentityAllowedReason, nil
	case an.Selector == nil:
		return false, infrav1.NamespaceNotAllowedReason, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(an.Selector)
	// An invalid selector cannot be fixed by retrying, so it is a denial,
	// not an error.
	if err != nil {
		return false, infrav1.NamespaceNotAllowedReason, nil
	}
	// The empty selector {} is the explicit spelling of "every namespace";
	// no namespace lookup is needed.
	if sel.Empty() {
		return true, infrav1.IdentityAllowedReason, nil
	}
	ns := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return false, infrav1.NamespaceNotAllowedReason, nil
		}
		return false, infrav1.IdentityCheckFailedReason, fmt.Errorf("identity: get namespace %s: %w", namespace, err)
	}
	if sel.Matches(labels.Set(ns.Labels)) {
		return true, infrav1.IdentityAllowedReason, nil
	}
	return false, infrav1.NamespaceNotAllowedReason, nil
}
