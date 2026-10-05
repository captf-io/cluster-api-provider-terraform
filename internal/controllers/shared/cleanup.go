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
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/plankey"
	"github.com/captf-io/cluster-api-provider-terraform/internal/rbac"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runlease"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// errMirrorConflict marks the error Cleanup returns when the credential
// mirror changed under the owner removal (a Conflict): an expected race
// that the caller retries with a requeue, quietly, not a failure.
var errMirrorConflict = errors.New("credential mirror changed")

// Cleanup runs after a successful destroy, or on deletion with no state,
// using ctx and the shared dependencies d: it deletes the state
// Secrets named by suffix and the Lease, the durable inputs Secret, the
// plan key Secret, the
// object's run and cluster write leases, drops the object from the
// credential mirror named identityName's owners (deleting the mirror with
// its last owner) and removes the finalizer from k's object; the caller
// persists that with its patch. It never runs while a state Secret may
// still describe live resources: only after the destroy Job succeeded. It
// returns any error from those deletes.
func Cleanup(ctx context.Context, d Deps, k Kind, suffix, identityName string) error {
	obj := k.Object()
	logger := klog.FromContext(ctx)
	states, backups, err := stateObjects(ctx, d.Client, obj.GetNamespace(), suffix)
	if err != nil {
		return err
	}
	if len(backups) > 0 && abandonedUID(obj) {
		// The finalizer was released without a destroy: the backups were the
		// only way back to the state of the infrastructure left running.
		logger.Info("Deleting the state backups of an abandoned infrastructure: the recovery path is lost",
			"object", klog.KObj(obj), "annotation", infrav1.AbandonInfrastructureAnnotation, "backups", backups)
	}
	if err := state.Cleanup(ctx, d.Client, obj.GetNamespace(), suffix); err != nil {
		return err
	}
	if err := inputs.Delete(ctx, d.Client, obj); err != nil {
		return err
	}
	// The plan fingerprint key: owner-referenced, but removed here so it
	// does not outlive a finished destroy waiting on garbage collection.
	short, err := state.KindShort(k.Kind())
	if err != nil {
		return err
	}
	if err := plankey.Delete(ctx, d.Client, obj.GetNamespace(), short, obj.GetName()); err != nil {
		return err
	}
	// The run lease, and a TerraformCluster's write lease: the object's
	// last Job has finished.
	if err := runlease.DeleteOwned(ctx, d.Client, d.APIReader, obj.GetNamespace(), k.Kind(), obj.GetName()); err != nil {
		return err
	}
	logger.Info("Deleted the object's state and run data",
		"object", klog.KObj(obj), "stateSecrets", states, "stateBackups", backups,
		"durableInputs", inputs.Name(short, obj.GetName()), "planKey", plankey.Name(short, obj.GetName()),
		"leases", "run and cluster write leases", "abandoned", abandonedUID(obj))
	if identityName != "" {
		mirror := &corev1.Secret{}
		key := client.ObjectKey{Namespace: obj.GetNamespace(), Name: identity.MirrorName(identityName)}
		err := d.Client.Get(ctx, key, mirror)
		switch {
		case err == nil:
			removed, err := identity.RemoveOwner(ctx, d.Client, mirror, obj)
			if apierrors.IsConflict(err) {
				// Another user changed the mirror between the read and the
				// delete: expected, the next pass reads it again.
				logger.V(LogDebug).Info("The credential mirror changed while dropping this owner; trying again",
					"object", klog.KObj(obj), "mirror", klog.KObj(mirror))
				return fmt.Errorf("%w: %w", errMirrorConflict, err)
			}
			if err != nil {
				return err
			}
			logger.Info("Dropped the object from the credential mirror's owners",
				"object", klog.KObj(obj), "mirror", klog.KObj(mirror), "mirrorDeleted", removed)
			if removed {
				d.Emit(obj, corev1.EventTypeNormal, EventMirrorRemoved, "Delete",
					"Removed credential mirror %s: this was its last user in namespace %s", mirror.Name, obj.GetNamespace())
			}
		case client.IgnoreNotFound(err) != nil:
			return fmt.Errorf("get mirror %s: %w", key, err)
		}
	}
	controllerutil.RemoveFinalizer(obj, k.Finalizer())
	return nil
}

// stateObjects lists, through c using ctx, the sorted names of the state
// Secrets and of the state backup sets of suffix in namespace, for
// Cleanup's log. It returns any list error.
func stateObjects(ctx context.Context, c client.Client, namespace, suffix string) (secrets, backups []string, _ error) {
	var list corev1.SecretList
	if err := c.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: state.Selector(suffix)}); err != nil {
		return nil, nil, fmt.Errorf("list state Secrets: %w", err)
	}
	for i := range list.Items {
		secrets = append(secrets, list.Items[i].Name)
	}
	slices.Sort(secrets)
	sets, err := state.ListBackups(ctx, c, namespace, suffix)
	if err != nil {
		return nil, nil, err
	}
	for _, b := range sets {
		backups = append(backups, b.Name)
	}
	slices.Sort(backups)
	return secrets, backups, nil
}

// abandonedUID reports whether obj carries the abandon annotation with its
// own uid, the value that releases a deletion without a destroy.
func abandonedUID(obj Object) bool {
	v, ok := obj.GetAnnotations()[infrav1.AbandonInfrastructureAnnotation]
	return ok && v == string(obj.GetUID())
}

// SweepRBAC removes, using ctx and the shared dependencies d, the runner
// ServiceAccount, RoleBinding and Leases of namespace when no Terraform*
// object is left there. Call it after the finalizer removal is persisted:
// the deleted object itself still counts until it is gone, in which case
// a later sweep removes them. It returns any error from the sweep.
func SweepRBAC(ctx context.Context, d Deps, namespace string) error {
	_, err := rbac.SweepNamespace(ctx, d.APIReader, d.Client, namespace)
	return err
}
