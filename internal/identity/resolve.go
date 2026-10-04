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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// ErrSecretNotFound reports that the identity's credential Secret does not
// exist (IdentityAllowed=False/SecretNotFound).
var ErrSecretNotFound = errors.New("identity: credential secret not found")

// EffectiveName returns the identity a machine or pool uses: its own
// identityRef, else the TerraformCluster's spec.defaults.identityRef, else
// the TerraformCluster's spec.identityRef. A TerraformCluster itself uses
// only its spec.identityRef. cluster may be nil while it is not resolved
// yet. The bool is false when none is set
// (IdentityAllowed=False/IdentityNotFound).
func EffectiveName(own infrav1.IdentityReference, cluster *infrav1.TerraformCluster) (string, bool) {
	if own.Name != "" {
		return own.Name, true
	}
	if name := MachineFallbackName(cluster); name != "" {
		return name, true
	}
	return "", false
}

// MachineFallbackName is the identity machines and pools without their own
// identityRef use: the cluster's spec.defaults.identityRef, else its
// spec.identityRef.
// It returns that name, or "" when cluster is nil or sets neither.
func MachineFallbackName(cluster *infrav1.TerraformCluster) string {
	if cluster == nil {
		return ""
	}
	if d := cluster.Spec.Defaults; d != nil && d.IdentityRef.Name != "" {
		return d.IdentityRef.Name
	}
	return cluster.Spec.IdentityRef.Name
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
