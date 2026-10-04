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

package app

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/spf13/cobra"

	"github.com/captf-io/cluster-api-provider-terraform/internal/lint"
	"github.com/captf-io/cluster-api-provider-terraform/internal/lint/image"
)

// jsonReport is the --json document: "module" for a directory, "image" for
// an image.
type jsonReport struct {
	Module   string         `json:"module,omitempty"`
	Image    *image.Info    `json:"image,omitempty"`
	Contract string         `json:"contract"`
	Role     string         `json:"role"`
	Findings []lint.Finding `json:"findings"`
	Summary  lint.Summary   `json:"summary"`
}

// applyAllow downgrades report's warnings whose check ID appears in allow
// to info, noting why in the message, recounts the summary, and returns
// the updated report.
func applyAllow(report lint.Report, allow []string) lint.Report {
	if len(allow) == 0 {
		return report
	}
	var s lint.Summary
	for i := range report.Findings {
		f := &report.Findings[i]
		if f.Severity == lint.SeverityWarning && slices.Contains(allow, f.ID) {
			f.Severity = lint.SeverityInfo
			f.Message += " (allowed by --allow-warning)"
		}
		switch f.Severity {
		case lint.SeverityError:
			s.Errors++
		case lint.SeverityWarning:
			s.Warnings++
		default:
			s.Infos++
		}
	}
	report.Summary = s
	return report
}

// finish applies f's --allow-warning downgrades to report, then prints it
// to cmd's output stream: as JSON headed by head when f.json is set,
// otherwise as text. A JSON encoding failure is printed to cmd's error
// stream. It returns nil for a clean run or one with only non-strict
// warnings, or an *exitCodeErr for ExitFindings or ExitUnparsable.
func finish(cmd *cobra.Command, f commonFlags, head jsonReport, report lint.Report) error {
	report = applyAllow(report, f.allow)
	if f.json {
		head.Findings, head.Summary = report.Findings, report.Summary
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(head); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", cmd.CommandPath(), err)
			return &exitCodeErr{Code: ExitUnparsable}
		}
	} else {
		writeText(cmd.OutOrStdout(), report)
	}
	s := report.Summary
	if s.Errors > 0 || (f.strict && s.Warnings > 0) {
		return &exitCodeErr{Code: ExitFindings}
	}
	return nil
}

// writeText prints r to w as one line per finding, <severity> <id>
// <file>:<line> <message>, then a summary line. A finding about the module
// or image as a whole has file "-".
func writeText(w io.Writer, r lint.Report) {
	for _, f := range r.Findings {
		file := f.File
		if file == "" {
			file = "-"
		}
		fmt.Fprintf(w, "%s %s %s:%d %s\n", f.Severity, f.ID, file, f.Line, f.Message)
	}
	s := r.Summary
	set := r.FileSet
	if set == "" {
		set = "no module"
	}
	fmt.Fprintf(w, "%d error(s), %d warning(s), %d info (read %s files)\n", s.Errors, s.Warnings, s.Infos, set)
}
