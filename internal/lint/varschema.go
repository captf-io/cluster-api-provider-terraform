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

package lint

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/varschema"
)

// VariablesSchema maps m's user variables, the root module's variables
// outside the captf_ names and role's contract inputs, to the schema an
// image publishes in io.captf.variables-schema. A variable without a
// default is required; a type that does not parse, or "any", accepts
// anything. It returns the schema, or an error for an unknown role.
func VariablesSchema(m *Module, role contract.Role) (*varschema.Schema, error) {
	inputs, err := contract.InputNames(role)
	if err != nil {
		return nil, fmt.Errorf("lint: variables schema: %w", err)
	}
	root := &varschema.Schema{
		Type:                 varschema.TypeObject,
		Properties:           map[string]*varschema.Schema{},
		AdditionalProperties: varschema.Closed(),
	}
	for _, name := range slices.Sorted(maps.Keys(m.Config.Variables)) {
		if strings.HasPrefix(name, contract.ReservedPrefix) || slices.Contains(inputs, name) {
			continue
		}
		v := m.Config.Variables[name]
		var s *varschema.Schema
		if t, err := parseType(strings.TrimSpace(v.Type)); err == nil && v.Type != "" {
			s = schemaOf(t)
		} else {
			s = &varschema.Schema{}
		}
		root.Properties[name] = s
		if v.Required {
			root.Required = append(root.Required, name)
		}
	}
	return root, nil
}

// schemaOf maps the parsed Terraform type t to a schema node. It returns
// the node; the any type is the empty schema.
func schemaOf(t *typeExpr) *varschema.Schema {
	switch t.kind {
	case "string":
		return &varschema.Schema{Type: varschema.TypeString}
	case "number":
		return &varschema.Schema{Type: varschema.TypeNumber}
	case "bool":
		return &varschema.Schema{Type: varschema.TypeBoolean}
	case "list", "set":
		return &varschema.Schema{Type: varschema.TypeArray, Items: schemaOf(t.elem)}
	case "map":
		return &varschema.Schema{Type: varschema.TypeObject, AdditionalProperties: varschema.OfValues(schemaOf(t.elem))}
	case "tuple":
		s := &varschema.Schema{Type: varschema.TypeArray, PrefixItems: []*varschema.Schema{}}
		for _, e := range t.elems {
			s.PrefixItems = append(s.PrefixItems, schemaOf(e))
		}
		return s
	case "object":
		s := &varschema.Schema{Type: varschema.TypeObject, Properties: map[string]*varschema.Schema{}}
		for _, k := range slices.Sorted(maps.Keys(t.attrs)) {
			s.Properties[k] = schemaOf(t.attrs[k])
			if !t.optional[k] {
				s.Required = append(s.Required, k)
			}
		}
		return s
	}
	return &varschema.Schema{}
}
