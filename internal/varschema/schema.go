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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Label is the image config label that carries the schema.
const Label = "io.captf.variables-schema"

// MaxLabelBytes caps the compact JSON of a schema. Image config labels
// have no fixed limit, but the manager reads them on every inspection, so
// tfcapi-lint fails a module whose schema is larger.
const MaxLabelBytes = 32 * 1024

// JSON Schema type names the subset uses.
const (
	TypeString  = "string"
	TypeNumber  = "number"
	TypeBoolean = "boolean"
	TypeArray   = "array"
	TypeObject  = "object"
)

// ErrInvalid reports a schema label that is not valid.
var ErrInvalid = errors.New("varschema: invalid schema")

// ErrTooLarge reports a schema whose compact JSON exceeds MaxLabelBytes.
var ErrTooLarge = errors.New("varschema: schema too large")

// Schema is one node of the schema. The zero value accepts anything.
type Schema struct {
	// Type is a JSON Schema type name, or "" for any value.
	Type string `json:"type,omitempty"`
	// Properties are an object's named attributes.
	Properties map[string]*Schema `json:"properties,omitempty"`
	// Required lists the properties that must be present, sorted.
	Required []string `json:"required,omitempty"`
	// Items is the element schema of a list, set or map-less array.
	Items *Schema `json:"items,omitempty"`
	// PrefixItems are a tuple's positional element schemas.
	PrefixItems []*Schema `json:"prefixItems,omitempty"`
	// AdditionalProperties is false for the root (an unknown key is an
	// error), or the value schema of a map.
	AdditionalProperties *Additional `json:"additionalProperties,omitempty"`
}

// Additional is the additionalProperties keyword: a boolean or a schema.
type Additional struct {
	// Allowed is the boolean form; it is used when Schema is nil.
	Allowed bool
	// Schema is the schema form, or nil.
	Schema *Schema
}

// MarshalJSON encodes a as a boolean or a schema; it returns the JSON and
// any encoding error.
func (a Additional) MarshalJSON() ([]byte, error) {
	if a.Schema != nil {
		return json.Marshal(a.Schema)
	}
	return json.Marshal(a.Allowed)
}

// UnmarshalJSON decodes b, a boolean or a schema, into a; it returns an
// error for any other JSON.
func (a *Additional) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("true")) || bytes.Equal(b, []byte("false")) {
		a.Allowed, a.Schema = b[0] == 't', nil
		return nil
	}
	s := &Schema{}
	if err := json.Unmarshal(b, s); err != nil {
		return err
	}
	a.Allowed, a.Schema = false, s
	return nil
}

// Closed returns the additionalProperties value false: no key outside
// properties is allowed.
func Closed() *Additional { return &Additional{Allowed: false} }

// OfValues returns the additionalProperties form of a map with values
// shaped like s.
func OfValues(s *Schema) *Additional { return &Additional{Schema: s} }

// Marshal returns the compact JSON of s, or ErrTooLarge when it exceeds
// MaxLabelBytes.
func Marshal(s *Schema) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("varschema: marshal: %w", err)
	}
	if len(b) > MaxLabelBytes {
		return "", fmt.Errorf("%w: %d bytes, the limit is %d", ErrTooLarge, len(b), MaxLabelBytes)
	}
	return string(b), nil
}

// Parse decodes label, the io.captf.variables-schema value. It returns the
// schema, or an error wrapping ErrInvalid or ErrTooLarge when label is not
// a JSON object of the subset or is larger than MaxLabelBytes.
func Parse(label string) (*Schema, error) {
	if len(label) > MaxLabelBytes {
		return nil, fmt.Errorf("%w: %s is %d bytes, the limit is %d", ErrTooLarge, Label, len(label), MaxLabelBytes)
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(label)))
	dec.DisallowUnknownFields()
	s := &Schema{}
	if err := dec.Decode(s); err != nil {
		return nil, fmt.Errorf("%w: %s is not a variables schema", ErrInvalid, Label)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: %s has trailing data", ErrInvalid, Label)
	}
	if s.Type != TypeObject {
		return nil, fmt.Errorf("%w: %s must describe an object", ErrInvalid, Label)
	}
	return s, nil
}
