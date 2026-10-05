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

package lint

import (
	"cmp"
	"fmt"
	"slices"
)

// Severity is how bad a finding is.
type Severity int

// Severities, least severe first.
const (
	SeverityInfo Severity = iota
	SeverityWarning
	SeverityError
)

// String returns s's name: "info", "warning" or "error", or
// "severity(N)" for an out-of-range value.
func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityWarning:
		return "warning"
	case SeverityError:
		return "error"
	}
	return fmt.Sprintf("severity(%d)", int(s))
}

// MarshalText writes s's name, as JSON output shows it, and returns it;
// the error return is always nil.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Finding is one problem in a module. File is relative to the module
// directory; File "" and Line 0 mean the module as a whole (something
// missing has no position).
type Finding struct {
	ID       string   `json:"id"`
	Severity Severity `json:"severity"`
	File     string   `json:"file"`
	Line     int      `json:"line"`
	Message  string   `json:"message"`
}

// Summary counts findings by severity; its JSON keys are error, warning and
// info.
type Summary struct {
	Errors   int `json:"error"`
	Warnings int `json:"warning"`
	Infos    int `json:"info"`
}

// Report is the findings of one run, ordered by file, line, ID and message,
// so output is stable across runs.
type Report struct {
	Findings []Finding `json:"findings"`
	Summary  Summary   `json:"summary"`
	// FileSet is the file set the hcl/v2 checks read: terraform, or
	// opentofu when the module ships .tofu files.
	FileSet string `json:"fileSet,omitempty"`
}

// NewReport orders findings by file, line, ID and message and counts them
// by severity, and returns the resulting Report (with a non-nil Findings
// slice even when findings is empty).
func NewReport(findings []Finding) Report {
	fs := slices.Clone(findings)
	if fs == nil {
		fs = []Finding{}
	}
	slices.SortFunc(fs, func(a, b Finding) int {
		return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Message, b.Message))
	})
	r := Report{Findings: fs}
	for _, f := range fs {
		switch f.Severity {
		case SeverityError:
			r.Summary.Errors++
		case SeverityWarning:
			r.Summary.Warnings++
		case SeverityInfo:
			r.Summary.Infos++
		}
	}
	return r
}
