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
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// writeDurable writes m's durable inputs with an image digest pinned,
// as a successful apply leaves them, failing t on error.
func (e *env) writeDurable(t *testing.T, m *infrav1.TerraformMachine) {
	t.Helper()
	if err := writeInputs(t.Context(), e.c, m, renderMachine(t), testMeta{
		Image: "registry.example/mod:1.0", Identity: testIdentity, ImageDigest: "registry.example/mod@sha256:abc",
	}); err != nil {
		t.Fatal(err)
	}
}

// writeApplied writes m's durable inputs carrying the applied marker but no
// pinned digest, as an apply whose digest was unknown, or an image change
// since, leaves them, failing t on error.
func (e *env) writeApplied(t *testing.T, m *infrav1.TerraformMachine) {
	t.Helper()
	if err := writeInputs(t.Context(), e.c, m, renderMachine(t), testMeta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
		t.Fatal(err)
	}
	if err := inputs.MarkApplied(t.Context(), e.c, m); err != nil {
		t.Fatal(err)
	}
}

// applied reports whether e's machine's durable inputs carry the applied
// marker, failing t on a read error.
func (e *env) applied(t *testing.T) bool {
	t.Helper()
	d, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil {
		t.Fatal(err)
	}
	return d.AppliedMark
}

// TestAppliedMarker: the durable Secret is marked applied at a successful
// apply whose image digest is unknown, and when a state with an inputs
// hash reads (a restore, or an object that applied before the marker); a
// moved object (no provisioned status) whose state is then missing holds
// with StateLost instead of applying from scratch.
func TestAppliedMarker(t *testing.T) {
	t.Parallel()
	t.Run("a successful apply without a digest", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(withFinalizer, notPaused))...)
		k := e.kindFor(t, readyOwner())
		if err := writeInputs(t.Context(), e.c, k.obj, renderMachine(t), testMeta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
			t.Fatal(err)
		}
		e.runner.jobs = append(e.runner.jobs, job(seedJob, jobs.OpApply, jobs.Succeeded, t0.Add(-time.Minute)))
		e.state.st = &state.State{Serial: 1}
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		if !e.applied(t) {
			t.Error("a successful apply left no applied marker")
		}
		if d, err := inputs.Read(t.Context(), e.c, testNS, "m", testName); err != nil || d.Applied == nil || d.Applied.Job != seedJob || d.Applied.Digest != "" {
			t.Errorf("records = %+v, %v; want the apply promoted without a digest", d, err)
		}
	})
	t.Run("a state with an inputs hash", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(withFinalizer, notPaused))...)
		k := e.kindFor(t, readyOwner())
		if err := writeInputs(t.Context(), e.c, k.obj, renderMachine(t), testMeta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
			t.Fatal(err)
		}
		e.state.st = &state.State{Serial: 4, InputsHash: "h1:restored"}
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		if !e.applied(t) {
			t.Error("a readable applied state left no applied marker")
		}
	})
	t.Run("a moved object with its state missing holds", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(withFinalizer, notPaused))...)
		e.writeApplied(t, e.get(t))
		k := e.kindFor(t, readyOwner())
		k.in = machineIn()
		requeue, err := reconcileOnce(t, e, k)
		if err != nil {
			t.Fatal(err)
		}
		c := conditions.Get(e.get(t), infrav1.StateReadableCondition)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.StateLostReason || requeue != StateRequeue || len(e.runner.created) != 0 {
			t.Errorf("StateReadable = %+v, requeue %s, created %v", c, requeue, e.runner.created)
		}
	})
}

// annotate sets the annotation key to value on the stored machine,
// failing t on error.
func (e *env) annotate(t *testing.T, key, value string) {
	t.Helper()
	m := e.get(t)
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	m.Annotations[key] = value
	if err := e.c.Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
}

// assertHeld fails t unless e's machine still exists with its finalizer,
// StateReadable False with reason, no Job started, and requeue is
// StateRequeue.
func assertHeld(t *testing.T, e *env, requeue time.Duration, reason string) {
	t.Helper()
	m := e.get(t)
	if m == nil || !slices.Contains(m.Finalizers, testFinal) {
		t.Fatalf("the finalizer was dropped: %+v", m)
	}
	c := conditions.Get(m, infrav1.StateReadableCondition)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reason || !strings.Contains(c.Message, "Deletion is held") {
		t.Errorf("StateReadable = %+v", c)
	}
	if requeue != StateRequeue || len(e.jobsOf(t)) != 0 {
		t.Errorf("requeue %s, Jobs %d", requeue, len(e.jobsOf(t)))
	}
}

// heldEnv returns, failing t on error, a deleting, provisioned machine m1
// whose state was lost after a backup of serial 5, with each mut applied;
// Jobs live in the fake client, the state reader is the real one.
func heldEnv(t *testing.T, mut ...func(*infrav1.TerraformMachine)) *env {
	t.Helper()
	e, _ := restoreEnv(t, "", append([]func(*infrav1.TerraformMachine){deleting}, mut...)...)
	return e
}

// TestReconcileLostStateOnDelete: a deleting object whose state is gone
// keeps its finalizer (and so its backups) when it ever applied, known
// from the provisioned status, a pinned digest or a backup; one that never
// applied drops it at once.
func TestReconcileLostStateOnDelete(t *testing.T) {
	t.Parallel()
	t.Run("never applied: finalizer dropped", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused))...)
		e.d.Jobs = &clientRunner{c: e.c}
		e.d.State = state.NewReader(e.c)
		if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
			t.Fatal(err)
		}
		if m := e.get(t); m != nil {
			t.Errorf("object kept: %+v", m.Finalizers)
		}
	})
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T) *env
	}{
		{"provisioned", func(t *testing.T) *env { return heldEnv(t) }},
		{"digest pinned, status lost by a move", func(t *testing.T) *env {
			e := newEnv(t, world(machine(deleting, notPaused))...)
			e.d.Jobs, e.d.State = &clientRunner{c: e.c}, state.NewReader(e.c)
			e.writeDurable(t, e.get(t))
			return e
		}},
		{"applied marker only, status lost by a move", func(t *testing.T) *env {
			e := newEnv(t, world(machine(deleting, notPaused))...)
			e.d.Jobs, e.d.State = &clientRunner{c: e.c}, state.NewReader(e.c)
			e.writeApplied(t, e.get(t))
			return e
		}},
		{"a backup exists, status lost by a move", func(t *testing.T) *env {
			e := newEnv(t, world(machine(deleting, notPaused))...)
			e.d.Jobs, e.d.State = &clientRunner{c: e.c}, state.NewReader(e.c)
			e.backupOf(t, state.KindTerraformMachine, testName, 5, "h1:backup")
			return e
		}},
	} {
		t.Run(tt.name+": held", func(t *testing.T) {
			t.Parallel()
			e := tt.setup(t)
			for range 2 {
				res, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName))
				if err != nil {
					t.Fatal(err)
				}
				assertHeld(t, e, res.RequeueAfter, infrav1.StateLostReason)
			}
			if n := len(e.rec.only(EventStateLost)); n != 1 || e.rec.only(EventStateLost)[0].eventType != corev1.EventTypeWarning {
				t.Errorf("StateLost events = %d, want one Warning", n)
			}
		})
	}
}

// TestReconcileUnreadableStateOnDelete: a deleting object whose state
// exists but cannot be read holds instead of starting a destroy that
// cannot read it.
func TestReconcileUnreadableStateOnDelete(t *testing.T) {
	t.Parallel()
	for _, err := range []error{state.ErrStateEncrypted, state.ErrStateCorrupt, state.ErrStateInconsistent} {
		e := newEnv(t, world(machine(deleting, notPaused, provisioned))...)
		e.d.Jobs = &clientRunner{c: e.c}
		e.writeDurable(t, e.get(t))
		e.state.err = err
		res, rerr := Reconcile(t.Context(), e.d, healthyKind(t, e, testName))
		if rerr != nil {
			t.Fatal(rerr)
		}
		reason := conditions.Get(e.get(t), infrav1.StateReadableCondition).Reason
		assertHeld(t, e, res.RequeueAfter, reason)
		if reason == infrav1.StateLostReason || reason == infrav1.StateReadReason {
			t.Errorf("%v: StateReadable reason %s", err, reason)
		}
	}
}

// TestReconcileHeldDeletionRestores: a held deletion restores the backup
// the restore annotation names, and destroys once the restored state
// reads.
func TestReconcileHeldDeletionRestores(t *testing.T) {
	t.Parallel()
	e := heldEnv(t)
	e.writeDurable(t, e.get(t))
	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	if backups, err := state.ListBackups(t.Context(), e.c, testNS, suffix); err != nil || len(backups) != 1 {
		t.Fatalf("backups %v, %v", backups, err)
	}

	e.annotate(t, infrav1.RestoreStateAnnotation, "5")
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	restores := restoreJobs(t, e)
	if len(restores) != 1 || e.get(t) == nil {
		t.Fatalf("restores %d", len(restores))
	}

	e.finishJob(t, restores[0].Name, jobs.Succeeded)
	e.setState(t, suffix, 5, "")
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	var destroys int
	for _, j := range e.jobsOf(t) {
		if jobs.OpOf(&j) == jobs.OpDestroy {
			destroys++
		}
	}
	m := e.get(t)
	if destroys != 1 || m == nil || m.Annotations[infrav1.RestoreStateAnnotation] != "" {
		t.Errorf("destroys %d, object %+v", destroys, m)
	}
}

// setRetain sets the stored machine's deletionPolicy to Retain, failing t
// on error.
func (e *env) setRetain(t *testing.T) {
	t.Helper()
	m := e.get(t)
	m.Spec.DeletionPolicy = infrav1.DeletionPolicyRetain
	if err := e.c.Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
}

// retainedWith fails t unless e's machine is gone with no Job started
// and exactly one InfrastructureRetained event.
func retainedWith(t *testing.T, e *env) {
	t.Helper()
	if m := e.get(t); m != nil || len(e.runner.created) != 0 || len(e.jobsOf(t)) != 0 {
		t.Fatalf("object %+v, created %v", m, e.runner.created)
	}
	if n := e.rec.count(EventInfrastructureRetained); n != 1 {
		t.Errorf("InfrastructureRetained events = %d, want 1 (%v)", n, e.rec.reasons)
	}
}

// TestReconcileHeldDeletionNamesRetain: a deletion held on a lost state
// says how to release it with deletionPolicy Retain, and setting it does.
func TestReconcileHeldDeletionNamesRetain(t *testing.T) {
	t.Parallel()
	e := heldEnv(t)
	res, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName))
	if err != nil {
		t.Fatal(err)
	}
	assertHeld(t, e, res.RequeueAfter, infrav1.StateLostReason)
	if c := conditions.Get(e.get(t), infrav1.StateReadableCondition); !strings.Contains(c.Message, "spec.deletionPolicy: Retain") {
		t.Errorf("StateReadable message %q does not name deletionPolicy Retain", c.Message)
	}
	e.setRetain(t)
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	if m := e.get(t); m != nil || e.rec.count(EventInfrastructureRetained) != 1 {
		t.Errorf("object %+v, events %v", m, e.rec.reasons)
	}
}

// TestReconcileDestroyCannotStartNamesRetain: a deletion whose state reads
// but whose destroy can never start (the durable inputs are gone, the
// identity does not allow the namespace or is gone, or the credentials
// cannot be created) says how to release it with deletionPolicy Retain,
// and setting it releases it without a Job.
func TestReconcileDestroyCannotStartNamesRetain(t *testing.T) {
	t.Parallel()
	disallowed := func() []client.Object {
		objs := world(machine(deleting, notPaused, provisioned))
		objs[1].(*infrav1.TerraformClusterIdentity).Spec.AllowedNamespaces = nil
		return objs
	}
	for _, tt := range []struct {
		name string
		objs []client.Object
		// durable writes the durable inputs Secret.
		durable bool
		// stop is the ApplyJobSucceeded reason the first pass leaves.
		stop string
	}{
		{"durable inputs missing", world(machine(deleting, notPaused, provisioned)), false, infrav1.DestroyFailedReason},
		{"identity does not allow the namespace", disallowed(), true, infrav1.IdentityNotAllowedReason},
		{"identity deleted", without[*infrav1.TerraformClusterIdentity](world(machine(deleting, notPaused, provisioned))), true, infrav1.IdentityNotAllowedReason},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, tt.objs...)
			if tt.durable {
				e.writeDurable(t, e.get(t))
			}
			e.state.st = &state.State{Serial: 3, InputsHash: "h1:x"}
			if _, err := reconcileOnce(t, e, e.kindFor(t, readyOwner())); err != nil {
				t.Fatal(err)
			}
			m := e.get(t)
			keepsFinalizer(t, m)
			c := conditions.Get(m, infrav1.ApplyJobSucceededCondition)
			if c == nil || c.Status != metav1.ConditionFalse || c.Reason != tt.stop {
				t.Fatalf("ApplyJobSucceeded = %+v, want False/%s", c, tt.stop)
			}
			if !strings.Contains(c.Message, "spec.deletionPolicy: Retain") {
				t.Errorf("ApplyJobSucceeded message %q does not name deletionPolicy Retain", c.Message)
			}
			e.setRetain(t)
			if _, err := reconcileOnce(t, e, e.kindFor(t, readyOwner())); err != nil {
				t.Fatal(err)
			}
			retainedWith(t, e)
		})
	}
	t.Run("credentials that cannot be created", func(t *testing.T) {
		t.Parallel()
		e, closed := terminatingEnv(t, world(machine(deleting, notPaused, provisioned))...)
		e.writeDurable(t, e.get(t))
		e.setState(t, suffixOf(t, state.KindTerraformMachine, testName), 3, "h1:x")
		closed.Store(true)
		if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
			t.Fatal(err)
		}
		if c := conditions.Get(e.get(t), clusterv1.DeletingCondition); c == nil || !strings.Contains(c.Message, "spec.deletionPolicy: Retain") {
			t.Errorf("Deleting = %+v, want it to name deletionPolicy Retain", c)
		}
		e.setRetain(t)
		if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
			t.Fatal(err)
		}
		if m := e.get(t); m != nil || len(e.jobsOf(t)) != 0 || e.rec.count(EventInfrastructureRetained) != 1 {
			t.Errorf("object %+v, events %v", m, e.rec.reasons)
		}
	})
	t.Run("a destroy that can start is not skipped without Retain", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused, provisioned))...)
		e.writeDurable(t, e.get(t))
		e.state.st = &state.State{Serial: 3, InputsHash: "h1:x"}
		if _, err := reconcileOnce(t, e, e.kindFor(t, readyOwner())); err != nil {
			t.Fatal(err)
		}
		if len(e.runner.created) != 1 || !strings.Contains(e.runner.created[0], "-destroy-") || e.get(t) == nil {
			t.Errorf("created %v", e.runner.created)
		}
	})
}

// terminatingEnv returns an env holding objs whose fake client, once the
// returned flag is set, refuses every create in testNS with Forbidden, as
// NamespaceLifecycle does in a terminating namespace; Jobs live in the
// fake client, the state reader is the real one. It uses t for setup.
func terminatingEnv(t *testing.T, objs ...client.Object) (*env, *atomic.Bool) {
	t.Helper()
	var closed atomic.Bool
	e := newEnvWith(t, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if closed.Load() && obj.GetNamespace() == testNS {
			return terminatingErr(obj.GetName())
		}
		return c.Create(ctx, obj, opts...)
	}}, objs...)
	e.d.Jobs, e.d.State = &clientRunner{c: e.c}, state.NewReader(e.c)
	return e, &closed
}

// terminatingErr returns the error NamespaceLifecycle refuses a create of
// name in testNS with while the namespace is terminating, its
// NamespaceTerminating cause included.
func terminatingErr(name string) error {
	err := apierrors.NewForbidden(schema.GroupResource{Resource: "objects"}, name,
		errors.New("unable to create new content in namespace "+testNS+" because it is being terminated"))
	err.ErrStatus.Details.Causes = append(err.ErrStatus.Details.Causes, metav1.StatusCause{
		Type: corev1.NamespaceTerminatingCause, Message: "namespace " + testNS + " is being terminated", Field: "metadata.namespace",
	})
	return err
}

// TestDeletionInTerminatingNamespace: in a terminating namespace, where
// the runner ServiceAccount, RoleBinding and mirror cannot be created, a
// deletion that needs no Job still drops the finalizer (no state, or
// retained), and a destroy waits on the Deleting condition instead of
// failing the reconcile.
func TestDeletionInTerminatingNamespace(t *testing.T) {
	t.Parallel()
	t.Run("never applied: finalizer dropped", func(t *testing.T) {
		t.Parallel()
		e, closed := terminatingEnv(t, world(machine(deleting, notPaused))...)
		closed.Store(true)
		if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
			t.Fatal(err)
		}
		if m := e.get(t); m != nil {
			t.Errorf("object kept: %+v", m.Finalizers)
		}
	})
	t.Run("held and retained: finalizer dropped", func(t *testing.T) {
		t.Parallel()
		e, closed := terminatingEnv(t, world(machine(deleting, notPaused, provisioned, retainPolicy))...)
		e.backupOf(t, state.KindTerraformMachine, testName, 5, "h1:backup")
		closed.Store(true)
		if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
			t.Fatal(err)
		}
		if m := e.get(t); m != nil || e.rec.count(EventInfrastructureRetained) != 1 {
			t.Errorf("object %+v, events %v", m, e.rec.reasons)
		}
	})
	t.Run("a destroy waits for its credentials", func(t *testing.T) {
		t.Parallel()
		e, closed := terminatingEnv(t, world(machine(deleting, notPaused, provisioned))...)
		e.writeDurable(t, e.get(t))
		e.setState(t, suffixOf(t, state.KindTerraformMachine, testName), 3, "h1:x")
		closed.Store(true)
		for range 2 {
			res, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName))
			if err != nil {
				t.Fatal(err)
			}
			if res.RequeueAfter != GateRequeue {
				t.Errorf("requeue %s, want %s", res.RequeueAfter, GateRequeue)
			}
		}
		m := e.get(t)
		keepsFinalizer(t, m)
		if n := len(e.jobsOf(t)); n != 0 {
			t.Errorf("%d Jobs started", n)
		}
		c := conditions.Get(m, clusterv1.DeletingCondition)
		if c == nil || c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "destroy Job waits for its credentials") ||
			!strings.Contains(c.Message, "being terminated") {
			t.Errorf("Deleting = %+v", c)
		}
	})
	t.Run("ready credentials: the destroy waits, saying why", func(t *testing.T) {
		t.Parallel()
		// The runner RBAC and mirror exist already; only the run lease,
		// which the destroy takes first, is refused.
		e := newEnvWith(t, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*coordinationv1.Lease); ok {
				return terminatingErr(obj.GetName())
			}
			return c.Create(ctx, obj, opts...)
		}}, world(machine(deleting, notPaused, provisioned))...)
		e.d.Jobs, e.d.State = &clientRunner{c: e.c}, state.NewReader(e.c)
		e.writeDurable(t, e.get(t))
		e.setState(t, suffixOf(t, state.KindTerraformMachine, testName), 3, "h1:x")
		res, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName))
		if err != nil || res.RequeueAfter != GateRequeue {
			t.Fatalf("Reconcile = %+v, %v; want no error and a requeue at %s", res, err, GateRequeue)
		}
		m := e.get(t)
		keepsFinalizer(t, m)
		c := conditions.Get(m, clusterv1.DeletingCondition)
		if c == nil || !strings.Contains(c.Message, "is terminating, so the destroy Job cannot be created") || !strings.Contains(c.Message, retainHint) {
			t.Errorf("Deleting = %+v", c)
		}
	})
}
