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
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/plankey"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// The UIDs of m1 before and after a management-cluster restore.
const (
	oldUID = types.UID("m1-uid")
	newUID = types.UID("m1-uid-restored")
)

// ownerRefTo returns the owner reference CAPTF's per-object Secrets carry
// to the TerraformMachine name with uid.
func ownerRefTo(name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: infrav1.GroupVersion.String(), Kind: state.KindTerraformMachine, Name: name, UID: uid}
}

// ownerCalls counts what the owner-reference repair may cost: Secret
// patches that carry ownerReferences, Secret updates, Gets of m1's plan
// key and Lists by a backup selector. fail, while set, makes every Secret
// patch carrying ownerReferences fail with that error.
type ownerCalls struct {
	mu                          sync.Mutex
	refPatches, updates         int
	planKeyGets, backupListings int
	patched                     []string
	fail                        atomic.Pointer[error]
}

// funcs returns interceptors recording into o and passing every call on.
func (o *ownerCalls) funcs() interceptor.Funcs {
	planKey := plankey.Name("m", testName)
	return interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				if data, err := p.Data(obj); err == nil && strings.Contains(string(data), "ownerReferences") {
					o.mu.Lock()
					o.refPatches++
					o.patched = append(o.patched, obj.GetName())
					o.mu.Unlock()
					if err := o.fail.Load(); err != nil {
						return *err
					}
				}
			}
			return c.Patch(ctx, obj, p, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				o.mu.Lock()
				o.updates++
				o.mu.Unlock()
			}
			return c.Update(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == planKey {
				o.mu.Lock()
				o.planKeyGets++
				o.mu.Unlock()
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lo := &client.ListOptions{}
			lo.ApplyOptions(opts)
			if lo.LabelSelector != nil && strings.Contains(lo.LabelSelector.String(), state.BackupLabel) {
				o.mu.Lock()
				o.backupListings++
				o.mu.Unlock()
			}
			return c.List(ctx, list, opts...)
		},
	}
}

// snapshot returns o's counters.
func (o *ownerCalls) snapshot() (refPatches, updates, planKeyGets, backupListings int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.refPatches, o.updates, o.planKeyGets, o.backupListings
}

// ownedEnv returns, built using t, an env holding a provisioned m1 with
// UID uid (each of mut applied too), its readable two-chunk state read by
// the real reader, a state backup, durable inputs (marked applied), a plan
// key and a credential mirror shared with m2, every Secret owned by m1 as
// UID oldOwner (before a restore, or m1 itself). It also returns the
// counters behind its client and the names of the Secrets that belong to
// m1 alone.
func ownedEnv(t *testing.T, uid types.UID, oldOwner types.UID, mut ...func(*infrav1.TerraformMachine)) (*env, *ownerCalls, []string) {
	t.Helper()
	calls := &ownerCalls{}
	mut = append([]func(*infrav1.TerraformMachine){withFinalizer, notPaused, provisioned, idle, func(m *infrav1.TerraformMachine) { m.UID = uid }}, mut...)
	e := newEnvWith(t, calls.funcs(), world(machine(mut...))...)
	e.d.State = state.NewReader(e.c)
	old := machine(func(m *infrav1.TerraformMachine) { m.UID = oldOwner })
	ctx := t.Context()

	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	names := writeChunks(t, e, suffix, ownerRefTo(testName, oldOwner), 2)
	b, created, err := state.TakeBackup(ctx, e.c, state.BackupOptions{
		Owner: old, OwnerKind: state.KindTerraformMachine, ClusterName: "c1", Suffix: suffix, Now: t0,
	})
	if err != nil || !created {
		t.Fatalf("backup: %v, %v", created, err)
	}
	names = append(names, b.Secrets...)
	if err := inputs.Write(ctx, e.c, old, render.Files{MainTF: []byte("main"), TFVars: []byte("{}")}, inputs.Meta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
		t.Fatal(err)
	}
	if err := inputs.MarkApplied(ctx, e.c, old); err != nil {
		t.Fatal(err)
	}
	pk, err := plankey.Ensure(ctx, e.c, old)
	if err != nil {
		t.Fatal(err)
	}
	names = append(names, inputs.Name("m", testName), pk)

	data := map[string][]byte{"KEY": []byte("v")}
	mirrorRef := func(name string, uid types.UID) metav1.OwnerReference {
		r := ownerRefTo(name, uid)
		r.BlockOwnerDeletion = new(false)
		return r
	}
	mirror := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNS, Name: identity.MirrorName(testIdentity),
			Labels:          map[string]string{identity.MirroredLabel: "true", state.ManagedLabel: "true"},
			Annotations:     map[string]string{identity.SourceHashAnnotation: identity.SourceHash(data), inputs.IdentityAnnotation: testIdentity},
			OwnerReferences: []metav1.OwnerReference{mirrorRef(testName, oldOwner), mirrorRef("m2", "m2-uid")},
		},
		Data: data,
	}
	if err := e.c.Create(ctx, mirror); err != nil {
		t.Fatal(err)
	}
	calls.reset()
	return e, calls, names
}

// reset zeroes o's counters, so the setup's own calls do not count.
func (o *ownerCalls) reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.refPatches, o.updates, o.planKeyGets, o.backupListings, o.patched = 0, 0, 0, 0, nil
}

// idle records on m a refresh and a drift check at t0 (the fake clock's
// now), so a pass over a provisioned m1 starts no Job and the next pass
// reads the state again.
func idle(m *infrav1.TerraformMachine) {
	now := metav1.NewTime(t0)
	m.Status.LastRefresh, m.Status.LastDriftCheck = &now, &now
}

// writeChunks writes into e's client the state of suffix (serial 3,
// inputs hash h1:x) split over n chunk Secrets labeled for m1 as the
// backend labels them, each owned by ref, and returns their names,
// failing t on error.
func writeChunks(t *testing.T, e *env, suffix string, ref metav1.OwnerReference, n int) []string {
	t.Helper()
	payload := gzState(t, 3)
	size := (len(payload) + n - 1) / n
	var names []string
	for i := range n {
		s := stateSecret(t, suffix, 3, "h1:x")
		if i > 0 {
			s.Name += "-part-" + strconv.Itoa(i)
			s.Annotations = nil
		}
		for k, v := range state.BackendLabels(state.KindTerraformMachine, testName, "c1") {
			s.Labels[k] = v
		}
		s.Data[state.DataKey] = payload[min(i*size, len(payload)):min((i+1)*size, len(payload))]
		s.OwnerReferences = []metav1.OwnerReference{ref}
		if err := e.c.Create(t.Context(), s); err != nil {
			t.Fatal(err)
		}
		names = append(names, s.Name)
	}
	return names
}

// secretRefs returns the owner references of Secret name in e's client,
// failing t on error.
func secretRefs(t *testing.T, e *env, name string) []metav1.OwnerReference {
	t.Helper()
	var s corev1.Secret
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, &s); err != nil {
		t.Fatal(err)
	}
	return s.OwnerReferences
}

// setRefs replaces, in e's client, the owner references of each Secret of
// names with refs, failing t on error.
func setRefs(t *testing.T, e *env, refs []metav1.OwnerReference, names ...string) {
	t.Helper()
	for _, name := range names {
		var s corev1.Secret
		if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, &s); err != nil {
			t.Fatal(err)
		}
		s.OwnerReferences = refs
		if err := e.c.Update(t.Context(), &s); err != nil {
			t.Fatal(err)
		}
	}
}

// reconcileM1 reconciles e's m1 once with owner, failing t on error.
func reconcileM1(t *testing.T, e *env, owner OwnerInfo) {
	t.Helper()
	k := healthyKind(t, e, testName)
	k.owner = owner
	if _, err := Reconcile(t.Context(), e.d, k); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// TestReconcileRepairsOwnersAfterRestore proves one reconcile after a
// management-cluster restore re-owns every Secret of the object, whether
// the restore kept references to its old UID or stripped them: the state
// chunks, the backup, the durable inputs and the plan key carry exactly
// one reference to the new UID, and the shared mirror names the new UID
// while keeping m2's reference. It emits OwnerReferencesRepaired, and the
// next reconcile writes nothing.
func TestReconcileRepairsOwnersAfterRestore(t *testing.T) {
	t.Parallel()
	for _, stripped := range []bool{false, true} {
		name := map[bool]string{false: "old UID", true: "stripped"}[stripped]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, calls, owned := ownedEnv(t, newUID, oldUID)
			if stripped {
				setRefs(t, e, nil, owned...)
			}
			reconcileM1(t, e, readyOwner)
			want := []metav1.OwnerReference{ownerRefTo(testName, newUID)}
			for _, s := range owned {
				if got := secretRefs(t, e, s); !reflect.DeepEqual(got, want) {
					t.Errorf("%s: ownerRefs = %+v, want %+v", s, got, want)
				}
			}
			mirror := secretRefs(t, e, identity.MirrorName(testIdentity))
			var uids []types.UID
			for _, r := range mirror {
				uids = append(uids, r.UID)
			}
			if !reflect.DeepEqual(uids, []types.UID{newUID, "m2-uid"}) || mirror[0].BlockOwnerDeletion == nil || *mirror[0].BlockOwnerDeletion {
				t.Errorf("mirror ownerRefs = %+v", mirror)
			}
			events := e.rec.only(EventOwnerReferencesRepaired)
			if len(events) != 2 || !strings.Contains(events[0].note, identity.MirrorName(testIdentity)) ||
				!strings.Contains(events[1].note, "Owned 6 Secret(s)") || !strings.Contains(events[1].note, "state 2, backups 2, inputs 1, planKey 1") {
				t.Errorf("OwnerReferencesRepaired = %+v", events)
			}

			before, updates, _, _ := calls.snapshot()
			reconcileM1(t, e, readyOwner)
			if after, u, _, _ := calls.snapshot(); after != before || u != updates {
				t.Errorf("second reconcile: %d owner-reference patches, %d updates", after-before, u-updates)
			}
		})
	}
}

// TestReconcileOwnsLaterChunk proves a state chunk a refresh or drift Job
// wrote without an owner reference is owned on the next reconcile, and
// only that chunk is patched.
func TestReconcileOwnsLaterChunk(t *testing.T) {
	t.Parallel()
	e, calls, owned := ownedEnv(t, newUID, newUID)
	part := owned[1]
	setRefs(t, e, nil, part)
	reconcileM1(t, e, readyOwner)
	if got := secretRefs(t, e, part); !reflect.DeepEqual(got, []metav1.OwnerReference{ownerRefTo(testName, newUID)}) {
		t.Errorf("chunk ownerRefs = %+v", got)
	}
	calls.mu.Lock()
	defer calls.mu.Unlock()
	if !reflect.DeepEqual(calls.patched, []string{part}) {
		t.Errorf("patched %v, want only %s", calls.patched, part)
	}
}

// TestReconcileLeavesForeignSecrets proves Secrets found by m1's names and
// selectors but labeled for another object (a chunk, a backup and the plan
// key naming m2) are never claimed, while m1's own are repaired.
func TestReconcileLeavesForeignSecrets(t *testing.T) {
	t.Parallel()
	e, _, owned := ownedEnv(t, newUID, oldUID)
	// owned: chunk 0, chunk 1, backup chunks 0 and 1, durable, plan key.
	foreign := []string{owned[1], owned[3], owned[5]}
	for _, name := range foreign {
		var s corev1.Secret
		if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, &s); err != nil {
			t.Fatal(err)
		}
		s.Labels[state.OwnerNameLabel] = "m2"
		if err := e.c.Update(t.Context(), &s); err != nil {
			t.Fatal(err)
		}
	}
	reconcileM1(t, e, readyOwner)
	for _, name := range foreign {
		if got := secretRefs(t, e, name); !reflect.DeepEqual(got, []metav1.OwnerReference{ownerRefTo(testName, oldUID)}) {
			t.Errorf("foreign %s: ownerRefs = %+v, want untouched", name, got)
		}
	}
	for _, name := range []string{owned[0], owned[2], owned[4]} {
		if got := secretRefs(t, e, name); !reflect.DeepEqual(got, []metav1.OwnerReference{ownerRefTo(testName, newUID)}) {
			t.Errorf("own %s: ownerRefs = %+v", name, got)
		}
	}
}

// TestReconcileOwnedCostsNothing proves the repair adds no API call to a
// pass over an object whose Secrets are all owned: no owner-reference
// patch, no Secret update, no plan key read and no backup listing.
func TestReconcileOwnedCostsNothing(t *testing.T) {
	t.Parallel()
	e, calls, _ := ownedEnv(t, newUID, newUID)
	reconcileM1(t, e, readyOwner)
	if p, u, g, l := calls.snapshot(); p != 0 || u != 0 || g != 0 || l != 0 {
		t.Errorf("owner-reference patches %d, Secret updates %d, plan key reads %d, backup listings %d; want none", p, u, g, l)
	}
	if n := e.rec.count(EventOwnerReferencesRepaired); n != 0 {
		t.Errorf("OwnerReferencesRepaired events = %d", n)
	}
}

// TestReconcilePausedLeavesOwners proves a paused Cluster's object repairs
// nothing: clusterctl move rewrites the owner references itself.
func TestReconcilePausedLeavesOwners(t *testing.T) {
	t.Parallel()
	e, calls, owned := ownedEnv(t, newUID, oldUID)
	reconcileM1(t, e, OwnerInfo{HasOwnerRef: true, Cluster: cluster(true)})
	if p, u, g, l := calls.snapshot(); p != 0 || u != 0 || g != 0 || l != 0 {
		t.Errorf("paused: owner-reference patches %d, Secret updates %d, plan key reads %d, backup listings %d", p, u, g, l)
	}
	for _, name := range owned {
		if got := secretRefs(t, e, name); !reflect.DeepEqual(got, []metav1.OwnerReference{ownerRefTo(testName, oldUID)}) {
			t.Errorf("%s: ownerRefs = %+v, want untouched", name, got)
		}
	}
}

// TestReconcileChunksWaitForRunLease proves the state chunks' owner
// references wait while a live Job holds the run lease (the backend's own
// update of a chunk would conflict with the patch), while the object's
// other Secrets are repaired, and the chunks follow once the lease is
// gone.
func TestReconcileChunksWaitForRunLease(t *testing.T) {
	t.Parallel()
	e, _, owned := ownedEnv(t, newUID, oldUID)
	lease := foreignLease(runLeaseOf(t, state.KindTerraformMachine, testName), "captf-m-m1-drift-a9-000000", jobs.OpDrift, t0)
	if err := e.c.Create(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	reconcileM1(t, e, readyOwner)
	want := map[bool][]metav1.OwnerReference{true: {ownerRefTo(testName, oldUID)}, false: {ownerRefTo(testName, newUID)}}
	for i, name := range owned {
		if got := secretRefs(t, e, name); !reflect.DeepEqual(got, want[i < 2]) {
			t.Errorf("%s: ownerRefs = %+v, want %+v while the lease is live", name, got, want[i < 2])
		}
	}
	if err := e.c.Delete(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	reconcileM1(t, e, readyOwner)
	for _, name := range owned[:2] {
		if got := secretRefs(t, e, name); !reflect.DeepEqual(got, want[false]) {
			t.Errorf("%s: ownerRefs = %+v after the lease is gone", name, got)
		}
	}
}

// TestReconcileRepairFailureTolerated proves a conflict or a vanished
// Secret during the repair never fails the reconcile, and that the next
// reconcile, once the writes go through, finishes the repair.
func TestReconcileRepairFailureTolerated(t *testing.T) {
	t.Parallel()
	gr := schema.GroupResource{Resource: "secrets"}
	for name, failure := range map[string]error{
		"conflict":  apierrors.NewConflict(gr, "x", nil),
		"not found": apierrors.NewNotFound(gr, "x"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, calls, owned := ownedEnv(t, newUID, oldUID)
			calls.fail.Store(&failure)
			reconcileM1(t, e, readyOwner)
			if p, _, _, _ := calls.snapshot(); p == 0 {
				t.Fatal("no repair was attempted")
			}
			calls.fail.Store(nil)
			reconcileM1(t, e, readyOwner)
			for _, s := range owned {
				if got := secretRefs(t, e, s); !reflect.DeepEqual(got, []metav1.OwnerReference{ownerRefTo(testName, newUID)}) {
					t.Errorf("%s: ownerRefs = %+v after the retry", s, got)
				}
			}
		})
	}
}
