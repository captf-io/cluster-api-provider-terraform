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
	"context"
	"fmt"
	"sync"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestUsedInNamespace proves UsedInNamespace finds a user only in the
// namespace asked for, through its own identityRef, its cluster's
// fallback or its CAPI Cluster's infrastructureRef, and that every List it
// makes, the CAPI Cluster one included, is scoped to that namespace, so
// the manager's cache can serve it.
func TestUsedInNamespace(t *testing.T) {
	t.Parallel()
	s := newScheme(t)
	if err := clusterv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		objs []client.Object
		want bool
	}{
		{name: "unused", objs: []client.Object{labeledMachine(tenant, "m", "", "other")}},
		{name: "used only in another namespace", objs: []client.Object{
			labeledMachine("other", "m", "", idName), labeledCluster("other", "tc", "c1", idName, ""),
		}},
		{name: "machine identityRef", objs: []client.Object{labeledMachine(tenant, "m", "", idName)}, want: true},
		{name: "pool via cluster defaults", objs: []client.Object{
			labeledCluster(tenant, "tc", "c1", "other", idName), labeledPool(tenant, "p", "c1", ""),
		}, want: true},
		{name: "machine via infrastructureRef", objs: []client.Object{
			labeledCluster(tenant, "tc", "", "other", idName), capiCluster(tenant, "c1", "tc"), labeledMachine(tenant, "m", "c1", ""),
		}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var unscoped []string
			r := fake.NewClientBuilder().WithScheme(s).WithObjects(tt.objs...).WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if lo := (&client.ListOptions{}).ApplyOptions(opts); lo.Namespace != tenant {
						mu.Lock()
						unscoped = append(unscoped, fmt.Sprintf("%T in %q", list, lo.Namespace))
						mu.Unlock()
					}
					return c.List(ctx, list, opts...)
				},
			}).Build()
			got, err := UsedInNamespace(t.Context(), r, idName, tenant)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("UsedInNamespace = %v, want %v", got, tt.want)
			}
			if len(unscoped) > 0 {
				t.Errorf("Lists not scoped to %s: %v", tenant, unscoped)
			}
		})
	}
}
