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

package identity

import (
	"context"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// ErrSecretNotFound reports that the identity's credential Secret does not
// exist (IdentityAllowed=False/SecretNotFound).
var ErrSecretNotFound = errors.New("identity: credential secret not found")

// EffectiveRef returns the credentials a machine or pool uses: its own
// identityRef, else the TerraformCluster's spec.defaults.identityRef, else
// the TerraformCluster's spec.identityRef. A TerraformCluster itself uses
// only its spec.identityRef. cluster may be nil while it is not resolved
// yet. The bool is false when none is set
// (IdentityAllowed=False/IdentityNotFound).
func EffectiveRef(own infrav1.IdentityReference, cluster *infrav1.TerraformCluster) (infrav1.IdentityReference, bool) {
	if own.Name != "" {
		return own, true
	}
	if ref := MachineFallbackRef(cluster); ref.Name != "" {
		return ref, true
	}
	return infrav1.IdentityReference{}, false
}

// MachineFallbackRef is the identityRef machines and pools without their own
// use: the cluster's spec.defaults.identityRef, else its spec.identityRef.
// It returns the zero reference when cluster is nil or sets neither.
func MachineFallbackRef(cluster *infrav1.TerraformCluster) infrav1.IdentityReference {
	if cluster == nil {
		return infrav1.IdentityReference{}
	}
	if d := cluster.Spec.Defaults; d != nil && d.IdentityRef.Name != "" {
		return d.IdentityRef
	}
	return cluster.Spec.IdentityRef
}

// EffectiveName returns the name EffectiveRef resolves for own (the object's
// own identityRef) and cluster when it names a TerraformClusterIdentity. A
// Secret reference (kind: Secret) yields "" and false: it is no
// TerraformClusterIdentity, and a machine that sets one never falls through
// to its cluster's. The bool is false when no TerraformClusterIdentity is in
// use.
func EffectiveName(own infrav1.IdentityReference, cluster *infrav1.TerraformCluster) (string, bool) {
	ref, ok := EffectiveRef(own, cluster)
	if !ok || ref.IsSecret() {
		return "", false
	}
	return ref.Name, true
}

// MachineFallbackName returns MachineFallbackRef's name when it names a
// TerraformClusterIdentity, and "" when cluster is nil, sets neither or
// names a Secret.
func MachineFallbackName(cluster *infrav1.TerraformCluster) string {
	return MachineFallbackRef(cluster).ClusterIdentityName()
}

// CredentialsSecretName returns the name of the Secret a Job mounts for ref:
// the Secret itself for kind Secret, else the identity's mirror (MirrorName).
func CredentialsSecretName(ref infrav1.IdentityReference) string {
	if ref.IsSecret() {
		return ref.Name
	}
	return MirrorName(ref.Name)
}

// LocalSecret reads the Secret called name, which a kind: Secret reference
// names, in namespace, bounded by ctx, through reader. It returns the
// Secret, or ErrSecretNotFound when it does not exist.
func LocalSecret(ctx context.Context, reader client.Reader, namespace, name string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s/%s", ErrSecretNotFound, namespace, name)
		}
		return nil, fmt.Errorf("identity: get secret %s/%s: %w", namespace, name, err)
	}
	return s, nil
}

// MissingKeys returns the keys of id.Spec.RequiredKeys that src's data does
// not hold, sorted; nil when none is missing or none is required. Only key
// names are compared, never values.
func MissingKeys(id *infrav1.TerraformClusterIdentity, src *corev1.Secret) []string {
	var missing []string
	for _, k := range id.Spec.RequiredKeys {
		if _, ok := src.Data[k]; !ok {
			missing = append(missing, k)
		}
	}
	slices.Sort(missing)
	return slices.Compact(missing)
}

// Get reads the identity called name, bounded by ctx, through reader. It
// returns the identity, or an error for which apierrors.IsNotFound is true
// when name does not exist.
func Get(ctx context.Context, reader client.Reader, name string) (*infrav1.TerraformClusterIdentity, error) {
	id := &infrav1.TerraformClusterIdentity{}
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, id); err != nil {
		return nil, fmt.Errorf("identity: get %s: %w", name, err)
	}
	return id, nil
}

// SourceSecret reads id's credential Secret, bounded by ctx, through reader,
// which must be uncached: the Secret usually lives in a namespace the
// manager does not cache. It returns the Secret, or ErrSecretNotFound when
// it does not exist.
func SourceSecret(ctx context.Context, reader client.Reader, id *infrav1.TerraformClusterIdentity) (*corev1.Secret, error) {
	ref := id.Spec.SecretRef
	s := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s/%s", ErrSecretNotFound, ref.Namespace, ref.Name)
		}
		return nil, fmt.Errorf("identity: get secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	return s, nil
}
