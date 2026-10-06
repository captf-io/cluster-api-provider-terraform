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
	"fmt"
	"maps"
	"slices"
	"strconv"
)

// Value is one user variable to validate.
type Value struct {
	// JSON is the variable's JSON value.
	JSON json.RawMessage
	// Lenient marks a value that came from a string-format variablesFrom
	// source: Terraform's type system converts it, so only an impossible
	// top-level type is rejected, not nested shape.
	Lenient bool
}

// Validate checks vars against s, the module's variables schema. It
// returns one message per problem, sorted by variable name, each naming
// the variable and the path inside it but never a value; nil when vars is
// acceptable. An unknown variable is the "Unsupported argument" failure a
// Job would hit. Terraform's own primitive conversions are accepted: a
// number or bool where a string is wanted, and a string that parses where
// a number or bool is wanted.
func (s *Schema) Validate(vars map[string]Value) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(vars)) {
		v := vars[name]
		prop, ok := s.Properties[name]
		if !ok {
			if s.AdditionalProperties != nil && (s.AdditionalProperties.Allowed || s.AdditionalProperties.Schema != nil) {
				continue
			}
			out = append(out, fmt.Sprintf("variable %q is not declared by the module image", name))
			continue
		}
		var val any
		if err := json.Unmarshal(v.JSON, &val); err != nil {
			out = append(out, fmt.Sprintf("variable %q is not valid JSON", name))
			continue
		}
		out = append(out, check(prop, val, fmt.Sprintf("variable %q", name), v.Lenient)...)
	}
	for _, name := range s.Required {
		if _, ok := vars[name]; !ok {
			out = append(out, fmt.Sprintf("variable %q is required by the module image and is not set", name))
		}
	}
	return out
}

// check validates val against s at where, a path for messages; lenient
// restricts it to the top-level type. It returns the problems found.
func check(s *Schema, val any, where string, lenient bool) []string {
	if s == nil || val == nil {
		return nil
	}
	if lenient {
		// A string-format value is always a JSON string; Terraform can
		// only convert it to a primitive.
		if s.Type == TypeArray || s.Type == TypeObject {
			return []string{fmt.Sprintf("%s must be %s, but a string-format source supplies a string; use format JSON", where, article(s.Type))}
		}
	}
	switch s.Type {
	case "":
		return nil
	case TypeString:
		switch val.(type) {
		case string, float64, bool:
			return nil
		}
	case TypeNumber:
		switch t := val.(type) {
		case float64:
			return nil
		case string:
			if _, err := strconv.ParseFloat(t, 64); err == nil {
				return nil
			}
		}
	case TypeBoolean:
		switch t := val.(type) {
		case bool:
			return nil
		case string:
			if t == "true" || t == "false" {
				return nil
			}
		}
	case TypeArray:
		arr, ok := val.([]any)
		if !ok {
			break
		}
		if lenient {
			return nil
		}
		var out []string
		for i, e := range arr {
			item := s.Items
			if i < len(s.PrefixItems) {
				item = s.PrefixItems[i]
			}
			out = append(out, check(item, e, fmt.Sprintf("%s[%d]", where, i), false)...)
		}
		if s.PrefixItems != nil && len(arr) != len(s.PrefixItems) {
			out = append(out, fmt.Sprintf("%s must have %d elements, got %d", where, len(s.PrefixItems), len(arr)))
		}
		return out
	case TypeObject:
		obj, ok := val.(map[string]any)
		if !ok {
			break
		}
		return checkObject(s, obj, where)
	}
	return []string{fmt.Sprintf("%s must be %s, got %s", where, article(s.Type), kind(val))}
}

// checkObject validates obj against s, an object schema at where. It
// returns the problems found.
func checkObject(s *Schema, obj map[string]any, where string) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		at := where + "." + k
		if p, ok := s.Properties[k]; ok {
			out = append(out, check(p, obj[k], at, false)...)
		} else if s.AdditionalProperties != nil && s.AdditionalProperties.Schema != nil {
			out = append(out, check(s.AdditionalProperties.Schema, obj[k], at, false)...)
		}
		// An attribute the type does not name is dropped by Terraform's
		// object conversion, so it is not an error.
	}
	for _, k := range s.Required {
		if _, ok := obj[k]; !ok {
			out = append(out, fmt.Sprintf("%s is missing the required attribute %q", where, k))
		}
	}
	return out
}

// article returns the JSON Schema type t with an indefinite article.
func article(t string) string {
	if t == TypeArray || t == TypeObject {
		return "an " + t
	}
	return "a " + t
}

// kind names the JSON type of v, a decoded JSON value.
func kind(v any) string {
	switch v.(type) {
	case string:
		return "a string"
	case float64:
		return "a number"
	case bool:
		return "a bool"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return "null"
}
