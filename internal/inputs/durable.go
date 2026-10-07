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

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
	"github.com/captf-io/cluster-api-provider-terraform/internal/strutil"
)

// ErrNotFound is returned by Read when neither the durable Secret nor the
// applied Secret exists, and by the calls that patch the durable Secret
// when it does not exist.
var ErrNotFound = errors.New("inputs: durable inputs Secret not found")

// Name returns the durable Secret name, captf-inputs-<kindshort>-<name>. A
// name that would exceed 253 characters keeps its start and gets a hash of
// the full name appended, so it stays deterministic and unique.
func Name(kindshort, name string) string {
	return strutil.BoundedName(durablePrefix, kindshort, name)
}

// AppliedName returns the applied Secret name,
// captf-applied-<kindshort>-<name>, bounded as Name bounds its own.
func AppliedName(kindshort, name string) string {
	return strutil.BoundedName(appliedPrefix, kindshort, name)
}

// Record is one rendered root and what an apply Job ran it with: the
// files of the newest apply Job created (Durable.Attempt), or of the
// newest one that succeeded (Durable.Applied).
type Record struct {
	// Files are the rendered root and tfvars the Job mounted.
	Files render.Files
	// Image is spec.source.image as written when the Job started.
	Image string
	// Digest is the digest the runtime resolved Image to, repo@sha256:…,
	// read from the Job's pod; "" when unknown. Only the applied record
	// has one.
	Digest string
	// Identity is the TerraformClusterIdentity name, or the Secret name when
	// IdentityKind is Secret.
	Identity string
	// IdentityKind is the kind Identity names, as an infrav1.IdentityKind
	// string; "" for a TerraformClusterIdentity.
	IdentityKind string
	// InputsHash is the inputs hash the Job rendered; a successful apply
	// writes it to the state.
	InputsHash string
	// Job is the apply Job that mounted Files.
	Job string
	// MayHaveApplied is true, on the attempt record, once its Job failed
	// in a way that may have changed resources (MayHaveAppliedAnnotation).
	MayHaveApplied bool
}

// Durable is the content of an object's two inputs records: the durable
// Secret (the attempt record, and the object's metadata hub) and the
// applied Secret.
type Durable struct {
	// Attempt is the attempt record the durable Secret holds; nil when
	// that Secret does not exist.
	Attempt *Record
	// Applied is the applied record; nil before an apply succeeded, or
	// when the applied Secret does not exist.
	Applied *Record
	// AppliedMark is true once MarkApplied recorded a successful apply or
	// restore (AppliedAnnotation).
	AppliedMark bool
	// Secret is the durable Secret's metadata as Read returned it: what an
	// owner-reference repair checks and locks its patch to, without
	// reading the Secret again. Its Name is "" when the Secret does not
	// exist.
	Secret metav1.ObjectMeta
	// AppliedSecret is the applied Secret's metadata, as Secret is the
	// durable one's; its Name is "" when the Secret does not exist.
	AppliedSecret metav1.ObjectMeta
	// AppliedClusterOutputs is the captf_cluster_outputs value of a
	// TerraformMachinePool's last successful apply
	// (AppliedClusterOutputsKey); nil when none is recorded, or the
	// recorded value is not JSON or not the one AppliedExportsHash names.
	// WriteAttempt keeps it while it fits.
	AppliedClusterOutputs json.RawMessage
	// AppliedExportsHash is hash.Exports of those exports
	// (AppliedClusterOutputsHashAnnotation, else the hash of a record
	// written before it); "" when neither is recorded. It stays when the
	// record is dropped for size.
	AppliedExportsHash string
	// Pending is the pool's pending exports change
	// (PendingClusterOutputsAnnotation); nil when none, or unparsable.
	// WriteAttempt keeps it.
	Pending *Pending
	// Partial is the pool's change of the exports that may be partly
	// applied (PartialClusterOutputsAnnotation); nil when none.
	// WriteAttempt keeps it.
	Partial *Partial
	// InterruptedApply is the apply Job whose outcome is unconfirmed: it
	// ended without a result, or is gone before it finished
	// (InterruptedApplyAnnotation); "" when none. WriteAttempt keeps it.
	InterruptedApply string
	// Unpullable lists the image references a non-apply Job could not
	// pull since the last successful apply (UnpullableImagesAnnotation),
	// with when each was recorded, oldest first; nil when none.
	// UnpullableRefs gives those still passed over. WriteAttempt keeps it.
	Unpullable []Unpullable
}

// LastAttempt returns d's attempt record; nil when d is nil or has none.
func (d *Durable) LastAttempt() *Record {
	if d == nil {
		return nil
	}
	return d.Attempt
}

// AppliedOrAttempt returns d's applied record, else its attempt record
// (an object whose apply never succeeded); nil when d is nil or has
// neither.
func (d *Durable) AppliedOrAttempt() *Record {
	switch {
	case d == nil:
		return nil
	case d.Applied != nil:
		return d.Applied
	}
	return d.Attempt
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

// appliedName returns owner's applied Secret name and Kubernetes kind, as
// ownerName returns the durable Secret's; c supplies the scheme that
// resolves owner's kind. It returns an error when the kind is unknown.
func appliedName(c client.Client, owner client.Object) (name, kind string, err error) {
	gvk, err := apiutil.GVKForObject(owner, c.Scheme())
	if err != nil {
		return "", "", fmt.Errorf("inputs: kind of %T: %w", owner, err)
	}
	short, err := state.KindShort(gvk.Kind)
	if err != nil {
		return "", "", fmt.Errorf("inputs: %w", err)
	}
	return AppliedName(short, owner.GetName()), gvk.Kind, nil
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

// WriteAttempt creates or updates owner's durable Secret with rec, the
// files of an apply Job just created (or a running one adopted) and what
// it runs with: the attempt record. The Secret gets exactly one
// ownerReference (owner, blockOwnerDeletion unset) and the two data keys,
// plus the recorded AppliedClusterOutputsKey while it fits next to the
// files (fitsNext: files and record together within maxDataBytes,
// 1,000,000 bytes, the budget render allows the files alone); any other
// key is dropped. A record that no longer fits is dropped without an
// error, so a write never fails on the Secret's size: a pool then has no
// exports to hold, but their hash (AppliedClusterOutputsHashAnnotation)
// still tells a change of them, so it guards any change and, a
// destructive one, waits for its approval instead of holding it, until a
// successful apply records the exports again.
//
// It sets the image, identity, identity-kind, inputs-hash and job
// annotations from rec, and keeps MayHaveAppliedAnnotation: it marks that
// an attempt since the last successful apply may have changed resources
// the applied record does not describe, which a retry that fails before
// its apply step does not undo, so only a successful apply's promotion
// or a restore removes it (ClearMayHaveApplied). It never touches AppliedAnnotation,
// PendingClusterOutputsAnnotation, PartialClusterOutputsAnnotation,
// AppliedClusterOutputsHashAnnotation, InterruptedApplyAnnotation or
// UnpullableImagesAnnotation, nor the applied Secret. rec.Digest and rec.MayHaveApplied are ignored.
// Between applies, the reconciler keeps the owner reference pointing at
// owner's current UID (ownership.RepairSecret on the metadata Read
// returned), so a Secret restored without it, or with owner's earlier
// UID, is owned again.
//
// It uses ctx for the get, create and patch calls through c, and returns
// any error from them or from resolving owner's kind and owner reference.
func WriteAttempt(ctx context.Context, c client.Client, owner client.Object, rec Record) error {
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
		setRecord(s, refs, kind, owner.GetName(), rec)
		if err := c.Create(ctx, s); err != nil {
			return fmt.Errorf("inputs: create %s: %w", name, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("inputs: get %s: %w", name, err)
	}
	orig := existing.DeepCopy()
	applied := existing.Data[AppliedClusterOutputsKey]
	setRecord(existing, refs, kind, owner.GetName(), rec)
	if len(applied) > 0 && fitsNext(rec.Files, applied) {
		existing.Data[AppliedClusterOutputsKey] = applied
	}
	if err := c.Patch(ctx, existing, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("inputs: update %s: %w", name, err)
	}
	return nil
}

// Promote creates or replaces owner's applied Secret with rec, the files
// of an apply Job that succeeded and what it ran with, its image digest
// included: the applied record. The Secret has the durable Secret's owner
// reference and labels, so it moves, is garbage collected and is retained
// with it, and holds nothing but the two files and the record's
// annotations. Two Secrets, not one: a copy of the files of both records
// may not fit one Secret. It uses ctx for the get, create and patch calls
// through c, and returns any error from them or from resolving owner's
// kind and owner reference.
func Promote(ctx context.Context, c client.Client, owner client.Object, rec Record) error {
	name, kind, err := appliedName(c, owner)
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
		setRecord(s, refs, kind, owner.GetName(), rec)
		setDigest(s, rec.Digest)
		if err := c.Create(ctx, s); err != nil {
			return fmt.Errorf("inputs: create %s: %w", name, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("inputs: get %s: %w", name, err)
	}
	orig := existing.DeepCopy()
	setRecord(existing, refs, kind, owner.GetName(), rec)
	setDigest(existing, rec.Digest)
	if err := c.Patch(ctx, existing, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("inputs: update %s: %w", name, err)
	}
	return nil
}

// setRecord sets what WriteAttempt and Promote own on s: refs as the
// owner references, rec's files as the data, kind and ownerName as the
// owner-kind/owner-name labels, state.ProtectionFinalizer (a record is
// what a destroy renders, so a cascading deletion must not take it), and
// rec's image, identity, inputs-hash and job annotations.
func setRecord(s *corev1.Secret, refs []metav1.OwnerReference, kind, ownerName string, rec Record) {
	s.OwnerReferences = refs
	s.Data = fileData(rec.Files)
	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	s.Labels[state.ManagedLabel] = "true"
	s.Labels[state.OwnerKindLabel] = kind
	s.Labels[state.OwnerNameLabel] = state.LabelValue(ownerName)
	state.Protect(&s.ObjectMeta)
	setAnnotations(&s.ObjectMeta, rec)
	metav1.SetMetaDataAnnotation(&s.ObjectMeta, JobAnnotation, rec.Job)
}

// setAnnotations sets rec's image, identity, identity kind and inputs
// hash as annotations on m: what CreateRun records on a per-run Secret
// and setRecord on either record.
func setAnnotations(m *metav1.ObjectMeta, rec Record) {
	metav1.SetMetaDataAnnotation(m, ImageAnnotation, rec.Image)
	metav1.SetMetaDataAnnotation(m, IdentityAnnotation, rec.Identity)
	if rec.IdentityKind == string(infrav1.IdentityKindSecret) {
		metav1.SetMetaDataAnnotation(m, IdentityKindAnnotation, rec.IdentityKind)
	} else {
		delete(m.Annotations, IdentityKindAnnotation)
	}
	metav1.SetMetaDataAnnotation(m, InputsHashAnnotation, rec.InputsHash)
}

// setDigest sets digest on s as ImageDigestAnnotation, or removes it when
// digest is "".
func setDigest(s *corev1.Secret, digest string) {
	if digest == "" {
		delete(s.Annotations, ImageDigestAnnotation)
		return
	}
	metav1.SetMetaDataAnnotation(&s.ObjectMeta, ImageDigestAnnotation, digest)
}

// recordOf returns the record s, a durable, applied or per-run Secret,
// holds.
func recordOf(s *corev1.Secret) *Record {
	return &Record{
		Files:          render.Files{MainTF: s.Data[MainTFKey], TFVars: s.Data[TFVarsKey]},
		Image:          s.Annotations[ImageAnnotation],
		Digest:         s.Annotations[ImageDigestAnnotation],
		Identity:       s.Annotations[IdentityAnnotation],
		IdentityKind:   s.Annotations[IdentityKindAnnotation],
		InputsHash:     s.Annotations[InputsHashAnnotation],
		Job:            s.Annotations[JobAnnotation],
		MayHaveApplied: s.Annotations[MayHaveAppliedAnnotation] == "true",
	}
}

// Read returns the inputs records of the object named name, of short kind
// kindshort, in namespace, read through c using ctx: the durable Secret
// (the attempt record and the metadata it carries) and the applied
// Secret. Either may be missing. It returns ErrNotFound if neither
// exists.
func Read(ctx context.Context, c client.Reader, namespace, kindshort, name string) (*Durable, error) {
	d := &Durable{}
	s := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: Name(kindshort, name)}
	switch err := c.Get(ctx, key, s); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, fmt.Errorf("inputs: get %s: %w", key.Name, err)
	default:
		d.Attempt = recordOf(s)
		d.Attempt.Digest = ""
		d.AppliedMark = s.Annotations[AppliedAnnotation] == "true"
		d.Secret = s.ObjectMeta
		d.Pending = parsePending(s.Annotations[PendingClusterOutputsAnnotation])
		d.Partial = parsePartial(s.Annotations[PartialClusterOutputsAnnotation])
		d.InterruptedApply = s.Annotations[InterruptedApplyAnnotation]
		d.Unpullable = parseUnpullable(s.Annotations[UnpullableImagesAnnotation])
		d.AppliedExportsHash, d.AppliedClusterOutputs = appliedExports(s)
	}
	a := &corev1.Secret{}
	key.Name = AppliedName(kindshort, name)
	switch err := c.Get(ctx, key, a); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, fmt.Errorf("inputs: get %s: %w", key.Name, err)
	default:
		d.Applied = recordOf(a)
		d.Applied.MayHaveApplied = false
		d.AppliedSecret = a.ObjectMeta
	}
	if d.Attempt == nil && d.Applied == nil {
		return nil, ErrNotFound
	}
	return d, nil
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

// SetMayHaveApplied marks the attempt record on owner's durable Secret
// as one whose Job may have changed resources (MayHaveAppliedAnnotation),
// with one merge patch through c using ctx. The next WriteAttempt, or
// ClearMayHaveApplied, removes it. It returns ErrNotFound when the Secret
// does not exist, or any other patch error.
func SetMayHaveApplied(ctx context.Context, c client.Client, owner client.Object) error {
	return patchAnnotation(ctx, c, owner, MayHaveAppliedAnnotation, "true")
}

// ClearMayHaveApplied removes MayHaveAppliedAnnotation from owner's
// durable Secret, with one merge patch through c using ctx. It returns
// ErrNotFound when the Secret does not exist, or any other patch error.
func ClearMayHaveApplied(ctx context.Context, c client.Client, owner client.Object) error {
	return patchAnnotation(ctx, c, owner, MayHaveAppliedAnnotation, nil)
}

// SetInterruptedApply records job, an apply Job of owner whose outcome is
// unconfirmed (it ended without a result, or is gone before it finished),
// on owner's durable Secret (InterruptedApplyAnnotation), with one merge
// patch through c using ctx. Only ClearInterruptedApply removes it. It
// returns ErrNotFound when the Secret does not exist, or any other patch
// error.
func SetInterruptedApply(ctx context.Context, c client.Client, owner client.Object, job string) error {
	return patchAnnotation(ctx, c, owner, InterruptedApplyAnnotation, job)
}

// ClearInterruptedApply removes InterruptedApplyAnnotation from owner's
// durable Secret, with one merge patch through c using ctx, once an apply
// started after the unconfirmed one succeeded, a restore replaced the
// state, or the operator confirmed it created nothing. It returns
// ErrNotFound when the Secret does not exist, or any other patch error.
func ClearInterruptedApply(ctx context.Context, c client.Client, owner client.Object) error {
	return patchAnnotation(ctx, c, owner, InterruptedApplyAnnotation, nil)
}

// Delete removes owner's durable and applied Secrets through c using ctx,
// taking state.ProtectionFinalizer off each first (state.DeleteSecretNamed),
// and returns any error other than a Secret already being gone.
func Delete(ctx context.Context, c client.Client, owner client.Object) error {
	durable, _, err := ownerName(c, owner)
	if err != nil {
		return err
	}
	applied, _, err := appliedName(c, owner)
	if err != nil {
		return err
	}
	for _, name := range []string{durable, applied} {
		if err := state.DeleteSecretNamed(ctx, c, owner.GetNamespace(), name); err != nil {
			return fmt.Errorf("inputs: delete %s: %w", name, err)
		}
	}
	return nil
}
