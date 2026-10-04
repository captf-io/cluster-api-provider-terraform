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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/ownership"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Mirror metadata. The mirror also carries state.ManagedLabel, so the
// manager's Secret cache and watch see it, and inputs.IdentityAnnotation
// naming its identity.
const (
	// MirrorPrefix starts every mirror Secret name.
	MirrorPrefix = "captf-creds-"
	// MirroredLabel marks a mirror Secret.
	MirroredLabel = "captf.io/mirrored"
	// SourceHashAnnotation holds the sha256 of the source Secret's data the
	// mirror was last written from.
	SourceHashAnnotation = "captf.io/source-hash"
)

// maxSecretName is the DNS subdomain limit on Secret names.
const maxSecretName = 253

// ErrMirrorConflict reports that a Secret with the mirror's name exists but
// is not a mirror of this identity; it is never overwritten.
var ErrMirrorConflict = errors.New("identity: secret with the mirror name is not a mirror of this identity")

// MirrorName returns the mirror Secret name for an identity, shared by every
// object of the namespace that uses it: captf-creds-<identity>. A name that
// would exceed 253 characters becomes captf-creds- plus the first 16 hex
// characters of the identity name's sha256. internal/jobs receives this name
// as its credentials Secret, so it is computed only here.
func MirrorName(identity string) string {
	if len(MirrorPrefix)+len(identity) <= maxSecretName {
		return MirrorPrefix + identity
	}
	sum := sha256.Sum256([]byte(identity))
	return MirrorPrefix + hex.EncodeToString(sum[:])[:16]
}

// SourceHash returns the hex sha256 of a Secret's data, over the keys in
// sorted order with length-prefixed keys and values, so no two different
// maps share a hash. Empty data has a hash too: the no-op identity's Secret
// may be empty.
func SourceHash(data map[string][]byte) string {
	h := sha256.New()
	var n [8]byte
	for _, k := range slices.Sorted(maps.Keys(data)) {
		binary.BigEndian.PutUint64(n[:], uint64(len(k)))
		h.Write(n[:])
		h.Write([]byte(k))
		binary.BigEndian.PutUint64(n[:], uint64(len(data[k])))
		h.Write(n[:])
		h.Write(data[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// MirrorResult says what EnsureMirror changed besides the data.
type MirrorResult struct {
	// Created is true when this call created the mirror.
	Created bool
	// OwnerRepaired is true when the mirror named owner with an earlier
	// UID and that ref was replaced by one with owner's current UID.
	OwnerRepaired bool
}

// EnsureMirror makes the identity's mirror in namespace match its source
// Secret and carry an ownerRef to owner:
//
//   - a missing mirror is (re)created;
//   - a mirror whose captf.io/source-hash differs is rewritten, which is how
//     rotation propagates;
//   - owner is added to the ownerRefs with blockOwnerDeletion=false, or,
//     when a ref names owner (its group, kind and name) with an earlier
//     UID, as a management-cluster restore leaves it, that ref is replaced
//     in place (ownership.EnsureRef); the refs of the namespace's other
//     objects using the mirror are left alone.
//
// The source is read through apiReader (uncached); a missing source is
// ErrSecretNotFound. The mirror is always Opaque: its data is a byte copy,
// and a typed source (for example a service-account token) could not be
// created under another name. A same-named Secret that is not a mirror of
// this identity is ErrMirrorConflict, and its refs are never touched. ctx
// bounds every call this makes; c is the client used to read and write the
// mirror; id is the identity whose mirror this ensures. It returns the
// mirror Secret, what this call changed (MirrorResult), and an error from
// a failed read or write.
func EnsureMirror(ctx context.Context, c client.Client, apiReader client.Reader, id *infrav1.TerraformClusterIdentity, namespace string, owner client.Object) (*corev1.Secret, MirrorResult, error) {
	src, err := SourceSecret(ctx, apiReader, id)
	if err != nil {
		return nil, MirrorResult{}, err
	}
	ref, err := ownerReference(owner, c.Scheme())
	if err != nil {
		return nil, MirrorResult{}, err
	}
	hash := SourceHash(src.Data)
	key := client.ObjectKey{Namespace: namespace, Name: MirrorName(id.Name)}

	m := &corev1.Secret{}
	err = c.Get(ctx, key, m)
	if apierrors.IsNotFound(err) {
		m = newMirror(key, id.Name, hash, src.Data, ref)
		err = c.Create(ctx, m)
		if err == nil {
			klog.FromContext(ctx).Info("Created credential mirror", "secret", key, "identity", id.Name)
			return m, MirrorResult{Created: true}, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return nil, MirrorResult{}, fmt.Errorf("identity: create mirror %s: %w", key, err)
		}
		// Another reconcile created it first; update that one.
		m = &corev1.Secret{}
		err = c.Get(ctx, key, m)
	}
	if err != nil {
		return nil, MirrorResult{}, fmt.Errorf("identity: get mirror %s: %w", key, err)
	}
	if m.Labels[MirroredLabel] != "true" || m.Annotations[inputs.IdentityAnnotation] != id.Name {
		return nil, MirrorResult{}, fmt.Errorf("%w: %s", ErrMirrorConflict, key)
	}

	changed, dataRewritten := false, false
	hadOwner := slices.ContainsFunc(m.OwnerReferences, func(r metav1.OwnerReference) bool { return r.UID == ref.UID })
	if m.Annotations[SourceHashAnnotation] != hash {
		dataRewritten = true
		m.Data = maps.Clone(src.Data)
		m.Annotations[SourceHashAnnotation] = hash
		changed = true
	}
	if m.Labels[state.ManagedLabel] != "true" {
		m.Labels[state.ManagedLabel] = "true"
		changed = true
	}
	var res MirrorResult
	if refs, change := ownership.EnsureRef(m.OwnerReferences, ref); change != ownership.RefUnchanged {
		m.OwnerReferences = refs
		res.OwnerRepaired = change == ownership.RefReplaced
		changed = true
	}
	if !changed {
		return m, res, nil
	}
	if err := c.Update(ctx, m); err != nil {
		return nil, MirrorResult{}, fmt.Errorf("identity: update mirror %s: %w", key, err)
	}
	klog.FromContext(ctx).Info("Updated credential mirror", "secret", key, "identity", id.Name,
		"ownerAdded", res.OwnerRepaired || !hadOwner, "ownerRepaired", res.OwnerRepaired, "dataRewritten", dataRewritten)
	return m, res, nil
}

// newMirror builds the mirror Secret at key for identity, with hash as its
// SourceHashAnnotation, data copied as its Data, and ref as its sole owner
// reference. It returns the built Secret, not yet created on the API
// server.
func newMirror(key client.ObjectKey, identity, hash string, data map[string][]byte, ref metav1.OwnerReference) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: key.Namespace,
			Name:      key.Name,
			Labels: map[string]string{
				MirroredLabel:      "true",
				state.ManagedLabel: "true",
			},
			Annotations: map[string]string{
				SourceHashAnnotation:      hash,
				inputs.IdentityAnnotation: identity,
			},
			OwnerReferences: []metav1.OwnerReference{ref},
		},
		Type: corev1.SecretTypeOpaque,
		Data: maps.Clone(data),
	}
}

// Revoke deletes the identity's mirror in namespace, whatever its owners:
// the identity no longer allows the namespace. A missing mirror, or a
// same-named Secret that is not a mirror of this identity, is left alone.
// ctx bounds the call and c is the client used to read and delete the
// mirror. It returns whether this call deleted the mirror, and an error
// from a failed read or delete.
func Revoke(ctx context.Context, c client.Client, identity, namespace string) (removed bool, _ error) {
	key := client.ObjectKey{Namespace: namespace, Name: MirrorName(identity)}
	m := &corev1.Secret{}
	if err := c.Get(ctx, key, m); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("identity: get mirror %s: %w", key, err)
	}
	if m.Labels[MirroredLabel] != "true" || m.Annotations[inputs.IdentityAnnotation] != identity {
		return false, nil
	}
	err := deleteMirror(ctx, c, m, identity)
	return err == nil, err
}

// RemoveOwner drops owner from the mirror's ownerRefs and deletes the mirror
// once no owner remains, so Destroy cleanup does not wait for garbage
// collection. The delete carries the UID and resourceVersion of mirror as
// read, and the Update sends that resourceVersion too: when another object
// of the namespace added its ownerRef in between, the API server answers
// Conflict, the mirror stays, and the returned error satisfies
// apierrors.IsConflict, so the caller retries from a fresh read rather
// than delete a mirror a Job now needs. ctx bounds the call and c is the
// client used to update or delete the mirror; mirror is the Secret as the
// caller read it, and owner is the object to drop. It returns whether it
// deleted the mirror, and an error from a failed update or delete.
func RemoveOwner(ctx context.Context, c client.Client, mirror *corev1.Secret, owner client.Object) (removed bool, _ error) {
	refs := slices.DeleteFunc(slices.Clone(mirror.OwnerReferences), func(r metav1.OwnerReference) bool {
		return r.UID == owner.GetUID()
	})
	if len(refs) == 0 {
		err := deleteMirror(ctx, c, mirror, mirror.Annotations[inputs.IdentityAnnotation],
			client.Preconditions{UID: nonEmpty(mirror.UID), ResourceVersion: nonEmpty(mirror.ResourceVersion)})
		return err == nil, err
	}
	if len(refs) == len(mirror.OwnerReferences) {
		return false, nil
	}
	mirror.OwnerReferences = refs
	if err := c.Update(ctx, mirror); err != nil {
		return false, fmt.Errorf("identity: update mirror %s: %w", client.ObjectKeyFromObject(mirror), err)
	}
	klog.FromContext(ctx).Info("Removed an owner from the credential mirror", "secret", client.ObjectKeyFromObject(mirror),
		"identity", mirror.Annotations[inputs.IdentityAnnotation], "owner", client.ObjectKeyFromObject(owner), "ownersLeft", len(refs))
	return false, nil
}

// nonEmpty returns a pointer to v, or nil when v is empty, so a
// precondition on an unset UID or resourceVersion is omitted. v is the
// value to point at.
func nonEmpty[T ~string](v T) *T {
	if v == "" {
		return nil
	}
	return &v
}

// deleteMirror deletes the mirror Secret m of the identity named identity
// through c with the delete options opts, bounded by ctx, and returns nil
// when it was already gone. It returns an error from a failed delete other
// than not-found; a failed precondition wraps the API server's Conflict.
func deleteMirror(ctx context.Context, c client.Client, m *corev1.Secret, identity string, opts ...client.DeleteOption) error {
	if err := client.IgnoreNotFound(c.Delete(ctx, m, opts...)); err != nil {
		return fmt.Errorf("identity: delete mirror %s: %w", client.ObjectKeyFromObject(m), err)
	}
	klog.FromContext(ctx).Info("Deleted credential mirror", "secret", client.ObjectKeyFromObject(m), "identity", identity)
	return nil
}

// ownerReference builds a non-controller ownerRef to obj with
// blockOwnerDeletion=false, resolving obj's kind through scheme. It returns
// the built OwnerReference, or an error when scheme cannot resolve obj's
// GroupVersionKind.
func ownerReference(obj client.Object, scheme *runtime.Scheme) (metav1.OwnerReference, error) {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return metav1.OwnerReference{}, fmt.Errorf("identity: owner kind: %w", err)
	}
	return metav1.OwnerReference{
		APIVersion:         gvk.GroupVersion().String(),
		Kind:               gvk.Kind,
		Name:               obj.GetName(),
		UID:                obj.GetUID(),
		BlockOwnerDeletion: new(false),
	}, nil
}
