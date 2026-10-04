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

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mod is the module path used by the fixtures.
const mod = "example.com/m"

// profA is a profile with two packages.
const profA = `mode: atomic
example.com/m/a/a.go:1.1,2.2 4 1
example.com/m/a/a.go:3.1,4.2 6 0
example.com/m/b/b.go:1.1,2.2 10 0
example.com/m/empty/e.go:1.1,2.2 0 0
`

// profB covers the block profA left uncovered in package b.
const profB = `mode: set
example.com/m/b/b.go:1.1,2.2 10 1
example.com/m/b/b.go:1.1,2.2 10 0
`

// parse merges the given profile texts for t and returns the Profile.
func parse(t *testing.T, texts ...string) Profile {
	t.Helper()
	p := Profile{}
	for _, s := range texts {
		if err := ParseProfile(strings.NewReader(s), p); err != nil {
			t.Fatalf("ParseProfile: %v", err)
		}
	}
	return p
}

// TestParseAndPackages checks statement sums and the zero-statement skip.
func TestParseAndPackages(t *testing.T) {
	pkgs := Packages(parse(t, profA))
	if len(pkgs) != 2 {
		t.Fatalf("got %d packages, want 2 (empty skipped): %+v", len(pkgs), pkgs)
	}
	if pkgs[0].Pkg != mod+"/a" || pkgs[0].Stmts != 10 || pkgs[0].Covered != 4 {
		t.Errorf("package a = %+v", pkgs[0])
	}
	if pkgs[1].Covered != 0 {
		t.Errorf("package b = %+v", pkgs[1])
	}
}

// TestGeneratedSkipped checks that zz_generated files are left out of a
// package's statements, and that a package of only generated code is
// dropped.
func TestGeneratedSkipped(t *testing.T) {
	t.Parallel()
	pkgs := Packages(parse(t, `mode: atomic
example.com/m/a/a.go:1.1,2.2 4 1
example.com/m/a/zz_generated.deepcopy.go:1.1,2.2 90 0
example.com/m/gen/zz_generated.deepcopy.go:1.1,2.2 5 0
`))
	if len(pkgs) != 1 || pkgs[0].Stmts != 4 || pkgs[0].Covered != 4 {
		t.Errorf("Packages = %+v, want only a with 4/4 statements", pkgs)
	}
	for file, want := range map[string]bool{
		"example.com/m/a/zz_generated.deepcopy.go": true,
		"example.com/m/a/a.go":                     false,
		"example.com/m/a/generated.go":             false,
	} {
		if got := Generated(file); got != want {
			t.Errorf("Generated(%q) = %v, want %v", file, got, want)
		}
	}
}

// TestMerge checks that a block covered in any profile or occurrence counts.
func TestMerge(t *testing.T) {
	pkgs := Packages(parse(t, profA, profB))
	if pkgs[1].Pkg != mod+"/b" || pkgs[1].Covered != 10 || pkgs[1].Stmts != 10 {
		t.Errorf("merged package b = %+v", pkgs[1])
	}
	if got := Packages(parse(t, profB, profA))[1].Covered; got != 10 {
		t.Errorf("merge is order dependent: covered %d", got)
	}
}

// TestParseProfileErrors checks malformed profiles are rejected.
func TestParseProfileErrors(t *testing.T) {
	for name, in := range map[string]string{
		"empty":      "",
		"no header":  "example.com/m/a/a.go:1.1,2.2 1 1\n",
		"bad mode":   "mode: bogus\n",
		"no colon":   "mode: set\nnonsense\n",
		"few fields": "mode: set\na/a.go:1.1,2.2 1\n",
		"bad number": "mode: set\na/a.go:1.1,2.2 x 1\n",
		"negative":   "mode: set\na/a.go:1.1,2.2 1 -1\n",
	} {
		if err := ParseProfile(strings.NewReader(in), Profile{}); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// TestParseFloors checks accepted syntax and every rejected line.
func TestParseFloors(t *testing.T) {
	f, err := ParseFloors(strings.NewReader(`# header

default 80
a 90.5  # ratchet
b exempt  # wiring
`))
	if err != nil {
		t.Fatal(err)
	}
	if f.Default != 80 || f.Pkgs["a"].Pct != 90.5 || !f.Pkgs["b"].Exempt {
		t.Errorf("floors = %+v", f)
	}
	for name, in := range map[string]string{
		"no default":   "a 90\n",
		"bad pct":      "default x\n",
		"over 100":     "default 101\n",
		"negative":     "default -1\n",
		"extra field":  "default 80\na 90 extra\n",
		"one field":    "default 80\na\n",
		"dup default":  "default 80\ndefault 70\n",
		"dup package":  "default 80\na 1\na 2\n",
		"exempt dflt":  "default exempt\n",
		"bad pkg pct":  "default 80\na high\n",
		"comment only": "# nothing\n",
	} {
		if _, err := ParseFloors(strings.NewReader(in)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// evaluate runs Evaluate for t over the fixture with the given floors
// text and returns the Result.
func evaluate(t *testing.T, floors string) Result {
	t.Helper()
	f, err := ParseFloors(strings.NewReader(floors))
	if err != nil {
		t.Fatal(err)
	}
	return Evaluate(Packages(parse(t, profA)), f, mod)
}

// TestViolationsAndExemptions checks the default, an override and exemption.
func TestViolationsAndExemptions(t *testing.T) {
	res := evaluate(t, "default 50\n")
	if len(res.Violations) != 2 {
		t.Fatalf("violations = %v, want a and b", res.Violations)
	}
	if res.Rows[0].Pkg != "b" {
		t.Errorf("rows not lowest first: %+v", res.Rows)
	}
	res = evaluate(t, "default 50\na 40\nb exempt\n")
	if len(res.Violations) != 0 {
		t.Errorf("violations = %v, want none", res.Violations)
	}
	if res.Total != 20 {
		t.Errorf("total = %v, want 20", res.Total)
	}
	res = evaluate(t, "default 0\na 41\n")
	if len(res.Violations) != 1 || !strings.Contains(res.Violations[0], "a:") {
		t.Errorf("violations = %v", res.Violations)
	}
}

// TestOutputs checks the text table and Markdown section.
func TestOutputs(t *testing.T) {
	res := evaluate(t, "default 50\nb exempt\n")
	var tb bytes.Buffer
	if err := WriteTable(&tb, res); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PACKAGE", "FAIL", "exempt", "TOTAL", "20.0%"} {
		if !strings.Contains(tb.String(), want) {
			t.Errorf("table missing %q:\n%s", want, tb.String())
		}
	}
	var md bytes.Buffer
	if err := WriteMarkdown(&md, res); err != nil {
		t.Fatal(err)
	}
	out := md.String()
	for _, want := range []string{"## Test coverage", "20.0%", "| `a` | 40.0% | 50.0% | ❌ |", "| `b` | 0.0% | exempt | ✅ |"} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "`b`") > strings.Index(out, "`a`") {
		t.Errorf("markdown not lowest first:\n%s", out)
	}
}

// TestRun checks exit codes and the summary append end to end.
func TestRun(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pa, pb := write("a.out", profA), write("b.out", profB)
	pass := write("pass.txt", "default 30\n")
	fail := write("fail.txt", "default 90\n")
	sum := write("sum.md", "existing\n")
	var out, errb bytes.Buffer

	if code := run([]string{"-floors", pass, "-module", mod, "-summary", sum, pa, pb}, &out, &errb); code != 0 {
		t.Fatalf("pass: exit %d: %s", code, errb.String())
	}
	got, _ := os.ReadFile(sum)
	if !strings.HasPrefix(string(got), "existing\n") || !strings.Contains(string(got), "## Test coverage") {
		t.Errorf("summary not appended:\n%s", got)
	}
	errb.Reset()
	if code := run([]string{"-floors", fail, "-module", mod, pa}, &out, &errb); code != 1 {
		t.Errorf("fail: exit %d", code)
	}
	if !strings.Contains(errb.String(), "a:") || !strings.Contains(errb.String(), "b:") {
		t.Errorf("violations not listed: %s", errb.String())
	}
	for name, args := range map[string][]string{
		"no args":       {},
		"no profile":    {"-floors", pass},
		"missing floor": {"-floors", filepath.Join(dir, "nope"), pa},
		"bad floors":    {"-floors", write("bad.txt", "x\n"), pa},
		"bad profile":   {"-floors", pass, write("bad.out", "junk\n")},
		"missing prof":  {"-floors", pass, filepath.Join(dir, "nope.out")},
		"bad flag":      {"-nope"},
	} {
		if code := run(args, &out, &errb); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
}
