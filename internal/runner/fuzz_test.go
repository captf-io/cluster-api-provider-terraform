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
	"strings"
	"testing"
)

// FuzzRedact proves Redact never lets a secret through: for any two secret
// values and any text, once the replacement marker is cut out, no piece of
// the output contains a value, its JSON-escaped form or (for a multi-line
// value) a trimmed line of it, of at least minRedactLen bytes. The text
// handed to Redact embeds both secrets so every run exercises a match.
func FuzzRedact(f *testing.F) {
	f.Add("hunter2-hunter2", "AKIAIOSFODNN7EXAMPLE", "Error: invalid credential AKIAIOSFODNN7EXAMPLE for hunter2-hunter2")
	f.Add("-----BEGIN TEST BLOCK-----\nfixture-line-one\n-----END TEST BLOCK-----\n", "x", "block line: fixture-line-one")
	f.Add("#!/bin/bash\r\n  echo join-token-abc123  \r\nfi\n", "", `{"user_data":"#!/bin/bash\r\n  echo join-token-abc123  \r\nfi\n"}`)
	f.Add("pass\"word\\1", "tab\tsecret", `"pass\"word\\1" and tab\tsecret`)
	f.Add("abc", "abcd", "abcd abc")
	f.Add("prefix-secret", "prefix-secret-longer", "prefix-secret-longer")
	f.Fuzz(func(t *testing.T, a, b, text string) {
		secrets := []string{a, b}
		in := text + "\n" + a + " " + b + " " + jsonEscaped(a) + text
		out := NewRedactor(secrets...).Redact(in)
		var forms []string
		for _, v := range secrets {
			forms = append(forms, v, jsonEscaped(v))
			for _, l := range secretLines(v) {
				forms = append(forms, l, jsonEscaped(l))
			}
		}
		for _, piece := range strings.Split(out, Redacted) {
			for _, form := range forms {
				if len(form) >= minRedactLen && strings.Contains(piece, form) {
					t.Fatalf("redacted output still contains %q: %q", form, out)
				}
			}
		}
	})
}
