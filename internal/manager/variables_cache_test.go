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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestVariablesCacheOptions: the variables cache holds only ConfigMaps and
// Secrets labeled captf.io/variables=true, in the manager's namespace
// scope, and strips their data.
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
	seen := sets.New[string]()
	for obj, by := range scoped.ByObject {
		switch obj.(type) {
		case *corev1.Secret:
			seen.Insert("secret")
		case *corev1.ConfigMap:
			seen.Insert("configmap")
		default:
			t.Errorf("unexpected ByObject entry %T", obj)
		}
		if by.Label == nil || !by.Label.Matches(labels.Set{infrav1.VariablesSourceLabel: "true"}) ||
			by.Label.Matches(labels.Set{}) || by.Label.Matches(labels.Set{"captf.io/managed": "true"}) {
			t.Errorf("%T selector = %v, want captf.io/variables=true", obj, by.Label)
		}
		if by.Transform == nil {
			t.Errorf("%T: no data-stripping transform", obj)
		}
	}
	if !seen.Has("secret") || !seen.Has("configmap") {
		t.Errorf("ByObject covers %v, want Secret and ConfigMap", seen)
	}
}

// TestStripData proves StripData clears a Secret's or ConfigMap's data
// fields, passes through a value of any other type unchanged, and that
// VariablesCache.GetCache returns the wrapped cache.
func TestStripData(t *testing.T) {
	t.Parallel()
	sec := &corev1.Secret{Data: map[string][]byte{"k": []byte("v")}, StringData: map[string]string{"k": "v"}}
	cm := &corev1.ConfigMap{Data: map[string]string{"k": "v"}, BinaryData: map[string][]byte{"b": {1}}}
	for _, o := range []any{sec, cm} {
		if _, err := StripData(o); err != nil {
			t.Fatal(err)
		}
	}
	if sec.Data != nil || sec.StringData != nil || cm.Data != nil || cm.BinaryData != nil {
		t.Errorf("data kept: %+v %+v", sec, cm)
	}
	if got, err := StripData("other"); got != "other" || err != nil {
		t.Errorf("StripData(other) = %v, %v", got, err)
	}
	fake := &informertest.FakeInformers{}
	if (VariablesCache{Cache: fake}).GetCache() != fake {
		t.Error("GetCache does not return the wrapped cache")
	}
}
