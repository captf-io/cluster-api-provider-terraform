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

package hash

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
)

// Scheme versions the hash; it prefixes every Sum and is part of Hashable,
// so changing what is hashed is a deliberate migration.
const Scheme = "h2"

// ErrNonInteger is returned for a number that is not an integer literal.
var ErrNonInteger = errors.New("hash: only integer numbers can be hashed")

// integerLiteral matches a JSON number literal with no fraction or
// exponent, the only numbers Canonical accepts outside canonicalAnyNumber.
var integerLiteral = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// Canonical returns the canonical JSON encoding of v.
func Canonical(v any) ([]byte, error) {
	return canonical(v, false)
}

// canonicalAnyNumber is Canonical for v, a user-authored value (module
// variables): any JSON number is accepted and written exactly as given, so
// 1.5 and 1.50 hash differently (a spurious re-apply at worst) but a value
// is never rounded into another one. It returns the canonical encoding of v.
func canonicalAnyNumber(v any) ([]byte, error) {
	return canonical(v, true)
}

// canonical returns the canonical JSON encoding of v; anyNumber selects
// between Canonical's integer-only rule and canonicalAnyNumber's rule of
// accepting every JSON number as written.
func canonical(v any, anyNumber bool) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("hash: marshal: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("hash: decode: %w", err)
	}
	var buf bytes.Buffer
	if err := (encoder{anyNumber: anyNumber}).encode(&buf, tree); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Sum returns Scheme + ":" + hex(sha256(Canonical(v))).
func Sum(v any) (string, error) {
	b, err := Canonical(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return Scheme + ":" + hex.EncodeToString(sum[:]), nil
}

// encoder writes the canonical form. anyNumber accepts every JSON number
// literal as given instead of integers only.
type encoder struct {
	anyNumber bool
}

// encode writes the canonical encoding of v to buf, applying e's anyNumber
// rule to any json.Number it finds; nested slices and maps recurse in place.
// It returns a non-nil error when v holds a rejected number or a value that
// is not a decoded JSON type.
func (e encoder) encode(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		if !e.anyNumber && !integerLiteral.MatchString(t.String()) {
			return fmt.Errorf("%w: %s", ErrNonInteger, t)
		}
		buf.WriteString(t.String())
	case string:
		return encodeString(buf, t)
	case []any:
		buf.WriteByte('[')
		for i, elem := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := e.encode(buf, elem); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := e.encode(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("hash: unexpected JSON value %T", v)
	}
	return nil
}

// encodeString writes s to buf as a JSON string without HTML escaping, so
// the canonical bytes do not depend on how the caller marshaled. It returns
// a non-nil error only if the underlying JSON encoder fails.
func encodeString(buf *bytes.Buffer, s string) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return fmt.Errorf("hash: encode string: %w", err)
	}
	buf.Write(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	return nil
}
