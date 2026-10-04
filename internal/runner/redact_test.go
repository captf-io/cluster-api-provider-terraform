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

package runner

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestRedactorRedact checks the replacement rules of Redact.
func TestRedactorRedact(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		values []string
		in     string
		want   string
	}{
		{"every occurrence", []string{"hunter2xx"}, "a hunter2xx b hunter2xx", "a (sensitive) b (sensitive)"},
		{"short untouched", []string{"abc", "", "on"}, "abc on abc", "abc on abc"},
		{"four bytes redacted", []string{"abcd"}, "xabcdx", "x(sensitive)x"},
		{"prefix overlap longest first", []string{"secret", "secret-token-9"}, "got secret-token-9 and secret", "got (sensitive) and (sensitive)"},
		{"prefix overlap reversed order", []string{"secret-token-9", "secret"}, "got secret-token-9", "got (sensitive)"},
		{"json escaped form", []string{"line1\nline2 \"q\""}, `{"detail":"line1\nline2 \"q\""}`, `{"detail":"(sensitive)"}`},
		{"multi-line each line", []string{"-----BEGIN KEY-----\nMIIabcdef\nxy\n-----END KEY-----"}, "bad MIIabcdef here; xy stays", "bad (sensitive) here; xy stays"},
		{"no values", nil, "plain", "plain"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NewRedactor(tc.values...).Redact(tc.in); got != tc.want {
				t.Errorf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactorNil checks that a nil Redactor is a no-op.
func TestRedactorNil(t *testing.T) {
	t.Parallel()
	var r *Redactor
	if got := r.Redact("keep me"); got != "keep me" {
		t.Errorf("nil Redact = %q", got)
	}
}

// TestFailureSummaryRedacts checks that stderr, validate JSON and the
// fallback never carry a secret value.
func TestFailureSummaryRedacts(t *testing.T) {
	t.Parallel()
	r := NewRedactor("s3cr3t-value")
	got := FailureSummary(r, StepApply, nil, "noise\nError: bad token s3cr3t-value\n", "")
	if got != "Error: bad token (sensitive)" {
		t.Errorf("stderr summary = %q", got)
	}
	stdout := []byte(`{"diagnostics":[{"severity":"error","summary":"bad s3cr3t-value","detail":"x\ny"}]}`)
	if got := FailureSummary(r, StepValidate, stdout, "", ""); got != "Error: bad (sensitive): x" {
		t.Errorf("validate summary = %q", got)
	}
	if got := FailureSummary(r, StepApply, nil, "", "step apply exited 1: s3cr3t-value"); got != "step apply exited 1: (sensitive)" {
		t.Errorf("fallback summary = %q", got)
	}
	if got := FailureSummary(nil, StepApply, nil, "Error: s3cr3t-value", ""); got != "Error: s3cr3t-value" {
		t.Errorf("nil redactor summary = %q", got)
	}
}

// TestFailureSummaryFitsAfterRedaction checks that redaction, which can
// lengthen text, still leaves a summary within MaxSummary and valid UTF-8,
// with no fragment of the value.
func TestFailureSummaryFitsAfterRedaction(t *testing.T) {
	t.Parallel()
	r := NewRedactor("abcd")
	stderr := "Error: " + strings.Repeat("abcd é ", 200)
	got := FailureSummary(r, StepApply, nil, stderr, "")
	if len(got) > MaxSummary || !utf8.ValidString(got) {
		t.Fatalf("summary len %d valid %v", len(got), utf8.ValidString(got))
	}
	if strings.Contains(got, "abcd") || !strings.HasPrefix(got, "Error: (sensitive) é") {
		t.Errorf("summary = %q", got)
	}
}
