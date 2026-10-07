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

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

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
