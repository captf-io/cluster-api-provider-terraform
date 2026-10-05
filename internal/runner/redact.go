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

package runner

import (
	"encoding/json"
	"slices"
	"strings"
)

// Redacted replaces every secret value found in a redacted text.
const Redacted = "(sensitive)"

// minRedactLen is the shortest value a Redactor acts on. Shorter values
// ("1", "on", "dev") are common in ordinary text, so replacing them would
// mangle diagnostics without hiding anything that matters.
const minRedactLen = 4

// Redactor removes known secret values from free text before it leaves the
// runner (the termination message, events). Terraform's own "Error:" line
// selection is not redaction: a provider diagnostic can echo a credential, a
// sensitive variable or a sensitive plan value. A nil *Redactor is valid
// and redacts nothing. A Redactor never stores or reports its values
// anywhere but its replacer.
type Redactor struct {
	replacer *strings.Replacer
}

// secret is one value a Redactor removes, and minLine the shortest line of
// it, after trimming surrounding white space, that the Redactor also
// removes on its own when the value has several lines.
type secret struct {
	value   string
	minLine int
}

// secretsOf returns values as secrets whose lines of at least minRedactLen
// bytes are redacted on their own.
func secretsOf(values []string) []secret {
	return secretsWithMin(values, minRedactLen)
}

// secretsWithMin returns values as secrets whose lines of at least minLine
// bytes are redacted on their own.
func secretsWithMin(values []string, minLine int) []secret {
	out := make([]secret, 0, len(values))
	for _, v := range values {
		out = append(out, secret{value: v, minLine: minLine})
	}
	return out
}

// NewRedactor returns a Redactor for values, each line of a multi-line
// value of at least 4 bytes included: newRedactor of secretsOf(values).
func NewRedactor(values ...string) *Redactor {
	return newRedactor(secretsOf(values))
}

// newRedactor returns a Redactor for secrets. Values shorter than 4 bytes
// are ignored (see minRedactLen), duplicates are collapsed, and the longest
// values are replaced first so that a value which is a prefix of another is
// never left half-visible. For each value the Redactor also matches its
// JSON-escaped form (as `validate -json` prints it), and, for a multi-line
// value, each line of it of at least its minLine bytes (and never under
// minRedactLen) after trimming surrounding white space, so a diagnostic
// that echoes one line of a PEM block or a bootstrap script is still
// redacted.
//
// A value or a line registered more than once (bootstrap data decoded
// from its variable and again from a plan's sensitive user_data) gets the
// largest minLine any copy asks for: a copy with a smaller minimum must
// not bring back the short, ordinary lines ("then", "done") the larger one
// leaves alone.
func newRedactor(secrets []secret) *Redactor {
	valueMin := make(map[string]int, len(secrets))
	var values []string
	for _, s := range secrets {
		if m, ok := valueMin[s.value]; ok {
			valueMin[s.value] = max(m, s.minLine)
			continue
		}
		valueMin[s.value] = s.minLine
		values = append(values, s.value)
	}
	lineMin := make(map[string]int)
	for _, v := range values {
		for _, l := range secretLines(v) {
			lineMin[l] = max(lineMin[l], valueMin[v])
		}
	}
	seen := make(map[string]struct{})
	var forms []string
	add := func(s string) {
		if len(s) < minRedactLen {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		forms = append(forms, s)
	}
	for _, v := range values {
		add(v)
		add(jsonEscaped(v))
		for _, l := range secretLines(v) {
			if len(l) >= lineMin[l] {
				add(l)
				add(jsonEscaped(l))
			}
		}
	}
	if len(forms) == 0 {
		return &Redactor{}
	}
	// A Replacer tries its pairs in argument order at each position, so
	// sorting by length (descending) makes the longest match win. The sort
	// is stable on a deterministic input order.
	slices.SortStableFunc(forms, func(a, b string) int { return len(b) - len(a) })
	// The capacity is a hint (append grows it to two entries per form);
	// keeping arithmetic out of the allocation size rules out an overflow.
	pairs := make([]string, 0, len(forms))
	for _, f := range forms {
		pairs = append(pairs, f, Redacted)
	}
	return &Redactor{replacer: strings.NewReplacer(pairs...)}
}

// secretLines returns the lines of the multi-line value v, each trimmed of
// surrounding white space, or nil when v has a single line.
func secretLines(v string) []string {
	if !strings.ContainsAny(v, "\r\n") {
		return nil
	}
	lines := strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == '\r' })
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return lines
}

// jsonEscaped returns s as it appears inside a JSON string literal, without
// the quotes.
func jsonEscaped(s string) string {
	b, err := json.Marshal(s)
	if err != nil || len(b) < 2 {
		return s
	}
	return string(b[1 : len(b)-1])
}

// Redact returns s with every secret value replaced by "(sensitive)". It is
// safe to call on a nil Redactor, which returns s unchanged. The result can
// be longer than s, so cap it afterwards, never before: truncating first
// could leave a prefix of a value that no longer matches.
func (r *Redactor) Redact(s string) string {
	if r == nil || r.replacer == nil {
		return s
	}
	return r.replacer.Replace(s)
}
