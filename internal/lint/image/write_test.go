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

// TestTreeWriteStaysUnderRoot proves write accepts every name that resolves
// under the extraction root, including a file whose name merely starts
// with "..", and refuses one that climbs out.
func TestTreeWriteStaysUnderRoot(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		ok   bool
	}{
		{name: "/captf/module/main.tf", ok: true},
		{name: "/captf/module/..notes.tf", ok: true},
		{name: "/../escape.tf", ok: false},
		{name: "/captf/../../escape.tf", ok: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			root := filepath.Join(parent, "root")
			tr := &tree{root: root}
			err := tr.write(tt.name, strings.NewReader("x"), 1)
			if (err == nil) != tt.ok {
				t.Fatalf("write(%q) error = %v, want ok=%v", tt.name, err, tt.ok)
			}
			if _, statErr := os.Stat(filepath.Join(parent, "escape.tf")); !os.IsNotExist(statErr) {
				t.Errorf("write(%q) created a file outside the root (stat err %v)", tt.name, statErr)
			}
		})
	}
}
