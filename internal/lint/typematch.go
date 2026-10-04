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
	"fmt"
	"strings"
	"unicode"
)

// errType is an unparsable type expression.
var errType = errors.New("unparsable type expression")

// typeExpr is a parsed Terraform type constraint.
type typeExpr struct {
	// kind is string, number, bool, any, list, set, map, tuple or object.
	kind string
	// elem is the element type of list, set and map.
	elem *typeExpr
	// elems are a tuple's element types.
	elems []*typeExpr
	// attrs are an object's attributes; optional marks optional() ones.
	attrs    map[string]*typeExpr
	optional map[string]bool
}

// parseType parses src, the source text of a variable's type (tfconfig
// gives it as written): primitives, any, list/set/map(T), tuple([…]) and
// object({k = T, …}) with optional(T[, default]) attributes. Whitespace
// and newlines are insignificant; attributes may be separated by commas
// or newlines. It returns the parsed typeExpr, or an error wrapping
// errType when src does not parse or has trailing tokens.
func parseType(src string) (*typeExpr, error) {
	p := &typeParser{toks: tokenize(src)}
	t, err := p.expr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("%w: trailing %q", errType, p.toks[p.pos])
	}
	return t, nil
}

// tokenize splits src into tokens: brackets and separators as
// single-character tokens, a quoted string literal (its escapes
// honored, so a `\"` inside it does not end it) as one opaque token
// including its quotes, and everything else as runs of non-space,
// non-bracket characters. A bracket character inside a string literal is
// part of that token, not a bracket, so it never desyncs a caller (like
// skipTo) that counts brackets by token equality: without this,
// optional(string, "N/A (legacy)") would tokenize its default's "(" and
// ")" as real brackets and fail to parse. It returns the
// resulting tokens.
func tokenize(src string) []string {
	runes := []rune(src)
	var toks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(runes); {
		switch r := runes[i]; {
		case r == '"':
			flush()
			toks = append(toks, stringLiteral(runes, &i))
		case unicode.IsSpace(r):
			flush()
			i++
		case strings.ContainsRune("(){}[]=,:", r):
			flush()
			toks = append(toks, string(r))
			i++
		default:
			cur.WriteRune(r)
			i++
		}
	}
	flush()
	return toks
}

// stringLiteral reads a quoted string starting at runes[*i] (a `"`) and
// advances *i past its closing quote (or to len(runes), for one that
// never closes: tokenize never errors, so an unterminated literal
// surfaces later, as a parseType error). It returns the token read,
// quotes and escapes included, verbatim.
func stringLiteral(runes []rune, i *int) string {
	var lit strings.Builder
	lit.WriteRune(runes[*i])
	*i++
	for *i < len(runes) {
		c := runes[*i]
		lit.WriteRune(c)
		*i++
		if c == '\\' && *i < len(runes) {
			lit.WriteRune(runes[*i])
			*i++
			continue
		}
		if c == '"' {
			break
		}
	}
	return lit.String()
}

// typeParser is a recursive-descent parser over tokenize's output, parsing
// one type expression per call to expr, tuple or object.
type typeParser struct {
	toks []string
	pos  int
}

// next consumes and returns p's current token, or "" past the end.
func (p *typeParser) next() string {
	if p.pos >= len(p.toks) {
		return ""
	}
	t := p.toks[p.pos]
	p.pos++
	return t
}

// peek returns p's current token without consuming it, or "" past the
// end.
func (p *typeParser) peek() string {
	if p.pos >= len(p.toks) {
		return ""
	}
	return p.toks[p.pos]
}

// expect consumes p's current token and returns an error wrapping errType
// when it is not tok.
func (p *typeParser) expect(tok string) error {
	if got := p.next(); got != tok {
		return fmt.Errorf("%w: want %q, got %q", errType, tok, got)
	}
	return nil
}

// expr parses one type expression off p: a primitive or any, a
// list/set/map(T), a tuple or an object. It returns the parsed typeExpr,
// or an error wrapping errType for an unrecognized token or a malformed
// nested expression.
func (p *typeParser) expr() (*typeExpr, error) {
	switch kind := p.next(); kind {
	case "string", "number", "bool", "any":
		return &typeExpr{kind: kind}, nil
	case "list", "set", "map":
		if err := p.expect("("); err != nil {
			return nil, err
		}
		elem, err := p.expr()
		if err != nil {
			return nil, err
		}
		return &typeExpr{kind: kind, elem: elem}, p.expect(")")
	case "tuple":
		return p.tuple()
	case "object":
		return p.object()
	default:
		return nil, fmt.Errorf("%w: unknown type %q", errType, kind)
	}
}

// tuple parses a tuple([…]) expression off p, after "tuple" has already
// been consumed. It returns the parsed typeExpr, or an error wrapping
// errType when the syntax is malformed.
func (p *typeParser) tuple() (*typeExpr, error) {
	if err := p.expect("("); err != nil {
		return nil, err
	}
	if err := p.expect("["); err != nil {
		return nil, err
	}
	t := &typeExpr{kind: "tuple"}
	for p.peek() != "]" {
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		t.elems = append(t.elems, e)
		if p.peek() == "," {
			p.next()
		}
	}
	p.next()
	return t, p.expect(")")
}

// object parses an object({k = T, …}) expression off p, after "object" has
// already been consumed, honoring optional(T[, default]) attributes. It
// returns the parsed typeExpr, or an error wrapping errType when the
// syntax is malformed.
func (p *typeParser) object() (*typeExpr, error) {
	if err := p.expect("("); err != nil {
		return nil, err
	}
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	t := &typeExpr{kind: "object", attrs: map[string]*typeExpr{}, optional: map[string]bool{}}
	for p.peek() != "}" {
		key := strings.Trim(p.next(), `"`)
		if key == "" {
			return nil, fmt.Errorf("%w: unterminated object", errType)
		}
		if sep := p.next(); sep != "=" && sep != ":" {
			return nil, fmt.Errorf("%w: want = after %q, got %q", errType, key, sep)
		}
		if p.peek() == "optional" {
			p.next()
			if err := p.expect("("); err != nil {
				return nil, err
			}
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			if err := p.skipTo(")"); err != nil { // an optional default, if any
				return nil, err
			}
			t.attrs[key], t.optional[key] = e, true
		} else {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			t.attrs[key] = e
		}
		if p.peek() == "," {
			p.next()
		}
	}
	p.next()
	return t, p.expect(")")
}

// skipTo consumes tokens through the closing tok at the current nesting,
// and returns an error wrapping errType when the tokens run out first.
func (p *typeParser) skipTo(tok string) error {
	depth := 0
	for {
		switch t := p.next(); t {
		case "":
			return fmt.Errorf("%w: unterminated optional()", errType)
		case "(", "{", "[":
			depth++
		case ")", "}", "]":
			if depth == 0 && t == tok {
				return nil
			}
			depth--
		}
	}
}

// compatible reports whether a module declaring declared accepts every
// value the generated root passes as contract. Either side any matches.
// Collections match element-wise. A declared list or set also accepts a
// contract list, set or tuple (Terraform converts all three to the
// declared collection automatically, provided the element types are
// compatible); a declared tuple, whose type fixes a length, accepts only
// a contract tuple of the same length — Terraform has no conversion from
// a list or set, whose length isn't known until runtime, into a tuple
// type (cty's convert package defines tuple-to-list/set but not the
// reverse). A declared object matches when each of its attributes is in
// the contract object with a compatible type, or is optional():
// Terraform drops the contract's extra attributes on conversion, but a
// required attribute the contract lacks fails it. Primitives match only
// themselves; an implicit conversion (number to string) would hide a
// mistake.
func compatible(declared, contract *typeExpr) bool {
	if declared.kind == "any" || contract.kind == "any" {
		return true
	}
	if isSequence(declared) && isSequence(contract) {
		return compatibleSequence(declared, contract)
	}
	if declared.kind != contract.kind {
		return false
	}
	switch declared.kind {
	case "map":
		return compatible(declared.elem, contract.elem)
	case "object":
		for k, d := range declared.attrs {
			c, ok := contract.attrs[k]
			if !ok {
				if !declared.optional[k] {
					return false
				}
				continue
			}
			if !compatible(d, c) {
				return false
			}
		}
	}
	return true
}

// isSequence reports whether t is a list, set or tuple: the three kinds
// Terraform interconverts.
func isSequence(t *typeExpr) bool {
	return t.kind == "list" || t.kind == "set" || t.kind == "tuple"
}

// compatibleSequence reports whether every value contract can hold
// converts to an element declared accepts. A declared tuple only ever
// matches a contract tuple of the same length, position-by-position: a
// list or set contract has no statically known length, so it cannot
// convert to a fixed-length tuple. A declared list or set compares its
// single elem type against contract's, taken position-by-position when
// contract is a tuple.
func compatibleSequence(declared, contract *typeExpr) bool {
	switch {
	case declared.kind == "tuple" && contract.kind == "tuple":
		if len(declared.elems) != len(contract.elems) {
			return false
		}
		for i := range declared.elems {
			if !compatible(declared.elems[i], contract.elems[i]) {
				return false
			}
		}
		return true
	case declared.kind == "tuple":
		// Terraform has no list/set-to-tuple conversion: a fixed-length
		// tuple cannot statically accept a variable-length collection.
		return false
	case contract.kind == "tuple":
		for _, e := range contract.elems {
			if !compatible(declared.elem, e) {
				return false
			}
		}
		return true
	default:
		return compatible(declared.elem, contract.elem)
	}
}
