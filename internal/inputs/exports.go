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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
)

// maxDataBytes bounds the durable Secret's data: the rendered files plus
// the recorded exports stay within the budget render allows the files
// alone, comfortably under what a Secret can hold. Files close enough to
// it leave no room for the record (Write, RecordClusterOutputs).
const maxDataBytes = 1_000_000

// Pending is a TerraformMachinePool's change of the cluster's exports
// whose guarded apply was blocked before a destructive plan
// (PendingClusterOutputsAnnotation).
type Pending struct {
	// ExportsHash is hash.Exports of the exports the blocked apply
	// rendered: the change that waits.
	ExportsHash string `json:"exportsHash"`
	// ApprovalHash is the approval hash the blocked apply's inputs had
	// (hash.Approval).
	ApprovalHash string `json:"approvalHash"`
	// Job is the blocked apply Job's name.
	Job string `json:"job"`
	// Summary is the runner's summary of what the plan would delete or
	// replace: addresses and actions, never values; "" when unknown.
	Summary string `json:"summary,omitempty"`
}

// parsePending returns the Pending encoded in raw, an annotation value, or
// nil when raw is empty or does not parse into one that names a change.
func parsePending(raw string) *Pending {
	if raw == "" {
		return nil
	}
	var p Pending
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p.ExportsHash == "" {
		return nil
	}
	return &p
}

// Partial is a TerraformMachinePool's change of the cluster's exports
// that may be partly applied (PartialClusterOutputsAnnotation): a guarded
// apply of it failed in its apply step, or after its runner started
// without a result to tell how far it got.
type Partial struct {
	// ExportsHash is hash.Exports of the exports the failed apply
	// rendered.
	ExportsHash string `json:"exportsHash"`
	// Job is the failed apply Job's name.
	Job string `json:"job"`
}

// parsePartial returns the Partial encoded in raw, an annotation value.
// An empty value is nil. A value that does not parse into one naming a
// Job still counts, as a Partial with no Job: the record only ever makes
// the pool more careful, so a damaged one must not make it less.
func parsePartial(raw string) *Partial {
	if raw == "" {
		return nil
	}
	var p Partial
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return &Partial{}
	}
	return &p
}

// appliedExports returns the hash of the exports s, a durable Secret,
// records as those of the last successful apply
// (AppliedClusterOutputsHashAnnotation, else the hash of the
// AppliedClusterOutputsKey record, written before the annotation
// existed), and the recorded value: nil when there is none, or it is not
// JSON, or it is not what the annotation names. The hash is "" when
// neither is recorded.
func appliedExports(s *corev1.Secret) (string, json.RawMessage) {
	h, raw := s.Annotations[AppliedClusterOutputsHashAnnotation], s.Data[AppliedClusterOutputsKey]
	if len(raw) == 0 {
		return h, nil
	}
	got, err := hash.Exports(raw)
	switch {
	case err != nil:
		return h, nil
	case h == "":
		return got, raw
	case got != h:
		return h, nil
	}
	return h, raw
}

// fitsNext reports whether exports fit in the durable Secret next to
// files (maxDataBytes).
func fitsNext(files render.Files, exports []byte) bool {
	return files.Size()+len(exports) <= maxDataBytes
}

// LastClusterOutputs returns the captf_cluster_outputs value of d's
// rendered tfvars: the exports its apply rendered, which a successful
// apply's caller records with RecordClusterOutputs. A nil Record,
// unparsable tfvars or an absent key is nil.
func LastClusterOutputs(d *Record) json.RawMessage {
	if d == nil {
		return nil
	}
	var v struct {
		ClusterOutputs json.RawMessage `json:"captf_cluster_outputs"`
	}
	if err := json.Unmarshal(d.Files.TFVars, &v); err != nil {
		return nil
	}
	return v.ClusterOutputs
}

// RecordClusterOutputs records exports, the captf_cluster_outputs a
// successful apply of owner rendered, on owner's durable Secret
// (AppliedClusterOutputsKey) with their hash
// (AppliedClusterOutputsHashAnnotation), and removes
// PendingClusterOutputsAnnotation and PartialClusterOutputsAnnotation: a
// successful apply that rendered the cluster's own exports leaves no
// change pending, and none partly applied. Exports that do not fit next
// to the rendered files (fitsNext: the files and the record share
// maxDataBytes) are not recorded, and an older record is removed; their
// hash is recorded all the same. It reads and patches the Secret through
// c using ctx, the patch locked to the read resourceVersion. It reports
// whether exports are recorded, and returns ErrNotFound when the Secret
// does not exist, or any other error from hashing the exports, the read
// or the patch.
func RecordClusterOutputs(ctx context.Context, c client.Client, owner client.Object, exports json.RawMessage) (bool, error) {
	name, _, err := ownerName(c, owner)
	if err != nil {
		return false, err
	}
	h, err := hash.Exports(exports)
	if err != nil {
		return false, fmt.Errorf("inputs: %w", err)
	}
	s := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: owner.GetNamespace(), Name: name}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("inputs: get %s: %w", name, err)
	}
	files := render.Files{MainTF: s.Data[MainTFKey], TFVars: s.Data[TFVarsKey]}
	fits := fitsNext(files, exports)
	_, pending := s.Annotations[PendingClusterOutputsAnnotation]
	_, partial := s.Annotations[PartialClusterOutputsAnnotation]
	current, has := s.Data[AppliedClusterOutputsKey]
	if !pending && !partial && fits == has && (!fits || bytes.Equal(current, exports)) && s.Annotations[AppliedClusterOutputsHashAnnotation] == h {
		return fits, nil
	}
	orig := s.DeepCopy()
	metav1.SetMetaDataAnnotation(&s.ObjectMeta, AppliedClusterOutputsHashAnnotation, h)
	if fits {
		if s.Data == nil {
			s.Data = map[string][]byte{}
		}
		s.Data[AppliedClusterOutputsKey] = slices.Clone(exports)
	} else {
		delete(s.Data, AppliedClusterOutputsKey)
	}
	delete(s.Annotations, PendingClusterOutputsAnnotation)
	delete(s.Annotations, PartialClusterOutputsAnnotation)
	if err := c.Patch(ctx, s, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, fmt.Errorf("inputs: record cluster outputs on %s: %w", name, err)
	}
	return fits, nil
}

// ErrExportsRecorded is returned by SeedClusterOutputs when the durable
// Secret already records the hash of applied exports: there is nothing
// to seed.
var ErrExportsRecorded = errors.New("inputs: applied cluster exports already recorded")

// SeedClusterOutputs records exports, the captf_cluster_outputs a
// TerraformMachinePool that applied before such records existed is proven
// to have applied last, on owner's durable Secret, as RecordClusterOutputs
// does (the record when it fits next to the rendered files, its hash
// either way), once: it returns ErrExportsRecorded, and writes nothing,
// when the Secret records a hash already
// (AppliedClusterOutputsHashAnnotation). Unlike RecordClusterOutputs it
// keeps PendingClusterOutputsAnnotation and
// PartialClusterOutputsAnnotation: no apply succeeded now, so no change
// is settled by it. It reads and patches the Secret through c using ctx,
// the patch locked to the read resourceVersion. It reports whether the
// record fits, and returns ErrNotFound when the Secret does not exist, or
// any other error from hashing the exports, the read or the patch.
func SeedClusterOutputs(ctx context.Context, c client.Client, owner client.Object, exports json.RawMessage) (bool, error) {
	name, _, err := ownerName(c, owner)
	if err != nil {
		return false, err
	}
	h, err := hash.Exports(exports)
	if err != nil {
		return false, fmt.Errorf("inputs: %w", err)
	}
	s := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: owner.GetNamespace(), Name: name}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("inputs: get %s: %w", name, err)
	}
	if s.Annotations[AppliedClusterOutputsHashAnnotation] != "" {
		return false, ErrExportsRecorded
	}
	fits := fitsNext(render.Files{MainTF: s.Data[MainTFKey], TFVars: s.Data[TFVarsKey]}, exports)
	orig := s.DeepCopy()
	metav1.SetMetaDataAnnotation(&s.ObjectMeta, AppliedClusterOutputsHashAnnotation, h)
	if fits {
		if s.Data == nil {
			s.Data = map[string][]byte{}
		}
		s.Data[AppliedClusterOutputsKey] = slices.Clone(exports)
	} else {
		delete(s.Data, AppliedClusterOutputsKey)
	}
	if err := c.Patch(ctx, s, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, fmt.Errorf("inputs: seed cluster outputs on %s: %w", name, err)
	}
	return fits, nil
}

// SetPending records p on owner's durable Secret
// (PendingClusterOutputsAnnotation), with one merge patch through c using
// ctx. Only RecordClusterOutputs removes it: exports that return to the
// applied ones withdraw the change but keep its record, so a return to
// it holds it again. It returns ErrNotFound when the Secret does not
// exist, or any other error from encoding p or the patch.
func SetPending(ctx context.Context, c client.Client, owner client.Object, p Pending) error {
	value, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("inputs: encode pending change: %w", err)
	}
	return patchAnnotation(ctx, c, owner, PendingClusterOutputsAnnotation, string(value))
}

// SetPartial records p on owner's durable Secret
// (PartialClusterOutputsAnnotation), with one merge patch through c
// using ctx. Only RecordClusterOutputs removes it. It returns ErrNotFound
// when the Secret does not exist, or any other error from encoding p or
// the patch.
func SetPartial(ctx context.Context, c client.Client, owner client.Object, p Partial) error {
	value, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("inputs: encode partial change: %w", err)
	}
	return patchAnnotation(ctx, c, owner, PartialClusterOutputsAnnotation, string(value))
}

// patchAnnotation sets the annotation key on owner's durable Secret to
// value, a string, or removes it for a nil value, with one merge patch
// through c using ctx. It returns ErrNotFound when the Secret does not
// exist, or any other error from resolving the Secret's name, encoding
// or sending the patch.
func patchAnnotation(ctx context.Context, c client.Client, owner client.Object, key string, value any) error {
	name, _, err := ownerName(c, owner)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{key: value}}})
	if err != nil {
		return fmt.Errorf("inputs: encode patch: %w", err)
	}
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: owner.GetNamespace(), Name: name}}
	if err := c.Patch(ctx, s, client.RawPatch(types.MergePatchType, patch)); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNotFound
		}
		return fmt.Errorf("inputs: patch %s on %s: %w", key, name, err)
	}
	return nil
}
