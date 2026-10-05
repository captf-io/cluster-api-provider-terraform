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
	"regexp"
	"strings"

	"github.com/captf-io/cluster-api-provider-terraform/internal/strutil"
)

// MaxSummary is the byte cap of the failure summary written to Error.Tail:
// status.lastRun.error.summary's limit, enforced here too so the runner
// never depends on the controller to shrink it.
const MaxSummary = 512

// ansiEscape strips ANSI escape sequences: -no-color is always passed, but a
// provider or local-exec child of the runtime is not bound by that flag.
var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

// validateDiagnostics is the subset of `validate -json`'s output the runner
// reads (tf/cli/commands/validate#json-output-format; the OpenTofu format is
// the same, verified with OpenTofu 1.12.6).
type validateDiagnostics struct {
	Diagnostics []struct {
		Severity string `json:"severity"`
		Summary  string `json:"summary"`
		Detail   string `json:"detail"`
	} `json:"diagnostics"`
}

// diagnosticSummary builds a failure summary from a failing step's own
// output: for validate, the -json diagnostics captured from stdout; for
// every other step, the "Error: " diagnostic header lines from the stderr
// tail. It returns "" when neither yields anything (an unparseable
// validate output, or no diagnostic line in the tail), so the caller can
// fall back to a message that still names the step and its exit code. The
// full stderr still reaches the pod log unabridged; only this summary is
// capped, by the caller, at MaxSummary bytes. stderrTail is the stderr tail
// used when step is not StepValidate or stdout does not parse.
func diagnosticSummary(step string, stdout []byte, stderrTail string) string {
	var lines []string
	if step == StepValidate {
		lines = validateErrorLines(stdout)
	}
	if len(lines) == 0 {
		lines = stderrErrorLines(stderrTail)
	}
	return strings.Join(lines, "\n")
}

// FailureSummary builds the capped, redacted failure summary of a failing
// step: the diagnostic lines of its output (see diagnosticSummary), or
// fallback when there are none. Every input is redacted before any line is
// selected, so a value spanning lines or echoed in a JSON diagnostic is
// caught, and the cap comes last, because redaction can lengthen text and
// truncating first could leave a fragment of a value. A nil r redacts
// nothing. stdout is the step's captured stdout (read only for validate),
// stderrTail its stderr tail, and it returns the summary, at most MaxSummary bytes.
func FailureSummary(r *Redactor, step string, stdout []byte, stderrTail, fallback string) string {
	summary := diagnosticSummary(step, []byte(r.Redact(string(stdout))), r.Redact(stderrTail))
	if summary == "" {
		summary = r.Redact(fallback)
	}
	return strutil.Truncate(summary, MaxSummary)
}

// validateErrorLines returns stdout's validate -json error diagnostics as
// "Error: <summary>: <detail's first line>" lines, the CLI's own human
// rendering of a diagnostic header. A stdout that is not valid JSON (an
// unexpected runtime behavior) returns no lines, so the caller falls back
// to stderr.
func validateErrorLines(stdout []byte) []string {
	var v validateDiagnostics
	if err := json.Unmarshal(stdout, &v); err != nil {
		return nil
	}
	var lines []string
	for _, d := range v.Diagnostics {
		if d.Severity != "error" {
			continue
		}
		line := "Error: " + d.Summary
		if detail, _, _ := strings.Cut(d.Detail, "\n"); detail != "" {
			line += ": " + detail
		}
		lines = append(lines, line)
	}
	return lines
}

// stderrErrorLines returns every line of s beginning "Error: " (the first
// line of a Terraform/OpenTofu diagnostic block; verified with both
// runtimes), in order.
func stderrErrorLines(s string) []string {
	s = ansiEscape.ReplaceAllString(s, "")
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "Error: ") {
			lines = append(lines, l)
		}
	}
	return lines
}
