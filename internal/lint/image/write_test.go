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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTreeWriteStaysUnderRoot proves write puts every name below the
// extraction root: a plain path, a file whose name merely starts with "..",
// and names that try to climb out, which are cleaned against the root and
// land inside it, never beside it.
func TestTreeWriteStaysUnderRoot(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, want string
	}{
		{name: "/captf/module/main.tf", want: "captf/module/main.tf"},
		{name: "/captf/module/..notes.tf", want: "captf/module/..notes.tf"},
		{name: "/../escape.tf", want: "escape.tf"},
		{name: "/captf/../../escape.tf", want: "escape.tf"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			root := filepath.Join(parent, "root")
			tr := &tree{root: root}
			if err := tr.write(tt.name, strings.NewReader("x"), 1); err != nil {
				t.Fatalf("write(%q): %v", tt.name, err)
			}
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(tt.want))); err != nil {
				t.Errorf("write(%q) did not create %s under the root: %v", tt.name, tt.want, err)
			}
			if _, err := os.Stat(filepath.Join(parent, "escape.tf")); !os.IsNotExist(err) {
				t.Errorf("write(%q) created a file outside the root (stat err %v)", tt.name, err)
			}
		})
	}
}
