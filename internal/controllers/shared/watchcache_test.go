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
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/manager"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// secretMeta returns the metadata-only Secret a managed-Secret or variables
// watch delivers: om under the v1 Secret GVK (manager.SecretMeta).
func secretMeta(om metav1.ObjectMeta) *metav1.PartialObjectMetadata {
	m := manager.SecretMeta()
	m.ObjectMeta = om
	return m
}

// configMapMeta returns the metadata-only ConfigMap a variables watch
// delivers: om under the v1 ConfigMap GVK (manager.ConfigMapMeta).
func configMapMeta(om metav1.ObjectMeta) *metav1.PartialObjectMetadata {
	m := manager.ConfigMapMeta()
	m.ObjectMeta = om
	return m
}

// testMirror returns the metadata of a credential mirror in testNS used by
// the TerraformMachines named owners, as identity.EnsureMirror labels it.
func testMirror(owners ...string) *metav1.PartialObjectMetadata {
	m := secretMeta(metav1.ObjectMeta{
		Namespace:   testNS,
		Name:        identity.MirrorName("fleet"),
		Labels:      map[string]string{state.ManagedLabel: "true", identity.MirroredLabel: "true"},
		Annotations: map[string]string{inputs.IdentityAnnotation: "fleet"},
	})
	for _, o := range owners {
		m.OwnerReferences = append(m.OwnerReferences, metav1.OwnerReference{
			APIVersion: infrav1.GroupVersion.String(), Kind: state.KindTerraformMachine, Name: o,
		})
	}
	return m
}

// TestManagedSecretEvents proves ManagedSecretEvents passes every event of
// a managed state Secret, and only the deletion of a credential mirror: an
// ownerRef added for one more user must not wake every user of the
// identity (finding 5). An unmanaged Secret passes nothing.
func TestManagedSecretEvents(t *testing.T) {
	t.Parallel()
	stateSecret := secretMeta(metav1.ObjectMeta{Namespace: testNS, Name: "tfstate-default-x", ResourceVersion: "1",
		Labels: map[string]string{state.ManagedLabel: "true", state.OwnerKindLabel: state.KindTerraformMachine}})
	stateWritten := stateSecret.DeepCopy()
	stateWritten.ResourceVersion = "2"
	mirrorOld := testMirror("m1")
	mirrorNew := testMirror("m1", "m2")
	unmanaged := secretMeta(metav1.ObjectMeta{Namespace: testNS, Name: "user"})
	unmanagedNew := unmanaged.DeepCopy()
	unmanagedNew.Annotations = map[string]string{"k": "v"}

	p := ManagedSecretEvents()
	for _, tt := range []struct {
		name string
		got  bool
		want bool
	}{
		{"state create", p.Create(event.CreateEvent{Object: stateSecret}), true},
		{"state update", p.Update(event.UpdateEvent{ObjectOld: stateSecret, ObjectNew: stateWritten}), true},
		{"state delete", p.Delete(event.DeleteEvent{Object: stateSecret}), true},
		{"state generic", p.Generic(event.GenericEvent{Object: stateSecret}), true},
		{"mirror create", p.Create(event.CreateEvent{Object: mirrorOld}), false},
		{"mirror ownerRef-only update", p.Update(event.UpdateEvent{ObjectOld: mirrorOld, ObjectNew: mirrorNew}), false},
		{"mirror delete", p.Delete(event.DeleteEvent{Object: mirrorNew}), true},
		{"mirror generic", p.Generic(event.GenericEvent{Object: mirrorOld}), false},
		{"unmanaged create", p.Create(event.CreateEvent{Object: unmanaged}), false},
		{"unmanaged update", p.Update(event.UpdateEvent{ObjectOld: unmanaged, ObjectNew: unmanagedNew}), false},
		{"unmanaged delete", p.Delete(event.DeleteEvent{Object: unmanaged}), false},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
}

// TestSecretToOwnerMirrorDelete proves a deleted mirror, as its metadata
// watch delivers it, maps to every TerraformMachine using it, so each
// recreates it.
func TestSecretToOwnerMirrorDelete(t *testing.T) {
	t.Parallel()
	if got := names(SecretToOwner(state.KindTerraformMachine)(t.Context(), testMirror("m1", "m2"))); !slices.Equal(got, []string{"team-a/m1", "team-a/m2"}) {
		t.Errorf("mirror owners = %v", got)
	}
}

// TestIdentitySpecChanged proves IdentitySpecChanged passes an identity's
// creation, deletion and spec change, and drops a status-only update such
// as a new namespace in status.namespaces (finding 21).
func TestIdentitySpecChanged(t *testing.T) {
	t.Parallel()
	old := &infrav1.TerraformClusterIdentity{ObjectMeta: metav1.ObjectMeta{Name: "fleet", Generation: 1}}
	status := old.DeepCopy()
	status.Status.Namespaces = []string{testNS}
	spec := old.DeepCopy()
	spec.Generation = 2
	spec.Spec.SecretRef.Name = "rotated"

	p := IdentitySpecChanged()
	if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: status}) {
		t.Error("a status-only update passes")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: spec}) {
		t.Error("a spec update is dropped")
	}
	if !p.Create(event.CreateEvent{Object: old}) || !p.Delete(event.DeleteEvent{Object: old}) {
		t.Error("create or delete is dropped")
	}
}

// TestMirrorIdentityIndexer proves MirrorIdentityIndexer keys a mirror's
// metadata by the identity it mirrors, and indexes nothing for a Secret
// that lacks the mirror label or the identity annotation.
func TestMirrorIdentityIndexer(t *testing.T) {
	t.Parallel()
	if got := MirrorIdentityIndexer(testMirror()); !slices.Equal(got, []string{"fleet"}) {
		t.Errorf("mirror = %v, want [fleet]", got)
	}
	unlabeled := testMirror()
	delete(unlabeled.Labels, identity.MirroredLabel)
	unannotated := testMirror()
	unannotated.Annotations = nil
	for name, o := range map[string]*metav1.PartialObjectMetadata{"unlabeled": unlabeled, "unannotated": unannotated} {
		if got := MirrorIdentityIndexer(o); got != nil {
			t.Errorf("%s = %v, want none", name, got)
		}
	}
}
