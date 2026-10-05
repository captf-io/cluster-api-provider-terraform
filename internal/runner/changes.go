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
	"bytes"
	"regexp"
	"strconv"
	"sync"
)

// maxSummaryLine bounds the partial line the summary scanner buffers: the
// summary line is short, and a longer line (a provider's debug output) is
// skipped up to its newline rather than kept.
const maxSummaryLine = 512

// Both runtimes end a successful apply or destroy with one of these lines
// (Terraform internal/command/views/apply.go, OpenTofu the same): "Apply
// complete! Resources: [N imported, ]N added, N changed, N destroyed[, N
// forgotten]." and "Destroy complete! Resources: N destroyed[, N
// forgotten].". The trailing part is not matched, so the forgotten count of
// newer releases does not hide the line.
var (
	applySummary   = regexp.MustCompile(`^Apply complete! Resources: (?:(\d+) imported, )?(\d+) added, (\d+) changed, (\d+) destroyed`)
	destroySummary = regexp.MustCompile(`^Destroy complete! Resources: (\d+) destroyed`)
)

// changesScanner is an io.Writer that watches an apply or destroy step's
// stdout, line by line, for the runtime's final summary line. It keeps only
// the current partial line (at most maxSummaryLine bytes) and the last
// summary it parsed, never the output itself.
type changesScanner struct {
	mu      sync.Mutex
	line    []byte
	skip    bool
	changes *Changes
}

// Write scans p for complete lines, parsing each finished one as a possible
// summary line, and always reports len(p) written with a nil error: a
// changesScanner never fails a write.
func (s *changesScanner) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.add(p)
			break
		}
		s.add(p[:i])
		if !s.skip {
			s.parse(s.line)
		}
		s.line, s.skip = s.line[:0], false
		p = p[i+1:]
	}
	return n, nil
}

// add appends p, part of a line, or marks the line skipped once too long.
func (s *changesScanner) add(p []byte) {
	if s.skip {
		return
	}
	if len(s.line)+len(p) > maxSummaryLine {
		s.line, s.skip = s.line[:0], true
		return
	}
	s.line = append(s.line, p...)
}

// parse records line when it is a summary line.
func (s *changesScanner) parse(line []byte) {
	line = bytes.TrimRight(ansiEscape.ReplaceAll(line, nil), "\r")
	if m := applySummary.FindSubmatch(line); m != nil {
		s.changes = &Changes{Import: atoi(m[1]), Add: atoi(m[2]), Change: atoi(m[3]), Destroy: atoi(m[4])}
		return
	}
	if m := destroySummary.FindSubmatch(line); m != nil {
		s.changes = &Changes{Destroy: atoi(m[1])}
	}
}

// Changes returns the last summary seen, including an unterminated last
// line, or nil.
func (s *changesScanner) Changes() *Changes {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.skip && len(s.line) > 0 {
		s.parse(s.line)
		s.line = s.line[:0]
	}
	if s.changes == nil {
		return nil
	}
	c := *s.changes
	return &c
}

// atoi returns b parsed as a decimal integer, or 0 if it is not one.
func atoi(b []byte) int {
	n, _ := strconv.Atoi(string(b))
	return n
}
