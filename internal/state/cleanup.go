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

package state

import (
	"context"
	"fmt"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Cleanup deletes, in namespace through c using ctx, every state Secret of
// suffix and its lock Lease, after a successful destroy, and returns any
// error from those calls. Neither backend does this itself: destroy only
// empties the state and the default workspace cannot be deleted. Objects
// already gone are fine.
//
// Secrets are listed and deleted one by one: DeleteAllOf needs the
// deletecollection verb, which the manager role does not have.
func Cleanup(ctx context.Context, c client.Client, namespace, suffix string) error {
	var list corev1.SecretList
	if err := c.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: Selector(suffix)}); err != nil {
		return fmt.Errorf("state: list Secrets: %w", err)
	}
	for i := range list.Items {
		if err := client.IgnoreNotFound(c.Delete(ctx, &list.Items[i])); err != nil {
			return fmt.Errorf("state: delete Secret %s: %w", list.Items[i].Name, err)
		}
	}
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: LeaseName(suffix)}}
	if err := c.Delete(ctx, lease); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("state: delete Lease %s: %w", lease.Name, err)
	}
	return nil
}
