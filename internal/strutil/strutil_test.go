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

package strutil

import "testing"

// TestTruncate checks Truncate's byte limit, its empty result for a zero or
// negative limit, and that a cut inside a multi-byte rune backs up to the
// rune's start.
func TestTruncate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		s     string
		limit int
		want  string
	}{
		{"short", "abc", 5, "abc"},
		{"exact", "abc", 3, "abc"},
		{"ascii", "abcdef", 4, "abcd"},
		{"zero", "abc", 0, ""},
		{"negative", "abc", -1, ""},
		{"empty", "", 3, ""},
		// "é" is two bytes: a cut inside it backs up to before it.
		{"mid rune", "aé", 2, "a"},
		{"after rune", "aéb", 3, "aé"},
		// "€" is three bytes.
		{"three byte", "€€", 4, "€"},
		{"only rune", "€", 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Truncate(tc.s, tc.limit); got != tc.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tc.s, tc.limit, got, tc.want)
			}
		})
	}
}
