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
	"bytes"
	"context"
	"errors"
	"maps"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Fixture names shared by every test in this package: tenant and system are
// namespaces, idName is the identity's name and srcSecret is its credential
// Secret's name.
const (
	tenant    = "team-a"
	system    = "captf-system"
	idName    = "aws-prod"
	srcSecret = "aws-prod-creds"
)

// newScheme builds the runtime.Scheme every test in this package uses,
// failing test t if a scheme registration fails. It returns the built
// scheme.
func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := infrav1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// testIdentity returns a TerraformClusterIdentity named idName, pointing at
// the srcSecret credential Secret in namespace system, with an
// spec.allowedNamespaces of an.
func testIdentity(an *infrav1.AllowedNamespaces) *infrav1.TerraformClusterIdentity {
	return &infrav1.TerraformClusterIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: idName, UID: "id-uid"},
		Spec: infrav1.TerraformClusterIdentitySpec{
			SecretRef:         infrav1.SecretReference{Name: srcSecret, Namespace: system},
			AllowedNamespaces: an,
		},
	}
}

// source returns the identity's credential Secret in namespace system, named
// srcSecret, holding data.
func source(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: system, Name: srcSecret},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

// machine returns a TerraformMachine in namespace tenant, named name, with
// UID uid.
func machine(name, uid string) *infrav1.TerraformMachine {
	return &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: tenant, Name: name, UID: types.UID(uid)}}
}

// TestEffectiveName proves EffectiveName prefers an object's own identityRef,
// then the cluster's spec.defaults.identityRef, then its spec.identityRef,
// and reports false when the cluster is unresolved or sets no identity.
func TestEffectiveName(t *testing.T) {
	t.Parallel()
	withDefaults := &infrav1.TerraformCluster{Spec: infrav1.TerraformClusterSpec{
		WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: "cluster-own"}},
		Defaults:      &infrav1.TerraformClusterDefaults{IdentityRef: infrav1.IdentityReference{Name: "from-cluster"}},
	}}
	onlyOwn := &infrav1.TerraformCluster{Spec: infrav1.TerraformClusterSpec{WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: "cluster-own"}}}}
	emptyDefaults := onlyOwn.DeepCopy()
	emptyDefaults.Spec.Defaults = &infrav1.TerraformClusterDefaults{}
	tests := []struct {
		name    string
		own     infrav1.IdentityReference
		cluster *infrav1.TerraformCluster
		want    string
		ok      bool
	}{
		{"own wins", infrav1.IdentityReference{Name: "own"}, withDefaults, "own", true},
		{"cluster default before cluster identity", infrav1.IdentityReference{}, withDefaults, "from-cluster", true},
		{"falls back to the cluster's identityRef", infrav1.IdentityReference{}, onlyOwn, "cluster-own", true},
		{"empty defaults fall back too", infrav1.IdentityReference{}, emptyDefaults, "cluster-own", true},
		{"cluster not resolved", infrav1.IdentityReference{}, nil, "", false},
		{"cluster without any identity", infrav1.IdentityReference{}, &infrav1.TerraformCluster{}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := EffectiveName(tt.own, tt.cluster)
			if got != tt.want || ok != tt.ok {
				t.Errorf("EffectiveName = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestGet proves Get returns an existing identity by name and a NotFound
// error for one that does not exist.
func TestGet(t *testing.T) {
	t.Parallel()
	r := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(testIdentity(nil)).Build()
	id, err := Get(t.Context(), r, idName)
	if err != nil || id.Name != idName {
		t.Fatalf("Get = %v, %v", id, err)
	}
	if _, err := Get(t.Context(), r, "missing"); !apierrors.IsNotFound(err) {
		t.Errorf("Get missing = %v, want NotFound", err)
	}
}

// TestSourceSecret proves SourceSecret returns the identity's credential
// Secret when it exists, ErrSecretNotFound when it is missing, and the
// underlying read error, not wrapped as ErrSecretNotFound, on any other
// read failure.
func TestSourceSecret(t *testing.T) {
	t.Parallel()
	s := newScheme(t)
	r := fake.NewClientBuilder().WithScheme(s).WithObjects(source(nil)).Build()
	if _, err := SourceSecret(t.Context(), r, testIdentity(nil)); err != nil {
		t.Fatalf("SourceSecret: %v", err)
	}
	empty := fake.NewClientBuilder().WithScheme(s).Build()
	if _, err := SourceSecret(t.Context(), empty, testIdentity(nil)); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("missing source = %v, want ErrSecretNotFound", err)
	}
	boom := errors.New("boom")
	failing := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		},
	}).Build()
	if _, err := SourceSecret(t.Context(), failing, testIdentity(nil)); !errors.Is(err, boom) || errors.Is(err, ErrSecretNotFound) {
		t.Errorf("read error = %v, want boom", err)
	}
}

// namespace returns a Namespace named name, carrying labels.
func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// TestAllowed covers every combination of list, selector and namespace
// existence Allowed's doc comment describes.
func TestAllowed(t *testing.T) {
	t.Parallel()
	gold := &metav1.LabelSelector{MatchLabels: map[string]string{"captf.io/tenant": "gold"}}
	tests := []struct {
		name   string
		an     *infrav1.AllowedNamespaces
		labels map[string]string
		noNS   bool
		want   bool
		reason string
	}{
		{name: "nil allows none", an: nil, want: false, reason: infrav1.NamespaceNotAllowedReason},
		{name: "empty object (rejected by admission) allows none", an: &infrav1.AllowedNamespaces{}, want: false, reason: infrav1.NamespaceNotAllowedReason},
		{name: "in list", an: &infrav1.AllowedNamespaces{List: []string{"x", tenant}}, want: true, reason: infrav1.IdentityAllowedReason},
		{name: "not in list", an: &infrav1.AllowedNamespaces{List: []string{"x"}}, want: false, reason: infrav1.NamespaceNotAllowedReason},
		{name: "explicit empty list matches nothing", an: &infrav1.AllowedNamespaces{List: []string{}}, want: false, reason: infrav1.NamespaceNotAllowedReason},
		{name: "empty selector allows all", an: &infrav1.AllowedNamespaces{Selector: &metav1.LabelSelector{}}, labels: map[string]string{"a": "b"}, want: true, reason: infrav1.IdentityAllowedReason},
		{name: "empty selector allows all without a namespace lookup", an: &infrav1.AllowedNamespaces{Selector: &metav1.LabelSelector{}}, noNS: true, want: true, reason: infrav1.IdentityAllowedReason},
		{name: "empty selector ORed with a list", an: &infrav1.AllowedNamespaces{List: []string{"x"}, Selector: &metav1.LabelSelector{}}, want: true, reason: infrav1.IdentityAllowedReason},
		{name: "selector matches", an: &infrav1.AllowedNamespaces{Selector: gold}, labels: map[string]string{"captf.io/tenant": "gold"}, want: true, reason: infrav1.IdentityAllowedReason},
		{name: "selector misses", an: &infrav1.AllowedNamespaces{Selector: gold}, labels: map[string]string{"captf.io/tenant": "silver"}, want: false, reason: infrav1.NamespaceNotAllowedReason},
		{name: "union: list misses, selector hits", an: &infrav1.AllowedNamespaces{List: []string{"x"}, Selector: gold}, labels: map[string]string{"captf.io/tenant": "gold"}, want: true, reason: infrav1.IdentityAllowedReason},
		{name: "union: list hits, selector misses", an: &infrav1.AllowedNamespaces{List: []string{tenant}, Selector: gold}, want: true, reason: infrav1.IdentityAllowedReason},
		{name: "invalid selector matches nothing", an: &infrav1.AllowedNamespaces{Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "k", Operator: "Bogus"}}}}, want: false, reason: infrav1.NamespaceNotAllowedReason},
		{name: "namespace gone", an: &infrav1.AllowedNamespaces{Selector: gold}, noNS: true, want: false, reason: infrav1.NamespaceNotAllowedReason},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := fake.NewClientBuilder().WithScheme(newScheme(t))
			if !tt.noNS {
				b = b.WithObjects(namespace(tenant, tt.labels))
			}
			got, reason, err := Allowed(t.Context(), b.Build(), testIdentity(tt.an), tenant)
			if err != nil || got != tt.want || reason != tt.reason {
				t.Errorf("Allowed = %v, %q, %v; want %v, %q", got, reason, err, tt.want, tt.reason)
			}
		})
	}
}

// TestAllowedReadsGivenReader shows the Namespace comes from the reader the
// caller passes (the uncached API reader in the manager): one read per call
// and no other client, so a label change is seen on the next call.
func TestAllowedReadsGivenReader(t *testing.T) {
	t.Parallel()
	s := newScheme(t)
	var gets atomic.Int32
	apiReader := fake.NewClientBuilder().WithScheme(s).
		WithObjects(namespace(tenant, map[string]string{"captf.io/tenant": "gold"})).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				gets.Add(1)
				return c.Get(ctx, k, o, opts...)
			},
		}).Build()
	id := testIdentity(&infrav1.AllowedNamespaces{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"captf.io/tenant": "gold"}}})
	ok, _, err := Allowed(t.Context(), apiReader, id, tenant)
	if err != nil || !ok || gets.Load() != 1 {
		t.Fatalf("Allowed = %v, %v with %d reads; want true with 1", ok, err, gets.Load())
	}
	ns := &corev1.Namespace{}
	if err := apiReader.Get(t.Context(), client.ObjectKey{Name: tenant}, ns); err != nil {
		t.Fatal(err)
	}
	ns.Labels = map[string]string{"captf.io/tenant": "silver"}
	if err := apiReader.Update(t.Context(), ns); err != nil {
		t.Fatal(err)
	}
	if ok, reason, _ := Allowed(t.Context(), apiReader, id, tenant); ok || reason != infrav1.NamespaceNotAllowedReason {
		t.Errorf("after label change Allowed = %v, %q; want false, NamespaceNotAllowed", ok, reason)
	}
}

// TestAllowedReadError proves Allowed reports IdentityCheckFailed and the
// underlying error when reading the Namespace fails for a reason other than
// not-found.
func TestAllowedReadError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	r := fake.NewClientBuilder().WithScheme(newScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		},
	}).Build()
	id := testIdentity(&infrav1.AllowedNamespaces{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}}})
	ok, reason, err := Allowed(t.Context(), r, id, tenant)
	if ok || reason != infrav1.IdentityCheckFailedReason || !errors.Is(err, boom) {
		t.Errorf("Allowed = %v, %q, %v; want false, IdentityCheckFailed, boom", ok, reason, err)
	}
}

// TestMirrorName proves MirrorName prefixes a short identity name as-is and
// falls back to a distinct, fixed-length hashed name once the prefixed name
// would exceed the 253-character limit.
func TestMirrorName(t *testing.T) {
	t.Parallel()
	if got := MirrorName(idName); got != "captf-creds-aws-prod" {
		t.Errorf("MirrorName = %q", got)
	}
	fits := strings.Repeat("a", maxSecretName-len(MirrorPrefix))
	if got := MirrorName(fits); got != MirrorPrefix+fits {
		t.Errorf("MirrorName at the limit was hashed: %q", got)
	}
	long := fits + "b"
	got := MirrorName(long)
	if len(got) != len(MirrorPrefix)+16 || !strings.HasPrefix(got, MirrorPrefix) || got == MirrorName(fits+"c") {
		t.Errorf("MirrorName(long) = %q", got)
	}
}

// TestSourceHash proves SourceHash is independent of map iteration order,
// distinguishes data that differs only in a key/value boundary, and gives
// nil and empty data the same 64-character hash.
func TestSourceHash(t *testing.T) {
	t.Parallel()
	a := map[string][]byte{"k1": []byte("v1"), "k2": []byte("v2")}
	b := maps.Clone(a)
	if SourceHash(a) != SourceHash(b) {
		t.Error("hash depends on map order")
	}
	if SourceHash(map[string][]byte{"ab": []byte("c")}) == SourceHash(map[string][]byte{"a": []byte("bc")}) {
		t.Error("key/value boundary not in hash")
	}
	if SourceHash(nil) != SourceHash(map[string][]byte{}) || len(SourceHash(nil)) != 64 {
		t.Error("empty data hash")
	}
}

// mirrorEnv is a mirror test's fixture: c is the client under test, reader
// is the separate uncached reader the source Secret lives behind, and
// updates counts the Update calls c has seen.
type mirrorEnv struct {
	c, reader client.WithWatch
	updates   *atomic.Int32
}

// newMirrorEnv puts the source Secret src only in the API reader, so a
// mirror can only be written if the source was read through it; objs seeds
// the client under test, and test t fails on setup errors. It returns the
// built mirrorEnv.
func newMirrorEnv(t *testing.T, src *corev1.Secret, objs ...client.Object) mirrorEnv {
	t.Helper()
	s := newScheme(t)
	updates := &atomic.Int32{}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
			updates.Add(1)
			return c.Update(ctx, o, opts...)
		},
	}).Build()
	rb := fake.NewClientBuilder().WithScheme(s)
	if src != nil {
		rb = rb.WithObjects(src)
	}
	return mirrorEnv{c: c, reader: rb.Build(), updates: updates}
}

// mirror reads back the identity's mirror Secret in namespace tenant,
// failing test t if it cannot be read. It returns the mirror.
func (e mirrorEnv) mirror(t *testing.T) *corev1.Secret {
	t.Helper()
	m := &corev1.Secret{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: tenant, Name: MirrorName(idName)}, m); err != nil {
		t.Fatalf("get mirror: %v", err)
	}
	return m
}

// TestEnsureMirrorCreates proves EnsureMirror creates a new mirror Secret
// with the source's data copied, the mirrored and managed labels, the
// source-hash and identity annotations, and a non-blocking owner reference
// to the given owner.
func TestEnsureMirrorCreates(t *testing.T) {
	t.Parallel()
	data := map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("id"), "AWS_SECRET_ACCESS_KEY": []byte("secret")}
	e := newMirrorEnv(t, source(data))
	owner := machine("m1", "uid-1")
	if _, _, err := EnsureMirror(t.Context(), e.c, e.reader, testIdentity(nil), tenant, owner); err != nil {
		t.Fatalf("EnsureMirror: %v", err)
	}
	m := e.mirror(t)
	if m.Type != corev1.SecretTypeOpaque || !maps.EqualFunc(m.Data, data, bytes.Equal) {
		t.Errorf("mirror type/data = %s %v", m.Type, m.Data)
	}
	if m.Labels[MirroredLabel] != "true" || m.Labels[state.ManagedLabel] != "true" {
		t.Errorf("labels = %v", m.Labels)
	}
	if m.Annotations[SourceHashAnnotation] != SourceHash(data) || m.Annotations[inputs.IdentityAnnotation] != idName {
		t.Errorf("annotations = %v", m.Annotations)
	}
	if len(m.OwnerReferences) != 1 {
		t.Fatalf("ownerRefs = %v", m.OwnerReferences)
	}
	ref := m.OwnerReferences[0]
	if ref.Kind != "TerraformMachine" || ref.APIVersion != infrav1.GroupVersion.String() || ref.UID != "uid-1" ||
		ref.BlockOwnerDeletion == nil || *ref.BlockOwnerDeletion || (ref.Controller != nil && *ref.Controller) {
		t.Errorf("ownerRef = %+v", ref)
	}
}

// TestEnsureMirrorLifecycle proves EnsureMirror is a no-op when the source
// and owner are unchanged, adds a second owner's reference without
// disturbing the first, follows the source when it rotates, and re-creates
// the mirror when it is deleted out of band.
func TestEnsureMirrorLifecycle(t *testing.T) {
	t.Parallel()
	e := newMirrorEnv(t, source(map[string][]byte{"k": []byte("v1")}))
	id := testIdentity(nil)
	m1, m2 := machine("m1", "uid-1"), machine("m2", "uid-2")
	ensure := func(owner client.Object) bool {
		t.Helper()
		_, res, err := EnsureMirror(t.Context(), e.c, e.reader, id, tenant, owner)
		if err != nil {
			t.Fatalf("EnsureMirror: %v", err)
		}
		if res.OwnerRepaired {
			t.Error("a new or current owner was reported as a repair")
		}
		return res.Created
	}
	if !ensure(m1) {
		t.Error("the first EnsureMirror did not report creating the mirror")
	}

	// Unchanged source and owner: no write.
	if ensure(m1) {
		t.Error("an existing mirror was reported created")
	}
	if n := e.updates.Load(); n != 0 {
		t.Errorf("no-op reconcile wrote %d times", n)
	}

	// A second object shares the mirror.
	ensure(m2)
	if refs := e.mirror(t).OwnerReferences; len(refs) != 2 {
		t.Errorf("ownerRefs after second owner = %v", refs)
	}

	// Rotation: the source changes, the mirror follows.
	src := &corev1.Secret{}
	if err := e.reader.Get(t.Context(), client.ObjectKey{Namespace: system, Name: srcSecret}, src); err != nil {
		t.Fatal(err)
	}
	src.Data = map[string][]byte{"k": []byte("v2")}
	if err := e.reader.Update(t.Context(), src); err != nil {
		t.Fatal(err)
	}
	ensure(m1)
	if m := e.mirror(t); string(m.Data["k"]) != "v2" || m.Annotations[SourceHashAnnotation] != SourceHash(src.Data) {
		t.Errorf("after rotation mirror = %v %v", m.Data, m.Annotations)
	}

	// Deleted out of band: re-created.
	if err := e.c.Delete(t.Context(), e.mirror(t)); err != nil {
		t.Fatal(err)
	}
	if !ensure(m1) {
		t.Error("the re-created mirror was not reported created")
	}
	if m := e.mirror(t); string(m.Data["k"]) != "v2" {
		t.Errorf("re-created mirror = %v", m.Data)
	}
}

// TestEnsureMirrorEmptySource proves EnsureMirror creates a mirror with no
// data, and the hash of empty data, when the source Secret's data is nil.
func TestEnsureMirrorEmptySource(t *testing.T) {
	t.Parallel()
	e := newMirrorEnv(t, source(nil))
	if _, _, err := EnsureMirror(t.Context(), e.c, e.reader, testIdentity(nil), tenant, machine("m1", "uid-1")); err != nil {
		t.Fatalf("EnsureMirror: %v", err)
	}
	if m := e.mirror(t); len(m.Data) != 0 || m.Annotations[SourceHashAnnotation] != SourceHash(nil) {
		t.Errorf("mirror of empty source = %v %v", m.Data, m.Annotations)
	}
}

// TestEnsureMirrorErrors proves EnsureMirror returns ErrSecretNotFound when
// the source is missing, ErrMirrorConflict without overwriting a
// same-named Secret that is not one of its mirrors, and an error when the
// owner's kind is not in the scheme.
func TestEnsureMirrorErrors(t *testing.T) {
	t.Parallel()
	owner := machine("m1", "uid-1")
	t.Run("source missing", func(t *testing.T) {
		t.Parallel()
		e := newMirrorEnv(t, nil)
		if _, _, err := EnsureMirror(t.Context(), e.c, e.reader, testIdentity(nil), tenant, owner); !errors.Is(err, ErrSecretNotFound) {
			t.Errorf("err = %v, want ErrSecretNotFound", err)
		}
	})
	t.Run("user secret with the mirror name", func(t *testing.T) {
		t.Parallel()
		user := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: tenant, Name: MirrorName(idName)}, Data: map[string][]byte{"mine": nil}}
		e := newMirrorEnv(t, source(nil), user)
		if _, _, err := EnsureMirror(t.Context(), e.c, e.reader, testIdentity(nil), tenant, owner); !errors.Is(err, ErrMirrorConflict) {
			t.Errorf("err = %v, want ErrMirrorConflict", err)
		}
		if _, ok := e.mirror(t).Data["mine"]; !ok {
			t.Error("user secret was overwritten")
		}
	})
	t.Run("owner kind not in scheme", func(t *testing.T) {
		t.Parallel()
		e := newMirrorEnv(t, source(nil))
		if _, _, err := EnsureMirror(t.Context(), e.c, e.reader, testIdentity(nil), tenant, &unknownKind{}); err == nil {
			t.Error("unknown owner kind accepted")
		}
	})
}

// TestEnsureMirrorCreateRace: another reconcile created the mirror between
// our Get and Create; we update theirs instead of failing.
func TestEnsureMirrorCreateRace(t *testing.T) {
	t.Parallel()
	s := newScheme(t)
	stale := newMirror(client.ObjectKey{Namespace: tenant, Name: MirrorName(idName)}, idName, "old", nil, metav1.OwnerReference{
		APIVersion: infrav1.GroupVersion.String(), Kind: "TerraformMachine", Name: "m0", UID: "uid-0",
	})
	var first atomic.Bool
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(stale).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if !first.Swap(true) {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, k.Name)
			}
			return c.Get(ctx, k, o, opts...)
		},
	}).Build()
	reader := fake.NewClientBuilder().WithScheme(s).WithObjects(source(map[string][]byte{"k": []byte("v")})).Build()
	m, res, err := EnsureMirror(t.Context(), c, reader, testIdentity(nil), tenant, machine("m1", "uid-1"))
	if err != nil || res.Created {
		t.Fatalf("EnsureMirror: created %v, %v", res.Created, err)
	}
	if string(m.Data["k"]) != "v" || len(m.OwnerReferences) != 2 {
		t.Errorf("mirror after race = %v %v", m.Data, m.OwnerReferences)
	}
}

// TestEnsureMirrorRepairsStaleOwner proves EnsureMirror, on a restored
// mirror whose ref to the object carries the object's earlier UID,
// replaces that ref in place with one to the current UID (keeping the
// non-blocking convention), reports the repair, and leaves the other
// objects' refs, stale or not, alone; a second call writes nothing.
func TestEnsureMirrorRepairsStaleOwner(t *testing.T) {
	t.Parallel()
	ref := func(name, uid string) metav1.OwnerReference {
		return metav1.OwnerReference{APIVersion: infrav1.GroupVersion.String(), Kind: "TerraformMachine", Name: name, UID: types.UID(uid), BlockOwnerDeletion: new(false)}
	}
	data := map[string][]byte{"k": []byte("v")}
	restored := newMirror(client.ObjectKey{Namespace: tenant, Name: MirrorName(idName)}, idName, SourceHash(data), data, ref("m0", "uid-0-old"))
	restored.OwnerReferences = append(restored.OwnerReferences, ref("m1", "uid-1-old"), ref("m2", "uid-2"))
	e := newMirrorEnv(t, source(data), restored)
	_, res, err := EnsureMirror(t.Context(), e.c, e.reader, testIdentity(nil), tenant, machine("m1", "uid-1"))
	if err != nil || !res.OwnerRepaired || res.Created {
		t.Fatalf("EnsureMirror = %+v, %v; want a repair", res, err)
	}
	want := []metav1.OwnerReference{ref("m0", "uid-0-old"), ref("m1", "uid-1"), ref("m2", "uid-2")}
	if got := e.mirror(t).OwnerReferences; !reflect.DeepEqual(got, want) {
		t.Errorf("ownerRefs = %+v, want %+v", got, want)
	}
	before := e.updates.Load()
	if _, res, err := EnsureMirror(t.Context(), e.c, e.reader, testIdentity(nil), tenant, machine("m1", "uid-1")); err != nil || res.OwnerRepaired || e.updates.Load() != before {
		t.Errorf("second EnsureMirror = %+v, %v, %d writes", res, err, e.updates.Load()-before)
	}
}

// TestRevoke proves Revoke deletes the mirror regardless of its owners,
// reports removed=false for an already-missing mirror, and leaves alone a
// same-named Secret that is not a mirror of the identity.
func TestRevoke(t *testing.T) {
	t.Parallel()
	e := newMirrorEnv(t, source(nil))
	for _, o := range []client.Object{machine("m1", "uid-1"), machine("m2", "uid-2")} {
		if _, _, err := EnsureMirror(t.Context(), e.c, e.reader, testIdentity(nil), tenant, o); err != nil {
			t.Fatal(err)
		}
	}
	if removed, err := Revoke(t.Context(), e.c, idName, tenant); err != nil || !removed {
		t.Fatalf("Revoke: removed %v, %v", removed, err)
	}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: tenant, Name: MirrorName(idName)}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("mirror with owners survived Revoke: %v", err)
	}
	if removed, err := Revoke(t.Context(), e.c, idName, tenant); err != nil || removed {
		t.Errorf("Revoke of missing mirror: removed %v, %v", removed, err)
	}

	user := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: tenant, Name: MirrorName(idName)}}
	u := newMirrorEnv(t, nil, user)
	if removed, err := Revoke(t.Context(), u.c, idName, tenant); err != nil || removed {
		t.Fatalf("Revoke of a user Secret: removed %v, %v", removed, err)
	}
	u.mirror(t) // still there
}

// TestRemoveOwner proves RemoveOwner leaves the mirror alone for a
// non-owner, drops one of several owners without deleting the mirror, and
// deletes the mirror once its last owner is removed.
func TestRemoveOwner(t *testing.T) {
	t.Parallel()
	e := newMirrorEnv(t, source(nil))
	m1, m2 := machine("m1", "uid-1"), machine("m2", "uid-2")
	for _, o := range []client.Object{m1, m2} {
		if _, _, err := EnsureMirror(t.Context(), e.c, e.reader, testIdentity(nil), tenant, o); err != nil {
			t.Fatal(err)
		}
	}
	before := e.updates.Load()
	if removed, err := RemoveOwner(t.Context(), e.c, e.mirror(t), machine("other", "uid-x")); err != nil || removed || e.updates.Load() != before {
		t.Errorf("removing a non-owner: removed %v, %v, %d writes", removed, err, e.updates.Load()-before)
	}
	if removed, err := RemoveOwner(t.Context(), e.c, e.mirror(t), m1); err != nil || removed {
		t.Fatalf("removing one of two owners: removed %v, %v", removed, err)
	}
	if refs := e.mirror(t).OwnerReferences; len(refs) != 1 || refs[0].UID != "uid-2" {
		t.Errorf("ownerRefs = %v", refs)
	}
	if removed, err := RemoveOwner(t.Context(), e.c, e.mirror(t), m2); err != nil || !removed {
		t.Fatalf("removing the last owner: removed %v, %v", removed, err)
	}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: tenant, Name: MirrorName(idName)}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("mirror without owners survived: %v", err)
	}
}

// TestEnsureSourceOwnerRef: the user's Secret is never owned by the identity
// (deleting the identity must not garbage-collect it). A ref left by an
// earlier version, for this identity or a re-created one, is removed; other
// owners stay; clusterctl's move label is NOT added (clusterctl would delete
// a label-moved Secret from the source); a second call writes nothing.
func TestEnsureSourceOwnerRef(t *testing.T) {
	t.Parallel()
	id := testIdentity(nil)
	other := metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "keep", UID: "keep-uid"}
	src := source(nil)
	src.Labels = map[string]string{"user": "label"}
	src.OwnerReferences = []metav1.OwnerReference{
		{APIVersion: infrav1.GroupVersion.String(), Kind: "TerraformClusterIdentity", Name: idName, UID: id.UID},
		other,
		{APIVersion: infrav1.GroupVersion.String(), Kind: "TerraformClusterIdentity", Name: idName, UID: "old-uid"},
	}
	var updates atomic.Int32
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(src).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
			updates.Add(1)
			return c.Update(ctx, o, opts...)
		},
	}).Build()
	for range 2 {
		got := &corev1.Secret{}
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(src), got); err != nil {
			t.Fatal(err)
		}
		if err := EnsureSourceOwnerRef(t.Context(), c, id, got); err != nil {
			t.Fatalf("EnsureSourceOwnerRef: %v", err)
		}
	}
	if n := updates.Load(); n != 1 {
		t.Errorf("updates = %d, want 1", n)
	}
	got := &corev1.Secret{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(src), got); err != nil {
		t.Fatal(err)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0] != other {
		t.Errorf("ownerRefs = %+v, want only %+v", got.OwnerReferences, other)
	}
	if _, ok := got.Labels[clusterctlv1.ClusterctlMoveLabel]; ok || got.Labels["user"] != "label" {
		t.Errorf("labels = %v, want only the user's label, never %s", got.Labels, clusterctlv1.ClusterctlMoveLabel)
	}
}

// unknownKind is a client.Object whose type no scheme knows.
type unknownKind struct{ corev1.Secret }

// DeepCopyObject returns u itself: unknownKind exists only to be a
// client.Object type absent from every test scheme, so it never needs a
// real deep copy.
func (u *unknownKind) DeepCopyObject() runtime.Object { return u }
