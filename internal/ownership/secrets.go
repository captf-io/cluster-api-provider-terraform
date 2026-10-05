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

package ownership

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// RefChange says what EnsureRef changed in a list of owner references.
type RefChange int

const (
	// RefUnchanged means the list already held exactly one reference to
	// the object, with its current UID.
	RefUnchanged RefChange = iota
	// RefAdded means no reference named the object; one was appended.
	RefAdded
	// RefReplaced means a reference named the object with another UID (a
	// restore, or a deleted and recreated object of the same name), or
	// named it more than once; the list now names it once, with its
	// current UID.
	RefReplaced
)

// SecretOwnerRef returns the owner reference CAPTF's per-object Secrets
// (state chunks, state backups, durable inputs, plan key) carry to obj, a
// Terraform* object: obj's apiVersion and kind resolved through scheme
// (typed objects carry no TypeMeta), its name and current UID, with
// neither controller nor blockOwnerDeletion set. It returns an error when
// scheme does not know obj's type.
func SecretOwnerRef(obj client.Object, scheme *runtime.Scheme) (metav1.OwnerReference, error) {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return metav1.OwnerReference{}, fmt.Errorf("ownership: kind of %T: %w", obj, err)
	}
	return metav1.OwnerReference{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind, Name: obj.GetName(), UID: obj.GetUID()}, nil
}

// sameObject reports whether ref names the object of group, kind and name,
// whatever its UID and API version; a malformed apiVersion never matches.
func sameObject(ref metav1.OwnerReference, group, kind, name string) bool {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	return err == nil && gv.Group == group && ref.Kind == kind && ref.Name == name
}

// EnsureRef returns refs changed to hold exactly one reference to want's
// object (want's apiVersion group, kind and name), carrying want's UID,
// and what it changed. A reference that already carries want's UID is
// kept as it is; otherwise the first reference to the object is replaced
// in place by want, or want is appended when there is none. Every other
// reference to the object (a stale UID, a duplicate) is dropped, and
// references to other owners are kept in order: a credential mirror has
// one per object using it. refs itself is never modified; with
// RefUnchanged the result is refs.
func EnsureRef(refs []metav1.OwnerReference, want metav1.OwnerReference) ([]metav1.OwnerReference, RefChange) {
	gv, err := schema.ParseGroupVersion(want.APIVersion)
	if err != nil {
		return refs, RefUnchanged
	}
	same, current := 0, -1
	for i, ref := range refs {
		if !sameObject(ref, gv.Group, want.Kind, want.Name) {
			continue
		}
		same++
		if current < 0 && ref.UID == want.UID {
			current = i
		}
	}
	switch {
	case same == 0:
		out := make([]metav1.OwnerReference, 0, len(refs)+1)
		return append(append(out, refs...), want), RefAdded
	case same == 1 && current >= 0:
		return refs, RefUnchanged
	}
	out := make([]metav1.OwnerReference, 0, len(refs)-same+1)
	placed := false
	for i, ref := range refs {
		switch {
		case !sameObject(ref, gv.Group, want.Kind, want.Name):
			out = append(out, ref)
		case i == current:
			out = append(out, ref)
			placed = true
		case current < 0 && !placed:
			out = append(out, want)
			placed = true
		}
	}
	return out, RefReplaced
}

// LabeledFor reports whether labels, a Secret's, name the object of kind
// and name through state.OwnerKindLabel and state.OwnerNameLabel (the
// latter as state.LabelValue(name)). Those labels, set by whoever created
// the Secret, are what tie a Secret to one object; a Secret without them,
// or naming another object, is never claimed.
func LabeledFor(labels map[string]string, kind, name string) bool {
	return labels[state.OwnerKindLabel] == kind && labels[state.OwnerNameLabel] == state.LabelValue(name)
}

// NeedsRepair reports whether the Secret whose metadata is meta is labeled
// for want's object (LabeledFor) and lacks exactly one owner reference to
// it with its current UID (EnsureRef would change it).
func NeedsRepair(meta *metav1.ObjectMeta, want metav1.OwnerReference) bool {
	if !LabeledFor(meta.Labels, want.Kind, want.Name) {
		return false
	}
	_, change := EnsureRef(meta.OwnerReferences, want)
	return change != RefUnchanged
}

// RepairSecret makes the Secret whose metadata is meta, as read with its
// resourceVersion, hold exactly one owner reference to want's object
// (EnsureRef), using ctx and c. It patches only metadata.ownerReferences,
// with a merge patch locked to meta's resourceVersion, and only when
// NeedsRepair holds, so a Secret labeled for another object is never
// touched. It reports whether it patched. A Secret gone since it was read
// is false with a nil error; a conflict (the Secret changed since) and
// every other patch error are returned for the caller to skip or retry.
func RepairSecret(ctx context.Context, c client.Writer, meta *metav1.ObjectMeta, want metav1.OwnerReference) (bool, error) {
	if !NeedsRepair(meta, want) {
		return false, nil
	}
	refs, _ := EnsureRef(meta.OwnerReferences, want)
	// Metadata only: the diff, and so the patch, is the owner references
	// plus the resourceVersion of the optimistic lock, never the data.
	orig := &corev1.Secret{ObjectMeta: *meta.DeepCopy()}
	s := orig.DeepCopy()
	s.OwnerReferences = refs
	err := c.Patch(ctx, s, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("ownership: owner references of Secret %s: %w", meta.Name, err)
	}
	return true, nil
}
