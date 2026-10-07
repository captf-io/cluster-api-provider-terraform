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
	"sync/atomic"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/rbac"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// completeJobs marks every Job the runner of e holds as succeeded, so the
// next pass finds no active Job. It returns nothing.
func completeJobs(e *env) {
	for i := range e.runner.jobs {
		e.runner.jobs[i].Status.Conditions = []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(t0),
		}}
	}
}

// TestCredentialErrorKeepsBookkeeping proves a persistent credential
// failure does not stop a finished apply Job from being bookkept (adopted,
// provisioned latched), while the error still surfaces for a backoff retry.
func TestCredentialErrorKeepsBookkeeping(t *testing.T) {
	t.Parallel()
	suffix, err := state.Suffix(testNS, state.KindTerraformMachine, testName)
	if err != nil {
		t.Fatal(err)
	}
	base := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: state.SecretName(suffix), Labels: map[string]string{
		state.BackendStateLabel: "true", state.BackendSuffixLabel: suffix, state.BackendWorkspaceLabel: state.Workspace,
	}}}
	boom := errors.New("boom")
	e := newEnvWith(t, interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.ServiceAccount); ok {
			return boom
		}
		return cl.Get(ctx, key, obj, opts...)
	}}, world(machine(withFinalizer, notPaused), base)...)
	applied := job("a", jobs.OpApply, jobs.Succeeded, t0)
	applied.Annotations = map[string]string{state.InputsHashAnnotation: "h1:applied"}
	e.runner.jobs = append(e.runner.jobs, applied)
	e.state.st = &state.State{Serial: 7}
	k := e.kindFor(t, readyOwner)
	k.in = machineIn()
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	if _, err := reconcileOnce(t, e, k); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the credential error", err)
	}
	m := e.get(t)
	if m.Status.Initialization.Provisioned == nil || !*m.Status.Initialization.Provisioned {
		t.Errorf("provisioned not latched: %+v", m.Status)
	}
	if c := conditions.Get(m, infrav1.RunnerRBACReadyCondition); c == nil || c.Reason != infrav1.RBACFailedReason {
		t.Errorf("RunnerRBACReady = %+v, want RBACFailed", c)
	}
	if len(e.runner.created) != 0 {
		t.Errorf("created %v, want no Job", e.runner.created)
	}
}

// TestCredentialsSkippedWhileJobActive proves a pass with a running Job
// makes no credential calls and leaves the credential conditions as they
// were, and that an idle pass with the RBAC in place makes no
// ServiceAccount Create.
func TestCredentialsSkippedWhileJobActive(t *testing.T) {
	t.Parallel()
	var saGets, saCreates, rbGets atomic.Int32
	e := newEnvWith(t, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			switch obj.(type) {
			case *corev1.ServiceAccount:
				saGets.Add(1)
			case *rbacv1.RoleBinding:
				rbGets.Add(1)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.ServiceAccount); ok {
				saCreates.Add(1)
			}
			return cl.Create(ctx, obj, opts...)
		},
	}, world(machine(withFinalizer, notPaused))...)
	reconcileMachine(t, e, readyOwner) // starts the apply; creates the RBAC
	if saCreates.Load() != 1 || len(e.runner.created) != 1 {
		t.Fatalf("SA creates %d, Jobs %v", saCreates.Load(), e.runner.created)
	}
	// The Job is running: no credential call.
	e.runner.jobs[0].Labels[jobs.AttemptLabel] = "1"
	gets, rbs := saGets.Load(), rbGets.Load()
	reconcileMachine(t, e, readyOwner)
	if saGets.Load() != gets || rbGets.Load() != rbs || saCreates.Load() != 1 {
		t.Errorf("credential calls while a Job runs: SA gets %d->%d, RB gets %d->%d", gets, saGets.Load(), rbs, rbGets.Load())
	}
	for _, ct := range []string{infrav1.IdentityAllowedCondition, infrav1.CredentialsMirroredCondition, infrav1.RunnerRBACReadyCondition} {
		if c := conditions.Get(e.get(t), ct); c == nil || c.Status != metav1.ConditionTrue {
			t.Errorf("%s = %+v, want it kept True", ct, c)
		}
	}
	// Idle again: credentials run, and the existing ServiceAccount is not created again.
	completeJobs(e)
	reconcileMachine(t, e, readyOwner)
	if saGets.Load() == gets || saCreates.Load() != 1 {
		t.Errorf("idle pass: SA gets %d (was %d), creates %d, want the RBAC read and no new Create", saGets.Load(), gets, saCreates.Load())
	}
}

// TestConcurrentFirstUseOfNamespace proves a RoleBinding create that
// answers AlreadyExists and an update that conflicts once, as when several
// objects first use a namespace together, end with RunnerRBACReady True and
// no Warning event.
func TestConcurrentFirstUseOfNamespace(t *testing.T) {
	t.Parallel()
	gr := schema.GroupResource{Group: rbacv1.GroupName, Resource: "rolebindings"}
	for _, tt := range []struct {
		name string
		objs []client.Object
	}{
		{"create races", nil},
		{"update conflicts", []client.Object{&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: rbac.RoleBinding, Labels: map[string]string{state.ManagedLabel: "true"}},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: rbac.ClusterRole},
		}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var raced, conflicted atomic.Bool
			e := newEnvWith(t, interceptor.Funcs{
				Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if rb, ok := obj.(*rbacv1.RoleBinding); ok && raced.CompareAndSwap(false, true) {
						if err := cl.Create(ctx, rb.DeepCopy(), opts...); err != nil {
							return err
						}
						return apierrors.NewAlreadyExists(gr, rb.Name)
					}
					return cl.Create(ctx, obj, opts...)
				},
				Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*rbacv1.RoleBinding); ok && conflicted.CompareAndSwap(false, true) {
						return apierrors.NewConflict(gr, obj.GetName(), nil)
					}
					return cl.Update(ctx, obj, opts...)
				},
			}, append(world(machine(withFinalizer, notPaused)), tt.objs...)...)
			reconcileMachine(t, e, readyOwner)
			if c := conditions.Get(e.get(t), infrav1.RunnerRBACReadyCondition); c == nil || c.Status != metav1.ConditionTrue {
				t.Errorf("RunnerRBACReady = %+v, want True", c)
			}
			for _, ev := range e.rec.events {
				if ev.eventType == corev1.EventTypeWarning {
					t.Errorf("Warning event %s: %s", ev.reason, ev.note)
				}
			}
			if len(e.runner.created) != 1 {
				t.Errorf("created %v, want one Job", e.runner.created)
			}
		})
	}
}

// TestDeletedIdentityRevokesMirror proves a TerraformClusterIdentity that is
// gone has its mirror removed (and MirrorRemoved emitted) once, while an
// identity of kind Secret is left alone.
func TestDeletedIdentityRevokesMirror(t *testing.T) {
	t.Parallel()
	mirrorKey := client.ObjectKey{Namespace: testNS, Name: identity.MirrorName(testIdentity)}
	t.Run("cluster identity", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(withFinalizer, notPaused))...)
		reconcileMachine(t, e, readyOwner)
		if err := e.c.Get(t.Context(), mirrorKey, &corev1.Secret{}); err != nil {
			t.Fatalf("mirror: %v", err)
		}
		if err := e.c.Delete(t.Context(), &infrav1.TerraformClusterIdentity{ObjectMeta: metav1.ObjectMeta{Name: testIdentity}}); err != nil {
			t.Fatal(err)
		}
		completeJobs(e)
		reconcileMachine(t, e, readyOwner)
		reconcileMachine(t, e, readyOwner)
		if err := e.c.Get(t.Context(), mirrorKey, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Errorf("mirror: %v, want not found", err)
		}
		if n := e.rec.count(EventMirrorRemoved); n != 1 {
			t.Errorf("MirrorRemoved = %d (%v)", n, e.rec.reasons)
		}
		if c := conditions.Get(e.get(t), infrav1.IdentityAllowedCondition); c == nil || c.Reason != infrav1.IdentityNotFoundReason {
			t.Errorf("IdentityAllowed = %+v", c)
		}
	})
	t.Run("secret identity", func(t *testing.T) {
		t.Parallel()
		const secretName = testIdentity
		local := machine(withFinalizer, notPaused, func(m *infrav1.TerraformMachine) {
			m.Spec.IdentityRef = infrav1.IdentityReference{Name: secretName, Kind: infrav1.IdentityKindSecret}
		})
		// A Secret that merely has the name a mirror would have stays.
		bystander := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: identity.MirrorName(secretName)}}
		creds := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: secretName}, Data: map[string][]byte{"KEY": []byte("v")}}
		e := newEnv(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS}}, creds, bystander, local)
		reconcileMachine(t, e, readyOwner)
		if err := e.c.Get(t.Context(), mirrorKey, &corev1.Secret{}); err != nil {
			t.Errorf("Secret: %v, want it untouched", err)
		}
		if n := e.rec.count(EventMirrorRemoved); n != 0 {
			t.Errorf("MirrorRemoved = %d", n)
		}
	})
}
