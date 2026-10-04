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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// MinPackageDoc is the shortest package comment that counts as a package
// overview, in characters.
const MinPackageDoc = 400

// skipDirs are directory names never walked: pinned tools, build output,
// VCS and editor state.
var skipDirs = []string{".git", ".claude", ".idea", ".vscode", "bin", "dist", "out", "node_modules", ".terraform"}

// returnWords is how a doc comment says what a function returns.
var returnWords = regexp.MustCompile(`(?i)\b(return|returns|returned|reports)\b`)

// generatedHeader is the Go convention marking a generated file.
var generatedHeader = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

// Finding is one documentation gap.
type Finding struct {
	// Pos is where the gap is.
	Pos token.Position
	// Msg says what is missing.
	Msg string
}

// main runs the check over the directory named by the first argument, or
// the current directory, and exits 1 when it finds a gap or 2 when the
// tree cannot be read.
func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	os.Exit(run(root, os.Stdout))
}

// run checks every Go file under root, writes each finding and a summary to
// w, and returns the process exit code: 0 clean, 1 findings, 2 on a read or
// parse error.
func run(root string, w io.Writer) int {
	findings, err := Check(root)
	if err != nil {
		fmt.Fprintf(w, "godoccheck: %v\n", err)
		return 2
	}
	for _, f := range findings {
		fmt.Fprintf(w, "%s: %s\n", f.Pos, f.Msg)
	}
	if len(findings) > 0 {
		fmt.Fprintf(w, "godoccheck: %d finding(s)\n", len(findings))
		return 1
	}
	fmt.Fprintln(w, "godoccheck: every declaration and package is documented")
	return 0
}

// pkgKey identifies one package: its directory and its package name, so an
// external _test package in the same directory is a separate package.
type pkgKey struct{ dir, name string }

// pkgInfo collects what the package rule needs about one package.
type pkgInfo struct {
	// hasDocGo is true when the package has a doc.go file.
	hasDocGo bool
	// doc is the package comment found in doc.go.
	doc string
	// docPos is where that comment is, for a too-short finding.
	docPos token.Position
	// firstFile is a file of the package, for a missing-doc.go finding.
	firstFile string
}

// Check walks root and returns every documentation gap in the Go files it
// finds, sorted by position, or an error when a file cannot be read or
// parsed.
func Check(root string) ([]Finding, error) {
	fset := token.NewFileSet()
	pkgs := map[pkgKey]*pkgInfo{}
	var findings []Finding
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (slices.Contains(skipDirs, d.Name()) || strings.HasPrefix(d.Name(), ".") ||
				filepath.ToSlash(path) == filepath.ToSlash(filepath.Join(root, "hack", "tools"))) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasPrefix(d.Name(), "zz_generated.") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if generatedHeader.Match(src) {
			return nil
		}
		file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		findings = append(findings, checkFile(fset, file, strings.HasSuffix(path, "_test.go"))...)
		notePackage(fset, pkgs, path, file)
		return nil
	})
	if err != nil {
		return nil, err
	}
	findings = append(findings, checkPackages(pkgs)...)
	slices.SortFunc(findings, func(a, b Finding) int {
		if c := strings.Compare(a.Pos.Filename, b.Pos.Filename); c != 0 {
			return c
		}
		return a.Pos.Line - b.Pos.Line
	})
	return findings, nil
}

// notePackage records file's package, its doc.go and package comment in
// pkgs, keyed by the file's directory and package name; path is the file's
// path and fset resolves positions.
func notePackage(fset *token.FileSet, pkgs map[pkgKey]*pkgInfo, path string, file *ast.File) {
	name := file.Name.Name
	if strings.HasSuffix(name, "_test") {
		return // an external test package needs no doc.go
	}
	key := pkgKey{dir: filepath.Dir(path), name: name}
	info := pkgs[key]
	if info == nil {
		info = &pkgInfo{firstFile: path}
		pkgs[key] = info
	}
	if filepath.Base(path) == "doc.go" {
		info.hasDocGo = true
		if file.Doc != nil {
			info.doc = file.Doc.Text()
			info.docPos = fset.Position(file.Doc.Pos())
		} else {
			info.docPos = fset.Position(file.Package)
		}
	}
}

// checkPackages returns a finding for every package in pkgs without a
// doc.go, and for every doc.go whose package comment is shorter than
// MinPackageDoc.
func checkPackages(pkgs map[pkgKey]*pkgInfo) []Finding {
	var out []Finding
	for key, info := range pkgs {
		switch {
		case !info.hasDocGo:
			out = append(out, Finding{Pos: token.Position{Filename: info.firstFile, Line: 1},
				Msg: fmt.Sprintf("package %s (%s) has no doc.go", key.name, key.dir)})
		case len(strings.TrimSpace(info.doc)) < MinPackageDoc:
			out = append(out, Finding{Pos: info.docPos,
				Msg: fmt.Sprintf("package %s: the doc.go package comment is %d characters, want an overview of at least %d",
					key.name, len(strings.TrimSpace(info.doc)), MinPackageDoc)})
		}
	}
	return out
}

// checkFile returns the gaps in file's top-level declarations; isTest marks
// a _test.go file, whose Test, Benchmark and Fuzz functions need not
// mention their testing parameter, and fset resolves positions.
func checkFile(fset *token.FileSet, file *ast.File, isTest bool) []Finding {
	var out []Finding
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			out = append(out, checkFunc(fset, d, isTest)...)
		case *ast.GenDecl:
			out = append(out, checkGen(fset, d)...)
		}
	}
	return out
}

// checkFunc returns the gaps of one function or method declaration d; isTest
// marks a _test.go file and fset resolves positions.
func checkFunc(fset *token.FileSet, d *ast.FuncDecl, isTest bool) []Finding {
	pos := fset.Position(d.Pos())
	name := d.Name.Name
	if d.Doc == nil {
		return []Finding{{Pos: pos, Msg: fmt.Sprintf("func %s has no doc comment", name)}}
	}
	doc := d.Doc.Text()
	var out []Finding
	if !startsWithName(doc, name) {
		out = append(out, Finding{Pos: pos, Msg: fmt.Sprintf("func %s: the doc comment does not start with %q", name, name)})
	}
	exemptTestingParam := isTest && d.Recv == nil && isTestingFunc(name)
	out = append(out, checkSignature(pos, "func "+name, doc, d.Type, exemptTestingParam)...)
	return out
}

// checkSignature returns the parameter and return gaps of typ against doc,
// for the declaration what at pos; exemptTestingParam skips the *testing.T,
// *testing.B or *testing.F parameter of a Test, Benchmark or Fuzz function.
func checkSignature(pos token.Position, what, doc string, typ *ast.FuncType, exemptTestingParam bool) []Finding {
	var out []Finding
	if typ.Params != nil {
		for _, field := range typ.Params.List {
			if exemptTestingParam && isTestingType(field.Type) {
				continue
			}
			for _, n := range field.Names {
				if n.Name == "_" || mentions(doc, n.Name) {
					continue
				}
				out = append(out, Finding{Pos: pos, Msg: fmt.Sprintf("%s: parameter %s is not documented", what, n.Name)})
			}
		}
	}
	if typ.Results != nil && len(typ.Results.List) > 0 && !returnWords.MatchString(doc) {
		out = append(out, Finding{Pos: pos, Msg: fmt.Sprintf("%s: the return value is not documented (say what it returns)", what)})
	}
	return out
}

// checkGen returns the gaps of one const, var or type declaration d; fset
// resolves positions. Imports need no documentation.
func checkGen(fset *token.FileSet, d *ast.GenDecl) []Finding {
	var out []Finding
	switch d.Tok {
	case token.TYPE:
		for _, spec := range d.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			pos := fset.Position(ts.Pos())
			doc := ts.Doc
			if doc == nil && len(d.Specs) == 1 {
				doc = d.Doc
			}
			if doc == nil {
				out = append(out, Finding{Pos: pos, Msg: fmt.Sprintf("type %s has no doc comment", ts.Name.Name)})
			} else if !startsWithName(doc.Text(), ts.Name.Name) {
				out = append(out, Finding{Pos: pos, Msg: fmt.Sprintf("type %s: the doc comment does not start with %q", ts.Name.Name, ts.Name.Name)})
			}
			if it, ok := ts.Type.(*ast.InterfaceType); ok {
				out = append(out, checkInterface(fset, ts.Name.Name, it)...)
			}
		}
	case token.CONST, token.VAR:
		if d.Doc != nil {
			return nil // one comment documents the whole group
		}
		for _, spec := range d.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || vs.Doc != nil || vs.Comment != nil {
				continue
			}
			for _, n := range vs.Names {
				if n.Name == "_" {
					continue // a compile-time assertion such as var _ I = (*T)(nil)
				}
				out = append(out, Finding{Pos: fset.Position(n.Pos()), Msg: fmt.Sprintf("%s %s has no doc comment", d.Tok, n.Name)})
			}
		}
	}
	return out
}

// checkInterface returns the gaps of the methods of interface it, named
// iface; fset resolves positions. Embedded interfaces need no comment.
func checkInterface(fset *token.FileSet, iface string, it *ast.InterfaceType) []Finding {
	var out []Finding
	for _, m := range it.Methods.List {
		ft, ok := m.Type.(*ast.FuncType)
		if !ok || len(m.Names) == 0 {
			continue
		}
		name := iface + "." + m.Names[0].Name
		pos := fset.Position(m.Pos())
		if m.Doc == nil {
			out = append(out, Finding{Pos: pos, Msg: fmt.Sprintf("method %s has no doc comment", name)})
			continue
		}
		out = append(out, checkSignature(pos, "method "+name, m.Doc.Text(), ft, false)...)
	}
	return out
}

// startsWithName reports whether doc starts with name, optionally after an
// article ("A", "An", "The"), which Go allows for type comments.
func startsWithName(doc, name string) bool {
	doc = strings.TrimSpace(doc)
	for _, article := range []string{"", "A ", "An ", "The "} {
		rest, ok := strings.CutPrefix(doc, article+name)
		if ok && (rest == "" || !isIdentRune(rune(rest[0]))) {
			return true
		}
	}
	return false
}

// mentions reports whether doc contains name as a whole word.
func mentions(doc, name string) bool {
	for i := 0; ; {
		j := strings.Index(doc[i:], name)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(name)
		before := start == 0 || !isIdentRune(rune(doc[start-1]))
		after := end == len(doc) || !isIdentRune(rune(doc[end]))
		if before && after {
			return true
		}
		i = end
	}
}

// isIdentRune reports whether r can be part of a Go identifier.
func isIdentRune(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// isTestingFunc reports whether name is a Test, Benchmark or Fuzz function
// name, whose single testing parameter is conventional.
func isTestingFunc(name string) bool {
	for _, p := range []string{"Test", "Benchmark", "Fuzz"} {
		if rest, ok := strings.CutPrefix(name, p); ok && (rest == "" || !isLowerStart(rest)) {
			return true
		}
	}
	return false
}

// isLowerStart reports whether s starts with a lowercase letter, which makes
// e.g. "Testable" not a test function name.
func isLowerStart(s string) bool {
	return s != "" && s[0] >= 'a' && s[0] <= 'z'
}

// isTestingType reports whether expr is *testing.T, *testing.B or
// *testing.F.
func isTestingType(expr ast.Expr) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing" && (sel.Sel.Name == "T" || sel.Sel.Name == "B" || sel.Sel.Name == "F")
}
