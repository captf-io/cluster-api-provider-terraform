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

package lint

import (
	"errors"
	"slices"
	"testing"
)

// TestTokenizeStrings: a quoted string literal is one token, even with an
// embedded space or bracket, so it never desyncs skipTo's bracket
// counting.
func TestTokenizeStrings(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		src  string
		want []string
	}{
		{`optional(string, "N/A (legacy)")`, []string{"optional", "(", "string", ",", `"N/A (legacy)"`, ")"}},
		{`optional(string, "say \"hi\"")`, []string{"optional", "(", "string", ",", `"say \"hi\""`, ")"}},
		{`object({zone=optional(string, "a")})`, []string{"object", "(", "{", "zone", "=", "optional", "(", "string", ",", `"a"`, ")", "}", ")"}},
	} {
		if got := tokenize(tt.src); !slices.Equal(got, tt.want) {
			t.Errorf("tokenize(%q) = %v, want %v", tt.src, got, tt.want)
		}
	}
}

// TestOptionalWithQuotedDefault: optional(string, "N/A (legacy)") parses,
// and the object it defaults still matches the contract: this is the case
// that desynced the old, non-string-aware tokenizer.
func TestOptionalWithQuotedDefault(t *testing.T) {
	t.Parallel()
	const declared = `object({zone=string, note=optional(string, "N/A (legacy)")})`
	const contractType = "object({zone=string})"
	d, err := parseType(declared)
	if err != nil {
		t.Fatalf("parse %q: %v", declared, err)
	}
	if !d.optional["note"] {
		t.Errorf("note = %+v, want optional", d.attrs["note"])
	}
	c, err := parseType(contractType)
	if err != nil {
		t.Fatalf("parse %q: %v", contractType, err)
	}
	if !compatible(d, c) {
		t.Errorf("compatible(%s, %s) = false, want true", declared, contractType)
	}
}

// TestCompatible proves compatible matches primitives only to themselves,
// treats any as a universal match, matches collections element-wise,
// applies Terraform's list/set/tuple conversions and their limits (a
// tuple's fixed length rejects a list or set), and matches an object only
// when every required attribute of the contract is satisfied.
func TestCompatible(t *testing.T) {
	t.Parallel()
	const (
		endpoint = "object({host=string, port=number})"
		network  = "object({pods=list(string), services=list(string), service_domain=string, api_server_port=number})"
	)
	for _, tt := range []struct {
		declared, contract string
		want               bool
	}{
		{"string", "string", true},
		{"bool", "bool", true},
		{"string", "number", false},
		{"any", endpoint, true},
		{"map(string)", "any", true},
		{"map(string)", "map(string)", true},
		{"map(number)", "map(string)", false},
		{"list(string)", "map(string)", false},
		{endpoint, endpoint, true},
		// Whitespace and newlines, as written in a module.
		{"object({\n  host = string\n  port = number\n})", endpoint, true},
		{"object({ host = string, port = number, })", endpoint, true},
		// A subset: Terraform drops the extra contract attributes.
		{"object({host=string})", endpoint, true},
		// An attribute the contract lacks fails the conversion, unless optional.
		{"object({host=string, port=number, zone=string})", endpoint, false},
		{`object({host=string, port=number, zone=optional(string, "a")})`, endpoint, true},
		{"object({host=string, port=optional(number)})", endpoint, true},
		{"object({host=number, port=number})", endpoint, false},
		{network, network, true},
		{"object({pods=list(number), services=list(string)})", network, false},
		{"list(object({type=string, address=string}))", "list(object({type=string, address=string}))", true},
		{"set(string)", "set(string)", true},
		{"tuple([string, number])", "tuple([string, number])", true},
		{"tuple([string])", "tuple([string, number])", false},
		{"tuple([string, string])", "tuple([string, number])", false},
		// Terraform converts between list/set/tuple of compatible element
		// types automatically, in either direction.
		{"set(string)", "list(string)", true},
		{"list(string)", "set(string)", true},
		{"list(string)", "tuple([string, string])", true},
		{"set(string)", "tuple([string, string])", true},
		// A tuple's fixed length has no conversion from a list or set,
		// whose length isn't known statically, even with matching
		// elements.
		{"tuple([string, string])", "list(string)", false},
		{"tuple([string])", "set(string)", false},
		{"tuple([])", "list(string)", false},
		// Element types must still be compatible.
		{"list(number)", "tuple([string, string])", false},
		{"set(number)", "list(string)", false},
		// Genuinely incompatible kinds are still rejected.
		{"map(string)", "list(string)", false},
		{"tuple([string])", "map(string)", false},
	} {
		d, err := parseType(tt.declared)
		if err != nil {
			t.Fatalf("parse %q: %v", tt.declared, err)
		}
		c, err := parseType(tt.contract)
		if err != nil {
			t.Fatalf("parse %q: %v", tt.contract, err)
		}
		if got := compatible(d, c); got != tt.want {
			t.Errorf("compatible(%s, %s) = %v, want %v", tt.declared, tt.contract, got, tt.want)
		}
	}
}

// TestParseTypeErrors proves parseType returns an error wrapping errType
// for empty, truncated, malformed or trailing-token type expressions.
func TestParseTypeErrors(t *testing.T) {
	t.Parallel()
	for _, src := range []string{
		"", "strin", "list(string", "list string", "object({host string})", "object({host=string",
		"object({host=optional(string", "tuple([string)", "string extra", "object(host=string)",
	} {
		if _, err := parseType(src); !errors.Is(err, errType) {
			t.Errorf("parseType(%q) = %v", src, err)
		}
	}
}
