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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestReconcileNotReadyRequeue proves the source Secret is re-read within
// NotReadyRequeueAfter while it is missing or incomplete (it cannot be
// watched), within RequeueAfter when that is shorter, and within
// RequeueAfter once Ready is True.
func TestReconcileNotReadyRequeue(t *testing.T) {
	t.Parallel()
	incomplete := sourceSecret()
	for _, tc := range []struct {
		name   string
		source *corev1.Secret
		every  time.Duration
		keys   []string
		want   time.Duration
	}{
		{"missing", nil, time.Minute, nil, NotReadyRequeueAfter},
		{"missing, default period", nil, 0, nil, NotReadyRequeueAfter},
		{"missing, shorter period", nil, 10 * time.Second, nil, 10 * time.Second},
		{"incomplete", incomplete, time.Minute, []string{"A"}, NotReadyRequeueAfter},
		{"ready", sourceSecret(), time.Minute, nil, time.Minute},
		{"ready, default period", sourceSecret(), 0, nil, DefaultRequeueAfter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var src *corev1.Secret
			if tc.source != nil {
				src = tc.source.DeepCopy()
			}
			e := newEnv(t, src)
			e.r.RequeueAfter = tc.every
			if tc.keys != nil {
				id := &infrav1.TerraformClusterIdentity{}
				if err := e.c.Get(t.Context(), client.ObjectKey{Name: idName}, id); err != nil {
					t.Fatal(err)
				}
				id.Spec.RequiredKeys = tc.keys
				if err := e.c.Update(t.Context(), id); err != nil {
					t.Fatal(err)
				}
			}
			if res, _ := e.reconcile(t); res.RequeueAfter != tc.want {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, tc.want)
			}
		})
	}
}

// TestReconcileCacheListError proves a failed mirror List from the cache
// fails the pass without patching status.
func TestReconcileCacheListError(t *testing.T) {
	t.Parallel()
	e := newEnv(t, sourceSecret())
	boom := errors.New("boom")
	e.r.Cache = fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return boom },
	}).Build()
	if _, err := e.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: idName}}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
	if n := e.patches.Load(); n != 0 {
		t.Errorf("status patched %d times on a list error", n)
	}
}
