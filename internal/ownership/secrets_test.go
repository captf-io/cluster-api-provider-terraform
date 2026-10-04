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

package ownership

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// wantRef is the owner reference the tests ensure: TerraformMachine tm1
// with its current UID "new".
var wantRef = metav1.OwnerReference{APIVersion: infrav1.GroupVersion.String(), Kind: state.KindTerraformMachine, Name: "tm1", UID: "new"}

// ref returns an owner reference to the object of apiVersion, kind and name
// with uid.
func ref(apiVersion, kind, name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid}
}

// TestEnsureRef proves EnsureRef adds a missing reference, replaces a
// stale-UID one in place, drops duplicates, keeps a current one untouched
// (with whatever flags it carries), and never touches other owners, even
// ones with the same name in another group or kind.
func TestEnsureRef(t *testing.T) {
	t.Parallel()
	gv := infrav1.GroupVersion.String()
	otherVersion := infrav1.GroupVersion.Group + "/v1alpha9"
	other := ref(gv, state.KindTerraformMachine, "tm2", "u2")
	otherGroup := ref("example.io/v1", state.KindTerraformMachine, "tm1", "x")
	otherKind := ref(gv, state.KindTerraformCluster, "tm1", "y")
	current := ref(otherVersion, state.KindTerraformMachine, "tm1", "new")
	current.Controller = new(true)
	stale := ref(gv, state.KindTerraformMachine, "tm1", "old")
	tests := []struct {
		name   string
		refs   []metav1.OwnerReference
		want   []metav1.OwnerReference
		change RefChange
	}{
		{"none", nil, []metav1.OwnerReference{wantRef}, RefAdded},
		{"only other owners", []metav1.OwnerReference{other, otherGroup, otherKind}, []metav1.OwnerReference{other, otherGroup, otherKind, wantRef}, RefAdded},
		{"current kept as is", []metav1.OwnerReference{other, current}, []metav1.OwnerReference{other, current}, RefUnchanged},
		{"stale replaced in place", []metav1.OwnerReference{stale, other}, []metav1.OwnerReference{wantRef, other}, RefReplaced},
		{"stale and current", []metav1.OwnerReference{stale, other, current}, []metav1.OwnerReference{other, current}, RefReplaced},
		{"duplicates of current", []metav1.OwnerReference{current, current}, []metav1.OwnerReference{current}, RefReplaced},
		{"two stale", []metav1.OwnerReference{other, stale, stale}, []metav1.OwnerReference{other, wantRef}, RefReplaced},
		{"malformed apiVersion is another owner", []metav1.OwnerReference{ref("a/b/c", state.KindTerraformMachine, "tm1", "z")}, []metav1.OwnerReference{ref("a/b/c", state.KindTerraformMachine, "tm1", "z"), wantRef}, RefAdded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			before := append([]metav1.OwnerReference(nil), tt.refs...)
			got, change := EnsureRef(tt.refs, wantRef)
			if change != tt.change || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("EnsureRef = %+v, %v; want %+v, %v", got, change, tt.want, tt.change)
			}
			if !reflect.DeepEqual(tt.refs, before) {
				t.Errorf("EnsureRef modified its input: %+v", tt.refs)
			}
		})
	}
	if got, change := EnsureRef(nil, metav1.OwnerReference{APIVersion: "a/b/c"}); got != nil || change != RefUnchanged {
		t.Errorf("malformed want: %+v, %v", got, change)
	}
}

// TestLabeledForAndNeedsRepair proves a Secret is claimed only through
// owner-kind and owner-name labels naming the object (a long name through
// its hashed label value), and needs a repair only when also misowned.
func TestLabeledForAndNeedsRepair(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("n", 80)
	if !LabeledFor(map[string]string{state.OwnerKindLabel: state.KindTerraformMachine, state.OwnerNameLabel: state.LabelValue(long)}, state.KindTerraformMachine, long) {
		t.Error("hashed owner-name label not matched")
	}
	labels := map[string]string{state.OwnerKindLabel: state.KindTerraformMachine, state.OwnerNameLabel: "tm1"}
	tests := []struct {
		name   string
		labels map[string]string
		refs   []metav1.OwnerReference
		want   bool
	}{
		{"labeled, no refs", labels, nil, true},
		{"labeled, stale", labels, []metav1.OwnerReference{ref(wantRef.APIVersion, wantRef.Kind, "tm1", "old")}, true},
		{"labeled, correct", labels, []metav1.OwnerReference{wantRef}, false},
		{"unlabeled", nil, nil, false},
		{"labeled for another name", map[string]string{state.OwnerKindLabel: state.KindTerraformMachine, state.OwnerNameLabel: "tm2"}, nil, false},
		{"labeled for another kind", map[string]string{state.OwnerKindLabel: state.KindTerraformCluster, state.OwnerNameLabel: "tm1"}, nil, false},
	}
	for _, tt := range tests {
		meta := &metav1.ObjectMeta{Labels: tt.labels, OwnerReferences: tt.refs}
		if got := NeedsRepair(meta, wantRef); got != tt.want {
			t.Errorf("%s: NeedsRepair = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestSecretOwnerRef proves SecretOwnerRef resolves a typed object's kind
// through the scheme, sets neither controller nor blockOwnerDeletion, and
// fails for a type the scheme does not know.
func TestSecretOwnerRef(t *testing.T) {
	t.Parallel()
	s := runtime.NewScheme()
	if err := infrav1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	tm := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Name: "tm1", UID: "new"}}
	got, err := SecretOwnerRef(tm, s)
	if err != nil || !reflect.DeepEqual(got, wantRef) {
		t.Errorf("SecretOwnerRef = %+v, %v; want %+v", got, err, wantRef)
	}
	if _, err := SecretOwnerRef(&corev1.Secret{}, s); err == nil {
		t.Error("unknown type accepted")
	}
}

// secret returns Secret name in namespace "ns", labeled for tm1 when
// labeled, with refs.
func secret(name string, labeled bool, refs ...metav1.OwnerReference) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, OwnerReferences: refs}, Data: map[string][]byte{"k": []byte("v")}}
	if labeled {
		s.Labels = map[string]string{state.OwnerKindLabel: state.KindTerraformMachine, state.OwnerNameLabel: "tm1"}
	}
	return s
}

// patchCounter returns interceptor funcs counting Patch calls in n.
func patchCounter(n *int) interceptor.Funcs {
	return interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		*n++
		return c.Patch(ctx, obj, p, opts...)
	}}
}

// TestRepairSecret proves RepairSecret patches only a labeled, misowned
// Secret, leaves its data and other owners alone, issues no call for a
// correct or foreign one, and reports a vanished Secret as no repair and
// a stale resourceVersion as a conflict.
func TestRepairSecret(t *testing.T) {
	t.Parallel()
	other := ref("cluster.x-k8s.io/v1beta2", "Machine", "m", "mu")
	stale := ref(wantRef.APIVersion, wantRef.Kind, "tm1", "old")
	read := func(t *testing.T, c client.Client, name string) *corev1.Secret {
		t.Helper()
		s := &corev1.Secret{}
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: "ns", Name: name}, s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	var patches int
	c := fake.NewClientBuilder().WithInterceptorFuncs(patchCounter(&patches)).WithObjects(
		secret("stale", true, other, stale), secret("correct", true, wantRef), secret("foreign", false, stale),
	).Build()

	s := read(t, c, "stale")
	if ok, err := RepairSecret(t.Context(), c, &s.ObjectMeta, wantRef); !ok || err != nil {
		t.Fatalf("stale: %v, %v", ok, err)
	}
	if got := read(t, c, "stale"); !reflect.DeepEqual(got.OwnerReferences, []metav1.OwnerReference{other, wantRef}) || string(got.Data["k"]) != "v" {
		t.Errorf("repaired Secret = %+v, data %q", got.OwnerReferences, got.Data)
	}
	for _, name := range []string{"correct", "foreign"} {
		s := read(t, c, name)
		if ok, err := RepairSecret(t.Context(), c, &s.ObjectMeta, wantRef); ok || err != nil {
			t.Errorf("%s: %v, %v", name, ok, err)
		}
	}
	if patches != 1 {
		t.Errorf("patches = %d, want 1", patches)
	}

	// The read is stale: the Secret changed since.
	conflict := read(t, c, "stale")
	conflict.OwnerReferences = nil
	conflict.ResourceVersion = "1"
	if _, err := RepairSecret(t.Context(), c, &conflict.ObjectMeta, wantRef); !apierrors.IsConflict(err) {
		t.Errorf("stale resourceVersion: %v, want a conflict", err)
	}
	gone := secret("gone", true)
	gone.ResourceVersion = "1"
	if ok, err := RepairSecret(t.Context(), c, &gone.ObjectMeta, wantRef); ok || err != nil {
		t.Errorf("gone: %v, %v", ok, err)
	}
}

// TestRepairSecretPatchError proves a patch error other than NotFound is
// returned, wrapped.
func TestRepairSecretPatchError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return boom
		},
	}).Build()
	s := secret("s", true)
	if _, err := RepairSecret(t.Context(), c, &s.ObjectMeta, wantRef); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
}
