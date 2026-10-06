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

package varschema

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// testSchema covers every keyword of the subset.
const testSchema = `{"type":"object","properties":{` +
	`"name":{"type":"string"},"size":{"type":"number"},"on":{"type":"boolean"},` +
	`"zones":{"type":"array","items":{"type":"string"}},` +
	`"tags":{"type":"object","additionalProperties":{"type":"string"}},` +
	`"vol":{"type":"object","properties":{"size":{"type":"number"},"kind":{"type":"string"}},"required":["size"]},` +
	`"pair":{"type":"array","prefixItems":[{"type":"string"},{"type":"number"}]},` +
	`"any":{}},"required":["name"],"additionalProperties":false}`

// TestValidate: every kind of problem, in strict and lenient mode.
func TestValidate(t *testing.T) {
	t.Parallel()
	s, err := Parse(testSchema)
	if err != nil {
		t.Fatal(err)
	}
	strict := func(j string) Value { return Value{JSON: json.RawMessage(j)} }
	lenient := func(j string) Value { return Value{JSON: json.RawMessage(j), Lenient: true} }
	for _, tt := range []struct {
		name string
		vars map[string]Value
		want []string // substrings, one per problem, in order; nil means valid
	}{
		{"valid", map[string]Value{"name": strict(`"a"`), "size": strict(`3`), "on": strict(`true`), "zones": strict(`["a"]`),
			"tags": strict(`{"k":"v"}`), "vol": strict(`{"size":1,"extra":2}`), "pair": strict(`["a",1]`), "any": strict(`{"x":[1]}`)}, nil},
		{"null is accepted", map[string]Value{"name": strict(`"a"`), "size": strict(`null`)}, nil},
		{"conversions", map[string]Value{"name": strict(`5`), "size": strict(`"3.5"`), "on": strict(`"true"`)}, nil},
		{"unknown key", map[string]Value{"name": strict(`"a"`), "instnce_type": strict(`1`)}, []string{`variable "instnce_type" is not declared`}},
		{"missing required", map[string]Value{}, []string{`variable "name" is required`}},
		{"wrong type", map[string]Value{"name": strict(`"a"`), "size": strict(`"big"`), "on": strict(`"yes"`)},
			[]string{`variable "on" must be a boolean, got a string`, `variable "size" must be a number, got a string`}},
		{"nested", map[string]Value{"name": strict(`"a"`), "zones": strict(`["a",{}]`), "tags": strict(`{"k":[]}`), "vol": strict(`{"kind":"x"}`)},
			[]string{`variable "tags".k must be a string`, `variable "vol" is missing the required attribute "size"`, `variable "zones"[1] must be a string`}},
		{"tuple", map[string]Value{"name": strict(`"a"`), "pair": strict(`["a"]`)}, []string{`must have 2 elements, got 1`}},
		{"not an array", map[string]Value{"name": strict(`"a"`), "zones": strict(`"a"`)}, []string{`variable "zones" must be an array, got a string`}},
		{"lenient primitives", map[string]Value{"name": lenient(`"a"`), "size": lenient(`"3"`)}, nil},
		{"lenient impossible", map[string]Value{"name": lenient(`"a"`), "zones": lenient(`"a,b"`), "vol": lenient(`"x"`)},
			[]string{`variable "vol" must be an object`, `variable "zones" must be an array`}},
		{"lenient bad number", map[string]Value{"name": lenient(`"a"`), "size": lenient(`"big"`)}, []string{`variable "size" must be a number`}},
		{"invalid json", map[string]Value{"name": strict(`{`)}, []string{`not valid JSON`}},
	} {
		got := s.Validate(tt.vars)
		if len(got) != len(tt.want) {
			t.Errorf("%s: got %q, want %d problems %q", tt.name, got, len(tt.want), tt.want)
			continue
		}
		for i := range got {
			if !strings.Contains(got[i], tt.want[i]) {
				t.Errorf("%s: problem %d = %q, want %q", tt.name, i, got[i], tt.want[i])
			}
		}
	}
}

// TestOpenSchemaAllowsUnknown: a root that is not closed accepts an
// unknown key.
func TestOpenSchemaAllowsUnknown(t *testing.T) {
	t.Parallel()
	for _, add := range []string{`,"additionalProperties":true`, `,"additionalProperties":{"type":"string"}`} {
		s, err := Parse(`{"type":"object"` + add + `}`)
		if err != nil {
			t.Fatal(err)
		}
		if p := s.Validate(map[string]Value{"x": {JSON: json.RawMessage(`1`)}}); p != nil {
			t.Errorf("%s: %q", add, p)
		}
	}
}

// TestParse: a label that is not a schema of the subset, or is too large,
// is rejected.
func TestParse(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{``, `[]`, `{"type":"string"}`, `{"type":"object","bogus":1}`, `{"type":"object"} x`, `{"type":"object","additionalProperties":3}`} {
		if _, err := Parse(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) = %v, want ErrInvalid", bad, err)
		}
	}
	if _, err := Parse(`{"type":"object","properties":{"a":{"type":"` + strings.Repeat("x", MaxLabelBytes) + `"}}}`); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized label: %v", err)
	}
}

// TestMarshalRoundTripAndLimit: Marshal and Parse round trip, and Marshal
// enforces the size cap.
func TestMarshalRoundTripAndLimit(t *testing.T) {
	t.Parallel()
	s := &Schema{Type: TypeObject, Properties: map[string]*Schema{"m": {Type: TypeObject, AdditionalProperties: OfValues(&Schema{Type: TypeString})}}, AdditionalProperties: Closed()}
	out, err := Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := Marshal(back); again != out {
		t.Errorf("round trip: %s != %s", again, out)
	}
	big := &Schema{Type: TypeObject, Properties: map[string]*Schema{}}
	for i := range 3000 {
		big.Properties[strings.Repeat("v", 10)+string(rune('a'+i%26))+strings.Repeat("w", i%7)+strings.Repeat("z", i/26)] = &Schema{Type: TypeString}
	}
	if _, err := Marshal(big); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized schema: %v", err)
	}
}
