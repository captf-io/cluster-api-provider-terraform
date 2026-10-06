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
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// reorderedPlan is destructivePlan's changes in another order, with a no-op
// and an input variable that differs: the same plan for review.
const reorderedPlan = `{"variables":{"bootstrap_data":{"value":"b3RoZXI="}},"resource_changes":[` +
	`{"address":"module.role.tags","change":{"actions":["update"]}},` +
	`{"address":"module.role.keep","change":{"actions":["no-op"],"before":{"x":1},"after":{"x":1}}},` +
	`{"address":"module.role.old","change":{"actions":["delete"]}},` +
	`{"address":"module.role.net","change":{"actions":["create","delete"]}},` +
	`{"address":"module.role.lb","change":{"actions":["delete","create"],"after":{"name":"c2VjcmV0"}}}]}`

// TestPlanHash: the fingerprint depends on the changes, not on their
// order, no-ops or input variables; the two replace orders differ; a plan
// without changes has EmptyPlanHash.
func TestPlanHash(t *testing.T) {
	t.Parallel()
	if got := PlanHash(nil); got != EmptyPlanHash {
		t.Errorf("PlanHash(nil) = %s, want EmptyPlanHash %s", got, EmptyPlanHash)
	}
	a, b := mustPlan(t, destructivePlan), mustPlan(t, reorderedPlan)
	if a.Hash != b.Hash || !strings.HasPrefix(a.Hash, PlanHashPrefix) {
		t.Errorf("hashes differ across ordering: %s vs %s", a.Hash, b.Hash)
	}
	replace := func(actions string) string {
		return mustPlan(t, `{"resource_changes":[{"address":"x","change":{"actions":[`+actions+`]}}]}`).Hash
	}
	if replace(`"delete","create"`) == replace(`"create","delete"`) {
		t.Error("the two replace orders hash alike")
	}
	if PlanHash([]string{"x|update"}) == PlanHash([]string{"y|update"}) {
		t.Error("different addresses hash alike")
	}
	empty := mustPlan(t, `{"resource_changes":[{"address":"a","change":{"actions":["no-op"]}}]}`)
	if empty.Hash != EmptyPlanHash || len(empty.Resources) != 0 {
		t.Errorf("no-op plan = %+v", empty)
	}
}

// importPlan returns a plan that creates module.role.x and imports
// module.role.y (a no-op otherwise) with the importing object importing.
func importPlan(importing string) string {
	return `{"resource_changes":[` +
		`{"address":"module.role.x","change":{"actions":["create"],"after":{"n":1}}},` +
		`{"address":"module.role.y","change":{"actions":["no-op"],"before":{"id":"1"},"after":{"id":"1"},"importing":` + importing + `}}]}`
}

// TestPlanHashImportsAndMoves: a resource the plan imports or moves is a
// change even when its action is a no-op: it is fingerprinted, with what it
// imports and where from it moves, and listed as "(import)" or "(move)",
// but it is neither drift nor an add, change or destroy.
func TestPlanHashImportsAndMoves(t *testing.T) {
	t.Parallel()
	const (
		importOnly = `{"resource_changes":[{"address":"module.role.y","change":{"actions":["no-op"],"importing":{"id":"i-1"}}}]}`
		moveOnly   = `{"resource_changes":[{"address":"module.role.new","previous_address":"module.role.old","change":{"actions":["no-op"]}}]}`
		moveOther  = `{"resource_changes":[{"address":"module.role.new","previous_address":"module.role.older","change":{"actions":["no-op"]}}]}`
		both       = `{"resource_changes":[{"address":"module.role.z","previous_address":"module.role.w",` +
			`"change":{"actions":["update"],"before":{"n":1},"after":{"n":2},"importing":{"id":"z"}}}]}`
		nullImport = `{"resource_changes":[{"address":"module.role.y","change":{"actions":["no-op"],"importing":null}}]}`
	)
	imp := mustPlan(t, importOnly)
	if imp.Hash == EmptyPlanHash || imp.Import != 1 || imp.Create+imp.Update+imp.Replace+imp.Delete+imp.Move != 0 ||
		!slices.Equal(imp.Resources, []string{"module.role.y (import)"}) {
		t.Errorf("import-only plan = %+v", imp)
	}
	if !strings.Contains(imp.counts(), "1 to import") {
		t.Errorf("counts = %q", imp.counts())
	}
	if mustPlan(t, importPlan(`{"id":"1"}`)).Hash == mustPlan(t, importPlan(`{"id":"2"}`)).Hash {
		t.Error("two import IDs hash alike")
	}
	if mustPlan(t, importPlan(`{"identity":{"name":"a"}}`)).Hash == mustPlan(t, importPlan(`{"identity":{"name":"b"}}`)).Hash {
		t.Error("two import identities hash alike")
	}
	mv := mustPlan(t, moveOnly)
	if mv.Hash == EmptyPlanHash || mv.Move != 1 || mv.Create+mv.Update+mv.Replace+mv.Delete+mv.Import != 0 ||
		!slices.Equal(mv.Resources, []string{"module.role.new (move)"}) {
		t.Errorf("move-only plan = %+v", mv)
	}
	if mv.Hash == mustPlan(t, moveOther).Hash {
		t.Error("two moves from different addresses hash alike")
	}
	if b := mustPlan(t, both); b.Update != 1 || b.Import != 1 || b.Move != 1 ||
		!slices.Equal(b.Resources, []string{"module.role.z (update, import, move)"}) {
		t.Errorf("updated, imported and moved = %+v", b)
	}
	if n := mustPlan(t, nullImport); n.Hash != EmptyPlanHash || n.Import != 0 {
		t.Errorf("null importing = %+v", n)
	}
	for _, plan := range []string{importOnly, moveOnly} {
		if d, err := ParseDrift([]byte(plan)); err != nil || d.Detected || len(d.Resources) != 0 {
			t.Errorf("ParseDrift(%s) = %+v, %v; want no drift", plan, d, err)
		}
	}
}

// TestParsePlan: counts like the CLI, sorted "<address> (<action>)"
// entries, and at most MaxPlanResources of them.
func TestParsePlan(t *testing.T) {
	t.Parallel()
	p := mustPlan(t, destructivePlan)
	want := []string{"module.role.lb (replace)", "module.role.net (replace)", "module.role.old (delete)", "module.role.tags (update)"}
	if p.Create != 0 || p.Update != 1 || p.Replace != 2 || p.Delete != 1 || !slices.Equal(p.Resources, want) || p.Truncated {
		t.Errorf("plan = %+v", p)
	}
	var changes []string
	for i := range MaxPlanResources + 5 {
		changes = append(changes, fmt.Sprintf(`{"address":"r.%03d","change":{"actions":["create"]}}`, i))
	}
	big, err := ParsePlan([]byte(`{"resource_changes":[`+strings.Join(changes, ",")+`]}`), []byte(testPlanKey))
	if err != nil || big.Create != MaxPlanResources+5 || len(big.Resources) != MaxPlanResources || !big.Truncated || big.Resources[0] != "r.000 (create)" {
		t.Errorf("big plan = %d resources, truncated %v, %v", len(big.Resources), big.Truncated, err)
	}
	if _, err := ParsePlan([]byte("{"), []byte(testPlanKey)); err == nil {
		t.Error("bad JSON accepted")
	}
}

// TestRunPlan: a plan Job runs init, validate, plan and show, applies
// nothing and reports the plan (never its values); a plan without changes
// reports EmptyPlanHash without a show.
func TestRunPlan(t *testing.T) {
	t.Parallel()
	f := newFixture(t, OpPlan, "FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW="+destructivePlan)
	stderr := &bytes.Buffer{}
	f.opts.Stderr = stderr
	ctx, log := logContext(t)
	r, code := Run(ctx, f.opts)
	if code != 0 || r.Error != nil {
		t.Fatalf("run = %d %+v", code, r.Error)
	}
	if got := f.calls(t); !slices.Equal(got, []string{"VERSION", "INIT", "VALIDATE", "PLAN", "SHOW"}) {
		t.Errorf("calls = %v", got)
	}
	want := mustPlan(t, destructivePlan)
	if r.Plan == nil || r.Plan.Hash != want.Hash || r.Plan.Replace != 2 || len(r.Plan.Resources) != 4 {
		t.Errorf("plan = %+v", r.Plan)
	}
	for _, out := range []string{log.String(), stderr.String(), f.stdout.String(), string(Encode(r))} {
		if strings.Contains(out, "c2VjcmV0") {
			t.Errorf("a plan value leaked: %s", out)
		}
	}
	if !strings.Contains(log.String(), want.Hash) {
		t.Errorf("the log does not name the plan hash: %s", log)
	}

	none := newFixture(t, OpPlan)
	r, code = Run(context.Background(), none.opts)
	if code != 0 || r.Plan == nil || r.Plan.Hash != EmptyPlanHash {
		t.Fatalf("no-change plan = %d %+v", code, r.Plan)
	}
	if got := none.calls(t); !slices.Equal(got, []string{"VERSION", "INIT", "VALIDATE", "PLAN"}) {
		t.Errorf("no-change calls = %v", got)
	}
}

// TestRunExpectPlan: an approved apply (--expect-plan) applies its saved
// plan when the plan's hash is the approved one, deletes included, and
// otherwise stops before the apply step with plan-changed and the new plan.
func TestRunExpectPlan(t *testing.T) {
	t.Parallel()
	approved := mustPlan(t, destructivePlan)
	const (
		lbUpdate   = `{"resource_changes":[{"address":"module.role.lb","change":{"actions":["update"]}}]}`
		endpointV1 = `{"output_changes":{"endpoint":{"actions":["update"],"before":"a","after":"v1"}},"resource_changes":[]}`
		endpointV2 = `{"output_changes":{"endpoint":{"actions":["update"],"before":"a","after":"v2"}},"resource_changes":[]}`
	)
	outputs := mustPlan(t, endpointV1)
	applied := []string{"VERSION", "INIT", "VALIDATE", "PLAN", "SHOW", "APPLY"}
	stopped := []string{"VERSION", "INIT", "VALIDATE", "PLAN", "SHOW"}
	cases := []struct {
		name    string
		env     []string
		expect  string
		calls   []string
		changed string // the new plan's hash when it stops
	}{
		{name: "the approved plan applies, deletes included", env: []string{"FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW=" + reorderedPlan},
			expect: approved.Hash, calls: applied},
		{name: "a changed plan stops", env: []string{"FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW=" + lbUpdate},
			expect: approved.Hash, calls: stopped, changed: mustPlan(t, lbUpdate).Hash},
		{name: "an approved output-only plan applies", env: []string{"FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW=" + endpointV1},
			expect: outputs.Hash, calls: applied},
		{name: "an output-only plan with another value stops", env: []string{"FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW=" + endpointV2},
			expect: outputs.Hash, calls: stopped, changed: mustPlan(t, endpointV2).Hash},
		{name: "an approved empty plan is done without an apply", expect: EmptyPlanHash, calls: []string{"VERSION", "INIT", "VALIDATE", "PLAN"}},
		{name: "a plan that emptied stops", expect: approved.Hash, calls: []string{"VERSION", "INIT", "VALIDATE", "PLAN"}, changed: EmptyPlanHash},
		{name: "changes where none were approved stop", env: []string{"FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW=" + destructivePlan},
			expect: EmptyPlanHash, calls: stopped, changed: approved.Hash},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, OpApply, c.env...)
			stderr := &bytes.Buffer{}
			f.opts.Stderr = stderr
			// The destructive guard is on, without an approval: the plan's
			// approval covers its deletes.
			f.opts.GuardDeletes, f.opts.InputsHash, f.opts.ExpectPlan = true, "h1:x", c.expect
			ctx, log := logContext(t)
			r, code := Run(ctx, f.opts)
			if got := f.calls(t); !slices.Equal(got, c.calls) {
				t.Errorf("calls = %v, want %v", got, c.calls)
			}
			if c.changed == "" {
				if code != 0 || r.Error != nil {
					t.Fatalf("run = %d %+v", code, r.Error)
				}
				return
			}
			if code != 1 || r.Error == nil || r.Error.Kind != ErrorKindPlanChanged || r.Error.Step != nil {
				t.Fatalf("run = %d %+v, want plan-changed", code, r.Error)
			}
			if r.Plan == nil || r.Plan.Hash != c.changed {
				t.Errorf("new plan = %+v, want hash %s", r.Plan, c.changed)
			}
			if !strings.Contains(r.Error.Tail, c.expect) || !strings.Contains(r.Error.Tail, c.changed) {
				t.Errorf("summary = %q", r.Error.Tail)
			}
			if strings.Contains(stderr.String()+log.String()+string(Encode(r)), "c2VjcmV0") {
				t.Error("a plan value leaked")
			}
		})
	}
}

// TestEncodePlan: a plan with MaxPlanResources long addresses still fits
// the termination message; its resources shrink, its hash and counts stay,
// even in the minimal document.
func TestEncodePlan(t *testing.T) {
	t.Parallel()
	p := &Plan{Hash: EmptyPlanHash, Create: 70, Update: 1, Replace: 6, Delete: 2, OutputChanges: 3, Import: 4, Move: 5, Forget: 7}
	for i := range MaxPlanResources {
		p.Resources = append(p.Resources, fmt.Sprintf("module.role.aws_security_group_rule.%s[%d] (create)", strings.Repeat("x", 60), i))
	}
	r := Result{Version: ResultVersion, Op: OpPlan, Steps: []Step{{Name: "init"}, {Name: "plan", Exit: 2}}, Plan: p}
	b := Encode(r)
	var got Result
	if len(b) > MaxResultBytes || json.Unmarshal(b, &got) != nil {
		t.Fatalf("encoded %d bytes", len(b))
	}
	if got.Plan == nil || got.Plan.Hash != p.Hash || got.Plan.Create != 70 || !got.Plan.Truncated ||
		len(got.Plan.Resources) == 0 || len(got.Plan.Resources) >= MaxPlanResources {
		t.Errorf("plan = %+v", got.Plan)
	}
	huge := Result{Version: 1, Op: OpPlan, Image: ResultImage{Ref: strings.Repeat("r", 5000)}, Plan: p}
	var minimal Result
	if err := json.Unmarshal(Encode(huge), &minimal); err != nil || minimal.Plan == nil || minimal.Plan.Hash != p.Hash || minimal.Plan.Delete != 2 || minimal.Plan.Replace != 6 || minimal.Plan.Forget != 7 ||
		minimal.Plan.OutputChanges != 3 || minimal.Plan.Import != 4 || minimal.Plan.Move != 5 || !minimal.Plan.Truncated || minimal.Plan.Resources != nil {
		t.Errorf("minimal = %+v, %v", minimal.Plan, err)
	}
}

// TestParsePlanForget: a resource a removed block takes out of the state
// without destroying it counts in Forget, and in no other count.
func TestParsePlanForget(t *testing.T) {
	t.Parallel()
	p := mustPlan(t, `{"resource_changes":[{"address":"a.b","change":{"actions":["forget"]}}]}`)
	if p.Forget != 1 || p.Create+p.Update+p.Replace+p.Delete != 0 || p.Hash == EmptyPlanHash {
		t.Errorf("forget plan = %+v", p)
	}
	if !strings.Contains(p.counts(), "1 to forget") {
		t.Errorf("counts = %q", p.counts())
	}
}
