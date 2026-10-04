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
	"context"
	"errors"
	"fmt"
	"maps"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A one-shot annotation (an approval, a restore request) is removed once
// what it asked for happened. The deferred patch carries no
// resourceVersion and sends the removal as null, which would also delete
// a new value the user wrote since the object was read. So each removal
// is its own patch under an optimistic lock, and the object in memory
// keeps the annotation: the deferred patch then has no annotation change
// to send at all. Reads in the same pass go through annotation, which
// hides the removed ones.

// errAnnotationConflict means the object changed since it was read, so a
// consumed annotation was not removed; the reconcile requeues and decides
// again from the new object.
var errAnnotationConflict = errors.New("the object changed since it was read")

// removeAnnotation removes key from the object in its own merge patch,
// using ctx, conditional on the resourceVersion the object was read at
// (or the one the previous removal of this pass left). On any failure it
// keeps the pass's finished Jobs from being marked bookkept, so the next
// pass consumes them again. It returns errAnnotationConflict, wrapped,
// when the object changed since, and any other patch error.
func (r *reconciler) removeAnnotation(ctx context.Context, key string) error {
	before, ok := r.obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("remove %s: %T is not a client.Object", key, r.obj)
	}
	if r.patchedRV != "" {
		before.SetResourceVersion(r.patchedRV)
	}
	after, ok := before.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("remove %s: %T is not a client.Object", key, r.obj)
	}
	annotations := maps.Clone(after.GetAnnotations())
	delete(annotations, key)
	after.SetAnnotations(annotations)
	err := r.d.Client.Patch(ctx, after, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	switch {
	case apierrors.IsConflict(err):
		r.skipMark = true
		return fmt.Errorf("remove %s: %w", key, errAnnotationConflict)
	case err != nil:
		r.skipMark = true
		return fmt.Errorf("remove %s: %w", key, err)
	}
	r.patchedRV = after.GetResourceVersion()
	if r.consumed == nil {
		r.consumed = map[string]bool{}
	}
	r.consumed[key] = true
	return nil
}

// annotation returns the object's annotation key, "" once this pass
// removed it (removeAnnotation).
func (r *reconciler) annotation(key string) string {
	if r.consumed[key] {
		return ""
	}
	return r.obj.GetAnnotations()[key]
}
