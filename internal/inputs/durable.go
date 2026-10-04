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

package inputs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
	"github.com/captf-io/cluster-api-provider-terraform/internal/strutil"
)

// ErrNotFound is returned by Read when the durable Secret does not exist.
var ErrNotFound = errors.New("inputs: durable inputs Secret not found")

// ErrEmptyDigest is returned by PinDigest for an empty digest: a digest that
// could not be read leaves the previous value in place.
var ErrEmptyDigest = errors.New("inputs: empty image digest")

// Name returns the durable Secret name, captf-inputs-<kindshort>-<name>. A
// name that would exceed 253 characters keeps its start and gets a hash of
// the full name appended, so it stays deterministic and unique.
func Name(kindshort, name string) string {
	return strutil.BoundedName(durablePrefix, kindshort, name)
}

// Meta is the pinned execution context of the last apply.
type Meta struct {
	// Image is spec.source.image as written.
	Image string
	// Identity is the TerraformClusterIdentity name.
	Identity string
	// ImageDigest is repo@sha256:…; empty until pinned.
	ImageDigest string
	// Applied is true once MarkApplied recorded a successful apply or
	// restore (AppliedAnnotation). Read reports it; Write ignores it.
	Applied bool
}

// Durable is the content of a durable inputs Secret.
type Durable struct {
	Files render.Files
	Meta  Meta
	// Secret is the Secret's metadata as Read returned it: what an
	// owner-reference repair checks and locks its patch to, without
	// reading the Secret again. Write ignores it.
	Secret metav1.ObjectMeta
	// AppliedClusterOutputs is the captf_cluster_outputs value of a
	// TerraformMachinePool's last successful apply
	// (AppliedClusterOutputsKey); nil when none is recorded, or the
	// recorded value is not JSON or not the one AppliedExportsHash names.
	// Write keeps it while it fits.
	AppliedClusterOutputs json.RawMessage
	// AppliedExportsHash is hash.Exports of those exports
	// (AppliedClusterOutputsHashAnnotation, else the hash of a record
	// written before it); "" when neither is recorded. It stays when the
	// record is dropped for size.
	AppliedExportsHash string
	// Pending is the pool's pending exports change
	// (PendingClusterOutputsAnnotation); nil when none, or unparsable.
	// Write keeps it.
	Pending *Pending
	// Partial is the pool's change of the exports that may be partly
	// applied (PartialClusterOutputsAnnotation); nil when none. Write
	// keeps it.
	Partial *Partial
	// InterruptedApply is the apply Job that is gone before it finished
	// (InterruptedApplyAnnotation); "" when none. Write keeps it.
	InterruptedApply string
}

// ownerName returns owner's durable Secret name and Kubernetes kind, using
// c's scheme to look up the kind: typed objects carry no TypeMeta.
func ownerName(c client.Client, owner client.Object) (name, kind string, err error) {
	gvk, err := apiutil.GVKForObject(owner, c.Scheme())
	if err != nil {
		return "", "", fmt.Errorf("inputs: kind of %T: %w", owner, err)
	}
	short, err := state.KindShort(gvk.Kind)
	if err != nil {
		return "", "", fmt.Errorf("inputs: %w", err)
	}
	return Name(short, owner.GetName()), gvk.Kind, nil
}

// soleOwnerRef returns exactly one ownerReference to owner, with
// blockOwnerDeletion unset, built with c's scheme.
func soleOwnerRef(c client.Client, owner client.Object) ([]metav1.OwnerReference, error) {
	// SetOwnerReference rejects a namespaced owner on an object without a
	// namespace, so the holder takes the owner's.
	holder := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: owner.GetNamespace()}}
	if err := controllerutil.SetOwnerReference(owner, holder, c.Scheme(), func(r *metav1.OwnerReference) { r.BlockOwnerDeletion = nil }); err != nil {
		return nil, fmt.Errorf("inputs: owner reference: %w", err)
	}
	return holder.OwnerReferences, nil
}

// fileData returns files as a Secret data map under MainTFKey and TFVarsKey.
func fileData(files render.Files) map[string][]byte {
	return map[string][]byte{MainTFKey: files.MainTF, TFVarsKey: files.TFVars}
}

// Write creates or updates owner's durable inputs Secret with files and
// meta. The Secret gets exactly one ownerReference (owner, blockOwnerDeletion
// unset) and the two data keys, plus the recorded AppliedClusterOutputsKey
// while it fits next to files (fitsNext: files and record together within
// maxDataBytes, 1,000,000 bytes, the budget render allows the files
// alone); any other key is dropped. A record that no longer fits is
// dropped without an error, so a write never fails on the Secret's size:
// a pool then has no exports to hold, but their hash
// (AppliedClusterOutputsHashAnnotation) still tells a change of them, so
// it guards any change and, a destructive one, waits for its approval
// instead of holding it, until a successful apply records the exports
// again. It never touches PendingClusterOutputsAnnotation,
// PartialClusterOutputsAnnotation, AppliedClusterOutputsHashAnnotation or
// InterruptedApplyAnnotation.
// Between applies, the reconciler
// keeps that reference pointing at owner's current UID
// (ownership.RepairSecret on the metadata Read returned), so a Secret
// restored without it, or with owner's earlier UID, is owned again.
//
// The image digest is never overwritten with another digest here: Write
// sets it only when the Secret has none and meta.ImageDigest is non-empty,
// and PinDigest is the only way to replace it. When meta.Image differs from
// the stored image, Write clears the stored digest: it belongs to the old
// image, and pairing it with the new one would run the old image's code
// against a state the new image's apply may have partly written. The next
// successful apply pins the new image's digest; until then other
// operations run the spec reference (DigestUnknown). Write never touches
// AppliedAnnotation: only MarkApplied sets it, and nothing removes it.
//
// It uses ctx for the get, create and patch calls through c, and returns
// any error from them or from resolving owner's kind and owner reference.
func Write(ctx context.Context, c client.Client, owner client.Object, files render.Files, meta Meta) error {
	name, kind, err := ownerName(c, owner)
	if err != nil {
		return err
	}
	refs, err := soleOwnerRef(c, owner)
	if err != nil {
		return err
	}
	existing := &corev1.Secret{}
	err = c.Get(ctx, client.ObjectKey{Namespace: owner.GetNamespace(), Name: name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: owner.GetNamespace(), Name: name},
			Type:       corev1.SecretTypeOpaque,
		}
		apply(s, refs, kind, owner.GetName(), files, meta)
		if meta.ImageDigest != "" {
			metav1.SetMetaDataAnnotation(&s.ObjectMeta, ImageDigestAnnotation, meta.ImageDigest)
		}
		if err := c.Create(ctx, s); err != nil {
			return fmt.Errorf("inputs: create %s: %w", name, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("inputs: get %s: %w", name, err)
	}
	orig := existing.DeepCopy()
	if existing.Annotations[ImageAnnotation] != meta.Image {
		delete(existing.Annotations, ImageDigestAnnotation)
	}
	applied := existing.Data[AppliedClusterOutputsKey]
	apply(existing, refs, kind, owner.GetName(), files, meta)
	if len(applied) > 0 && fitsNext(files, applied) {
		existing.Data[AppliedClusterOutputsKey] = applied
	}
	if existing.Annotations[ImageDigestAnnotation] == "" && meta.ImageDigest != "" {
		metav1.SetMetaDataAnnotation(&existing.ObjectMeta, ImageDigestAnnotation, meta.ImageDigest)
	}
	if err := c.Patch(ctx, existing, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("inputs: update %s: %w", name, err)
	}
	return nil
}

// apply sets everything Write owns on s, except the digest: refs as the
// owner references, files as the data, and kind, ownerName and meta as the
// owner-kind/owner-name labels and image/identity annotations.
func apply(s *corev1.Secret, refs []metav1.OwnerReference, kind, ownerName string, files render.Files, meta Meta) {
	s.OwnerReferences = refs
	s.Data = fileData(files)
	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	s.Labels[state.ManagedLabel] = "true"
	s.Labels[state.OwnerKindLabel] = kind
	s.Labels[state.OwnerNameLabel] = state.LabelValue(ownerName)
	metav1.SetMetaDataAnnotation(&s.ObjectMeta, ImageAnnotation, meta.Image)
	metav1.SetMetaDataAnnotation(&s.ObjectMeta, IdentityAnnotation, meta.Identity)
}

// Read returns the durable inputs of the object named name, of short kind
// kindshort, in namespace, read through c using ctx. It returns ErrNotFound
// if the Secret does not exist.
func Read(ctx context.Context, c client.Reader, namespace, kindshort, name string) (*Durable, error) {
	s := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: Name(kindshort, name)}
	if err := c.Get(ctx, key, s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("inputs: get %s: %w", key.Name, err)
	}
	d := &Durable{
		Files: render.Files{MainTF: s.Data[MainTFKey], TFVars: s.Data[TFVarsKey]},
		Meta: Meta{
			Image:       s.Annotations[ImageAnnotation],
			Identity:    s.Annotations[IdentityAnnotation],
			ImageDigest: s.Annotations[ImageDigestAnnotation],
			Applied:     s.Annotations[AppliedAnnotation] == "true",
		},
		Secret:           s.ObjectMeta,
		Pending:          parsePending(s.Annotations[PendingClusterOutputsAnnotation]),
		Partial:          parsePartial(s.Annotations[PartialClusterOutputsAnnotation]),
		InterruptedApply: s.Annotations[InterruptedApplyAnnotation],
	}
	d.AppliedExportsHash, d.AppliedClusterOutputs = appliedExports(s)
	return d, nil
}

// PinDigest records digest on owner's durable Secret, read and patched
// through c using ctx. With force false it is set only when unset:
// immutable machines keep the digest of their first apply forever. Clusters
// and pools re-pin after every apply with force true. It reports whether it
// changed the pin.
func PinDigest(ctx context.Context, c client.Client, owner client.Object, digest string, force bool) (bool, error) {
	if digest == "" {
		return false, ErrEmptyDigest
	}
	name, _, err := ownerName(c, owner)
	if err != nil {
		return false, err
	}
	s := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: owner.GetNamespace(), Name: name}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("inputs: get %s: %w", name, err)
	}
	current := s.Annotations[ImageDigestAnnotation]
	if current == digest || (current != "" && !force) {
		return false, nil
	}
	orig := s.DeepCopy()
	metav1.SetMetaDataAnnotation(&s.ObjectMeta, ImageDigestAnnotation, digest)
	if err := c.Patch(ctx, s, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, fmt.Errorf("inputs: pin digest on %s: %w", name, err)
	}
	return true, nil
}

// markAppliedPatch is the merge patch MarkApplied sends.
var markAppliedPatch = []byte(`{"metadata":{"annotations":{"` + AppliedAnnotation + `":"true"}}}`)

// MarkApplied sets AppliedAnnotation on owner's durable Secret through c
// using ctx, once an apply of owner succeeded or a state backup was
// restored for it. It is one merge patch, without a read: setting the
// marker again is harmless, so callers skip it once Read reports it. It
// returns ErrNotFound when the Secret does not exist, or any other patch
// error.
func MarkApplied(ctx context.Context, c client.Client, owner client.Object) error {
	name, _, err := ownerName(c, owner)
	if err != nil {
		return err
	}
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: owner.GetNamespace(), Name: name}}
	if err := c.Patch(ctx, s, client.RawPatch(types.MergePatchType, markAppliedPatch)); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNotFound
		}
		return fmt.Errorf("inputs: mark %s applied: %w", name, err)
	}
	return nil
}

// SetInterruptedApply records job, an apply Job of owner that is gone
// before it finished, on owner's durable Secret
// (InterruptedApplyAnnotation), with one merge patch through c using ctx.
// Only ClearInterruptedApply removes it. It returns ErrNotFound when the
// Secret does not exist, or any other patch error.
func SetInterruptedApply(ctx context.Context, c client.Client, owner client.Object, job string) error {
	return patchAnnotation(ctx, c, owner, InterruptedApplyAnnotation, job)
}

// ClearInterruptedApply removes InterruptedApplyAnnotation from owner's
// durable Secret, with one merge patch through c using ctx, once an apply
// started after the interrupted one succeeded. It returns ErrNotFound when
// the Secret does not exist, or any other patch error.
func ClearInterruptedApply(ctx context.Context, c client.Client, owner client.Object) error {
	return patchAnnotation(ctx, c, owner, InterruptedApplyAnnotation, nil)
}

// Delete removes owner's durable Secret through c using ctx, and returns
// any error other than the Secret already being gone.
func Delete(ctx context.Context, c client.Client, owner client.Object) error {
	name, _, err := ownerName(c, owner)
	if err != nil {
		return err
	}
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: owner.GetNamespace(), Name: name}}
	if err := client.IgnoreNotFound(c.Delete(ctx, s)); err != nil {
		return fmt.Errorf("inputs: delete %s: %w", name, err)
	}
	return nil
}
