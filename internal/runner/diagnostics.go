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

// This file reads the machine-readable UI (`apply -json`, `destroy -json`)
// of an apply or destroy step: it renders each message as a readable log
// line and keeps the error diagnostics, which name the failing resource.

package runner

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/captf-io/cluster-api-provider-terraform/internal/strutil"
)

// MaxErrorResources is the most failing resources Error.Resources lists,
// and MaxResourceBytes the size cap of each entry (status.lastRun.error's
// limits).
const (
	MaxErrorResources = 10
	MaxResourceBytes  = 512
)

// maxUILine bounds one buffered line of the JSON UI: a longer line (an
// enormous plan diff message) is dropped up to its newline, and a note
// says so (omittedLine), rather than kept whole or written unredacted.
const maxUILine = 1 << 20

// Diagnostic is the subset of a `-json` UI diagnostic the runner reads:
// the `diagnostic` object of a message whose type is "diagnostic" (the
// machine-readable UI of Terraform and of OpenTofu, which share the
// format: severity, summary, detail and, for a resource-related error,
// address, which is often null, even for a failure of one resource, such
// as a failed postcondition; verified with OpenTofu 1.11.5, Terraform
// documents the same fields). Snippet is the source excerpt, whose
// context names the block the diagnostic is about.
type Diagnostic struct {
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
	Detail   string `json:"detail"`
	Address  string `json:"address"`
	Snippet  *struct {
		Context string `json:"context"`
	} `json:"snippet"`
}

// uiHook is the hook of an "apply_errored" message: the resource whose
// create, update or delete failed. Its address carries the module path.
type uiHook struct {
	Resource struct {
		Addr string `json:"addr"`
	} `json:"resource"`
}

// snippetResource matches the context of a diagnostic about a resource or
// data source block: `resource "TYPE" "NAME"` or `data "TYPE" "NAME"`.
var snippetResource = regexp.MustCompile(`^(resource|data) "([^"]+)" "([^"]+)"$`)

// uiMessage is one line of the JSON UI.
type uiMessage struct {
	Message    string      `json:"@message"`
	Type       string      `json:"type"`
	Diagnostic *Diagnostic `json:"diagnostic"`
	Hook       *uiHook     `json:"hook"`
}

// uiRenderer is an io.Writer for the stdout of a `-json` step. It renders
// each JSON message as the human text the runtime prints without -json:
// to out, or for an error or warning diagnostic to errOut, so the pod log
// stays readable. It keeps the error diagnostics. Text that is not a JSON
// message (a panic, a runtime that ignored -json) passes through as is.
// Everything rendered is redacted first, whole, so a value spanning the
// lines of a diagnostic is caught.
type uiRenderer struct {
	out, errOut io.Writer
	red         *Redactor

	mu    sync.Mutex
	line  []byte
	skip  bool
	diags []Diagnostic
	// errored are the addresses of "apply_errored" messages, in order.
	errored []string
}

// newUIRenderer returns a uiRenderer writing messages to out and
// diagnostics to errOut, redacting with red (nil redacts nothing).
func newUIRenderer(out, errOut io.Writer, red *Redactor) *uiRenderer {
	return &uiRenderer{out: out, errOut: errOut, red: red}
}

// Write renders every complete line of p and buffers the rest. It always
// reports len(p) written with a nil error: a log write failure must not
// fail the step.
func (u *uiRenderer) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			u.add(p)
			break
		}
		u.add(p[:i])
		u.flushLine()
		p = p[i+1:]
	}
	return n, nil
}

// add buffers p, part of a line. A line that grows past maxUILine is
// dropped, up to its newline: it cannot be redacted whole, and a part of
// it written as it comes could carry a credential across the cut.
func (u *uiRenderer) add(p []byte) {
	if u.skip {
		return
	}
	if len(u.line)+len(p) > maxUILine {
		u.skip = true
		u.line = u.line[:0]
		return
	}
	u.line = append(u.line, p...)
}

// omittedLine replaces an oversized UI line in the log.
const omittedLine = "(a runtime output line over 1 MiB was omitted from the log)\n"

// flushLine renders the buffered line, if any, and resets the buffer.
func (u *uiRenderer) flushLine() {
	if u.skip {
		_, _ = io.WriteString(u.out, omittedLine)
	} else if len(u.line) > 0 {
		u.render(u.line)
	}
	u.line, u.skip = u.line[:0], false
}

// Flush renders an unterminated last line.
func (u *uiRenderer) Flush() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.flushLine()
}

// Diagnostics returns the error diagnostics seen so far, in order.
func (u *uiRenderer) Diagnostics() []Diagnostic {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]Diagnostic(nil), u.diags...)
}

// Errored returns the addresses of the resources whose apply failed
// ("apply_errored" messages), in order.
func (u *uiRenderer) Errored() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.errored...)
}

// render writes line, one JSON UI message or other text, to the log.
func (u *uiRenderer) render(line []byte) {
	var m uiMessage
	if json.Unmarshal(line, &m) != nil || (m.Type == "" && m.Message == "") {
		u.write(u.out, string(line))
		return
	}
	if m.Type == "diagnostic" && m.Diagnostic != nil {
		d := *m.Diagnostic
		switch d.Severity {
		case "error":
			u.diags = append(u.diags, d)
			u.write(u.errOut, formatDiagnostic("Error", d))
		case "warning":
			u.write(u.out, formatDiagnostic("Warning", d))
		default:
			u.write(u.out, m.Message)
		}
		return
	}
	if m.Type == "apply_errored" && m.Hook != nil && m.Hook.Resource.Addr != "" {
		u.errored = append(u.errored, m.Hook.Resource.Addr)
	}
	if m.Message != "" {
		u.write(u.out, m.Message)
	}
}

// write writes s, redacted, and a newline to w.
func (u *uiRenderer) write(w io.Writer, s string) {
	_, _ = io.WriteString(w, ansiEscape.ReplaceAllString(u.red.Redact(s), "")+"\n")
}

// syncWriter serializes writes to w: a step's stdout renderer and its
// stderr copy write the same error stream from two goroutines.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// Write writes p to the wrapped writer, one caller at a time, and returns
// its count and error.
func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// formatDiagnostic renders d as the runtime's human output does: a
// "<label>: <summary>" header, the resource it is about, and its detail. It
// returns the text, newline terminated.
func formatDiagnostic(label string, d Diagnostic) string {
	var b strings.Builder
	b.WriteString(label + ": " + d.Summary)
	if d.Address != "" {
		b.WriteString("\n\n  with " + d.Address)
	}
	if d.Detail != "" {
		b.WriteString("\n\n" + d.Detail)
	}
	return b.String() + "\n"
}

// errorLines returns ds as "Error: <summary>[: <detail's first line>]"
// lines, the same form as a validate diagnostic's.
func errorLines(ds []Diagnostic) []string {
	var lines []string
	for _, d := range ds {
		line := "Error: " + d.Summary
		if detail, _, _ := strings.Cut(d.Detail, "\n"); detail != "" {
			line += ": " + detail
		}
		lines = append(lines, line)
	}
	return lines
}

// failedResources returns the "<address>: <summary>" entries of ds's
// diagnostics, redacted with red and capped: at most MaxErrorResources of
// at most MaxResourceBytes bytes each, without repeats. nil when no
// diagnostic can be tied to a resource. A diagnostic's resource is, in
// this order: its own address (often null); else the next address in
// errored, the "apply_errored" addresses in the runtime's order (one is
// used up by a diagnostic naming it); else, from its source snippet's
// context `resource "T" "N"` or `data "T" "N"`, "T.N" or "data.T.N". That
// last form has no module path: the snippet does not carry one.
func failedResources(red *Redactor, ds []Diagnostic, errored []string) []string {
	queue := slices.Clone(errored)
	var out []string
	for _, d := range ds {
		addr := d.Address
		if addr != "" {
			if i := slices.Index(queue, addr); i >= 0 {
				queue = slices.Delete(queue, i, i+1)
			}
		} else if len(queue) > 0 {
			addr, queue = queue[0], queue[1:]
		} else {
			addr = snippetAddress(d)
		}
		if addr == "" {
			continue
		}
		entry := strutil.Truncate(red.Redact(addr+": "+firstLine(d.Summary)), MaxResourceBytes)
		if slices.Contains(out, entry) {
			continue
		}
		out = append(out, entry)
		if len(out) == MaxErrorResources {
			break
		}
	}
	return out
}

// snippetAddress returns the resource address d's snippet context names
// ("T.N", "data.T.N"), or "".
func snippetAddress(d Diagnostic) string {
	if d.Snippet == nil {
		return ""
	}
	m := snippetResource.FindStringSubmatch(d.Snippet.Context)
	switch {
	case m == nil:
		return ""
	case m[1] == "data":
		return "data." + m[2] + "." + m[3]
	}
	return m[2] + "." + m[3]
}

// firstLine returns s up to its first newline.
func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
