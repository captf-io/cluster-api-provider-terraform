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

package strutil_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/plankey"
	"github.com/captf-io/cluster-api-provider-terraform/internal/strutil"
)

// legacyName is the truncation both inputs.Name and plankey.Name carried
// before BoundedName, kept verbatim as the oracle. prefix is the name
// prefix, kindshort the short kind and name the object name. It returns
// the name those implementations built.
func legacyName(prefix, kindshort, name string) string {
	const maxName = 253
	full := prefix + kindshort + "-" + name
	if len(full) <= maxName {
		return full
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:])[:16]
	head := strings.TrimRight(full[:maxName-len(suffix)], "-.")
	return head + suffix
}

// TestBoundedNameMatchesLegacy proves BoundedName, inputs.Name and
// plankey.Name produce the names the duplicated implementations did.
func TestBoundedNameMatchesLegacy(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"short", "web"},
		{"exactly at the limit", strings.Repeat("a", 253-len("captf-inputs-tm-"))},
		{"one over the limit", strings.Repeat("a", 253-len("captf-inputs-tm-")+1)},
		{"long", strings.Repeat("b", 400)},
		{"cut lands on a dash", strings.Repeat("c", 100) + strings.Repeat("-", 60) + strings.Repeat("d", 100)},
		{"cut lands on a dot", strings.Repeat("e", 100) + strings.Repeat(".", 60) + strings.Repeat("f", 100)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, p := range []struct {
				prefix string
				got    string
			}{
				{"captf-inputs-", inputs.Name("tm", tt.in)},
				{"captf-plankey-", plankey.Name("tm", tt.in)},
			} {
				want := legacyName(p.prefix, "tm", tt.in)
				if p.got != want {
					t.Errorf("%s name = %q, want %q", p.prefix, p.got, want)
				}
				if b := strutil.BoundedName(p.prefix, "tm", tt.in); b != want {
					t.Errorf("BoundedName(%q) = %q, want %q", p.prefix, b, want)
				}
				if len(p.got) > 253 {
					t.Errorf("%s name is %d characters, want <= 253", p.prefix, len(p.got))
				}
			}
		})
	}
}
