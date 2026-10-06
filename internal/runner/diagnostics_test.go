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
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// uiLines is a failing apply's `-json` output: a progress message, a
// warning, and an error naming a resource (summary and detail carry a
// secret), then one error without an address.
const uiLines = `{"@level":"info","@message":"Terraform 1.16.4","type":"version"}
{"@level":"info","@message":"aws_instance.web: Creating...","type":"apply_start"}
{"@level":"warn","@message":"Warning: deprecated","type":"diagnostic","diagnostic":{"severity":"warning","summary":"deprecated","detail":"use x"}}
{"@level":"error","@message":"Error: InvalidAMI hunter2hunter2","type":"diagnostic","diagnostic":{"severity":"error","summary":"InvalidAMI hunter2hunter2","detail":"the image\nis gone","address":"aws_instance.web"}}
{"@level":"error","@message":"Error: no quota","type":"diagnostic","diagnostic":{"severity":"error","summary":"no quota","detail":""}}
`

// TestUIRenderer checks that the JSON UI is rendered as readable, redacted
// lines, with errors on the error stream, and that error diagnostics are
// kept; a plain line passes through and a split write is reassembled.
func TestUIRenderer(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	u := newUIRenderer(&out, &errOut, NewRedactor("hunter2hunter2"))
	in := uiLines + "panic: boom"
	for chunk := range slices.Chunk([]byte(in), 7) {
		if _, err := u.Write(chunk); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	u.Flush()
	for _, want := range []string{"Terraform 1.16.4\n", "aws_instance.web: Creating...\n", "Warning: deprecated\n\nuse x\n", "panic: boom\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("out = %q, missing %q", out.String(), want)
		}
	}
	if strings.Contains(out.String()+errOut.String(), "hunter2") || strings.Contains(out.String(), `"@level"`) {
		t.Errorf("secret or raw JSON leaked: %q %q", out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "Error: InvalidAMI") || !strings.Contains(errOut.String(), "with aws_instance.web") || !strings.Contains(errOut.String(), "is gone") {
		t.Errorf("errOut = %q", errOut.String())
	}
	if ds := u.Diagnostics(); len(ds) != 2 || ds[0].Address != "aws_instance.web" {
		t.Errorf("diagnostics = %+v", ds)
	}
}

// TestFailedResources checks the resources list: addresses only, redacted,
// capped in count and size, without repeats.
func TestFailedResources(t *testing.T) {
	t.Parallel()
	ds := []Diagnostic{{Summary: "no address"}, {Address: "a.b", Summary: "boom hunter2hunter2\nmore"}, {Address: "a.b", Summary: "boom hunter2hunter2"}}
	got := failedResources(NewRedactor("hunter2hunter2"), ds)
	if len(got) != 1 || strings.Contains(got[0], "hunter2") || !strings.HasPrefix(got[0], "a.b: boom ") {
		t.Errorf("got %q", got)
	}
	var many []Diagnostic
	for i := range 30 {
		many = append(many, Diagnostic{Address: "r.x" + strings.Repeat("y", i), Summary: strings.Repeat("s", 600)})
	}
	got = failedResources(nil, many)
	if len(got) != MaxErrorResources || len(got[0]) != MaxResourceBytes {
		t.Errorf("got %d resources, first %d bytes", len(got), len(got[0]))
	}
	if failedResources(nil, nil) != nil {
		t.Error("no diagnostics gave resources")
	}
}

// TestEncodeShrinksResourcesFirst checks that Encode sheds the failing
// resources before it cuts into the error tail.
func TestEncodeShrinksResourcesFirst(t *testing.T) {
	t.Parallel()
	tail := strings.Repeat("t", 1500)
	r := Result{Version: ResultVersion, Op: OpApply, Error: &Error{Kind: ErrorKindStep, Step: new("apply"), Tail: tail,
		Resources: slices.Repeat([]string{strings.Repeat("r", 400)}, MaxErrorResources)}}
	var got Result
	b := Encode(r)
	if len(b) > MaxResultBytes || json.Unmarshal(b, &got) != nil {
		t.Fatalf("encoded %d bytes", len(b))
	}
	if got.Error.Tail != tail || len(got.Error.Resources) == 0 || len(got.Error.Resources) >= MaxErrorResources {
		t.Errorf("tail kept %v, %d resources", got.Error.Tail == tail, len(got.Error.Resources))
	}
	r.Error.Tail = strings.Repeat("t", 4000)
	got = Result{}
	if err := json.Unmarshal(Encode(r), &got); err != nil || got.Error.Resources != nil || got.Error.Tail == "" {
		t.Errorf("resources %v, tail %d bytes", got.Error.Resources, len(got.Error.Tail))
	}
}

// TestRunApplyFailureResources runs a failing apply whose JSON diagnostics
// name a resource: the result carries the summary and resources, redacted,
// and the log is readable.
func TestRunApplyFailureResources(t *testing.T) {
	t.Parallel()
	f := newFixture(t, OpApply, "FAKE_EXIT_APPLY=1", "FAKE_STDOUT_APPLY="+uiLines, "AWS_SECRET_ACCESS_KEY=hunter2hunter2")
	r, code := Run(t.Context(), f.opts)
	if code != ExitFailure || r.Error == nil || r.Error.Step == nil || *r.Error.Step != StepApply {
		t.Fatalf("code %d, result %+v", code, r)
	}
	if want := []string{"aws_instance.web: InvalidAMI " + redactedMarker(t)}; len(r.Error.Resources) != 1 || r.Error.Resources[0] != want[0] {
		t.Errorf("resources = %q, want %q", r.Error.Resources, want)
	}
	if !strings.HasPrefix(r.Error.Tail, "Error: InvalidAMI") || !strings.Contains(r.Error.Tail, "no quota") || strings.Contains(r.Error.Tail, "hunter2") {
		t.Errorf("tail = %q", r.Error.Tail)
	}
	if got := f.stdout.String(); !strings.Contains(got, "aws_instance.web: Creating...") || strings.Contains(got, "hunter2") {
		t.Errorf("log = %q", got)
	}
}

// redactedMarker returns what a Redactor replaces a secret with; it fails
// t on nothing, t only marks the function a helper.
func redactedMarker(t *testing.T) string {
	t.Helper()
	return NewRedactor("hunter2hunter2").Redact("hunter2hunter2")
}

// TestRunApplyChangesFromJSON checks that the change counts are still read
// from a successful apply's JSON output.
func TestRunApplyChangesFromJSON(t *testing.T) {
	t.Parallel()
	out := `{"@message":"aws_instance.web: Creation complete","type":"apply_complete"}` + "\n" +
		`{"@message":"Apply complete! Resources: 1 added, 2 changed, 3 destroyed.","type":"change_summary"}` + "\n"
	f := newFixture(t, OpApply, "FAKE_STDOUT_APPLY="+out)
	r, code := Run(t.Context(), f.opts)
	if code != ExitOK || r.Changes == nil || *r.Changes != (Changes{Add: 1, Change: 2, Destroy: 3}) {
		t.Fatalf("code %d, changes %+v", code, r.Changes)
	}
}
