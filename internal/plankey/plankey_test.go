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
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// ns is the namespace these tests create objects in.
const ns = "team-a"

// newClient returns a fake client with the core and infrastructure types
// registered; t fails the test if a registration errors.
func newClient(t *testing.T) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, infrav1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return fake.NewClientBuilder().WithScheme(s).Build()
}

// TestEnsureCreatesOnceAndNeverRotates checks the created Secret's name, key,
// labels and ownerReference, that a second Ensure keeps the key, and that
// Delete removes it and tolerates a missing Secret.
func TestEnsureCreatesOnceAndNeverRotates(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	owner := &infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns, UID: "uid-c"}}

	name, err := Ensure(ctx, c, owner)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if name != "captf-plankey-c-web" {
		t.Errorf("name = %q, want captf-plankey-c-web", name)
	}
	first := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, first); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := len(first.Data[KeyFile]); got != KeySize {
		t.Errorf("key length = %d, want %d", got, KeySize)
	}
	if first.Labels[state.ManagedLabel] != "true" || first.Labels[state.OwnerKindLabel] != "TerraformCluster" || first.Labels[state.OwnerNameLabel] != "web" {
		t.Errorf("labels = %v", first.Labels)
	}
	if refs := first.OwnerReferences; len(refs) != 1 || refs[0].UID != "uid-c" || refs[0].BlockOwnerDeletion != nil {
		t.Errorf("ownerReferences = %+v, want one to uid-c with blockOwnerDeletion unset", refs)
	}

	if _, err := Ensure(ctx, c, owner); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	second := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, second); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(first.Data[KeyFile], second.Data[KeyFile]) {
		t.Error("second Ensure rotated the key")
	}

	if err := Delete(ctx, c, ns, "c", "web"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("get after Delete: %v, want NotFound", err)
	}
	if err := Delete(ctx, c, ns, "c", "web"); err != nil {
		t.Errorf("Delete of a missing Secret: %v", err)
	}
}

// TestEnsureKeysDifferPerObject checks that two objects get different keys.
func TestEnsureKeysDifferPerObject(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	keys := map[string][]byte{}
	for _, n := range []string{"a", "b"} {
		owner := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns, UID: types.UID("uid-" + n)}}
		name, err := Ensure(ctx, c, owner)
		if err != nil {
			t.Fatalf("Ensure %s: %v", n, err)
		}
		s := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, s); err != nil {
			t.Fatalf("get: %v", err)
		}
		keys[n] = s.Data[KeyFile]
	}
	if bytes.Equal(keys["a"], keys["b"]) {
		t.Error("two objects share a key")
	}
}

// TestNameIsValidAndBounded checks that names are DNS-1123 subdomains and
// that truncated names stay unique.
func TestNameIsValidAndBounded(t *testing.T) {
	for _, n := range []string{"web", strings.Repeat("x", 253)} {
		got := Name("mp", n)
		if errs := validation.IsDNS1123Subdomain(got); len(errs) != 0 {
			t.Errorf("Name(%d chars) = %q: %v", len(n), got, errs)
		}
	}
	if Name("m", strings.Repeat("x", 253)) == Name("m", strings.Repeat("x", 252)+"y") {
		t.Error("truncated names collide")
	}
}

// TestLoad checks that Load accepts a full key and rejects a short or
// missing file.
func TestLoad(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(good, bytes.Repeat([]byte{1}, KeySize), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(short, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := Load(good); err != nil || len(k) != KeySize {
		t.Errorf("Load(good) = %d bytes, %v", len(k), err)
	}
	if _, err := Load(short); err == nil {
		t.Error("Load(short) succeeded")
	}
	if _, err := Load(filepath.Join(dir, "missing")); err == nil {
		t.Error("Load(missing) succeeded")
	}
}
