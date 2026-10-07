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

package state

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestCachingReader proves a second read of an unchanged state lists only
// metadata and returns a copy of the first, which the caller may change
// freely; a write to a state Secret (Adopt's hash) makes the next read
// fetch and parse the payload again; and a state that is gone is
// ErrNoState and forgotten.
func TestCachingReader(t *testing.T) {
	t.Parallel()
	const suffix = "0123456789abcdef-m"
	owner := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: ns, UID: "uid-1"}}
	var full atomic.Int32
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(append(chunked(t, suffix, stateJSON, 2), owner)...).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.SecretList); ok {
				full.Add(1)
			}
			return c.List(ctx, list, opts...)
		}}).Build()
	r := NewCachingReader(c)
	ctx := context.Background()

	first, err := r.Read(ctx, ns, suffix)
	if err != nil || full.Load() != 1 {
		t.Fatalf("first read: %v, %d payload lists", err, full.Load())
	}
	first.Outputs["provider_id"] = Output{}
	first.Metadata[0].Name = "changed"
	second, err := r.Read(ctx, ns, suffix)
	if err != nil || full.Load() != 1 {
		t.Fatalf("second read: %v, %d payload lists; want none more", err, full.Load())
	}
	if string(second.Outputs["provider_id"].Value) != `"stub://m"` || second.Metadata[0].Name == "changed" || second.Serial != 7 {
		t.Errorf("second read = %+v: the first caller's changes leaked into the cache", second)
	}

	if err := Adopt(ctx, c, owner, suffix, "h1:new"); err != nil {
		t.Fatal(err)
	}
	before := full.Load()
	third, err := r.Read(ctx, ns, suffix)
	if err != nil || full.Load() != before+1 || third.InputsHash != "h1:new" {
		t.Errorf("after a write: %v, %d payload lists, hash %q; want one more", err, full.Load()-before, third.InputsHash)
	}

	if err := DeleteState(ctx, c, ns, suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(ctx, ns, suffix); !errors.Is(err, ErrNoState) || r.Len() != 0 {
		t.Errorf("after the delete: %v, %d kept", err, r.Len())
	}
}

// TestCachingReaderBounds proves the reader keeps at most MaxCachedStates
// states, dropping the oldest first.
func TestCachingReaderBounds(t *testing.T) {
	t.Parallel()
	r := NewCachingReader(nil)
	st := &State{Outputs: map[string]Output{"o": {Value: []byte(`"v"`)}}}
	for i := range MaxCachedStates + 3 {
		r.store(string(rune('a'+i%26))+string(rune(i)), "fp", st)
	}
	if r.Len() != MaxCachedStates {
		t.Errorf("kept %d states, want %d", r.Len(), MaxCachedStates)
	}
	if r.lookup("a"+string(rune(0)), "fp") != nil {
		t.Error("the oldest state was kept")
	}
}
