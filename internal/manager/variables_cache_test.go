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

package manager

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestVariablesCacheOptions: the variables cache holds only the metadata
// of ConfigMaps and Secrets labeled captf.io/variables=true, in the
// manager's namespace scope, without managedFields.
func TestVariablesCacheOptions(t *testing.T) {
	t.Parallel()
	s, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	scoped := VariablesCacheOptions("tenant-a", s, nil, nil)
	if _, ok := scoped.DefaultNamespaces["tenant-a"]; !ok || len(scoped.DefaultNamespaces) != 1 || scoped.Scheme != s {
		t.Errorf("namespaced options = %+v", scoped)
	}
	if all := VariablesCacheOptions("", s, nil, nil); all.DefaultNamespaces != nil {
		t.Errorf("cluster-wide DefaultNamespaces = %v", all.DefaultNamespaces)
	}
	assertStripsManagedFields(t, scoped.DefaultTransform)
	seen := sets.New[string]()
	for obj, by := range scoped.ByObject {
		m, ok := obj.(*metav1.PartialObjectMetadata)
		if !ok {
			t.Errorf("unexpected ByObject entry %T: a typed entry would cache every value", obj)
			continue
		}
		seen.Insert(m.GroupVersionKind().String())
		if by.Label == nil || !by.Label.Matches(labels.Set{infrav1.VariablesSourceLabel: "true"}) ||
			by.Label.Matches(labels.Set{}) || by.Label.Matches(labels.Set{"captf.io/managed": "true"}) {
			t.Errorf("%v selector = %v, want captf.io/variables=true", m.GroupVersionKind(), by.Label)
		}
		if by.Transform != nil {
			t.Errorf("%v sets a Transform: it would replace DefaultTransform", m.GroupVersionKind())
		}
	}
	want := sets.New(SecretMeta().GroupVersionKind().String(), ConfigMapMeta().GroupVersionKind().String())
	if !seen.Equal(want) {
		t.Errorf("ByObject covers %v, want %v", sets.List(seen), sets.List(want))
	}
}

// TestVariablesCacheGetCache proves VariablesCache.GetCache returns the
// wrapped cache.
func TestVariablesCacheGetCache(t *testing.T) {
	t.Parallel()
	fake := &informertest.FakeInformers{}
	if (VariablesCache{Cache: fake}).GetCache() != fake {
		t.Error("GetCache does not return the wrapped cache")
	}
}
