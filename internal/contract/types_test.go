/*
Copyright 2026 The cluster-api-provider-terraform Authors.

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

package contract

import (
	"errors"
	"maps"
	"slices"
	"testing"
)

// TestInputSpecs: the declaration table covers exactly the input names, and
// only bootstrap_data is sensitive.
func TestInputSpecs(t *testing.T) {
	t.Parallel()
	for _, role := range []Role{RoleCluster, RoleMachine, RoleMachinePool} {
		specs, err := InputSpecs(role)
		if err != nil {
			t.Fatal(err)
		}
		names, err := InputNames(role)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := slices.Sorted(maps.Keys(specs)), slices.Sorted(slices.Values(names)); !slices.Equal(got, want) {
			t.Errorf("%s: specs %v, names %v", role, got, want)
		}
		for name, s := range specs {
			if s.Type == "" || s.Sensitive != (name == "bootstrap_data") {
				t.Errorf("%s: %s = %+v", role, name, s)
			}
		}
	}
	if _, err := InputSpecs(Role("bogus")); !errors.Is(err, ErrUnknownRole) {
		t.Errorf("bogus: %v", err)
	}
}
