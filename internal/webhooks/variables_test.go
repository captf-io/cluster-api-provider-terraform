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

package webhooks

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// raw wraps s as the JSON payload of a runtime.RawExtension and returns it.
func raw(s string) runtime.RawExtension { return runtime.RawExtension{Raw: []byte(s)} }

// manyVariables returns a JSON object with n keys.
func manyVariables(n int) string {
	parts := make([]string, n)
	for i := range n {
		parts[i] = fmt.Sprintf(`"v%d":%d`, i, i)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// secretValue is a variables value that must never appear in an error
// message.
const secretValue = "s3cr3t-value"

// TestVariablesValidation: inline variables must be an object of at most
// 256 identifier keys that the role does not reserve; each source names
// exactly one ConfigMap or Secret. Every kind carrying variables applies
// the same rules, templates included. No error quotes a value.
func TestVariablesValidation(t *testing.T) {
	t.Parallel()
	ok := `{"instance_type":"t3.large","disk_gib":40,"db":{"password":"` + secretValue + `"}}`
	tests := []struct {
		name    string
		vars    string
		from    []infrav1.VariablesSource
		invalid bool
		frag    string
	}{
		{name: "object", vars: ok},
		{name: "none"},
		{name: "256 keys", vars: manyVariables(256)},
		{name: "257 keys", vars: manyVariables(257), invalid: true, frag: "spec.variables: Too many"},
		{name: "array", vars: `["` + secretValue + `"]`, invalid: true, frag: "must be a JSON object"},
		{name: "string", vars: `"` + secretValue + `"`, invalid: true, frag: "must be a JSON object"},
		{name: "null", vars: `null`, invalid: true, frag: "must be a JSON object"},
		{name: "invalid key", vars: `{"has.dot":"` + secretValue + `"}`, invalid: true, frag: "spec.variables[has.dot]"},
		{name: "leading digit", vars: `{"9x":1}`, invalid: true, frag: "not a Terraform identifier"},
		{name: "captf_ prefix", vars: `{"captf_x":1}`, invalid: true, frag: "captf_ prefix"},
		{name: "common contract input", vars: `{"captf_tags":{}}`, invalid: true, frag: "spec.variables[captf_tags]"},
		{name: "module meta-argument", vars: `{"source":"x"}`, invalid: true, frag: "module meta-argument"},
		{name: "both refs", from: []infrav1.VariablesSource{{
			ConfigMapRef: infrav1.VariablesSourceReference{Name: "a"}, SecretRef: infrav1.VariablesSourceReference{Name: "b"},
		}}, invalid: true, frag: "spec.variablesFrom[0]"},
		{name: "no ref", from: []infrav1.VariablesSource{{}}, invalid: true, frag: "exactly one of configMapRef and secretRef"},
		{name: "one ref each", from: []infrav1.VariablesSource{
			{ConfigMapRef: infrav1.VariablesSourceReference{Name: "a"}},
			{SecretRef: infrav1.VariablesSourceReference{Name: "b"}, Optional: new(true), Format: infrav1.VariablesFormatJSON},
		}},
	}
	ctx := context.Background()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var vars runtime.RawExtension
			if tt.vars != "" {
				vars = raw(tt.vars)
			}
			check := func(kind string, err error) {
				t.Helper()
				wantInvalid(t, err, tt.invalid, tt.frag)
				if err != nil && strings.Contains(err.Error(), secretValue) {
					t.Errorf("%s: error carries a value: %v", kind, err)
				}
			}
			m := machine("")
			m.Spec.Variables, m.Spec.VariablesFrom = vars, tt.from
			_, err := (&TerraformMachine{}).ValidateCreate(ctx, m)
			check("TerraformMachine", err)

			c := &infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Name: "c"}, Spec: infrav1.TerraformClusterSpec{
				WorkspaceSpec: infrav1.WorkspaceSpec{
					Source: infrav1.Source{Image: testImage}, IdentityRef: infrav1.IdentityReference{Name: "id"},
					Variables: vars, VariablesFrom: tt.from,
				},
			}}
			_, err = (&TerraformCluster{}).ValidateCreate(ctx, c)
			check("TerraformCluster", err)

			mt := &infrav1.TerraformMachineTemplate{ObjectMeta: metav1.ObjectMeta{Name: "t"}}
			mt.Spec.Template.Spec = m.Spec
			_, err = (&TerraformMachineTemplate{}).ValidateCreate(ctx, mt)
			check("TerraformMachineTemplate", err)

			ct := &infrav1.TerraformClusterTemplate{ObjectMeta: metav1.ObjectMeta{Name: "t"}}
			ct.Spec.Template.Spec = c.Spec
			_, err = (&TerraformClusterTemplate{}).ValidateCreate(ctx, ct)
			check("TerraformClusterTemplate", err)
		})
	}
}

// TestVariablesReservedPerRole: a role reserves its own contract inputs,
// not the other role's.
func TestVariablesReservedPerRole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := machine("")
	m.Spec.Variables = raw(`{"machine_name":"x"}`)
	_, err := (&TerraformMachine{}).ValidateCreate(ctx, m)
	wantInvalid(t, err, true, "spec.variables[machine_name]", "contract input of the machine role")
	m.Spec.Variables = raw(`{"control_plane_initialized":true}`)
	_, err = (&TerraformMachine{}).ValidateCreate(ctx, m)
	wantInvalid(t, err, false)

	c := &infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Name: "c"}, Spec: infrav1.TerraformClusterSpec{
		WorkspaceSpec: infrav1.WorkspaceSpec{
			Source: infrav1.Source{Image: testImage}, IdentityRef: infrav1.IdentityReference{Name: "id"},
			Variables: raw(`{"control_plane_initialized":true}`),
		},
	}}
	_, err = (&TerraformCluster{}).ValidateCreate(ctx, c)
	wantInvalid(t, err, true, "spec.variables[control_plane_initialized]", "contract input of the cluster role")
	c.Spec.Variables = raw(`{"machine_name":"x"}`)
	_, err = (&TerraformCluster{}).ValidateCreate(ctx, c)
	wantInvalid(t, err, false)
}

// TestVariablesMachineImmutable: variables and variablesFrom define the
// machine; a re-serialization of the same object is no change. A
// TerraformCluster may change both (they re-apply).
func TestVariablesMachineImmutable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	from := []infrav1.VariablesSource{{ConfigMapRef: infrav1.VariablesSourceReference{Name: "a"}}}
	tests := []struct {
		name    string
		mutate  func(u *infrav1.TerraformMachine)
		invalid bool
		frag    string
	}{
		{name: "unchanged"},
		{name: "reformatted", mutate: func(u *infrav1.TerraformMachine) { u.Spec.Variables = raw("{ \"b\": [1, 2],\n \"a\": \"x\" }") }},
		{name: "value changed", mutate: func(u *infrav1.TerraformMachine) { u.Spec.Variables = raw(`{"a":"y","b":[1,2]}`) },
			invalid: true, frag: "spec.variables: Forbidden"},
		{name: "removed", mutate: func(u *infrav1.TerraformMachine) { u.Spec.Variables = runtime.RawExtension{} },
			invalid: true, frag: "spec.variables: Forbidden"},
		{name: "source added", mutate: func(u *infrav1.TerraformMachine) {
			u.Spec.VariablesFrom = append(u.Spec.VariablesFrom, infrav1.VariablesSource{SecretRef: infrav1.VariablesSourceReference{Name: "s"}})
		}, invalid: true, frag: "spec.variablesFrom: Forbidden"},
		{name: "format changed", mutate: func(u *infrav1.TerraformMachine) { u.Spec.VariablesFrom[0].Format = infrav1.VariablesFormatJSON },
			invalid: true, frag: "spec.variablesFrom: Forbidden"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			old, updated := machine(""), machine("")
			for _, m := range []*infrav1.TerraformMachine{old, updated} {
				m.Spec.Variables = raw(`{"a":"x","b":[1,2]}`)
				m.Spec.VariablesFrom = []infrav1.VariablesSource{{ConfigMapRef: from[0].ConfigMapRef}}
			}
			if tt.mutate != nil {
				tt.mutate(updated)
			}
			_, err := (&TerraformMachine{}).ValidateUpdate(ctx, old, updated)
			wantInvalid(t, err, tt.invalid, tt.frag)
		})
	}

	spec := infrav1.TerraformClusterSpec{
		WorkspaceSpec: infrav1.WorkspaceSpec{
			Source: infrav1.Source{Image: testImage}, IdentityRef: infrav1.IdentityReference{Name: "id"},
			Variables: raw(`{"a":"x"}`), VariablesFrom: from,
		},
	}
	changed := *spec.DeepCopy()
	changed.Variables = raw(`{"a":"y"}`)
	changed.VariablesFrom = nil
	_, err := (&TerraformCluster{}).ValidateUpdate(ctx, &infrav1.TerraformCluster{Spec: spec},
		&infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Name: "c"}, Spec: changed})
	wantInvalid(t, err, false)

	// Templates stay fully immutable.
	tmpl := func(vars string) *infrav1.TerraformMachineTemplate {
		t := &infrav1.TerraformMachineTemplate{ObjectMeta: metav1.ObjectMeta{Name: "t"}}
		t.Spec.Template.Spec = infrav1.TerraformMachineSpec{WorkspaceSpec: infrav1.WorkspaceSpec{Source: infrav1.Source{Image: testImage}, Variables: raw(vars)}}
		return t
	}
	_, err = (&TerraformMachineTemplate{}).ValidateUpdate(dryRunContext(false), tmpl(`{"a":1}`), tmpl(`{"a":2}`))
	wantInvalid(t, err, true, "spec.template.spec: Forbidden")
}

// TestEqualVariables proves equalVariables treats two empty values as equal,
// a set value as unequal to an unset one, an unparsable value as unequal to
// any parsable one, and two values that differ only in key order as equal.
func TestEqualVariables(t *testing.T) {
	t.Parallel()
	if !equalVariables(runtime.RawExtension{}, runtime.RawExtension{}) {
		t.Error("empty != empty")
	}
	if equalVariables(raw(`{"a":1}`), runtime.RawExtension{}) {
		t.Error("set == unset")
	}
	if equalVariables(raw(`{"a":`), raw(`{"a":1}`)) {
		t.Error("unparsable == parsable")
	}
	if !equalVariables(raw(`{"a":1,"b":2}`), raw(`{"b":2, "a":1}`)) {
		t.Error("key order matters")
	}
}
