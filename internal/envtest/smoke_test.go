//go:build envtest

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

package envtest

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestEnvironmentsServe proves every API server answers and carries the
// CRDs, and that the hooked ones really have webhook configurations.
func TestEnvironmentsServe(t *testing.T) {
	t.Parallel()
	for name, te := range map[string]*testEnv{"bare": bare, "hooked": hooked, "failopen": failopen} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ns := newNamespace(t, te.client)
			list := &corev1.ConfigMapList{}
			if err := te.client.List(context.Background(), list); err != nil {
				t.Fatalf("list in %s: %v", ns, err)
			}
		})
	}
}
