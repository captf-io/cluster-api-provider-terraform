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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestRemoveOwnerConcurrentOwnerAdd proves RemoveOwner of the last owner
// does not delete a mirror that another object added its ownerRef to after
// the caller read it: it returns removed=false and a Conflict error, the
// mirror survives with both owners, and a retry from a fresh read drops
// only the first owner.
func TestRemoveOwnerConcurrentOwnerAdd(t *testing.T) {
	t.Parallel()
	e := newMirrorEnv(t, source(nil))
	m1, m2 := machine("m1", "uid-1"), machine("m2", "uid-2")
	if _, _, err := EnsureMirror(t.Context(), e.c, e.reader, testIdentity(nil), tenant, m1); err != nil {
		t.Fatal(err)
	}
	stale := e.mirror(t) // m1's Cleanup read: only its own ref
	if _, _, err := EnsureMirror(t.Context(), e.c, e.reader, testIdentity(nil), tenant, m2); err != nil {
		t.Fatal(err)
	}

	removed, err := RemoveOwner(t.Context(), e.c, stale, m1)
	if removed || !apierrors.IsConflict(err) {
		t.Fatalf("RemoveOwner on a stale mirror: removed %v, err %v; want false, Conflict", removed, err)
	}
	if refs := e.mirror(t).OwnerReferences; len(refs) != 2 {
		t.Fatalf("mirror ownerRefs after the refused delete = %v; want both owners", refs)
	}

	// The retry reads afresh: m2 is still an owner, so only m1 goes.
	if removed, err := RemoveOwner(t.Context(), e.c, e.mirror(t), m1); err != nil || removed {
		t.Fatalf("retry: removed %v, %v", removed, err)
	}
	if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(stale), &corev1.Secret{}); err != nil {
		t.Errorf("mirror gone after the retry: %v", err)
	}
}

// TestRemoveOwnerStaleUpdateConflicts proves the Update path of
// RemoveOwner sends the read resourceVersion: removing one of two owners
// from a stale copy is a Conflict, not a lost update of the other writer.
func TestRemoveOwnerStaleUpdateConflicts(t *testing.T) {
	t.Parallel()
	e := newMirrorEnv(t, source(nil))
	m1, m2, m3 := machine("m1", "uid-1"), machine("m2", "uid-2"), machine("m3", "uid-3")
	for _, o := range []client.Object{m1, m2} {
		if _, _, err := EnsureMirror(t.Context(), e.c, e.reader, testIdentity(nil), tenant, o); err != nil {
			t.Fatal(err)
		}
	}
	stale := e.mirror(t)
	if _, _, err := EnsureMirror(t.Context(), e.c, e.reader, testIdentity(nil), tenant, m3); err != nil {
		t.Fatal(err)
	}
	if removed, err := RemoveOwner(t.Context(), e.c, stale, m1); removed || !apierrors.IsConflict(err) {
		t.Fatalf("stale Update: removed %v, err %v; want false, Conflict", removed, err)
	}
	if refs := e.mirror(t).OwnerReferences; len(refs) != 3 {
		t.Errorf("ownerRefs = %v; want all three", refs)
	}
}
