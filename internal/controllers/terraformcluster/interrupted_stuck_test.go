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

package terraformcluster

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runlease"
)

// errLostWrite is the status write the tests below lose.
var errLostWrite = errors.New("status write lost")

// stuckApply starts an apply Job of a version roll in e and deletes its
// per-run Secret, so the Job can never start, and returns it.
func (e *clusterEnv) stuckApply() *batchv1.Job {
	e.t.Helper()
	e.setVersion("v1.37.0")
	j := e.reconcileStarts()
	if err := e.c.Delete(e.t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: inputs.RunName(j.Name)}}); err != nil {
		e.t.Fatal(err)
	}
	return j
}

// failStatusPatches makes every status patch of the cluster fail from now
// on while fail reports true, through a client that wraps e's.
func (e *clusterEnv) failStatusPatches(fail *atomic.Bool) {
	e.t.Helper()
	base, ok := e.c.(client.WithWatch)
	if !ok {
		e.t.Fatalf("client %T cannot be wrapped", e.c)
	}
	e.r.Deps.Client = interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if fail.Load() {
				return errLostWrite
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
}

// TestStuckJobDeleteLostStatus: a stuck Job the controller deletes is not
// recorded as a vanished apply when the pass's own status write is lost
// after the delete (a crash, lost leadership or a failed patch): the
// status naming it was cleared on the API server before the delete. When
// that clearing fails, the Job is not deleted at all.
func TestStuckJobDeleteLostStatus(t *testing.T) {
	t.Parallel()
	t.Run("status write lost after the delete", func(t *testing.T) {
		t.Parallel()
		e := newClusterEnv(t)
		j := e.stuckApply()
		var fail atomic.Bool
		e.failStatusPatches(&fail)
		e.runner.onDelete = func() { fail.Store(true) }
		if _, err := e.r.Reconcile(t.Context(), e.req); !errors.Is(err, errLostWrite) {
			t.Fatalf("reconcile = %v, want the lost status write", err)
		}
		if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(j), &batchv1.Job{}); !apierrors.IsNotFound(err) {
			t.Fatalf("the stuck Job was not deleted: %v", err)
		}
		fail.Store(false)
		e.setVersion("v1.36.2")
		e.clock.SetTime(e.clock.Now().Add(2 * runlease.Grace))
		e.reconcileNoApply()
		if got := e.interrupted(); got != "" {
			t.Errorf("interrupted apply recorded for a stuck Job whose status write was lost: %q", got)
		}
	})
	t.Run("status clearing fails", func(t *testing.T) {
		t.Parallel()
		e := newClusterEnv(t)
		j := e.stuckApply()
		var fail atomic.Bool
		fail.Store(true)
		e.failStatusPatches(&fail)
		if _, err := e.r.Reconcile(t.Context(), e.req); !errors.Is(err, errLostWrite) {
			t.Fatalf("reconcile = %v, want the lost status write", err)
		}
		if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(j), &batchv1.Job{}); err != nil {
			t.Errorf("the stuck Job was deleted though status.activeJob was not cleared: %v", err)
		}
	})
}
