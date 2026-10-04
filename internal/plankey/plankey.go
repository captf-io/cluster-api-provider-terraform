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

package plankey

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
	"github.com/captf-io/cluster-api-provider-terraform/internal/strutil"
)

const (
	// MountPath is the directory plan and apply Jobs mount the key Secret at.
	MountPath = "/captf/plan-key"
	// KeyFile is the key's data key in the Secret and its file name under
	// MountPath.
	KeyFile = "key"
	// KeySize is the length of a key in bytes.
	KeySize = 32

	// namePrefix starts every key Secret name.
	namePrefix = "captf-plankey-"
)

// Name returns the key Secret name, captf-plankey-<kindshort>-<name>. A name
// that would exceed 253 characters keeps its start and gets a hash of the
// full name appended, so it stays deterministic and unique.
func Name(kindshort, name string) string {
	return strutil.BoundedName(namePrefix, kindshort, name)
}

// Ensure makes sure owner's key Secret exists and returns its name. A new
// Secret gets KeySize random bytes under KeyFile, the managed and owner
// labels, and exactly one ownerReference to owner with blockOwnerDeletion
// unset, so it is garbage-collected and moved with owner. An existing
// Secret is never rotated: a new key would invalidate the fingerprint of
// every plan waiting for approval, and Ensure leaves its reference alone;
// the reconciler re-owns a key Secret that lost it or names owner's
// earlier UID (after a management-cluster restore) when it finds the
// object's other Secrets misowned.
//
// It uses ctx for the get and create calls through c, and returns any error
// from them, from resolving owner's kind or owner reference, or from
// reading random bytes. A concurrent create (AlreadyExists) is not an
// error.
func Ensure(ctx context.Context, c client.Client, owner client.Object) (string, error) {
	gvk, err := apiutil.GVKForObject(owner, c.Scheme())
	if err != nil {
		return "", fmt.Errorf("plankey: kind of %T: %w", owner, err)
	}
	short, err := state.KindShort(gvk.Kind)
	if err != nil {
		return "", fmt.Errorf("plankey: %w", err)
	}
	name := Name(short, owner.GetName())
	key := client.ObjectKey{Namespace: owner.GetNamespace(), Name: name}
	err = c.Get(ctx, key, &corev1.Secret{})
	switch {
	case err == nil:
		return name, nil
	case !apierrors.IsNotFound(err):
		return "", fmt.Errorf("plankey: get %s: %w", name, err)
	}
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return "", fmt.Errorf("plankey: random key: %w", err)
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: owner.GetNamespace(),
			Name:      name,
			Labels: map[string]string{
				state.ManagedLabel:   "true",
				state.OwnerKindLabel: gvk.Kind,
				state.OwnerNameLabel: state.LabelValue(owner.GetName()),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{KeyFile: k},
	}
	if err := controllerutil.SetOwnerReference(owner, s, c.Scheme(), func(r *metav1.OwnerReference) { r.BlockOwnerDeletion = nil }); err != nil {
		return "", fmt.Errorf("plankey: owner reference: %w", err)
	}
	if err := c.Create(ctx, s); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("plankey: create %s: %w", name, err)
	}
	return name, nil
}

// Delete removes the key Secret of the object named name, of short kind
// kindshort, in namespace, through c using ctx. A missing Secret is not an
// error; any other error from the delete call is returned.
func Delete(ctx context.Context, c client.Client, namespace, kindshort, name string) error {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: Name(kindshort, name)}}
	if err := c.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("plankey: delete %s: %w", s.Name, err)
	}
	return nil
}

// Load reads the key from the file at path, normally
// filepath.Join(MountPath, KeyFile). It returns an error when the file
// cannot be read or holds fewer than KeySize bytes.
func Load(path string) ([]byte, error) {
	k, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("plankey: read key: %w", err)
	}
	if len(k) < KeySize {
		return nil, fmt.Errorf("plankey: key %s has %d bytes, want at least %d", path, len(k), KeySize)
	}
	return k, nil
}
