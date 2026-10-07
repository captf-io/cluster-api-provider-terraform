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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/ownership"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Retained counts what Retain kept, by kind of Secret.
type Retained struct {
	// State is the number of state Secrets (chunks).
	State int
	// Backups is the number of state backup Secrets (chunks of every set).
	Backups int
	// Inputs is the number of inputs Secrets kept: the durable and the
	// applied one, each when it exists.
	Inputs int
}

// Retain removes the finalizer of k's object without a destroy, for
// deletionPolicy Retain, using ctx and the shared dependencies d: the
// infrastructure keeps running. It keeps the object's state Secrets
// (suffix), its state backups and its inputs records (the durable and
// the applied inputs Secrets), taking their owner
// references to the object away (so they are not garbage-collected with
// it) and labeling them state.RetainedFromUIDLabel with its uid
// (ownership.Retain), for a later object of the same kind, namespace and
// name to adopt (spec.adoptRetainedState). It deletes the state lock
// Lease, then releases the object as every finalizer removal does
// (release: plan key, run and write leases, the mirror named
// identityName, the finalizer). Every step is idempotent, so a pass cut
// short is finished by the next. Call it only when no Job of the object
// is active. It returns what it kept, and any error from those steps.
func Retain(ctx context.Context, d Deps, k Kind, suffix, identityName string) (Retained, error) {
	obj := k.Object()
	want, err := ownership.SecretOwnerRef(obj, d.Client.Scheme())
	if err != nil {
		return Retained{}, err
	}
	secrets, err := objectSecrets(ctx, d.Client, k, suffix)
	if err != nil {
		return Retained{}, err
	}
	var kept Retained
	for _, s := range secrets {
		if from := state.RetainedFrom(s.meta.Labels); from != "" && from != string(obj.GetUID()) {
			// Another object's retained Secret: never ours to keep or label.
			continue
		}
		if _, err := ownership.Retain(ctx, d.Client, s.meta, want); err != nil {
			return Retained{}, err
		}
		switch s.what {
		case ownedState:
			kept.State++
		case ownedBackup:
			kept.Backups++
		case ownedInputs:
			kept.Inputs++
		}
	}
	if err := state.DeleteLock(ctx, d.Client, obj.GetNamespace(), suffix); err != nil {
		return Retained{}, err
	}
	klog.FromContext(ctx).Info("Retained the object's state for adoption",
		"object", klog.KObj(obj), "label", state.RetainedFromUIDLabel+"="+string(obj.GetUID()),
		"stateSecrets", kept.State, "stateBackups", kept.Backups, "inputsSecrets", kept.Inputs)
	return kept, release(ctx, d, k, identityName)
}

// objectSecrets lists, through c using ctx, the metadata of the Secrets
// deletionPolicy Retain keeps for k's object, found by its deterministic
// names and selectors: its state chunks (suffix), its state backups and
// its durable and applied inputs Secrets. Metadata only: they hold the
// state and the inputs. It returns them, or any error from those reads
// but an inputs Secret being gone.
func objectSecrets(ctx context.Context, c client.Client, k Kind, suffix string) ([]ownedSecret, error) {
	obj := k.Object()
	ns := obj.GetNamespace()
	var out []ownedSecret
	for _, sel := range []struct {
		what string
		list client.MatchingLabelsSelector
	}{
		{ownedState, client.MatchingLabelsSelector{Selector: state.Selector(suffix)}},
		{ownedBackup, client.MatchingLabelsSelector{Selector: state.BackupSelector(suffix)}},
	} {
		var list metav1.PartialObjectMetadataList
		list.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
		if err := c.List(ctx, &list, client.InNamespace(ns), sel.list); err != nil {
			return nil, fmt.Errorf("list %s Secrets: %w", sel.what, err)
		}
		for i := range list.Items {
			out = append(out, ownedSecret{what: sel.what, meta: &list.Items[i].ObjectMeta})
		}
	}
	for _, name := range []string{inputs.Name(kindShort(k), obj.GetName()), inputs.AppliedName(kindShort(k), obj.GetName())} {
		var meta metav1.PartialObjectMetadata
		meta.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
		key := client.ObjectKey{Namespace: ns, Name: name}
		switch err := c.Get(ctx, key, &meta); {
		case apierrors.IsNotFound(err):
		case err != nil:
			return nil, fmt.Errorf("get inputs %s: %w", key.Name, err)
		default:
			out = append(out, ownedSecret{what: ownedInputs, meta: &meta.ObjectMeta})
		}
	}
	return out, nil
}

// errRetainedState marks a pass that found the object's state retained by
// another object (heldOnRetained): it is held, or was just adopted.
var errRetainedState = errors.New("retained state")

// checkRetained looks for Secrets of the object that deletionPolicy Retain
// kept (state.RetainedFromUIDLabel), using ctx: cheaply on the inputs
// Secrets' metadata setup read and chunks, the state chunks' metadata as
// read this pass, and, when listed is false (the state was missing or
// could not be read, so chunks is empty), by listing the object's
// Secrets. Another object's uid on them sets r.retainedFrom: with
// spec.adoptRetainedState the label is removed from every one of them
// (adoptRetained), otherwise StateReadable becomes False with reason
// RetainedStateFound. Either way it returns errRetainedState. The
// object's own uid on them (a Retain cut short, then the policy changed
// back) is removed again unless the object is being deleted with
// deletionPolicy Retain. It returns nil when no Secret is retained by
// another object, or any error from the reads and patches.
func (r *reconciler) checkRetained(ctx context.Context, chunks []metav1.ObjectMeta, listed bool) error {
	uid := string(r.obj.GetUID())
	var metas []*metav1.ObjectMeta
	if r.durable != nil {
		metas = append(metas, &r.durable.Secret, &r.durable.AppliedSecret)
	}
	for i := range chunks {
		metas = append(metas, &chunks[i])
	}
	if !listed {
		secrets, err := objectSecrets(ctx, r.d.Client, r.k, r.suffix)
		if err != nil {
			return err
		}
		for _, s := range secrets {
			metas = append(metas, s.meta)
		}
	}
	retaining := r.deleting && r.eff.DeletionPolicy == infrav1.DeletionPolicyRetain
	var foreign string
	own := false
	for _, m := range metas {
		switch from := state.RetainedFrom(m.Labels); {
		case from == "":
		case from == uid:
			own = true
		case foreign == "":
			foreign = from
		}
	}
	switch {
	case foreign != "":
		r.retainedFrom = foreign
		if adopt := r.k.Spec().AdoptRetainedState; adopt != nil && *adopt {
			return r.adoptRetained(ctx)
		}
		conditions.Set(r.obj, metav1.Condition{
			Type: infrav1.StateReadableCondition, Status: metav1.ConditionFalse, Reason: infrav1.RetainedStateFoundReason,
			Message: fmt.Sprintf("This object's state was kept by an earlier %s of the same name, deleted with deletionPolicy Retain: "+
				"its state, state backups and durable inputs carry %s=%s, and the infrastructure they describe may still be running. "+
				"Nothing adopts it silently, so no Job runs. Set spec.adoptRetainedState: true to manage that infrastructure as this object's, "+
				"or delete those Secrets to start afresh, leaving it unmanaged; "+
				"see https://captf.io/docs/concepts/deletion/retain.html",
				r.k.Kind(), state.RetainedFromUIDLabel, foreign),
		})
		return errRetainedState
	case own && !retaining:
		_, err := r.unretain(ctx)
		return err
	}
	return nil
}

// adoptRetained removes state.RetainedFromUIDLabel from every Secret of
// the object that carries it, using ctx, so repairOwners owns them again
// on the next pass and the object manages the infrastructure they
// describe; r.retainedFrom is the uid of the object that retained them.
// It emits RetainedStateAdopted and returns errRetainedState, so the pass
// ends and the next one reads the Secrets as they are now, or any error
// from listing or patching them.
func (r *reconciler) adoptRetained(ctx context.Context) error {
	n, err := r.unretain(ctx)
	if err != nil {
		return err
	}
	r.adopted = true
	klog.FromContext(ctx).Info("Adopted retained state", "retainedFromUID", r.retainedFrom, "secrets", n)
	r.d.Emit(r.obj, corev1.EventTypeNormal, EventRetainedStateAdopted, "Reconcile",
		"Adopted the state an earlier %s of this name retained (%s=%s): removed the label from %d Secret(s) (state, state backups, inputs); "+
			"this object now manages that infrastructure (spec.adoptRetainedState)",
		r.k.Kind(), state.RetainedFromUIDLabel, r.retainedFrom, n)
	return errRetainedState
}

// unretain removes state.RetainedFromUIDLabel from every Secret of the
// object that carries it (objectSecrets, ownership.Unretain), using ctx.
// It returns how many it patched, or any error from listing or patching
// them.
func (r *reconciler) unretain(ctx context.Context) (int, error) {
	secrets, err := objectSecrets(ctx, r.d.Client, r.k, r.suffix)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range secrets {
		ok, err := ownership.Unretain(ctx, r.d.Client, s.meta)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// heldOnRetained ends a pass that found the object's state retained by
// another object (checkRetained), using ctx and the pass's bookkeeping bk.
// Just adopted, it requeues at LagRequeue so the next pass reads the
// unlabeled Secrets. Otherwise no Job runs: a live object requeues at
// StateRequeue, and a deleting one removes its finalizer at once without
// touching those Secrets (cleanupReleased), since nothing of this object's
// own was ever created. It returns the result and error from finish or
// cleanup.
func (r *reconciler) heldOnRetained(ctx context.Context, bk *Bookkeeping) (ctrl.Result, error) {
	if r.adopted {
		return r.finish(bk, nil, ctrl.Result{RequeueAfter: LagRequeue})
	}
	if r.deleting {
		return r.cleanup(ctx, bk, cleanupReleased)
	}
	klog.FromContext(ctx).V(LogFlow).Info("Held on retained state", "retainedFromUID", r.retainedFrom)
	return r.finish(bk, nil, ctrl.Result{RequeueAfter: StateRequeue})
}
