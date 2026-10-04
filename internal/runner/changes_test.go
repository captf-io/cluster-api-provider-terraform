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
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestChangesScanner checks changesScanner.Write and Changes against apply
// and destroy summary lines split, colored or padded in various ways,
// including a line longer than the buffer and one that never appears at
// the start of a line.
func TestChangesScanner(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		chunks []string
		want   *Changes
	}{
		{name: "none", chunks: []string{"module.role.x: Creating...\n"}},
		{name: "apply", chunks: []string{"x: Creation complete\n\nApply complete! Resources: 3 added, 1 changed, 2 destroyed.\n"},
			want: &Changes{Add: 3, Change: 1, Destroy: 2}},
		{name: "imported", chunks: []string{"Apply complete! Resources: 1 imported, 0 added, 0 changed, 0 destroyed.\n"},
			want: &Changes{Import: 1}},
		{name: "forgotten suffix", chunks: []string{"Apply complete! Resources: 0 added, 0 changed, 0 destroyed, 2 forgotten.\n"},
			want: &Changes{}},
		{name: "destroy", chunks: []string{"Destroy complete! Resources: 7 destroyed.\r\n"}, want: &Changes{Destroy: 7}},
		{name: "split across writes, no final newline", chunks: []string{"Apply comp", "lete! Resources: 1", "2 added, 0 changed, 0 destroyed."},
			want: &Changes{Add: 12}},
		{name: "colored", chunks: []string{"\x1b[0m\x1b[1m\x1b[32mApply complete! Resources: 1 added, 0 changed, 0 destroyed.\x1b[0m\n"},
			want: &Changes{Add: 1}},
		{name: "not at line start", chunks: []string{"echo Apply complete! Resources: 1 added, 0 changed, 0 destroyed.\n"}},
		// A line longer than the buffer is skipped, not kept, and the
		// scanner recovers at the next line.
		{name: "overlong line", chunks: []string{strings.Repeat("x", 3*maxSummaryLine), "Apply complete! Resources: 9 added, 0 changed, 0 destroyed.\n",
			"\nApply complete! Resources: 5 added, 0 changed, 0 destroyed.\n"}, want: &Changes{Add: 5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := &changesScanner{}
			for _, chunk := range c.chunks {
				if n, err := s.Write([]byte(chunk)); n != len(chunk) || err != nil {
					t.Fatalf("Write = %d, %v", n, err)
				}
			}
			got := s.Changes()
			if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
				t.Errorf("Changes = %+v, want %+v", got, c.want)
			}
			if cap(s.line) > 2*maxSummaryLine {
				t.Errorf("buffered %d bytes", cap(s.line))
			}
		})
	}
}

// TestRunChanges: the apply and destroy steps' summary lines reach the
// result while their output still reaches the log; other steps are not
// scanned.
func TestRunChanges(t *testing.T) {
	t.Parallel()
	const applied = "Apply complete! Resources: 2 added, 1 changed, 0 destroyed.\n"
	cases := []struct {
		name  string
		op    string
		guard bool
		env   []string
		want  *Changes
	}{
		{name: "apply", op: OpApply, env: []string{"FAKE_STDOUT_APPLY=" + applied}, want: &Changes{Add: 2, Change: 1}},
		{name: "guarded apply of a saved plan", op: OpApply, guard: true,
			env:  []string{"FAKE_EXIT_PLAN=2", `FAKE_STDOUT_SHOW={"resource_changes":[{"address":"a","change":{"actions":["create"]}}]}`, "FAKE_STDOUT_APPLY=" + applied},
			want: &Changes{Add: 2, Change: 1}},
		{name: "destroy", op: OpDestroy, env: []string{"FAKE_STDOUT_DESTROY=Destroy complete! Resources: 4 destroyed.\n"}, want: &Changes{Destroy: 4}},
		{name: "refresh is not scanned", op: OpRefresh, env: []string{"FAKE_STDOUT_APPLY_REFRESH_ONLY=" + applied}},
		{name: "failed apply", op: OpApply, env: []string{"FAKE_EXIT_APPLY=1", "FAKE_STDOUT_APPLY=partial\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, c.op, c.env...)
			f.opts.GuardDeletes, f.opts.InputsHash = c.guard, "h1:x"
			r, _ := Run(context.Background(), f.opts)
			if (r.Changes == nil) != (c.want == nil) || (r.Changes != nil && *r.Changes != *c.want) {
				t.Errorf("Changes = %+v, want %+v", r.Changes, c.want)
			}
			if c.want != nil && !strings.Contains(f.stdout.String(), "complete! Resources:") {
				t.Errorf("the summary line did not reach the log: %q", f.stdout)
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(Encode(r), &doc); err != nil {
				t.Fatal(err)
			}
			if _, ok := doc["changes"]; ok != (c.want != nil) {
				t.Errorf("encoded changes present = %v: %s", ok, Encode(r))
			}
		})
	}
}

// TestEncodeDropsChangesFirst: a result just over the cap loses its
// resource changes before any byte of its error summary.
func TestEncodeDropsChangesFirst(t *testing.T) {
	t.Parallel()
	r := Result{Version: ResultVersion, Op: OpApply, Steps: []Step{{Name: StepInit}},
		Error:   &Error{Kind: ErrorKindStep, Step: new(StepApply), Tail: "x"},
		Changes: &Changes{Add: 1, Change: 2, Destroy: 3, Import: 4}}
	base := len(Encode(r))
	pad := MaxResultBytes - base + 10
	r.Error.Tail = strings.Repeat("e", pad)
	b := Encode(r)
	var got Result
	if err := json.Unmarshal(b, &got); err != nil || len(b) > MaxResultBytes {
		t.Fatalf("encoded %d bytes: %v", len(b), err)
	}
	if got.Changes != nil || got.Error == nil || got.Error.Tail != r.Error.Tail {
		t.Errorf("changes %+v, tail kept %v", got.Changes, got.Error != nil && got.Error.Tail == r.Error.Tail)
	}
	// Even large counts cost a few dozen bytes.
	if c, _ := json.Marshal(Changes{Add: 99999, Change: 99999, Destroy: 99999, Import: 99999}); len(c) > 64 {
		t.Errorf("changes costs %d bytes: %s", len(c), c)
	}
}

// TestStepLabel checks StepLabel's fixed names against a name it never
// changes and a name it maps to StepOther.
func TestStepLabel(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		StepInit: StepInit, StepApplyRefreshOnly: StepApplyRefreshOnly, StepShowJSON: StepShowJSON,
		StepPrepare: StepPrepare, "version": StepOther, "": StepOther, "x; rm -rf": StepOther,
	} {
		if got := StepLabel(name); got != want {
			t.Errorf("StepLabel(%q) = %q, want %q", name, got, want)
		}
	}
}
