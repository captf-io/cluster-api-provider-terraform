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

package locks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Fixture identifiers shared by the table-driven cases below: a namespace,
// a backend suffix, a runner pod name owned by TerraformMachine web and a
// state lock ID.
const (
	ns     = "team-a"
	suffix = "0123456789abcdef-m"
	pod    = "captf-m-web-apply-a1-abc123-x7k2p"
	lockID = "9f1c0c5e-0000-4000-8000-000000000001"
)

// lease returns a Lease named for suffix in ns, with holder as its
// holderIdentity (or none when holder is empty) and info as its
// LockInfoAnnotation (or none when info is empty).
func lease(holder string, info string) *coordinationv1.Lease {
	l := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: state.LeaseName(suffix), Namespace: ns}}
	if holder != "" {
		l.Spec.HolderIdentity = &holder
	}
	if info != "" {
		l.Annotations = map[string]string{LockInfoAnnotation: info}
	}
	return l
}

// check builds a fake client seeded with objs, calls Check for the fixed ns
// and suffix with ownsWeb, fails t on error, and returns the resulting
// Status.
func check(t *testing.T, objs ...client.Object) Status {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).Build()
	st, err := Check(context.Background(), c, ns, suffix, ownsWeb)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return st
}

// ownsWeb recognizes the pods of TerraformMachine web's Jobs; p is the
// candidate pod name. It reports whether p belongs to one of web's Jobs.
func ownsWeb(p string) bool { return jobs.OwnsPod("m", "web", p) }

// TestCheckIgnoresRunLeases: the manager's run and cluster write leases
// (captf-run-<suffix>, captf-cluster-<hash>) are never mistaken for the
// state lock, whatever they hold: Check reads only the backend's Lease.
func TestCheckIgnoresRunLeases(t *testing.T) {
	t.Parallel()
	holder := "captf-m-web-apply-a1-abc123"
	run := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "captf-run-" + suffix, Labels: state.BackendLabels(state.KindTerraformMachine, "web", "c1")},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
	if st := check(t, run); st.Held || st.Stale() {
		t.Errorf("Check = %+v with only a run lease, want not held", st)
	}
}

// TestCheck exercises Check across held, released, stale and unknown-holder
// Lease states, checking the resulting Status.Held, Holder, HolderPodExists
// and Stale for each.
func TestCheck(t *testing.T) {
	t.Parallel()
	info := func(who string) string {
		return `{"ID":"` + lockID + `","Operation":"OperationTypeApply","Who":"` + who + `","Version":"1.16.4","Created":"2026-09-25T20:00:00Z"}`
	}
	livePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: ns}}
	withPhase := func(phase corev1.PodPhase) *corev1.Pod {
		p := livePod.DeepCopy()
		p.Status.Phase = phase
		return p
	}
	terminating := withPhase(corev1.PodRunning)
	terminating.DeletionTimestamp = &metav1.Time{}
	terminating.Finalizers = []string{"test"}
	cases := []struct {
		name   string
		objs   []client.Object
		held   bool
		holder string
		exists bool
		stale  bool
	}{
		{name: "no Lease", held: false},
		{name: "released (no holder)", objs: []client.Object{lease("", "")}, held: false},
		{name: "held, pod alive", objs: []client.Object{lease(lockID, info("runner@"+pod)), livePod}, held: true, holder: pod, exists: true},
		{name: "held, pod gone", objs: []client.Object{lease(lockID, info("runner@"+pod))}, held: true, holder: pod, stale: true},
		// A finished Job keeps its pod until pruned: an OOM-killed, evicted
		// or SIGKILLed runner leaves a Failed pod object behind, and its lock
		// must count as stale.
		{name: "held, pod Failed", objs: []client.Object{lease(lockID, info("runner@"+pod)), withPhase(corev1.PodFailed)}, held: true, holder: pod, stale: true},
		{name: "held, pod Succeeded", objs: []client.Object{lease(lockID, info("runner@"+pod)), withPhase(corev1.PodSucceeded)}, held: true, holder: pod, stale: true},
		{name: "held, pod Running", objs: []client.Object{lease(lockID, info("runner@"+pod)), withPhase(corev1.PodRunning)}, held: true, holder: pod, exists: true},
		// Terminating but still inside its grace period: the runtime may be
		// finishing in-flight calls, so the lock is live.
		{name: "held, pod terminating", objs: []client.Object{lease(lockID, info("runner@"+pod)), terminating}, held: true, holder: pod, exists: true},
		// user.Current fails in distroless: Who is "@<pod>".
		{name: "empty user", objs: []client.Object{lease(lockID, info("@"+pod))}, held: true, holder: pod, stale: true},
		{name: "no lock info", objs: []client.Object{lease(lockID, "")}, held: true},
		{name: "garbage lock info", objs: []client.Object{lease(lockID, "{")}, held: true},
		{name: "no hostname", objs: []client.Object{lease(lockID, info("runner@"))}, held: true},
		// A lock taken from a workstation, or by another object's runner,
		// is live as far as CAPTF can tell: never stale, even with no pod
		// of that name.
		{name: "workstation holder", objs: []client.Object{lease(lockID, info("steven@laptop"))}, held: true},
		{name: "another object's pod", objs: []client.Object{lease(lockID, info("runner@captf-m-api-apply-a1-abc123-x7k2p"))}, held: true},
		{name: "look-alike pod", objs: []client.Object{lease(lockID, info("runner@captf-m-web-apply-a1-abc123"))}, held: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			st := check(t, c.objs...)
			if st.Held != c.held || st.Holder != c.holder || st.HolderPodExists != c.exists || st.Stale() != c.stale {
				t.Errorf("Status = %+v, Stale %v", st, st.Stale())
			}
			if c.held && st.LockID != lockID {
				t.Errorf("LockID = %q", st.LockID)
			}
		})
	}
}

// fixtureDir is the backend fixture directory owned by internal/state; it
// is shared so the real captures exist once.
const fixtureDir = "../state/testdata/fixtures"

// fixtureLease reads the Lease of <fixtureDir>/<name>.yaml: real
// captures from Terraform 1.16.4 and OpenTofu 1.12.6
// (hack/fixtures-state.sh). It fails t if the fixture is missing, unreadable
// or holds no Lease document, and returns the decoded Lease.
func fixtureLease(t *testing.T, name string) *coordinationv1.Lease {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, name+".yaml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var l *coordinationv1.Lease
	for {
		var doc json.RawMessage
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode: %v", err)
		}
		var meta metav1.TypeMeta
		_ = json.Unmarshal(doc, &meta)
		if meta.Kind == "Lease" {
			l = &coordinationv1.Lease{}
			if err := json.Unmarshal(doc, l); err != nil {
				t.Fatalf("Lease: %v", err)
			}
		}
	}
	if l == nil {
		t.Fatalf("fixture %s has no Lease", name)
	}
	return l
}

// TestFixtureLease: the Lease a real apply leaves behind, and the one left
// after SIGTERM to a runtime waiting with the lock held, are released: both
// runtimes clear the holder on unlock.
func TestFixtureLease(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"terraform-single", "terraform-locked-stopped", "opentofu-locked-stopped"} {
		l := fixtureLease(t, name)
		c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(l).Build()
		st, err := Check(context.Background(), c, l.Namespace, l.Labels[state.BackendSuffixLabel], ownsWeb)
		if err != nil || st.Held || st.Stale() {
			t.Errorf("%s: Lease = %+v (err %v), want released", name, st, err)
		}
	}
}

// TestFixtureLeaseHeld: a Lease captured while a real apply held the lock.
// Who is "<user>@<hostname>" on both runtimes (the fixture pod is named
// captf-fixtures), holderIdentity equals the lock info's ID, and the lock
// is live while the holder pod exists and stale once it is gone.
func TestFixtureLeaseHeld(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, version string }{
		{"terraform-locked", "1.16.4"},
		{"opentofu-locked", "1.12.6"},
	} {
		l := fixtureLease(t, tt.name)
		var info Info
		if err := json.Unmarshal([]byte(l.Annotations[LockInfoAnnotation]), &info); err != nil {
			t.Fatalf("%s: lock info: %v", tt.name, err)
		}
		// The user half depends on the host that captured the fixture.
		if !strings.HasSuffix(info.Who, "@captf-fixtures") || info.Operation != "OperationTypeApply" || info.Version != tt.version ||
			l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != info.ID {
			t.Errorf("%s: lock info %+v, holder %v", tt.name, info, l.Spec.HolderIdentity)
		}
		s := l.Labels[state.BackendSuffixLabel]
		holderPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "captf-fixtures", Namespace: l.Namespace}}
		for _, alive := range []bool{true, false} {
			objs := []client.Object{l.DeepCopy()}
			if alive {
				objs = append(objs, holderPod.DeepCopy())
			}
			c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).Build()
			isFixture := func(p string) bool { return p == "captf-fixtures" }
			st, err := Check(context.Background(), c, l.Namespace, s, isFixture)
			want := Status{Held: true, LockID: info.ID, Holder: "captf-fixtures", HolderPodExists: alive}
			if err != nil || st != want || st.Stale() == alive {
				t.Errorf("%s (holder alive %v): %+v (err %v), want %+v", tt.name, alive, st, err, want)
			}
			// The real Job-pod predicate does not claim the fixture pod.
			st, err = Check(context.Background(), c, l.Namespace, s, ownsWeb)
			if err != nil || st.Holder != "" || st.Stale() {
				t.Errorf("%s (holder alive %v, not ours): %+v (err %v), want unknown holder", tt.name, alive, st, err)
			}
		}
	}
}

// TestCheckErrors checks that Check propagates a Get failure on either the
// Lease or the holder Pod as a wrapped error.
func TestCheckErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	failGet := func(kind string) client.Client {
		return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
			WithObjects(lease(lockID, `{"ID":"x","Who":"u@`+pod+`"}`)).
			WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isPod := obj.(*corev1.Pod); isPod == (kind == "pod") {
					return boom
				}
				return c.Get(ctx, key, obj, opts...)
			}}).Build()
	}
	for _, kind := range []string{"lease", "pod"} {
		if _, err := Check(context.Background(), failGet(kind), ns, suffix, ownsWeb); !errors.Is(err, boom) {
			t.Errorf("%s Get failure: err = %v", kind, err)
		}
	}
}
