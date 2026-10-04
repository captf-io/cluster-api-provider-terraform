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
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

// DefaultModule is the module path stripped from package paths when the
// -module flag is not given.
const DefaultModule = "github.com/captf-io/cluster-api-provider-terraform"

// Block is one statement block of a cover profile: a source range.
type Block struct {
	// File is the import-path-qualified source file.
	File string
	// Range is the "L.C,L.C" span of the block.
	Range string
}

// BlockData is what a profile says about one block.
type BlockData struct {
	// Stmts is the number of statements in the block.
	Stmts int
	// Covered is true when any profile ran the block.
	Covered bool
}

// Profile is a merged set of cover blocks.
type Profile map[Block]*BlockData

// PkgCover is the statement coverage of one package.
type PkgCover struct {
	// Pkg is the package import path.
	Pkg string
	// Stmts is the package's statement count.
	Stmts int
	// Covered is the number of statements that ran.
	Covered int
}

// Pct returns the percentage of statements covered, or 100 for a package
// with no statements.
func (p PkgCover) Pct() float64 {
	if p.Stmts == 0 {
		return 100
	}
	return float64(p.Covered) * 100 / float64(p.Stmts)
}

// Floor is the coverage requirement for one package.
type Floor struct {
	// Pct is the minimum coverage percentage.
	Pct float64
	// Exempt marks a package that is never gated.
	Exempt bool
}

// Floors is a parsed floors file.
type Floors struct {
	// Default applies to every package without its own line.
	Default float64
	// Pkgs maps a module-relative import path to its floor.
	Pkgs map[string]Floor
}

// Row is one evaluated package for output.
type Row struct {
	// Pkg is the module-relative package path.
	Pkg string
	// Pct is the package's coverage percentage.
	Pct float64
	// Floor is the applied floor; meaningful only when Exempt is false.
	Floor float64
	// Exempt is true for an exempt package.
	Exempt bool
	// Pass is true when the package meets its floor or is exempt.
	Pass bool
}

// Result is the outcome of evaluating a profile against floors.
type Result struct {
	// Rows are the packages, lowest coverage first.
	Rows []Row
	// Total is the tree-wide coverage percentage.
	Total float64
	// Violations lists one message per failing package.
	Violations []string
}

// ParseProfile reads one cover profile from r and merges its blocks into
// into. It returns an error naming the line of a malformed header or block.
func ParseProfile(r io.Reader, into Profile) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if n == 1 {
			mode, ok := strings.CutPrefix(line, "mode:")
			if !ok {
				return fmt.Errorf("line 1: missing mode header: %q", line)
			}
			switch strings.TrimSpace(mode) {
			case "set", "count", "atomic":
			default:
				return fmt.Errorf("line 1: unknown mode %q", strings.TrimSpace(mode))
			}
			continue
		}
		colon := strings.LastIndex(line, ":")
		if colon < 0 {
			return fmt.Errorf("line %d: malformed block %q", n, line)
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) != 3 {
			return fmt.Errorf("line %d: malformed block %q", n, line)
		}
		stmts, err1 := strconv.Atoi(fields[1])
		count, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || stmts < 0 || count < 0 {
			return fmt.Errorf("line %d: bad numbers in %q", n, line)
		}
		key := Block{File: line[:colon], Range: fields[0]}
		if bd, ok := into[key]; ok {
			bd.Covered = bd.Covered || count > 0
			continue
		}
		into[key] = &BlockData{Stmts: stmts, Covered: count > 0}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("empty profile")
	}
	return nil
}

// Generated reports whether file is generated code (controller-gen's
// zz_generated.* files), which no test is expected to cover.
func Generated(file string) bool {
	return strings.HasPrefix(path.Base(file), "zz_generated")
}

// Packages sums p per package (the directory of each file), leaving out
// Generated files, and returns the packages that have statements, sorted
// by import path.
func Packages(p Profile) []PkgCover {
	by := map[string]*PkgCover{}
	for b, d := range p {
		if Generated(b.File) {
			continue
		}
		pkg := path.Dir(b.File)
		pc := by[pkg]
		if pc == nil {
			pc = &PkgCover{Pkg: pkg}
			by[pkg] = pc
		}
		pc.Stmts += d.Stmts
		if d.Covered {
			pc.Covered += d.Stmts
		}
	}
	var out []PkgCover
	for _, pc := range by {
		if pc.Stmts > 0 {
			out = append(out, *pc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pkg < out[j].Pkg })
	return out
}

// ParseFloors reads a floors file from r. It returns an error naming the
// line of any malformed entry, a duplicate package or a missing default.
func ParseFloors(r io.Reader) (*Floors, error) {
	f := &Floors{Default: -1, Pkgs: map[string]Floor{}}
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("floors line %d: want \"<package> <pct|exempt>\", got %q", n, strings.TrimSpace(line))
		}
		name, val := fields[0], fields[1]
		var fl Floor
		if val == "exempt" {
			if name == "default" {
				return nil, fmt.Errorf("floors line %d: default cannot be exempt", n)
			}
			fl.Exempt = true
		} else {
			pct, err := strconv.ParseFloat(val, 64)
			if err != nil || pct < 0 || pct > 100 {
				return nil, fmt.Errorf("floors line %d: bad percentage %q", n, val)
			}
			fl.Pct = pct
		}
		if name == "default" {
			if f.Default >= 0 {
				return nil, fmt.Errorf("floors line %d: duplicate default", n)
			}
			f.Default = fl.Pct
			continue
		}
		if _, dup := f.Pkgs[name]; dup {
			return nil, fmt.Errorf("floors line %d: duplicate package %q", n, name)
		}
		f.Pkgs[name] = fl
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if f.Default < 0 {
		return nil, fmt.Errorf("floors: no \"default <pct>\" line")
	}
	return f, nil
}

// rel strips the module prefix module from pkg and returns the result,
// mapping the module root to ".".
func rel(pkg, module string) string {
	if pkg == module {
		return "."
	}
	return strings.TrimPrefix(pkg, module+"/")
}

// Evaluate applies floors to pkgs and returns the rows (lowest coverage
// first), the total and a violation message per failing package. module is
// stripped from package paths before floors are looked up.
func Evaluate(pkgs []PkgCover, floors *Floors, module string) Result {
	var res Result
	var stmts, covered int
	for _, pc := range pkgs {
		stmts += pc.Stmts
		covered += pc.Covered
		row := Row{Pkg: rel(pc.Pkg, module), Pct: pc.Pct(), Floor: floors.Default, Pass: true}
		if fl, ok := floors.Pkgs[row.Pkg]; ok {
			row.Floor, row.Exempt = fl.Pct, fl.Exempt
		}
		if !row.Exempt && row.Pct < row.Floor {
			row.Pass = false
			res.Violations = append(res.Violations,
				fmt.Sprintf("%s: coverage %.1f%% is below the %.1f%% floor", row.Pkg, row.Pct, row.Floor))
		}
		res.Rows = append(res.Rows, row)
	}
	if stmts > 0 {
		res.Total = float64(covered) * 100 / float64(stmts)
	}
	sort.SliceStable(res.Rows, func(i, j int) bool {
		if res.Rows[i].Pct != res.Rows[j].Pct {
			return res.Rows[i].Pct < res.Rows[j].Pct
		}
		return res.Rows[i].Pkg < res.Rows[j].Pkg
	})
	return res
}

// floorText returns the floor column text for row r.
func floorText(r Row) string {
	if r.Exempt {
		return "exempt"
	}
	return fmt.Sprintf("%.1f%%", r.Floor)
}

// WriteTable writes res to w as a plain-text table sorted by package path,
// followed by the total. It returns the first write error.
func WriteTable(w io.Writer, res Result) error {
	rows := append([]Row(nil), res.Rows...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Pkg < rows[j].Pkg })
	width := len("PACKAGE")
	for _, r := range rows {
		width = max(width, len(r.Pkg))
	}
	if _, err := fmt.Fprintf(w, "%-*s  %8s  %8s  %s\n", width, "PACKAGE", "COVERAGE", "FLOOR", "STATUS"); err != nil {
		return err
	}
	for _, r := range rows {
		status := "ok"
		switch {
		case r.Exempt:
			status = "exempt"
		case !r.Pass:
			status = "FAIL"
		}
		if _, err := fmt.Fprintf(w, "%-*s  %7.1f%%  %8s  %s\n", width, r.Pkg, r.Pct, floorText(r), status); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "%-*s  %7.1f%%\n", width, "TOTAL", res.Total)
	return err
}

// WriteMarkdown writes res to w as a GitHub-flavored Markdown section:
// a heading, the total and a table with the lowest coverage first. It
// returns the write error, if any.
func WriteMarkdown(w io.Writer, res Result) error {
	var b strings.Builder
	b.WriteString("## Test coverage\n\n")
	fmt.Fprintf(&b, "**Total statement coverage: %.1f%%**\n\n", res.Total)
	if len(res.Violations) > 0 {
		fmt.Fprintf(&b, "%d package(s) below their floor.\n\n", len(res.Violations))
	}
	b.WriteString("| Package | Coverage | Floor | Status |\n| --- | ---: | ---: | :---: |\n")
	for _, r := range res.Rows {
		status := "✅"
		if !r.Pass {
			status = "❌"
		}
		fmt.Fprintf(&b, "| `%s` | %.1f%% | %s | %s |\n", r.Pkg, r.Pct, floorText(r), status)
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// main runs the gate over os.Args and exits with its status.
func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes covercheck with args, writing the table to stdout and
// problems to stderr. It returns 0 when every package meets its floor, 1
// on a violation and 2 on a usage or input error.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("covercheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	floorsPath := fs.String("floors", "", "floors file (required)")
	summary := fs.String("summary", "", "append a Markdown summary to this file")
	module := fs.String("module", DefaultModule, "module path stripped from package paths")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *floorsPath == "" || fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: covercheck -floors file [-module path] [-summary file] profile...")
		return 2
	}
	ff, err := os.Open(*floorsPath)
	if err != nil {
		fmt.Fprintln(stderr, "covercheck:", err)
		return 2
	}
	defer ff.Close()
	floors, err := ParseFloors(ff)
	if err != nil {
		fmt.Fprintf(stderr, "covercheck: %s: %v\n", *floorsPath, err)
		return 2
	}
	prof := Profile{}
	for _, p := range fs.Args() {
		f, err := os.Open(p)
		if err != nil {
			fmt.Fprintln(stderr, "covercheck:", err)
			return 2
		}
		err = ParseProfile(f, prof)
		f.Close()
		if err != nil {
			fmt.Fprintf(stderr, "covercheck: %s: %v\n", p, err)
			return 2
		}
	}
	res := Evaluate(Packages(prof), floors, *module)
	if err := WriteTable(stdout, res); err != nil {
		fmt.Fprintln(stderr, "covercheck:", err)
		return 2
	}
	if *summary != "" {
		f, err := os.OpenFile(*summary, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(stderr, "covercheck:", err)
			return 2
		}
		err = WriteMarkdown(f, res)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			fmt.Fprintln(stderr, "covercheck:", err)
			return 2
		}
	}
	if len(res.Violations) > 0 {
		fmt.Fprintf(stderr, "\ncovercheck: %d package(s) below their floor:\n", len(res.Violations))
		for _, v := range res.Violations {
			fmt.Fprintln(stderr, "  "+v)
		}
		return 1
	}
	return 0
}
