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
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ProtectionFinalizer keeps a Secret CAPTF cannot rebuild from anything
// else: the base state Secret, every state backup chunk and both inputs
// records. Those Secrets carry an owner reference to their object, and the
// object's own finalizer does not stop the garbage collector from deleting
// them: a foreground cascading deletion (kubectl delete
// --cascade=foreground, Argo CD's default prune) deletes every dependent
// of the object at once, while the object still waits for its destroy.
// With this finalizer such a delete only marks the Secret; its data stays
// readable until CAPTF itself deletes it (Cleanup, PruneBackups,
// inputs.Delete) or Retain releases it.
//
// The -part-N chunks Terraform adds to a large state are left unprotected:
// Terraform deletes the surplus ones itself when a state shrinks, and a
// finalizer would keep a stale one forever. Losing them makes the state
// inconsistent, which holds the object for a restore from a backup.
const ProtectionFinalizer = "captf.io/state-protection"

// Protect adds ProtectionFinalizer to m and reports whether it did. A
// Secret being deleted is left alone, since the API server rejects a new
// finalizer on it, and so is one that has it already.
func Protect(m *metav1.ObjectMeta) bool {
	if m.DeletionTimestamp != nil || slices.Contains(m.Finalizers, ProtectionFinalizer) {
		return false
	}
	m.Finalizers = append(m.Finalizers, ProtectionFinalizer)
	return true
}

// Unprotect removes ProtectionFinalizer from m and reports whether it did.
func Unprotect(m *metav1.ObjectMeta) bool {
	n := len(m.Finalizers)
	m.Finalizers = slices.DeleteFunc(m.Finalizers, func(f string) bool { return f == ProtectionFinalizer })
	return len(m.Finalizers) != n
}

// Protected reports whether m carries ProtectionFinalizer.
func Protected(m *metav1.ObjectMeta) bool {
	return slices.Contains(m.Finalizers, ProtectionFinalizer)
}

// DeleteSecret deletes the Secret whose metadata is meta, as read with its
// UID and resourceVersion, through c using ctx: it first removes
// ProtectionFinalizer with a merge patch locked to that resourceVersion,
// then deletes the Secret with a UID precondition, so a Secret recreated
// under the same name since the read is never deleted. A Secret gone
// already is fine. It returns any other error from the two calls.
func DeleteSecret(ctx context.Context, c client.Writer, meta *metav1.ObjectMeta) error {
	if Protected(meta) {
		orig := &corev1.Secret{ObjectMeta: *meta.DeepCopy()}
		s := orig.DeepCopy()
		Unprotect(&s.ObjectMeta)
		err := c.Patch(ctx, s, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
		switch {
		case apierrors.IsNotFound(err):
			return nil
		case err != nil:
			return fmt.Errorf("state: unprotect Secret %s: %w", meta.Name, err)
		}
	}
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: meta.Namespace, Name: meta.Name}}
	var opts []client.DeleteOption
	if meta.UID != "" {
		opts = append(opts, client.Preconditions{UID: &meta.UID})
	}
	if err := client.IgnoreNotFound(c.Delete(ctx, s, opts...)); err != nil {
		return fmt.Errorf("state: delete Secret %s: %w", meta.Name, err)
	}
	return nil
}

// DeleteSecretNamed reads the metadata of the Secret namespace/name
// through c using ctx and deletes it (DeleteSecret). A Secret gone already
// is fine. It returns any other error from the read or the delete.
func DeleteSecretNamed(ctx context.Context, c client.Client, namespace, name string) error {
	var meta metav1.PartialObjectMetadata
	meta.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &meta)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("state: get Secret %s: %w", name, err)
	}
	return DeleteSecret(ctx, c, &meta.ObjectMeta)
}
