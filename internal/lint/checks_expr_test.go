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

package lint

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPoolAutoscalingScope checks that only a resource that itself uses
// var.autoscaling can satisfy the ignore_changes requirement, and that
// nested modules are checked too.
func TestPoolAutoscalingScope(t *testing.T) {
	t.Parallel()
	const ignore = "  lifecycle {\n    ignore_changes = all\n  }\n"
	for _, tt := range []struct {
		name string
		tf   string
		want int
	}{
		{"unrelated resource ignores all", "variable \"autoscaling\" {}\n" +
			"resource \"null_resource\" \"n\" {\n" + ignore + "}\n" +
			"resource \"aws_autoscaling_group\" \"g\" {\n  min_size = var.autoscaling.min\n}\n", 1},
		{"unrelated resource ignores desired_capacity", "variable \"autoscaling\" {}\n" +
			"resource \"null_resource\" \"n\" {\n  lifecycle {\n    ignore_changes = [desired_capacity]\n  }\n}\n" +
			"resource \"aws_autoscaling_group\" \"g\" {\n  min_size = var.autoscaling.min\n}\n", 1},
		{"scaling group itself ignores", "variable \"autoscaling\" {}\n" +
			"resource \"null_resource\" \"n\" {}\n" +
			"resource \"aws_autoscaling_group\" \"g\" {\n  min_size = var.autoscaling.min\n" + ignore + "}\n", 0},
	} {
		m := poolModule(t, map[string]string{"main.tf": tt.tf})
		if got := poolAutoscaling(m); len(got) != tt.want {
			t.Errorf("%s: %+v", tt.name, got)
		}
	}
}

// TestPoolAutoscalingNested checks that a nested module that uses
// var.autoscaling without ignoring the desired count is flagged, with its
// path prefixed, and that a compliant one is not.
func TestPoolAutoscalingNested(t *testing.T) {
	t.Parallel()
	child := func(lifecycle string) string {
		return "variable \"autoscaling\" {}\nresource \"aws_autoscaling_group\" \"g\" {\n  min_size = var.autoscaling.min\n" + lifecycle + "}\n"
	}
	for _, tt := range []struct {
		name      string
		lifecycle string
		want      int
	}{
		{"nested unmanaged", "", 1},
		{"nested managed", "  lifecycle {\n    ignore_changes = [desired_capacity]\n  }\n", 0},
	} {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "child"), 0o750); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{
			"main.tf":       "module \"c\" {\n  source = \"./child\"\n}\n",
			"child/main.tf": child(tt.lifecycle),
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		m, err := LoadModule(dir)
		if err != nil {
			t.Fatal(err)
		}
		got := poolAutoscaling(m)
		if len(got) != tt.want {
			t.Fatalf("%s: %+v", tt.name, got)
		}
		if tt.want == 1 && got[0].File != "child" {
			t.Errorf("%s: File = %q, want child", tt.name, got[0].File)
		}
	}
}

// TestProviderIDStatusWords checks the status-word match is whole-word and
// case-insensitive on string literals and attribute names.
func TestProviderIDStatusWords(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, cond string
		want       int
	}{
		{"lower literal", "i.state == \"running\"", 1},
		{"upper literal", "i.state == \"RUNNING\"", 1},
		{"capitalized literal", "i.status == \"Healthy\"", 1},
		{"list of states", "contains([\"pending\", \"Running\"], i.state)", 1},
		{"healthy attribute", "i.healthy", 1},
		{"unhealthy threshold", "i.unhealthy_threshold > 2", 0},
		{"negated is_running", "!i.is_running", 0},
		{"hyphenated tag", "i.tag == \"healthy-group\"", 0},
		{"unrelated", "i.zone == \"a\"", 0},
	} {
		m := poolModule(t, map[string]string{"main.tf": "output \"provider_id_list\" {\n  value = sort([for i in data.x.y.instances : i.id if " + tt.cond + "])\n}\n"})
		if got := outputProviderIDShape(m); len(got) != tt.want {
			t.Errorf("%s: %+v", tt.name, got)
		}
	}
}

// TestProviderIDSortShapes checks which expression shapes count as sorted.
func TestProviderIDSortShapes(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, value string
		want        int
	}{
		{"sort", "sort(data.x.y.ids)", 0},
		{"parenthesized sort", "(sort(data.x.y.ids))", 0},
		{"tolist sort", "tolist(sort(data.x.y.ids))", 0},
		{"concat of sorts", "concat(sort(data.x.a), sort(data.x.b))", 0},
		{"concat with unsorted", "concat(sort(data.x.a), data.x.b)", 1},
		{"conditional both sorted", "var.x ? sort(data.x.a) : sort(data.x.b)", 0},
		{"conditional one unsorted", "var.x ? sort(data.x.a) : data.x.b", 1},
		{"distinct alone", "distinct(data.x.y.ids)", 1},
		{"distinct of sort", "distinct(sort(data.x.y.ids))", 0},
		{"sort of distinct", "sort(distinct(data.x.y.ids))", 0},
		{"toset", "toset(data.x.y.ids)", 1},
	} {
		m := poolModule(t, map[string]string{"main.tf": "output \"provider_id_list\" {\n  value = " + tt.value + "\n}\n"})
		if got := outputProviderIDShape(m); len(got) != tt.want {
			t.Errorf("%s: %+v", tt.name, got)
		}
	}
}
