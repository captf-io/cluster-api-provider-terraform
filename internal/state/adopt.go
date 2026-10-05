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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Adopt marks every state Secret of suffix as owned by owner (ownerRef with
// blockOwnerDeletion=false, so state moves with the cluster and is garbage
// collected with the object) and records inputsHash on the base Secret. It
// uses ctx for the list and patch calls through c and returns any error from
// them, or ErrNoState if suffix names no state Secret.
//
// Terraform creates new -part-N chunks with only the backend labels, so
// Adopt runs after every successful apply; it is idempotent. Call it only
// when no Job is active: the backends' Update would conflict with it.
// Between adoptions, the reconciler re-owns a chunk that lacks the
// reference or names owner's earlier UID (one a refresh or drift Job
// wrote, or one restored with a management cluster) from the metadata
// Reader.Read returns (State.Metadata, ownership.RepairSecret).
func Adopt(ctx context.Context, c client.Client, owner client.Object, suffix, inputsHash string) error {
	var list corev1.SecretList
	if err := c.List(ctx, &list, client.InNamespace(owner.GetNamespace()), client.MatchingLabelsSelector{Selector: Selector(suffix)}); err != nil {
		return fmt.Errorf("state: list Secrets: %w", err)
	}
	if len(list.Items) == 0 {
		return ErrNoState
	}
	base := SecretName(suffix)
	for i := range list.Items {
		s := &list.Items[i]
		orig := s.DeepCopy()
		if err := controllerutil.SetOwnerReference(owner, s, c.Scheme(), noBlockOwnerDeletion); err != nil {
			return fmt.Errorf("state: owner reference on %s: %w", s.Name, err)
		}
		if s.Name == base {
			metav1.SetMetaDataAnnotation(&s.ObjectMeta, InputsHashAnnotation, inputsHash)
		}
		if err := c.Patch(ctx, s, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("state: adopt %s: %w", s.Name, err)
		}
	}
	return nil
}

// noBlockOwnerDeletion clears the BlockOwnerDeletion field of ref, so the
// owner reference Adopt sets never blocks the owner's deletion.
func noBlockOwnerDeletion(ref *metav1.OwnerReference) {
	ref.BlockOwnerDeletion = nil
}

// HasInputsHash returns the inputs hash recorded on the base Secret of
// suffix among secrets, and whether one was found and non-empty.
func HasInputsHash(secrets []corev1.Secret, suffix string) (string, bool) {
	for i := range secrets {
		if secrets[i].Name == SecretName(suffix) {
			h, ok := secrets[i].Annotations[InputsHashAnnotation]
			return h, ok && h != ""
		}
	}
	return "", false
}
