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
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// EnsureSourceOwnerRef makes sure CAPTF does not own the user's credential
// Secret. The name is historical: it no longer adds an ownerRef, it removes
// the one earlier versions added (migration), with which deleting the
// identity garbage-collected the user's Secret.
//
// It deliberately does not add clusterctl's move label either: clusterctl
// deletes a label-moved Secret from the source after the move (mover.go:
// only global hierarchies and ClusterClasses are kept), so a
// namespace-scoped move would delete credentials that other namespaces on
// the source cluster still use. The Secret is the operator's: they copy or
// recreate it on the target after a clusterctl move.
//
// The Secret is written only when something changed.
// The identity argument is unused; it stays for the callers' signature.
// ctx bounds the call and c is the client used to update source. It returns
// nil on success or when nothing changed, or an error from a failed update.
func EnsureSourceOwnerRef(ctx context.Context, c client.Client, _ *infrav1.TerraformClusterIdentity, source *corev1.Secret) error {
	changed := false
	// Every ref to an identity goes, not only id's: one left by a deleted and
	// re-created identity would still let the garbage collector take the
	// Secret.
	isIdentity := func(r metav1.OwnerReference) bool {
		gv, err := schema.ParseGroupVersion(r.APIVersion)
		return err == nil && gv.Group == infrav1.GroupVersion.Group && r.Kind == identityKind
	}
	if slices.ContainsFunc(source.OwnerReferences, isIdentity) {
		source.OwnerReferences = slices.DeleteFunc(source.OwnerReferences, isIdentity)
		changed = true
	}
	if !changed {
		return nil
	}
	if err := c.Update(ctx, source); err != nil {
		return fmt.Errorf("identity: update secret %s: %w", client.ObjectKeyFromObject(source), err)
	}
	klog.FromContext(ctx).Info("Removed the ownerRef to a TerraformClusterIdentity from the credentials Secret",
		"secret", client.ObjectKeyFromObject(source))
	return nil
}

// identityKind is the kind of the ownerRefs earlier versions put on source
// Secrets.
const identityKind = "TerraformClusterIdentity"
