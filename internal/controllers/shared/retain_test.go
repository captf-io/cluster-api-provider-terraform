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
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/ownership"
	"github.com/captf-io/cluster-api-provider-terraform/internal/plankey"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// retainerUID is the uid of the earlier object of testName that retained the
// state a recreated one finds.
const retainerUID = "m1-retainer-uid"

// retainPolicy sets m's deletionPolicy to Retain.
func retainPolicy(m *infrav1.TerraformMachine) { m.Spec.DeletionPolicy = infrav1.DeletionPolicyRetain }

// noDrift disables m's drift checks, so a provisioned, healthy m has no
// Job due.
func noDrift(m *infrav1.TerraformMachine) {
	m.Spec.Drift = &infrav1.MachineDriftPolicy{IntervalSeconds: new(int32(0))}
}

// adopting sets m's adoptRetainedState to true.
func adopting(m *infrav1.TerraformMachine) { m.Spec.AdoptRetainedState = new(true) }

// writeChunkedState writes, failing t on error, the state of e's machine
// m1 at serial with inputs hash hash, split over a base Secret and one
// -part-1 chunk as the backend writes a large state: both carry the
// backend labels CAPTF passes for m1, and owner, when non-nil, as their
// owner reference (as Adopt leaves them).
func (e *env) writeChunkedState(t *testing.T, serial int64, hash string, owner *infrav1.TerraformMachine) {
	t.Helper()
	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	base := stateSecret(t, suffix, serial, hash)
	data := base.Data[state.DataKey]
	part := base.DeepCopy()
	part.Name += "-part-1"
	part.Annotations = nil
	base.Data = map[string][]byte{state.DataKey: data[:len(data)/2]}
	part.Data = map[string][]byte{state.DataKey: data[len(data)/2:]}
	for _, s := range []*corev1.Secret{base, part} {
		for k, v := range state.BackendLabels(state.KindTerraformMachine, testName, "c1") {
			s.Labels[k] = v
		}
		if owner != nil {
			ref, err := ownership.SecretOwnerRef(owner, e.c.Scheme())
			if err != nil {
				t.Fatal(err)
			}
			s.OwnerReferences = []metav1.OwnerReference{ref}
		}
		if err := e.c.Create(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
}

// retainEnv returns, failing t on error, an env whose provisioned machine
// m1, with each mut applied, applied before: its inputs records, a state
// of serial 3 in two chunks it owns, a backup set of serial 2, its plan
// key and its state lock Lease exist. Jobs live in the fake client, the
// state reader is the real one.
func retainEnv(t *testing.T, mut ...func(*infrav1.TerraformMachine)) *env {
	t.Helper()
	e := newEnv(t, world(machine(append([]func(*infrav1.TerraformMachine){withFinalizer, notPaused, provisioned}, mut...)...))...)
	e.d.Jobs, e.d.State = &clientRunner{c: e.c}, state.NewReader(e.c)
	m := e.get(t)
	e.writeDurable(t, m)
	e.backupOf(t, state.KindTerraformMachine, testName, 2, "h1:x")
	e.writeChunkedState(t, 3, "h1:x", m)
	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	ref, err := ownership.SecretOwnerRef(m, e.c.Scheme())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: plankey.Name("m", testName), OwnerReferences: []metav1.OwnerReference{ref}}},
		&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: state.LeaseName(suffix)}},
	} {
		if err := e.c.Create(t.Context(), o); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// objectSecretMetas returns the metadata of m1's state chunks, state
// backups and inputs Secrets in e, as Retain finds them, failing t on
// error.
func (e *env) objectSecretMetas(t *testing.T) []ownedSecret {
	t.Helper()
	secrets, err := objectSecrets(t.Context(), e.c, &fakeKind{obj: machine()}, suffixOf(t, state.KindTerraformMachine, testName))
	if err != nil {
		t.Fatal(err)
	}
	return secrets
}

// assertRetained fails t unless e holds m1's two state chunks, its backup
// and its two inputs Secrets (durable and applied), each without an owner
// reference to m1 and
// labeled retained from uid; and its plan key and state lock Lease are
// gone.
func assertRetained(t *testing.T, e *env, uid string) {
	t.Helper()
	counts := map[string]int{}
	for _, s := range e.objectSecretMetas(t) {
		counts[s.what]++
		if got := state.RetainedFrom(s.meta.Labels); got != uid {
			t.Errorf("%s Secret %s: retained from %q, want %q", s.what, s.meta.Name, got, uid)
		}
		if state.Protected(s.meta) {
			t.Errorf("%s Secret %s kept the protection finalizer: %v", s.what, s.meta.Name, s.meta.Finalizers)
		}
		for _, ref := range s.meta.OwnerReferences {
			if ref.Kind == state.KindTerraformMachine {
				t.Errorf("%s Secret %s still has owner reference %+v", s.what, s.meta.Name, ref)
			}
		}
	}
	if counts[ownedState] != 2 || counts[ownedBackup] != 1 || counts[ownedInputs] != 2 {
		t.Errorf("kept %v, want 2 state, 1 backup and 2 inputs Secrets", counts)
	}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: plankey.Name("m", testName)}, &corev1.Secret{}); client.IgnoreNotFound(err) != nil || err == nil {
		t.Errorf("plan key: %v, want it deleted", err)
	}
	if l := e.lease(t, state.LeaseName(suffixOf(t, state.KindTerraformMachine, testName))); l != nil {
		t.Errorf("state lock Lease kept: %+v", l)
	}
}

// TestReconcileRetain: a deletion with deletionPolicy Retain removes the
// finalizer without a Job and keeps the state chunks, the backups and the
// durable inputs, unowned and labeled with the object's uid, while the
// plan key and the state lock Lease go; it emits InfrastructureRetained.
// The state still reads afterwards: the label does not hide it from the
// backend's selector.
func TestReconcileRetain(t *testing.T) {
	t.Parallel()
	e := retainEnv(t, deleting, retainPolicy)
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	if m := e.get(t); m != nil || len(e.jobsOf(t)) != 0 {
		t.Fatalf("object %+v, Jobs %d", m, len(e.jobsOf(t)))
	}
	assertRetained(t, e, "m1-uid")
	ev := e.rec.only(EventInfrastructureRetained)
	if len(ev) != 1 || ev[0].eventType != corev1.EventTypeNormal ||
		!strings.Contains(ev[0].note, "Kept 2 state Secret(s), 1 state backup Secret(s) and 2 inputs Secret(s)") ||
		!strings.Contains(ev[0].note, state.RetainedFromUIDLabel+"=m1-uid") {
		t.Errorf("InfrastructureRetained events = %+v", ev)
	}
	if e.rec.count(EventDestroyed) != 0 {
		t.Errorf("events %v", e.rec.reasons)
	}
	st, err := state.NewReader(e.c).Read(t.Context(), testNS, suffixOf(t, state.KindTerraformMachine, testName))
	if err != nil || st.Serial != 3 {
		t.Errorf("retained state reads %+v, %v", st, err)
	}
}

// TestReconcileRetainLocalSecret: an object whose identityRef is a
// namespace-local Secret has no credential mirror, so Retain releases
// none, even one named as that Secret's mirror would be, and leaves the
// Secret itself alone.
func TestReconcileRetainLocalSecret(t *testing.T) {
	t.Parallel()
	const secretName = "my-creds"
	e := retainEnv(t, deleting, retainPolicy, func(m *infrav1.TerraformMachine) {
		m.Spec.IdentityRef = infrav1.IdentityReference{Name: secretName, Kind: infrav1.IdentityKindSecret}
	})
	m := e.get(t)
	if err := writeInputs(t.Context(), e.c, m, renderMachine(t), testMeta{
		Image: "registry.example/mod:1.0", Identity: secretName, IdentityKind: string(infrav1.IdentityKindSecret),
		ImageDigest: "registry.example/mod@sha256:abc",
	}); err != nil {
		t.Fatal(err)
	}
	ref, err := ownership.SecretOwnerRef(m, e.c.Scheme())
	if err != nil {
		t.Fatal(err)
	}
	mirror := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: testNS, Name: identity.MirrorName(secretName), OwnerReferences: []metav1.OwnerReference{ref},
	}}
	for _, s := range []*corev1.Secret{mirror, {ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: secretName}}} {
		if err := e.c.Create(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	if m := e.get(t); m != nil {
		t.Fatalf("object kept: %+v", m.Finalizers)
	}
	assertRetained(t, e, "m1-uid")
	for _, name := range []string{mirror.Name, secretName} {
		var s corev1.Secret
		if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, &s); err != nil {
			t.Errorf("Secret %s: %v, want it kept", name, err)
		}
	}
	if e.rec.count(EventMirrorRemoved) != 0 {
		t.Errorf("events %v", e.rec.reasons)
	}
}

// TestReconcileRetainWaitsForJob: Retain waits for a Job that runs, as a
// destroy would, and labels nothing until it finished.
func TestReconcileRetainWaitsForJob(t *testing.T) {
	t.Parallel()
	e := retainEnv(t, deleting, retainPolicy)
	running := job("captf-m-m1-refresh-a1", jobs.OpRefresh, jobs.Running, t0)
	running.Labels[state.OwnerKindLabel], running.Labels[state.OwnerNameLabel] = state.KindTerraformMachine, testName
	// Just created, so not stuck for its missing per-run Secret.
	running.CreationTimestamp = metav1.NewTime(t0)
	if err := e.c.Create(t.Context(), &running); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	keepsFinalizer(t, e.get(t))
	for _, s := range e.objectSecretMetas(t) {
		if state.RetainedFrom(s.meta.Labels) != "" {
			t.Errorf("%s Secret %s labeled while a Job runs", s.what, s.meta.Name)
		}
	}
	e.finishJob(t, e.jobsOf(t)[0].Name, jobs.Succeeded)
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	if m := e.get(t); m != nil {
		t.Fatalf("object kept after the Job finished: %+v", m.Finalizers)
	}
	assertRetained(t, e, "m1-uid")
}

// TestReconcileRetainReleasesHeldDeletion: setting deletionPolicy Retain
// on a deletion that waits releases it without a destroy: the state is
// lost or unreadable, the last destroy failed, the durable inputs are
// gone, or the identity no longer allows the namespace.
func TestReconcileRetainReleasesHeldDeletion(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T) *env
		// backups is how many backup Secrets are kept.
		backups int
	}{
		{"state lost", func(t *testing.T) *env { return heldEnv(t, retainPolicy) }, 1},
		{"state unreadable", func(t *testing.T) *env {
			e := retainEnv(t, deleting, retainPolicy)
			e.d.State = &fakeState{err: state.ErrStateCorrupt}
			return e
		}, 1},
		{"the last destroy failed", func(t *testing.T) *env {
			e := retainEnv(t, deleting, retainPolicy)
			d := job("captf-m-m1-destroy-a1", jobs.OpDestroy, jobs.Failed, t0.Add(-time.Minute))
			d.Labels[state.OwnerKindLabel], d.Labels[state.OwnerNameLabel] = state.KindTerraformMachine, testName
			if err := e.c.Create(t.Context(), &d); err != nil {
				t.Fatal(err)
			}
			return e
		}, 1},
		{"durable inputs gone", func(t *testing.T) *env {
			e := retainEnv(t, deleting, retainPolicy)
			if err := inputs.Delete(t.Context(), e.c, e.get(t)); err != nil {
				t.Fatal(err)
			}
			return e
		}, 1},
		{"identity does not allow the namespace", func(t *testing.T) *env {
			e := retainEnv(t, deleting, retainPolicy)
			id := &infrav1.TerraformClusterIdentity{}
			if err := e.c.Get(t.Context(), client.ObjectKey{Name: testIdentity}, id); err != nil {
				t.Fatal(err)
			}
			id.Spec.AllowedNamespaces = nil
			if err := e.c.Update(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			return e
		}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := tt.setup(t)
			if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
				t.Fatal(err)
			}
			if m := e.get(t); m != nil || len(e.jobsOf(t)) > 1 {
				t.Fatalf("object %+v, Jobs %d", m, len(e.jobsOf(t)))
			}
			backups := 0
			for _, s := range e.objectSecretMetas(t) {
				if state.RetainedFrom(s.meta.Labels) != "m1-uid" {
					t.Errorf("%s Secret %s not retained", s.what, s.meta.Name)
				}
				if s.what == ownedBackup {
					backups++
				}
			}
			if backups != tt.backups || e.rec.count(EventInfrastructureRetained) != 1 {
				t.Errorf("backups kept %d, want %d; events %v", backups, tt.backups, e.rec.reasons)
			}
		})
	}
}

// TestReconcileRetainInherited: an unset deletionPolicy takes the
// cluster's defaults.deletionPolicy, then the TerraformCluster's own;
// the machine's own Destroy wins over both.
func TestReconcileRetainInherited(t *testing.T) {
	t.Parallel()
	retainCluster := func(defaults, own infrav1.DeletionPolicy) *infrav1.TerraformCluster {
		tc := &infrav1.TerraformCluster{Spec: infrav1.TerraformClusterSpec{Defaults: &infrav1.TerraformClusterDefaults{DeletionPolicy: defaults}}}
		tc.Spec.DeletionPolicy = own
		return tc
	}
	for _, tt := range []struct {
		name     string
		mut      []func(*infrav1.TerraformMachine)
		cluster  *infrav1.TerraformCluster
		retained bool
	}{
		{"defaults.deletionPolicy", nil, retainCluster(infrav1.DeletionPolicyRetain, ""), true},
		{"the TerraformCluster's own", nil, retainCluster("", infrav1.DeletionPolicyRetain), true},
		{"defaults before the TerraformCluster's own", nil, retainCluster(infrav1.DeletionPolicyDestroy, infrav1.DeletionPolicyRetain), false},
		{"own Destroy wins", []func(*infrav1.TerraformMachine){func(m *infrav1.TerraformMachine) {
			m.Spec.DeletionPolicy = infrav1.DeletionPolicyDestroy
		}}, retainCluster(infrav1.DeletionPolicyRetain, infrav1.DeletionPolicyRetain), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := retainEnv(t, append([]func(*infrav1.TerraformMachine){deleting}, tt.mut...)...)
			k := healthyKind(t, e, testName)
			k.owner.InfraCluster = tt.cluster
			if _, err := Reconcile(t.Context(), e.d, k); err != nil {
				t.Fatal(err)
			}
			destroys := 0
			for _, j := range e.jobsOf(t) {
				if jobs.OpOf(&j) == jobs.OpDestroy {
					destroys++
				}
			}
			if gone := e.get(t) == nil; gone != tt.retained || (destroys == 0) != tt.retained {
				t.Errorf("gone %v, destroys %d; want retained %v", gone, destroys, tt.retained)
			}
		})
	}
}

// foreignRetainedEnv returns, failing t on error, an env whose machine m1
// (uid m1-uid, with each mut applied) is new, and whose namespace holds
// what an earlier m1 (retainerUID) retained: its durable inputs, a state of
// serial 3 in two chunks and a backup, unowned and labeled retained from
// retainerUID. Jobs live in the fake client, the state reader is the real one.
func foreignRetainedEnv(t *testing.T, mut ...func(*infrav1.TerraformMachine)) *env {
	t.Helper()
	e := newEnv(t, world(machine(append([]func(*infrav1.TerraformMachine){withFinalizer, notPaused}, mut...)...))...)
	e.d.Jobs, e.d.State = &clientRunner{c: e.c}, state.NewReader(e.c)
	old := machine(func(m *infrav1.TerraformMachine) { m.UID = retainerUID })
	if err := writeInputs(t.Context(), e.c, old, renderMachine(t), testMeta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
		t.Fatal(err)
	}
	if err := inputs.MarkApplied(t.Context(), e.c, old); err != nil {
		t.Fatal(err)
	}
	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	e.setState(t, suffix, 2, "h1:x")
	if _, _, err := state.TakeBackup(t.Context(), e.c, state.BackupOptions{Owner: old, OwnerKind: state.KindTerraformMachine, ClusterName: "c1", Suffix: suffix, Now: t0}); err != nil {
		t.Fatal(err)
	}
	if err := state.DeleteState(t.Context(), e.c, testNS, suffix); err != nil {
		t.Fatal(err)
	}
	e.writeChunkedState(t, 3, "h1:x", old)
	ref, err := ownership.SecretOwnerRef(old, e.c.Scheme())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range e.objectSecretMetas(t) {
		if _, err := ownership.Retain(t.Context(), e.c, s.meta, ref); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// assertForeignUntouched fails t unless every Secret the earlier m1
// retained is still in e, labeled retained from retainerUID, with no owner
// reference.
func assertForeignUntouched(t *testing.T, e *env) {
	t.Helper()
	secrets := e.objectSecretMetas(t)
	if len(secrets) != 4 {
		t.Errorf("%d retained Secrets left, want 4", len(secrets))
	}
	for _, s := range secrets {
		if got := state.RetainedFrom(s.meta.Labels); got != retainerUID || len(s.meta.OwnerReferences) != 0 {
			t.Errorf("%s Secret %s: retained from %q, owners %+v", s.what, s.meta.Name, got, s.meta.OwnerReferences)
		}
	}
}

// TestReconcileRetainedStateFound: a new object of the same kind,
// namespace and name as one that retained its state holds with
// StateReadable False/RetainedStateFound: no Job runs, its outputs are not
// mapped, and the retained Secrets are not owned, also over several
// passes; a RetainedStateFound Warning is emitted once.
func TestReconcileRetainedStateFound(t *testing.T) {
	t.Parallel()
	e := foreignRetainedEnv(t)
	for range 2 {
		res, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName))
		if err != nil {
			t.Fatal(err)
		}
		if res.RequeueAfter != StateRequeue {
			t.Errorf("requeue %s, want %s", res.RequeueAfter, StateRequeue)
		}
	}
	m := e.get(t)
	c := conditions.Get(m, infrav1.StateReadableCondition)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.RetainedStateFoundReason ||
		!strings.Contains(c.Message, state.RetainedFromUIDLabel+"="+retainerUID) || !strings.Contains(c.Message, "adoptRetainedState") {
		t.Errorf("StateReadable = %+v", c)
	}
	if len(e.jobsOf(t)) != 0 || (m.Status.Initialization.Provisioned != nil && *m.Status.Initialization.Provisioned) {
		t.Errorf("Jobs %d, provisioned %v", len(e.jobsOf(t)), m.Status.Initialization.Provisioned)
	}
	assertForeignUntouched(t, e)
	ev := e.rec.only(EventRetainedStateFound)
	if len(ev) != 1 || ev[0].eventType != corev1.EventTypeWarning {
		t.Errorf("RetainedStateFound events = %+v", ev)
	}
}

// TestReconcileRetainedStateFoundWithoutState: retained backups and
// durable inputs are found, and hold the object, also when no state
// Secret is left.
func TestReconcileRetainedStateFoundWithoutState(t *testing.T) {
	t.Parallel()
	e := foreignRetainedEnv(t)
	if err := state.DeleteState(t.Context(), e.c, testNS, suffixOf(t, state.KindTerraformMachine, testName)); err != nil {
		t.Fatal(err)
	}
	if err := inputs.Delete(t.Context(), e.c, e.get(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	c := conditions.Get(e.get(t), infrav1.StateReadableCondition)
	if c == nil || c.Reason != infrav1.RetainedStateFoundReason || len(e.jobsOf(t)) != 0 {
		t.Errorf("StateReadable = %+v, Jobs %d", c, len(e.jobsOf(t)))
	}
}

// TestReconcileAdoptRetainedState: spec.adoptRetainedState removes the
// label from every retained Secret and emits RetainedStateAdopted; the
// next pass owns them again and reads the state as the object's own,
// which makes the object provisioned without a new apply.
func TestReconcileAdoptRetainedState(t *testing.T) {
	t.Parallel()
	e := foreignRetainedEnv(t, adopting, noDrift)
	res, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName))
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != LagRequeue || e.rec.count(EventRetainedStateAdopted) != 1 {
		t.Fatalf("requeue %s, events %v", res.RequeueAfter, e.rec.reasons)
	}
	for _, s := range e.objectSecretMetas(t) {
		if state.RetainedFrom(s.meta.Labels) != "" {
			t.Errorf("%s Secret %s still labeled", s.what, s.meta.Name)
		}
	}
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	for _, s := range e.objectSecretMetas(t) {
		if len(s.meta.OwnerReferences) != 1 || s.meta.OwnerReferences[0].UID != types.UID("m1-uid") {
			t.Errorf("%s Secret %s owners %+v, want m1-uid", s.what, s.meta.Name, s.meta.OwnerReferences)
		}
	}
	m := e.get(t)
	if c := conditions.Get(m, infrav1.StateReadableCondition); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("StateReadable = %+v", c)
	}
	if p := m.Status.Initialization.Provisioned; p == nil || !*p || len(e.jobsOf(t)) != 0 {
		t.Errorf("provisioned %v, Jobs %d", p, len(e.jobsOf(t)))
	}
}

// TestReconcileDeleteWhileHeldOnRetained: deleting an object held on
// another object's retained state removes its finalizer without a Job and
// never deletes, owns or relabels those Secrets, whatever its own
// deletionPolicy.
func TestReconcileDeleteWhileHeldOnRetained(t *testing.T) {
	t.Parallel()
	for _, policy := range []infrav1.DeletionPolicy{"", infrav1.DeletionPolicyDestroy, infrav1.DeletionPolicyRetain} {
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()
			e := foreignRetainedEnv(t, deleting, func(m *infrav1.TerraformMachine) { m.Spec.DeletionPolicy = policy })
			if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
				t.Fatal(err)
			}
			if m := e.get(t); m != nil || len(e.jobsOf(t)) != 0 {
				t.Fatalf("object %+v, Jobs %d", m, len(e.jobsOf(t)))
			}
			assertForeignUntouched(t, e)
			if e.rec.count(EventFinalizerRemoved) != 1 || e.rec.count(EventInfrastructureRetained) != 0 || e.rec.count(EventDestroyed) != 0 {
				t.Errorf("events %v", e.rec.reasons)
			}
		})
	}
}

// TestReconcileOwnRetainedLabelCleared: the object's own uid on its
// Secrets (a Retain cut short, then deletionPolicy set back) is removed
// again, so they are owned as usual; while it is still being deleted with
// Retain they keep it.
func TestReconcileOwnRetainedLabelCleared(t *testing.T) {
	t.Parallel()
	e := retainEnv(t, noDrift)
	ref, err := ownership.SecretOwnerRef(e.get(t), e.c.Scheme())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range e.objectSecretMetas(t) {
		if _, err := ownership.Retain(t.Context(), e.c, s.meta, ref); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range e.objectSecretMetas(t) {
		if state.RetainedFrom(s.meta.Labels) != "" || len(s.meta.OwnerReferences) != 1 {
			t.Errorf("%s Secret %s: labels %v, owners %+v", s.what, s.meta.Name, s.meta.Labels, s.meta.OwnerReferences)
		}
	}
	if c := conditions.Get(e.get(t), infrav1.StateReadableCondition); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("StateReadable = %+v", c)
	}
}

// TestDecideOpRetain proves deletionPolicy Retain comes right after an
// active Job: before a requested restore, a held deletion and a destroy,
// and only while deleting.
func TestDecideOpRetain(t *testing.T) {
	t.Parallel()
	base := DecideInput{Deleting: true, Retain: true, State: StateView{Exists: true, InputsHash: "h1:a"}, Now: t0}
	for _, tt := range []struct {
		name string
		mut  func(*DecideInput)
		want Action
	}{
		{"a destroy", func(*DecideInput) {}, ActionRetain},
		{"a held deletion", func(in *DecideInput) { in.StateHeld = true }, ActionRetain},
		{"a requested restore", func(in *DecideInput) { in.StateHeld, in.Restore = true, true }, ActionRetain},
		{"no state", func(in *DecideInput) { in.State = StateView{} }, ActionRetain},
		{"an active Job first", func(in *DecideInput) { in.Jobs.Active = true }, ActionNone},
		{"not deleting", func(in *DecideInput) { in.Deleting = false }, ActionNone},
	} {
		in := base
		tt.mut(&in)
		if got := DecideOp(in); got.Action != tt.want {
			t.Errorf("%s: %+v, want action %d", tt.name, got, tt.want)
		}
	}
}

// TestResolveDeletionPolicy proves the deletionPolicy precedence: own,
// then defaults.deletionPolicy, then the TerraformCluster's own, then
// Destroy; a TerraformCluster takes only its own.
func TestResolveDeletionPolicy(t *testing.T) {
	t.Parallel()
	const destroy, retain = infrav1.DeletionPolicyDestroy, infrav1.DeletionPolicyRetain
	tc := func(defaults, own infrav1.DeletionPolicy) *infrav1.TerraformCluster {
		c := &infrav1.TerraformCluster{Spec: infrav1.TerraformClusterSpec{Defaults: &infrav1.TerraformClusterDefaults{DeletionPolicy: defaults}}}
		c.Spec.DeletionPolicy = own
		return c
	}
	machineSpec := func(own infrav1.DeletionPolicy) SpecView {
		return SpecView{InheritsDefaults: true, WorkspaceSpec: infrav1.WorkspaceSpec{DeletionPolicy: own}}
	}
	for _, tt := range []struct {
		name    string
		spec    SpecView
		cluster *infrav1.TerraformCluster
		want    infrav1.DeletionPolicy
	}{
		{"own wins", machineSpec(destroy), tc(retain, retain), destroy},
		{"defaults next", machineSpec(""), tc(retain, destroy), retain},
		{"the cluster's own next", machineSpec(""), tc("", retain), retain},
		{"Destroy last", machineSpec(""), tc("", ""), destroy},
		{"no cluster: unknown, never Destroy", machineSpec(""), nil, ""},
		{"no cluster, own value", machineSpec(retain), nil, retain},
		{"a pool inherits too", SpecView{InheritsDefaults: true, PoolDrift: &infrav1.MachinePoolDriftPolicy{}}, tc(retain, ""), retain},
		{"a cluster: its own", SpecView{WorkspaceSpec: infrav1.WorkspaceSpec{DeletionPolicy: retain}}, nil, retain},
		{"a cluster: not its defaults", SpecView{}, tc(retain, ""), destroy},
	} {
		if got := Resolve(tt.spec, tt.cluster, time.Minute, true).DeletionPolicy; got != tt.want {
			t.Errorf("%s: %s, want %s", tt.name, got, tt.want)
		}
	}
}

// TestReconcileDeletionPolicyUnresolved: a deleting machine that sets no
// deletionPolicy and whose TerraformCluster cannot be found (its owner is
// gone without a cluster, or forged) holds with Deleting
// DeletionPolicyUnresolved: no destroy, no Retain, nothing deleted or
// labeled, and Destroy is never assumed. A policy of its own proceeds, and
// a destroy that already succeeded still cleans up.
func TestReconcileDeletionPolicyUnresolved(t *testing.T) {
	t.Parallel()
	gone := OwnerInfo{HasOwnerRef: true, OwnerGone: true}
	forged := OwnerInfo{HasOwnerRef: true, Gate: &Gate{Status: metav1.ConditionFalse, Reason: infrav1.OwnerMismatchReason, Message: "forged"}}
	for _, owner := range []OwnerInfo{gone, forged} {
		e := retainEnv(t, deleting)
		k := healthyKind(t, e, testName)
		k.owner = owner
		res, err := Reconcile(t.Context(), e.d, k)
		if err != nil {
			t.Fatal(err)
		}
		m := e.get(t)
		keepsFinalizer(t, m)
		c := conditions.Get(m, clusterv1.DeletingCondition)
		if c == nil || c.Status != metav1.ConditionTrue || c.Reason != infrav1.DeletionPolicyUnresolvedReason ||
			!strings.Contains(c.Message, "spec.deletionPolicy") || res.RequeueAfter != GateRequeue {
			t.Errorf("%+v: Deleting = %+v, requeue %s", owner, c, res.RequeueAfter)
		}
		if n := len(e.jobsOf(t)); n != 0 {
			t.Errorf("%+v: %d Jobs started", owner, n)
		}
		for _, s := range e.objectSecretMetas(t) {
			if state.RetainedFrom(s.meta.Labels) != "" || len(s.meta.OwnerReferences) != 1 {
				t.Errorf("%+v: %s Secret %s touched: %v %+v", owner, s.what, s.meta.Name, s.meta.Labels, s.meta.OwnerReferences)
			}
		}
		if len(e.objectSecretMetas(t)) != 5 {
			t.Errorf("%+v: Secrets deleted", owner)
		}
	}
	t.Run("own Retain proceeds", func(t *testing.T) {
		t.Parallel()
		e := retainEnv(t, deleting, retainPolicy)
		k := healthyKind(t, e, testName)
		k.owner = gone
		if _, err := Reconcile(t.Context(), e.d, k); err != nil {
			t.Fatal(err)
		}
		if e.get(t) != nil {
			t.Fatal("object kept")
		}
		assertRetained(t, e, "m1-uid")
	})
	t.Run("own Destroy proceeds", func(t *testing.T) {
		t.Parallel()
		e := retainEnv(t, deleting, func(m *infrav1.TerraformMachine) { m.Spec.DeletionPolicy = infrav1.DeletionPolicyDestroy })
		k := healthyKind(t, e, testName)
		k.owner = gone
		if _, err := Reconcile(t.Context(), e.d, k); err != nil {
			t.Fatal(err)
		}
		if js := e.jobsOf(t); len(js) != 1 || jobs.OpOf(&js[0]) != jobs.OpDestroy {
			t.Errorf("Jobs %v, want one destroy", js)
		}
	})
	t.Run("a succeeded destroy still cleans up", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused))...)
		e.state.st = &state.State{InputsHash: "h1:x"}
		e.runner.jobs = append(e.runner.jobs, job("d", jobs.OpDestroy, jobs.Succeeded, t0))
		if _, err := reconcileOnce(t, e, e.kindFor(t, gone)); err != nil {
			t.Fatal(err)
		}
		if e.get(t) != nil || e.rec.count(EventDestroyed) != 1 {
			t.Errorf("events %v", e.rec.reasons)
		}
	})
}
