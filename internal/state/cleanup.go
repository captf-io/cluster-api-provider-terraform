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
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Cleanup deletes, in namespace through c using ctx, every state Secret of
// suffix, every backup of it and its lock Lease, after a successful
// destroy, and returns any error from those calls. Neither backend does
// this itself: destroy only empties the state and the default workspace
// cannot be deleted. The backups are deleted here rather than left to
// garbage collection: they carry ProtectionFinalizer, and an object of
// the same name created next would otherwise take them for a lost state
// of its own. Objects already gone are fine.
//
// Secrets are listed and deleted one by one: DeleteAllOf needs the
// deletecollection verb, which the manager role does not have.
func Cleanup(ctx context.Context, c client.Client, namespace, suffix string) error {
	if err := DeleteState(ctx, c, namespace, suffix); err != nil {
		return err
	}
	if err := deleteMatching(ctx, c, namespace, "backup", BackupSelector(suffix)); err != nil {
		return err
	}
	return DeleteLock(ctx, c, namespace, suffix)
}

// DeleteState deletes, in namespace through c using ctx, every state
// Secret of suffix (DeleteSecret, so the base Secret's
// ProtectionFinalizer goes first), and leaves its backups and its lock
// Lease. It returns any error from those calls; Secrets already gone are
// fine.
func DeleteState(ctx context.Context, c client.Client, namespace, suffix string) error {
	return deleteMatching(ctx, c, namespace, "state", Selector(suffix))
}

// deleteMatching deletes, in namespace through c using ctx, every Secret
// sel matches (DeleteSecret), listing metadata only; what names them in
// errors. It returns any error from the list or a delete.
func deleteMatching(ctx context.Context, c client.Client, namespace, what string, sel labels.Selector) error {
	var list metav1.PartialObjectMetadataList
	list.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	if err := c.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return fmt.Errorf("state: list %s Secrets: %w", what, err)
	}
	for i := range list.Items {
		if err := DeleteSecret(ctx, c, &list.Items[i].ObjectMeta); err != nil {
			return err
		}
	}
	return nil
}

// DeleteLock deletes, in namespace through c using ctx, the lock Lease of
// the state of suffix, and returns any error but its being gone already.
// Call it only when no Job of the object is active: the backend holds the
// Lease while a run has the state locked.
func DeleteLock(ctx context.Context, c client.Client, namespace, suffix string) error {
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: LeaseName(suffix)}}
	if err := c.Delete(ctx, lease); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("state: delete Lease %s: %w", lease.Name, err)
	}
	return nil
}
