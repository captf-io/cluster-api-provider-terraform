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

package shared

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
)

// TestEventsEmitted: every documented event reason is used outside its
// declaration and DocumentedEvents somewhere in the controllers' non-test
// code. A reason that is declared but never emitted, as DigestPinned was,
// fails here.
func TestEventsEmitted(t *testing.T) {
	t.Parallel()
	used := sets.New[string]()
	fset := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			switch decl := decl.(type) {
			case *ast.GenDecl:
				if decl.Tok == token.CONST {
					continue // the declarations themselves
				}
			case *ast.FuncDecl:
				if decl.Name.Name == "DocumentedEvents" {
					continue
				}
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && strings.HasPrefix(id.Name, "Event") {
					used.Insert(id.Name)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range DocumentedEvents() {
		if !used.Has("Event" + reason) {
			t.Errorf("event reason %s is never emitted", reason)
		}
	}
}
