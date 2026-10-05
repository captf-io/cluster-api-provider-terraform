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

package contract

import (
	"maps"
	"testing"
)

// TestNewCommonInputs: the constructor fills version, cluster, object and
// tags, and leaves the variables empty.
func TestNewCommonInputs(t *testing.T) {
	t.Parallel()
	got := NewCommonInputs("c", "cns", "TerraformMachine", "m", "ns", "tpl")
	if got.Contract != Version {
		t.Errorf("Contract = %q, want %q", got.Contract, Version)
	}
	if want := (Cluster{Name: "c", Namespace: "cns"}); got.Cluster != want {
		t.Errorf("Cluster = %+v, want %+v", got.Cluster, want)
	}
	if want := (Object{Kind: "TerraformMachine", Name: "m", Namespace: "ns"}); got.Object != want {
		t.Errorf("Object = %+v, want %+v", got.Object, want)
	}
	if want := Tags("c", "ns", "TerraformMachine", "m", "tpl"); !maps.Equal(got.Tags, want) {
		t.Errorf("Tags = %v, want %v", got.Tags, want)
	}
	if got.Variables != nil {
		t.Errorf("Variables = %v, want nil", got.Variables)
	}
}
