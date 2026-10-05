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
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/captf-io/cluster-api-provider-terraform/internal/ownership"
	"github.com/captf-io/cluster-api-provider-terraform/internal/plankey"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// The kinds of Secret repairOwners counts, in the order it patches them.
const (
	ownedBackup  = "backups"
	ownedPlanKey = "planKey"
	ownedInputs  = "inputs"
	ownedState   = "state"
)

// ownedSecret is one Secret of the object whose owner references need a
// repair: what it is (ownedBackup, ...) and its metadata as read.
type ownedSecret struct {
	what string
	meta *metav1.ObjectMeta
}

// repairOwners makes the Secrets that belong to the object carry exactly
// one owner reference to its current UID (ownership.RepairSecret), using
// ctx: after a management-cluster restore the object has a new UID, and
// its Secrets come back with their references stripped or naming the old
// one; a state chunk a refresh or drift Job wrote has none. Without the
// repair they are never garbage-collected with the object, or are
// collected as orphans of a UID that no longer exists.
//
// It costs nothing on a pass where everything is owned: it checks the
// durable inputs metadata setup read and the state chunks' metadata
// readState listed (chunks, nil when the state was not read or Adopt just
// rewrote it). Only when one of those needs a repair, which is what a
// restore or an unowned chunk looks like, does it list the object's state
// backups (metadata only) and get its plan key, the Secrets no pass reads
// otherwise. Each Secret is identified by this object's deterministic
// names and selectors and claimed only when its owner-kind and owner-name
// labels name this object. The credential mirror is repaired by
// EnsureMirror on its own read (identity).
//
// It is called only on the unpaused path, once no Job is active: never
// while the Cluster is paused, because clusterctl move rewrites every
// moved object's owner references itself (it creates each object in the
// target with references to the owners' new UIDs, owners first) and must
// not race a second writer, and never once cleanup has started (the
// cleanup path returns before the state is read). A deleting object is
// still repaired until then, so its backups are collected with it. The
// state chunks are skipped while a live Job holds the run lease, since
// the backend's own update of a chunk would then conflict with the patch.
//
// A repair never fails the reconcile: an error (a conflict with a write
// since the read, an API error) is logged and the next reconcile, which
// still sees the unrepaired Secret, tries again. The backups and plan
// key are patched first and the durable inputs and state chunks last, so
// a pass cut short keeps the signal that starts the next attempt.
func (r *reconciler) repairOwners(ctx context.Context, chunks []metav1.ObjectMeta) {
	logger := klog.FromContext(ctx)
	want, err := ownership.SecretOwnerRef(r.obj, r.d.Client.Scheme())
	if err != nil {
		logger.Error(err, "Cannot build the owner reference of this object's Secrets; not repairing them")
		return
	}
	var due []ownedSecret
	if r.durable != nil && ownership.NeedsRepair(&r.durable.Secret, want) {
		due = append(due, ownedSecret{what: ownedInputs, meta: &r.durable.Secret})
	}
	var stateDue []ownedSecret
	for i := range chunks {
		if ownership.NeedsRepair(&chunks[i], want) {
			stateDue = append(stateDue, ownedSecret{what: ownedState, meta: &chunks[i]})
		}
	}
	if len(due) == 0 && len(stateDue) == 0 {
		return
	}
	others, err := r.ownedByName(ctx, want)
	if err != nil {
		logger.Info("Cannot read this object's Secrets to repair their owner references; the next reconcile tries again", "reason", err.Error())
		return
	}
	due = append(others, due...)
	if len(stateDue) > 0 {
		holder, live, err := runLive(ctx, r.d, r.obj.GetNamespace(), r.suffix)
		switch {
		case err != nil:
			logger.Info("Cannot check the run lease; the state Secrets' owner references wait", "reason", err.Error())
		case live:
			logger.V(LogFlow).Info("A live Job holds the run lease; the state Secrets' owner references wait", "holder", holder)
		default:
			due = append(due, stateDue...)
		}
	}
	counts := map[string]int{}
	for _, s := range due {
		ok, err := ownership.RepairSecret(ctx, r.d.Client, s.meta, want)
		if err != nil {
			logger.Info("Repairing an owner reference failed; the next reconcile tries again", "secret", s.meta.Name, "reason", err.Error())
			break
		}
		if ok {
			counts[s.what]++
		}
	}
	r.reportRepaired(logger, counts)
}

// ownedByName returns, read using ctx, the object's state backups and
// plan key that are labeled for it and need want, its owner reference
// (ownership.NeedsRepair): the backups by their per-object selector, as
// metadata only (a backup holds a copy of the state), and the plan key by
// its exact name. It returns any error from those reads but NotFound.
func (r *reconciler) ownedByName(ctx context.Context, want metav1.OwnerReference) ([]ownedSecret, error) {
	ns := r.obj.GetNamespace()
	var backups metav1.PartialObjectMetadataList
	backups.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	if err := r.d.Client.List(ctx, &backups, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: state.BackupSelector(r.suffix)}); err != nil {
		return nil, fmt.Errorf("list state backups: %w", err)
	}
	var out []ownedSecret
	for i := range backups.Items {
		if m := &backups.Items[i].ObjectMeta; ownership.NeedsRepair(m, want) {
			out = append(out, ownedSecret{what: ownedBackup, meta: m})
		}
	}
	key := client.ObjectKey{Namespace: ns, Name: plankey.Name(kindShort(r.k), r.obj.GetName())}
	var pk corev1.Secret
	err := r.d.Client.Get(ctx, key, &pk)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, fmt.Errorf("get plan key %s: %w", key.Name, err)
	case ownership.NeedsRepair(&pk.ObjectMeta, want):
		out = append(out, ownedSecret{what: ownedPlanKey, meta: &pk.ObjectMeta})
	}
	return out, nil
}

// reportRepaired logs to logger, at LogFlow, and emits
// OwnerReferencesRepaired for the repairs in counts (per kind of Secret),
// when there were any.
func (r *reconciler) reportRepaired(logger klog.Logger, counts map[string]int) {
	total := 0
	var parts []string
	for _, what := range []string{ownedState, ownedBackup, ownedInputs, ownedPlanKey} {
		if n := counts[what]; n > 0 {
			total += n
			parts = append(parts, fmt.Sprintf("%s %d", what, n))
		}
	}
	if total == 0 {
		return
	}
	logger.V(LogFlow).Info("Repaired the owner references of this object's Secrets",
		"state", counts[ownedState], "backups", counts[ownedBackup], "inputs", counts[ownedInputs], "planKey", counts[ownedPlanKey])
	r.d.Emit(r.obj, corev1.EventTypeNormal, EventOwnerReferencesRepaired, "Reconcile",
		"Owned %d Secret(s) again whose owner references were missing or named an earlier UID of this object (%s)", total, strings.Join(parts, ", "))
}
