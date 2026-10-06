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
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/internal/plankey"
)

// testPlanKey is the plan key every fixture mounts.
const testPlanKey = "test plan key test plan key test"

// mustPlan returns ParsePlan of planJSON under testPlanKey, failing t on an
// error.
func mustPlan(t *testing.T, planJSON string) *Plan {
	t.Helper()
	p, err := ParsePlan([]byte(planJSON), []byte(testPlanKey))
	if err != nil {
		t.Fatalf("ParsePlan(%s): %v", planJSON, err)
	}
	return p
}

// oneChange returns a plan whose only resource change is module.role.lb
// with actions ["update"] and the change object fields fields (JSON object
// members, without braces, after "actions").
func oneChange(fields string) string {
	if fields != "" {
		fields = "," + fields
	}
	return `{"resource_changes":[{"address":"module.role.lb","change":{"actions":["update"]` + fields + `}}]}`
}

// TestPlanHashBindsValues: the fingerprint changes with any value or
// unknown or sensitive marker of a change, and with the key, and is stable
// for the same plan and key whatever the order of object keys.
func TestPlanHashBindsValues(t *testing.T) {
	t.Parallel()
	base := mustPlan(t, oneChange(`"before":{"size":1,"name":"a"},"after":{"size":2,"name":"a"}`))
	cases := []struct {
		name string
		plan string
		same bool
	}{
		{"the same plan", oneChange(`"before":{"size":1,"name":"a"},"after":{"size":2,"name":"a"}`), true},
		{"object keys in another order", oneChange(`"after":{"name":"a","size":2},"before":{"name":"a","size":1}`), true},
		{"another after value", oneChange(`"before":{"size":1,"name":"a"},"after":{"size":3,"name":"a"}`), false},
		{"another before value", oneChange(`"before":{"size":0,"name":"a"},"after":{"size":2,"name":"a"}`), false},
		{"an unknown marker", oneChange(`"before":{"size":1,"name":"a"},"after":{"size":2,"name":"a"},"after_unknown":{"id":true}`), false},
		{"a sensitive marker", oneChange(`"before":{"size":1,"name":"a"},"after":{"size":2,"name":"a"},"after_sensitive":{"name":true}`), false},
		{"a before sensitive marker", oneChange(`"before":{"size":1,"name":"a"},"after":{"size":2,"name":"a"},"before_sensitive":{"name":true}`), false},
	}
	for _, c := range cases {
		if got := mustPlan(t, c.plan).Hash; (got == base.Hash) != c.same {
			t.Errorf("%s: hash %s, base %s, want same %v", c.name, got, base.Hash, c.same)
		}
	}
	// Numbers keep their literal: two integers float64 cannot tell apart
	// still differ.
	if mustPlan(t, oneChange(`"after":12345678901234567890`)).Hash == mustPlan(t, oneChange(`"after":12345678901234567891`)).Hash {
		t.Error("large integers hash alike")
	}
	other, err := ParsePlan([]byte(oneChange(`"before":{"size":1,"name":"a"},"after":{"size":2,"name":"a"}`)), []byte(strings.Repeat("k", 32)))
	if err != nil || other.Hash == base.Hash {
		t.Errorf("another key: hash %s, base %s, %v", other.Hash, base.Hash, err)
	}
	if !strings.HasPrefix(base.Hash, "p2:") || PlanHashPrefix != "p2:" {
		t.Errorf("hash %s, prefix %s, want p2:", base.Hash, PlanHashPrefix)
	}
}

// TestPlanHashBindsEffect: the fingerprint keys what a change does, not
// the attributes it leaves alone: an unchanged attribute that moved, or the
// old value of one that becomes unknown, keeps the hash; a changed value,
// a value becoming unknown and a sensitivity flip change it.
func TestPlanHashBindsEffect(t *testing.T) {
	t.Parallel()
	update := func(fields string) string {
		return mustPlan(t, oneChange(fields)).Hash
	}
	base := update(`"before":{"size":1,"desired":3,"rv":"7","tags":{"a":"x"}},` +
		`"after":{"size":2,"desired":3,"tags":{"a":"x"}},"after_unknown":{"rv":true}`)
	cases := []struct {
		name   string
		fields string
		same   bool
	}{
		{"an unchanged attribute moved", `"before":{"size":1,"desired":5,"rv":"7","tags":{"a":"x"}},` +
			`"after":{"size":2,"desired":5,"tags":{"a":"x"}},"after_unknown":{"rv":true}`, true},
		{"an unknown attribute's old value moved", `"before":{"size":1,"desired":3,"rv":"9","tags":{"a":"x"}},` +
			`"after":{"size":2,"desired":3,"tags":{"a":"x"}},"after_unknown":{"rv":true}`, true},
		{"markers spelled differently", `"before":{"size":1,"desired":3,"rv":"7","tags":{"a":"x"}},` +
			`"after":{"size":2,"desired":3,"tags":{"a":"x"}},"after_unknown":{"rv":true,"tags":{},"size":false},` +
			`"before_sensitive":{"tags":{}},"after_sensitive":false`, true},
		{"a changed value", `"before":{"size":1,"desired":3,"rv":"7","tags":{"a":"x"}},` +
			`"after":{"size":4,"desired":3,"tags":{"a":"x"}},"after_unknown":{"rv":true}`, false},
		{"a nested changed value", `"before":{"size":1,"desired":3,"rv":"7","tags":{"a":"x"}},` +
			`"after":{"size":2,"desired":3,"tags":{"a":"y"}},"after_unknown":{"rv":true}`, false},
		{"a value becoming unknown", `"before":{"size":1,"desired":3,"rv":"7","tags":{"a":"x"}},` +
			`"after":{"size":2,"tags":{"a":"x"}},"after_unknown":{"rv":true,"desired":true}`, false},
		{"a whole object becoming unknown", `"before":{"size":1,"desired":3,"rv":"7","tags":{"a":"x"}},` +
			`"after":{"size":2,"desired":3},"after_unknown":{"rv":true,"tags":true}`, false},
		{"a sensitivity flip", `"before":{"size":1,"desired":3,"rv":"7","tags":{"a":"x"}},` +
			`"after":{"size":2,"desired":3,"tags":{"a":"x"}},"after_unknown":{"rv":true},"after_sensitive":{"tags":true}`, false},
		{"an attribute removed", `"before":{"size":1,"desired":3,"rv":"7","tags":{"a":"x"}},` +
			`"after":{"size":2,"desired":3,"tags":{}},"after_unknown":{"rv":true}`, false},
	}
	for _, c := range cases {
		if got := update(c.fields); (got == base) != c.same {
			t.Errorf("%s: hash %s, base %s, want same %v", c.name, got, base, c.same)
		}
	}
	// A whole unknown object hashes alike whatever it held before, and
	// unlike one only partly unknown.
	wholeA := update(`"before":{"tags":{"a":"x","b":"1"}},"after":{},"after_unknown":{"tags":true}`)
	wholeB := update(`"before":{"tags":{"a":"y"}},"after":{},"after_unknown":{"tags":true}`)
	partial := update(`"before":{"tags":{"a":"x","b":"1"}},"after":{"tags":{"b":"1"}},"after_unknown":{"tags":{"a":true}}`)
	if wholeA != wholeB || wholeA == partial {
		t.Errorf("whole unknown %s and %s, partly unknown %s", wholeA, wholeB, partial)
	}
	// A key holding "/" is not a nested path.
	if update(`"before":{},"after":{"a/b":1}`) == update(`"before":{},"after":{"a":{"b":1}}`) {
		t.Error("a key with a slash hashes like a nested key")
	}
	// A list compares element by element.
	if update(`"before":{"l":[1,2]},"after":{"l":[1,3]}`) == update(`"before":{"l":[1,2]},"after":{"l":[1,2,3]}`) {
		t.Error("two list changes hash alike")
	}
	if update(`"before":{"l":[1,2],"x":1},"after":{"l":[1,2],"x":2}`) != update(`"before":{"l":[5],"x":1},"after":{"l":[5],"x":2}`) {
		t.Error("an unchanged list reaches the hash")
	}
}

// TestPlanHashEffectByAction: a create binds its planned object, a delete
// and a forget only their address and actions, and a replacement only the
// paths it sets.
func TestPlanHashEffectByAction(t *testing.T) {
	t.Parallel()
	hash := func(actions, fields string) string {
		return mustPlan(t, `{"resource_changes":[{"address":"a.b","change":{"actions":[`+actions+`],`+fields+`}}]}`).Hash
	}
	if hash(`"create"`, `"after":{"n":1}`) == hash(`"create"`, `"after":{"n":2}`) {
		t.Error("two creates of different objects hash alike")
	}
	if hash(`"create"`, `"after":{"n":1}`) == hash(`"create"`, `"after":{"n":1},"after_unknown":{"id":true}`) {
		t.Error("a create's unknown marker does not reach the hash")
	}
	if hash(`"read"`, `"after":{"n":1}`) == hash(`"read"`, `"after":{"n":2}`) {
		t.Error("two reads of different objects hash alike")
	}
	for _, actions := range []string{`"delete"`, `"forget"`} {
		if hash(actions, `"before":{"n":1,"rv":"1"}`) != hash(actions, `"before":{"n":2,"rv":"2"}`) {
			t.Errorf("%s binds the old object", actions)
		}
	}
	for _, actions := range []string{`"delete","create"`, `"create","delete"`} {
		a := hash(actions, `"before":{"n":1,"arn":"x1","keep":"k"},"after":{"n":2,"keep":"k"},"after_unknown":{"arn":true}`)
		b := hash(actions, `"before":{"n":1,"arn":"x2","keep":"k"},"after":{"n":2,"keep":"k"},"after_unknown":{"arn":true}`)
		c := hash(actions, `"before":{"n":1,"arn":"x1","keep":"k"},"after":{"n":3,"keep":"k"},"after_unknown":{"arn":true}`)
		if a != b || a == c {
			t.Errorf("replace %s: %s, %s, %s", actions, a, b, c)
		}
	}
}

// diffOf returns diffPaths of an update from the JSON before to the JSON
// after, failing t on error.
func diffOf(t *testing.T, before, after string) []pathEffect {
	t.Helper()
	v, err := decodeValues(planChange{Before: json.RawMessage(before), After: json.RawMessage(after)})
	if err != nil {
		t.Fatal(err)
	}
	return diffPaths(v)
}

// TestDiffPathsPresence: an element or member added or removed is an
// effect even when its value is null, unlike a null that stays null.
func TestDiffPathsPresence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, before, after string
		want                []pathEffect
	}{
		{"a null element removed", `{"l":["x",null]}`, `{"l":["x"]}`,
			[]pathEffect{{Path: []any{"l", 1}, AfterAbsent: true}}},
		{"a null member added", `{"m":{"a":"1"}}`, `{"m":{"a":"1","b":null}}`,
			[]pathEffect{{Path: []any{"m", "b"}, BeforeAbsent: true}}},
		{"a null that stays null", `{"m":{"a":"1","b":null}}`, `{"m":{"a":"1","b":null}}`, nil},
		{"a member removed", `{"m":{"a":"1","b":"2"}}`, `{"m":{"a":"1"}}`,
			[]pathEffect{{Path: []any{"m", "b"}, Before: "2", AfterAbsent: true}}},
	}
	for _, c := range cases {
		if got := diffOf(t, c.before, c.after); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: diffPaths = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// TestPlanHashPresenceAndKind: the fingerprint tells an added or removed
// null from an unchanged one, and a map key "0" from a list index 0; an
// unchanged attribute still keeps the hash.
func TestPlanHashPresenceAndKind(t *testing.T) {
	t.Parallel()
	update := func(fields string) string {
		return mustPlan(t, oneChange(fields)).Hash
	}
	base := update(`"before":{"x":1,"l":["x"],"m":{"a":"1"}},"after":{"x":2,"l":["x"],"m":{"a":"1"}}`)
	cases := []struct {
		name   string
		fields string
		same   bool
	}{
		{"unchanged attributes moved", `"before":{"x":1,"l":["y"],"m":{"a":"2"}},"after":{"x":2,"l":["y"],"m":{"a":"2"}}`, true},
		{"an unchanged null", `"before":{"x":1,"l":["x",null],"m":{"a":"1"}},"after":{"x":2,"l":["x",null],"m":{"a":"1"}}`, true},
		{"a null element removed", `"before":{"x":1,"l":["x",null],"m":{"a":"1"}},"after":{"x":2,"l":["x"],"m":{"a":"1"}}`, false},
		{"a null member added", `"before":{"x":1,"l":["x"],"m":{"a":"1"}},"after":{"x":2,"l":["x"],"m":{"a":"1","b":null}}`, false},
	}
	for _, c := range cases {
		if got := update(c.fields); (got == base) != c.same {
			t.Errorf("%s: hash %s, base %s, want same %v", c.name, got, base, c.same)
		}
	}
	if update(`"before":{"x":null},"after":{}`) == update(`"before":{},"after":{"x":null}`) {
		t.Error("a null removed hashes like a null added")
	}
	if update(`"before":{"m":{"0":1}},"after":{"m":{"0":2}}`) == update(`"before":{"m":[1]},"after":{"m":[2]}`) {
		t.Error("a map key 0 hashes like a list index 0")
	}
}

// TestNormMarker: markers that mark the same values normalize alike.
func TestNormMarker(t *testing.T) {
	t.Parallel()
	for _, m := range []any{nil, false, map[string]any{}, []any{false}, map[string]any{"a": false, "b": []any{}}} {
		if n := normMarker(m); n != nil {
			t.Errorf("normMarker(%v) = %v, want nil", m, n)
		}
	}
	got := normMarker(map[string]any{"a": true, "b": false, "c": []any{false, true}})
	want := map[string]any{"a": true, "c": []any{nil, true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("normMarker = %v, want %v", got, want)
	}
}

// TestPlanHashOutputs: an output change makes the hash non-empty and
// counts in Outputs, but not as a resource; an output no-op does not.
func TestPlanHashOutputs(t *testing.T) {
	t.Parallel()
	p := mustPlan(t, `{"output_changes":{"endpoint":{"actions":["update"],"before":"a","after":"b"}},"resource_changes":[]}`)
	if p.Hash == EmptyPlanHash || p.OutputChanges != 1 || p.Create+p.Update+p.Replace+p.Delete != 0 || len(p.Resources) != 0 {
		t.Errorf("output-only plan = %+v", p)
	}
	q := mustPlan(t, `{"output_changes":{"endpoint":{"actions":["update"],"before":"a","after":"c"}}}`)
	if q.Hash == p.Hash {
		t.Error("output values do not reach the hash")
	}
	if n := mustPlan(t, `{"output_changes":{"endpoint":{"actions":["no-op"],"before":"a","after":"a"}}}`); n.Hash != EmptyPlanHash || n.OutputChanges != 0 {
		t.Errorf("output no-op plan = %+v", n)
	}
	if !strings.Contains(planChangedSummary("p2:x", p), "1 output(s) to change") {
		t.Errorf("summary = %q", planChangedSummary("p2:x", p))
	}
}

// TestParsePlanNeedsKey: ParsePlan never computes an unkeyed fingerprint.
func TestParsePlanNeedsKey(t *testing.T) {
	t.Parallel()
	for _, key := range [][]byte{nil, []byte("short")} {
		if _, err := ParsePlan([]byte(destructivePlan), key); !errors.Is(err, errNoPlanKey) {
			t.Errorf("key of %d bytes: err = %v, want errNoPlanKey", len(key), err)
		}
	}
}

// TestSensitiveValues: the string values the plan marks sensitive, nested
// or whole, before or after, of resources (no-ops included), de-duplicated
// and sorted; never unmarked values, numbers or empty strings, never the
// values of outputs (every re-exported output is marked sensitive), and
// never in the encoded plan.
func TestSensitiveValues(t *testing.T) {
	t.Parallel()
	p := mustPlan(t, `{"resource_changes":[`+
		`{"address":"a.b","change":{"actions":["update"],`+
		`"before":{"pw":"old-pw","name":"plain","list":["l1","l2"],"n":7},`+
		`"after":{"pw":"new-pw","name":"plain","list":["l1","l3"],"n":8,"empty":""},`+
		`"before_sensitive":{"pw":true,"list":[false,true]},`+
		`"after_sensitive":{"pw":true,"list":true,"n":true,"empty":true}}},`+
		`{"address":"a.c","change":{"actions":["no-op"],"before":{"t":"kept-token"},"after":{"t":"kept-token"},"after_sensitive":{"t":true}}}],`+
		`"output_changes":{"zone":{"actions":["create"],"after":{"failure_domain":"us-east-1a","ip":"10.0.0.7"},"after_sensitive":true},`+
		`"dup":{"actions":["update"],"before":"new-pw","after":"new-pw","before_sensitive":true}}}`)
	want := []string{"kept-token", "l1", "l2", "l3", "new-pw", "old-pw"}
	if got := p.SensitiveValues(); !slices.Equal(got, want) {
		t.Errorf("SensitiveValues = %v, want %v", got, want)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range want {
		if strings.Contains(string(b), v) {
			t.Errorf("encoded plan carries %q: %s", v, b)
		}
	}
	if (*Plan)(nil).SensitiveValues() != nil {
		t.Error("nil plan has sensitive values")
	}
}

// dataSourcePlan is a plan with no resource changes whose prior state
// holds data sources read during the plan: a sensitive value in the root
// module, one in a nested child module, an unmarked one, and a managed
// resource.
const dataSourcePlan = `{"prior_state":{"values":{"root_module":{` +
	`"resources":[{"address":"data.vault_generic_secret.db","mode":"data",` +
	`"values":{"data":{"password":"root-data-secret","user":"admin"}},"sensitive_values":{"data":{"password":true}}},` +
	`{"address":"aws_instance.m","mode":"managed","values":{"pw":"managed-value"},"sensitive_values":{"pw":true}}],` +
	`"child_modules":[{"resources":[],"child_modules":[{"resources":[{"address":"module.a.module.b.data.x.y","mode":"data",` +
	`"values":{"token":"child-data-secret","list":["plain"]},"sensitive_values":{"token":true,"list":[false]}}]}]}]}}}}`

// TestSensitiveDataSources: the sensitive values of data sources read
// during the plan, which live only in the prior state, are collected from
// the root and every child module; unmarked values and managed resources
// (whose values a resource change carries) are not. Invalid plan JSON is
// an error.
func TestSensitiveDataSources(t *testing.T) {
	t.Parallel()
	got, err := planSensitiveValues([]byte(dataSourcePlan))
	if want := []string{"child-data-secret", "root-data-secret"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("planSensitiveValues = %v, %v; want %v", got, err, want)
	}
	if got := mustPlan(t, dataSourcePlan).SensitiveValues(); !slices.Contains(got, "root-data-secret") {
		t.Errorf("SensitiveValues = %v, want the data source's", got)
	}
	bad := `{"prior_state":{"values":{"root_module":{"resources":[{"mode":"data","values":{},"sensitive_values":"x}]}}}}`
	if _, err := planSensitiveValues([]byte(bad)); err == nil {
		t.Error("invalid plan JSON parsed")
	}
}

// TestRunRedactsDataSourceValue: a guarded apply redacts a sensitive
// data-source value, used only in an output, from the failure of its apply
// step.
func TestRunRedactsDataSourceValue(t *testing.T) {
	t.Parallel()
	f := newFixture(t, OpApply, "FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW="+dataSourcePlan, "FAKE_EXIT_APPLY=1",
		"FAKE_STDERR_APPLY=Error: output endpoint: root-data-secret rejected\n")
	f.opts.GuardDeletes, f.opts.InputsHash = true, "h1:x"
	r, code := Run(context.Background(), f.opts)
	if code != ExitFailure || r.Error == nil {
		t.Fatalf("run = %d, error %+v", code, r.Error)
	}
	if want := "Error: output endpoint: (sensitive) rejected"; r.Error.Tail != want {
		t.Errorf("tail = %q, want %q", r.Error.Tail, want)
	}
}

// TestLoadPlanKey: the key file must exist and hold at least a key's
// bytes; the default path and size match internal/plankey's.
func TestLoadPlanKey(t *testing.T) {
	t.Parallel()
	if DefaultPlanKeyFile != filepath.Join(plankey.MountPath, plankey.KeyFile) || planKeySize != plankey.KeySize {
		t.Errorf("DefaultPlanKeyFile %s, size %d; plankey has %s, %d", DefaultPlanKeyFile, planKeySize,
			filepath.Join(plankey.MountPath, plankey.KeyFile), plankey.KeySize)
	}
	dir := t.TempDir()
	good, short := filepath.Join(dir, "good"), filepath.Join(dir, "short")
	if err := os.WriteFile(good, []byte(testPlanKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(short, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := loadPlanKey(good); err != nil || string(k) != testPlanKey {
		t.Errorf("good key: %v", err)
	}
	for _, path := range []string{"", short, filepath.Join(dir, "missing")} {
		if _, err := loadPlanKey(path); err == nil {
			t.Errorf("key %q accepted", path)
		} else if strings.Contains(err.Error(), testPlanKey) {
			t.Errorf("error names a key: %v", err)
		}
	}
}

// TestRunWithoutPlanKey: a plan and an approved apply fail closed before
// any step when the key cannot be read; drift and a guarded apply never
// read it.
func TestRunWithoutPlanKey(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		op     string
		expect string
		guard  bool
		fails  bool
	}{
		{name: "plan", op: OpPlan, fails: true},
		{name: "approved apply", op: OpApply, expect: EmptyPlanHash, fails: true},
		{name: "drift", op: OpDrift},
		{name: "guarded apply", op: OpApply, guard: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, c.op)
			f.opts.PlanKeyFile = filepath.Join(t.TempDir(), "missing")
			f.opts.ExpectPlan, f.opts.GuardDeletes, f.opts.InputsHash = c.expect, c.guard, "h1:x"
			r, code := Run(context.Background(), f.opts)
			if !c.fails {
				if code != 0 || r.Error != nil {
					t.Errorf("run = %d %+v", code, r.Error)
				}
				return
			}
			if code != ExitFailure || r.Error == nil || r.Error.Step == nil || *r.Error.Step != StepPrepare ||
				!strings.Contains(r.Error.Tail, "plan key") || len(r.Steps) != 0 {
				t.Errorf("run = %d %+v, steps %v; want a prepare failure", code, r.Error, r.Steps)
			}
			if _, err := os.Stat(f.log); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the runtime ran: %v", err)
			}
		})
	}
}
