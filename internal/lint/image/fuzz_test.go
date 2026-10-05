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

package image

import (
	"path/filepath"
	"testing"
)

// FuzzHostPath proves hostPath never escapes the extraction root: for any
// tar entry name it returns an error, or a path below the root whose
// relative path from the root is local.
func FuzzHostPath(f *testing.F) {
	for _, name := range []string{
		"/captf/module/main.tf", "captf/module/main.tf", "", "/", ".", "..",
		"../etc/passwd", "/../../etc/passwd", "captf/../../escape.tf",
		"a/b/../../../c", "//abs/path", `..\..\win`, "captf/module/..notes.tf",
		"a\x00b", "./a/./b/", "....//....//x",
	} {
		f.Add(name)
	}
	f.Fuzz(func(t *testing.T, name string) {
		root := t.TempDir()
		tr := &tree{root: root}
		got, err := tr.hostPath(name)
		if err != nil {
			return
		}
		rel, err := filepath.Rel(root, got)
		if err != nil {
			t.Fatalf("hostPath(%q) = %q: Rel from root: %v", name, got, err)
		}
		if !filepath.IsLocal(rel) && rel != "." {
			t.Fatalf("hostPath(%q) = %q escapes root %q (rel %q)", name, got, root, rel)
		}
	})
}
