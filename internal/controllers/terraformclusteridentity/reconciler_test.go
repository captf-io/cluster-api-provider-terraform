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

package terraformclusteridentity

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/manager"
)

// idName is the name of the TerraformClusterIdentity every test fixture
// uses.
const idName = "aws"

// scheme builds a runtime.Scheme with the core and infrav1 types this
// package's tests need, failing t if either fails to register; it returns
// the built scheme.
func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, infrav1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// testIdentity returns a TerraformClusterIdentity fixture named idName,
// referencing a fixed credentials Secret.
func testIdentity() *infrav1.TerraformClusterIdentity {
	return &infrav1.TerraformClusterIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: idName, Generation: 3},
		Spec:       infrav1.TerraformClusterIdentitySpec{SecretRef: infrav1.SecretReference{Namespace: "captf-system", Name: "creds"}},
	}
}

// sourceSecret returns the user's credential Secret as the controller
// leaves it: not owned and not labeled for clusterctl move (clusterctl
// would delete it from the source), so reconciling it writes nothing.
func sourceSecret() *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "captf-system", Name: "creds"}}
}

// mirror returns the metadata of a credential mirror Secret in namespace
// ns for the identity named idn, as the manager's cache holds it.
func mirror(ns, idn string) *metav1.PartialObjectMetadata {
	m := manager.SecretMeta()
	m.ObjectMeta = metav1.ObjectMeta{
		Namespace:   ns,
		Name:        identity.MirrorName(idn),
		Labels:      map[string]string{identity.MirroredLabel: "true"},
		Annotations: map[string]string{inputs.IdentityAnnotation: idn},
	}
	return m
}

// user returns a TerraformMachine in namespace ns that uses the identity
// idName through its own identityRef.
func user(ns string) *infrav1.TerraformMachine {
	return &infrav1.TerraformMachine{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "m"},
		Spec:       infrav1.TerraformMachineSpec{WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: idName}}},
	}
}

// errLiveList is what the live readers of an env return for any List: the
// mirrors and users must be read from the cache.
var errLiveList = errors.New("live List: mirrors and users must come from the cache")

// failList is an interceptor that fails every List with errLiveList.
var failList = interceptor.Funcs{
	List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return errLiveList
	},
}

// newCache builds the fake manager cache of an env: objs (mirrors and the
// identity's users) with the MirrorIdentityIndex the manager registers on
// the mirrors' metadata. The fake client stores only typed objects, so a
// mirror's metadata is stored as a Secret without data; the reconciler
// still lists it as metadata. t fails the test on setup error. It
// returns the built client.
func newCache(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	typed := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		if m, ok := o.(*metav1.PartialObjectMetadata); ok {
			o = &corev1.Secret{ObjectMeta: *m.ObjectMeta.DeepCopy()}
		}
		typed = append(typed, o)
	}
	return fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(typed...).
		WithIndex(manager.SecretMeta(), shared.MirrorIdentityIndex, shared.MirrorIdentityIndexer).Build()
}

// env is a Reconciler wired to fake clients, for a test to reconcile
// against and inspect.
type env struct {
	r       *Reconciler
	c       client.Client
	cache   client.Client
	patches *atomic.Int32
}

// newEnv builds an env with objs (mirrors and users) in the cache only,
// a fixed testIdentity in the main client, and source, if non-nil, in the
// API reader only, so Ready can only be True if the source was read
// through it. Both live readers fail every List, so status.namespaces can
// only come from the cache. t fails the test on setup error. It returns
// the built env.
func newEnv(t *testing.T, source *corev1.Secret, objs ...client.Object) env {
	t.Helper()
	s := scheme(t)
	patches := &atomic.Int32{}
	funcs := failList
	funcs.SubResourcePatch = func(ctx context.Context, c client.Client, sub string, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
		patches.Add(1)
		return c.SubResource(sub).Patch(ctx, o, p, opts...)
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(testIdentity()).
		WithStatusSubresource(&infrav1.TerraformClusterIdentity{}).
		WithInterceptorFuncs(funcs).Build()
	rb := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(failList)
	if source != nil {
		rb = rb.WithObjects(source)
	}
	cache := newCache(t, objs...)
	return env{r: &Reconciler{Client: c, Cache: cache, APIReader: rb.Build(), RequeueAfter: time.Minute}, c: c, cache: cache, patches: patches}
}

// reconcile runs one Reconcile of e's fixed identity, failing t on error,
// and returns the Result and the identity's state afterward.
func (e env) reconcile(t *testing.T) (ctrl.Result, *infrav1.TerraformClusterIdentity) {
	t.Helper()
	res, err := e.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: idName}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &infrav1.TerraformClusterIdentity{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Name: idName}, got); err != nil {
		t.Fatal(err)
	}
	return res, got
}

// TestReconcileSecretFound proves Reconcile sets Ready True/SecretFound at
// the identity's generation, collects only the sorted, non-deleting
// mirror namespaces for this identity, requeues after RequeueAfter, and
// patches status only when something changed.
func TestReconcileSecretFound(t *testing.T) {
	t.Parallel()
	deleting := mirror("team-z", idName)
	now := metav1.Now()
	deleting.DeletionTimestamp, deleting.Finalizers = &now, []string{"x"}
	e := newEnv(t, sourceSecret(),
		mirror("team-b", idName), mirror("team-a", idName), mirror("team-c", "other"), deleting,
		user("team-a"), user("team-b"), user("team-c"), user("team-z"),
		// Not a mirror: labeled wrong.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-d", Name: identity.MirrorName(idName), Annotations: map[string]string{inputs.IdentityAnnotation: idName}}},
		user("team-d"),
	)
	res, got := e.reconcile(t)
	if res.RequeueAfter != time.Minute {
		t.Errorf("RequeueAfter = %v, want 1m (the source Secret cannot be watched)", res.RequeueAfter)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, infrav1.ReadyCondition)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != infrav1.SecretFoundReason || c.ObservedGeneration != 3 {
		t.Errorf("Ready = %+v, want True/SecretFound at generation 3", c)
	}
	if want := []string{"team-a", "team-b"}; !slices.Equal(got.Status.Namespaces, want) {
		t.Errorf("namespaces = %v, want %v", got.Status.Namespaces, want)
	}
	// A second pass with nothing changed writes nothing.
	before := e.patches.Load()
	e.reconcile(t)
	if n := e.patches.Load() - before; n != 0 {
		t.Errorf("no-op reconcile patched status %d times", n)
	}
}

// TestReconcileMigratesSourceOwnerRef: an unused identity still gets the
// ownerRef an earlier version put on the user's Secret removed, so deleting
// it (which the webhook allows) cannot garbage-collect the Secret.
func TestReconcileMigratesSourceOwnerRef(t *testing.T) {
	t.Parallel()
	src := sourceSecret()
	src.Labels = nil
	src.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: infrav1.GroupVersion.String(), Kind: "TerraformClusterIdentity", Name: idName, UID: "id-uid",
	}}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(testIdentity(), src).
		WithStatusSubresource(&infrav1.TerraformClusterIdentity{}).Build()
	r := &Reconciler{Client: c, Cache: newCache(t), APIReader: c}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: idName}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &corev1.Secret{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(src), got); err != nil {
		t.Fatal(err)
	}
	if len(got.OwnerReferences) != 0 {
		t.Errorf("ownerRefs = %v, want none", got.OwnerReferences)
	}
	if _, ok := got.Labels[clusterctlv1.ClusterctlMoveLabel]; ok {
		t.Errorf("labels = %v: %s must never be added", got.Labels, clusterctlv1.ClusterctlMoveLabel)
	}
}

// TestReconcileSecretNotFound proves Reconcile sets Ready
// False/SecretNotFound and leaves status.namespaces empty when the source
// Secret does not exist.
func TestReconcileSecretNotFound(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	_, got := e.reconcile(t)
	c := meta.FindStatusCondition(got.Status.Conditions, infrav1.ReadyCondition)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.SecretNotFoundReason {
		t.Errorf("Ready = %+v, want False/SecretNotFound", c)
	}
	if len(got.Status.Namespaces) != 0 {
		t.Errorf("namespaces = %v, want none", got.Status.Namespaces)
	}
}

// TestReconcileRequiredKeys proves a Secret lacking a required key makes
// Ready False/CredentialsIncomplete naming the missing keys, and that it is
// True once every key is present. Only key names matter, never values.
func TestReconcileRequiredKeys(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		data       map[string][]byte
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsg    string
	}{
		{"all present", map[string][]byte{"A": nil, "B": []byte("")}, metav1.ConditionTrue, infrav1.SecretFoundReason, ""},
		{"one missing", map[string][]byte{"A": []byte("x")}, metav1.ConditionFalse, infrav1.CredentialsIncompleteReason, "B"},
		{"all missing", nil, metav1.ConditionFalse, infrav1.CredentialsIncompleteReason, "A, B"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := sourceSecret()
			src.Data = tc.data
			e := newEnv(t, src)
			id := &infrav1.TerraformClusterIdentity{}
			if err := e.c.Get(t.Context(), client.ObjectKey{Name: idName}, id); err != nil {
				t.Fatal(err)
			}
			id.Spec.RequiredKeys = []string{"B", "A"}
			if err := e.c.Update(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			_, got := e.reconcile(t)
			c := meta.FindStatusCondition(got.Status.Conditions, infrav1.ReadyCondition)
			if c == nil || c.Status != tc.wantStatus || c.Reason != tc.wantReason {
				t.Fatalf("Ready = %+v, want %s/%s", c, tc.wantStatus, tc.wantReason)
			}
			if tc.wantMsg != "" && !strings.Contains(c.Message, tc.wantMsg) {
				t.Errorf("message %q does not name %q", c.Message, tc.wantMsg)
			}
		})
	}
}

// recorder records event reasons.
type recorder struct {
	mu      sync.Mutex
	reasons []string
}

// Eventf records reason from an events.EventRecorder.Eventf call.
func (r *recorder) Eventf(_, _ runtime.Object, _, reason, _, _ string, _ ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reasons = append(r.reasons, reason)
}

// TestReadyEvents: IdentitySecretNotFound and IdentitySecretFound once per
// Ready transition; an identity whose Secret exists from the start is quiet.
func TestReadyEvents(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	rec := &recorder{}
	e.r.Recorder = rec
	e.reconcile(t)
	e.reconcile(t)
	if !slices.Equal(rec.reasons, []string{shared.EventIdentitySecretNotFound}) {
		t.Fatalf("events = %v, want one IdentitySecretNotFound", rec.reasons)
	}
	reader, ok := e.r.APIReader.(client.Client)
	if !ok {
		t.Fatal("the API reader is not a client")
	}
	if err := reader.Create(t.Context(), sourceSecret()); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	e.reconcile(t)
	if want := []string{shared.EventIdentitySecretNotFound, shared.EventIdentitySecretFound}; !slices.Equal(rec.reasons, want) {
		t.Errorf("events = %v, want %v", rec.reasons, want)
	}

	found := newEnv(t, sourceSecret())
	quiet := &recorder{}
	found.r.Recorder = quiet
	found.reconcile(t)
	if len(quiet.reasons) != 0 {
		t.Errorf("a new identity with its Secret emitted %v", quiet.reasons)
	}
}

// TestReconcileMirrorRemoved: a namespace drops out of status.namespaces
// once its mirror is gone, so the delete webhook stops blocking.
func TestReconcileMirrorRemoved(t *testing.T) {
	t.Parallel()
	m := mirror("team-a", idName)
	e := newEnv(t, sourceSecret(), m, user("team-a"))
	if _, got := e.reconcile(t); !slices.Equal(got.Status.Namespaces, []string{"team-a"}) {
		t.Fatalf("namespaces = %v", got.Status.Namespaces)
	}
	if err := e.cache.Delete(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if _, got := e.reconcile(t); len(got.Status.Namespaces) != 0 {
		t.Errorf("namespaces after mirror deletion = %v, want none", got.Status.Namespaces)
	}
}

// TestReconcileForgedMirrorDoesNotPin proves a Secret a tenant plants with
// the mirror label and annotation does not enter status.namespaces unless
// it has the mirror's name and an object in its namespace uses the
// identity, so it cannot block the identity's deletion.
func TestReconcileForgedMirrorDoesNotPin(t *testing.T) {
	t.Parallel()
	wrongName := mirror("team-b", idName)
	wrongName.Name = "forged"
	e := newEnv(t, sourceSecret(),
		mirror("team-a", idName),  // right name, but nothing in team-a uses the identity
		wrongName, user("team-b"), // a user, but the Secret is not the mirror
		mirror("team-c", idName), user("team-c"), // the real thing
	)
	if _, got := e.reconcile(t); !slices.Equal(got.Status.Namespaces, []string{"team-c"}) {
		t.Errorf("namespaces = %v, want [team-c]", got.Status.Namespaces)
	}
}

// TestReconcileMissingIdentity proves Reconcile returns a zero Result and
// no error for a request naming an identity that does not exist.
func TestReconcileMissingIdentity(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	res, err := e.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "gone"}})
	if err != nil || res != (ctrl.Result{}) {
		t.Errorf("Reconcile(missing) = %v, %v", res, err)
	}
}

// TestReconcileSourceReadError proves Reconcile returns a read error from
// the API reader unwrapped enough for errors.Is, and patches status zero
// times.
func TestReconcileSourceReadError(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	boom := errors.New("boom")
	e.r.APIReader = fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		},
	}).Build()
	if _, err := e.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: idName}}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
	if n := e.patches.Load(); n != 0 {
		t.Errorf("status patched %d times on a read error", n)
	}
}

// TestDefaultRequeue proves Reconcile requeues after DefaultRequeueAfter
// when RequeueAfter is zero.
func TestDefaultRequeue(t *testing.T) {
	t.Parallel()
	e := newEnv(t, sourceSecret())
	e.r.RequeueAfter = 0
	if res, _ := e.reconcile(t); res.RequeueAfter != DefaultRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, DefaultRequeueAfter)
	}
}

// TestMirrorToIdentity proves MirrorToIdentity maps a mirror Secret to one
// request for its identity, and maps a non-mirror Secret to no requests.
func TestMirrorToIdentity(t *testing.T) {
	t.Parallel()
	reqs := MirrorToIdentity(t.Context(), mirror("team-a", idName))
	if len(reqs) != 1 || reqs[0].Name != idName || reqs[0].Namespace != "" {
		t.Errorf("requests = %v", reqs)
	}
	if reqs := MirrorToIdentity(t.Context(), sourceSecret()); len(reqs) != 0 {
		t.Errorf("non-mirror mapped to %v", reqs)
	}
}
