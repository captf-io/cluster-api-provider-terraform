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

package lint

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// TestFileSets proves FileSets splits a mixed file list into Terraform's
// set (.tf, .tf.json) and OpenTofu's (.tofu, .tofu.json, plus a .tf or
// .tf.json file with no .tofu twin), each sorted.
func TestFileSets(t *testing.T) {
	t.Parallel()
	tofu, tf := FileSets([]string{"a.tf", "a.tofu", "b.tf", "c.tf.json", "c.tofu.json", "d.tf.json", "e.tofu", "f.tf"})
	if want := []string{"a.tf", "b.tf", "c.tf.json", "d.tf.json", "f.tf"}; !reflect.DeepEqual(tf, want) {
		t.Errorf("terraform set = %v", tf)
	}
	// a.tofu shadows a.tf; c.tofu.json shadows c.tf.json; f.tf has no .tofu twin.
	if want := []string{"a.tofu", "b.tf", "c.tofu.json", "d.tf.json", "e.tofu", "f.tf"}; !reflect.DeepEqual(tofu, want) {
		t.Errorf("opentofu set = %v", tofu)
	}
}

// TestHCLChecks: one fixture per hcl/v2 check, asserting exact IDs.
func TestHCLChecks(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		role contract.Role
		edit func(files, vars map[string]string)
		want []string
		set  string
	}{
		{"has-backend", contract.RoleMachine, func(f, _ map[string]string) {
			f["backend.tf"] = "terraform {\n  backend \"s3\" {\n    bucket = \"b\"\n  }\n}\n"
		}, []string{IDModuleBackend}, SetTerraform},
		{"cloud", contract.RoleMachine, func(f, _ map[string]string) {
			f["cloud.tf"] = "terraform {\n  cloud {\n    organization = \"o\"\n  }\n}\n"
		}, []string{IDModuleCloud}, SetTerraform},
		{"backend in JSON", contract.RoleMachine, func(f, _ map[string]string) {
			f["backend.tf.json"] = `{"terraform": {"backend": {"s3": {"bucket": "b"}}}}`
		}, []string{IDModuleBackend}, SetTerraform},
		{"literal provider credential", contract.RoleMachine, func(f, _ map[string]string) {
			f["provider.tf"] = "provider \"aws\" {\n  region = \"eu-west-1\"\n  access_key = \"AKIAEXAMPLE\"\n  assume_role {\n    role_arn = \"x\"\n  }\n}\n"
		}, []string{IDModuleProviderConfig}, SetTerraform},
		{"referenced provider credential", contract.RoleMachine, func(f, v map[string]string) {
			f["provider.tf"] = "provider \"aws\" {\n  token = var.token\n}\n"
			v["token"] = "  default = \"\"\n"
		}, nil, SetTerraform},
		{"tags never referenced", contract.RoleMachine, func(f, _ map[string]string) {
			delete(f, "main.tf")
		}, []string{IDInputTagsUnused}, SetTerraform},
		{"tags referenced inside a resource", contract.RoleMachine, func(f, _ map[string]string) {
			f["main.tf"] = "resource \"null_resource\" \"x\" {\n  triggers = merge(var.captf_tags, {a = 1})\n}\n"
		}, nil, SetTerraform},
		{"tags referenced only in JSON", contract.RoleMachine, func(f, _ map[string]string) {
			delete(f, "main.tf")
			f["main.tf.json"] = `{"resource": {"null_resource": {"x": {"triggers": "${var.captf_tags}"}}}}`
		}, nil, SetTerraform},
		{"endpoint literal null", contract.RoleCluster, func(f, _ map[string]string) {
			f["outputs.tf"] = strings.Replace(f["outputs.tf"], "value = var.control_plane_endpoint", "value = null", 1)
		}, []string{IDOutputEndpointNeverSet}, SetTerraform},
		{"endpoint conditional", contract.RoleCluster, func(f, _ map[string]string) {
			f["outputs.tf"] = strings.Replace(f["outputs.tf"], "value = var.control_plane_endpoint",
				"value = var.control_plane_endpoint != null ? var.control_plane_endpoint : null", 1)
		}, nil, SetTerraform},
		{"tofu-shadow: tofu copy omits an output", contract.RoleMachine, func(f, _ map[string]string) {
			f["outputs.tofu"] = strings.Replace(f["outputs.tf"], "output \"interruptible\"", "output \"renamed\"", 1)
		}, []string{IDModuleTofuShadow, IDModuleTofuShadow}, SetOpenTofu},
		{"tofu-shadow: identical copy, reformatted", contract.RoleMachine, func(f, _ map[string]string) {
			f["outputs.tofu"] = strings.ReplaceAll(f["outputs.tf"], "value = ", "value   =   ")
		}, nil, SetOpenTofu},
		{"tofu-shadow: a different default", contract.RoleMachine, func(f, _ map[string]string) {
			f["region.tf"] = "variable \"region\" {\n  default = \"a\"\n}\n"
			f["region.tofu"] = "variable \"region\" {\n  default = \"b\"\n}\n"
		}, []string{IDModuleTofuShadow}, SetOpenTofu},
		{"backend only in a .tofu file", contract.RoleMachine, func(f, _ map[string]string) {
			f["backend.tofu"] = "terraform {\n  backend \"local\" {}\n}\n"
		}, []string{IDModuleBackend, IDModuleTofuShadow}, SetOpenTofu},
		// module/backend, module/cloud and module/provider-config are
		// defense in depth over every nested local module too.
		{"literal provider credential in a nested module", contract.RoleMachine, func(f, _ map[string]string) {
			f["nested.tf"] = "module \"child\" {\n  source = \"./child\"\n}\n"
			f["child/provider.tf"] = "provider \"aws\" {\n  access_key = \"AKIAEXAMPLE\"\n}\n"
		}, []string{IDModuleProviderConfig}, SetTerraform},
		{"backend and cloud in a nested module", contract.RoleMachine, func(f, _ map[string]string) {
			f["nested.tf"] = "module \"child\" {\n  source = \"./child\"\n}\n"
			f["child/main.tf"] = "terraform {\n  backend \"s3\" {\n    bucket = \"b\"\n  }\n  cloud {\n    organization = \"o\"\n  }\n}\n"
		}, []string{IDModuleBackend, IDModuleCloud}, SetTerraform},
		{"a grandchild module is scanned too", contract.RoleMachine, func(f, _ map[string]string) {
			f["nested.tf"] = "module \"child\" {\n  source = \"./child\"\n}\n"
			f["child/main.tf"] = "module \"grandchild\" {\n  source = \"./grandchild\"\n}\n"
			f["child/grandchild/provider.tf"] = "provider \"aws\" {\n  token = \"literal\"\n}\n"
		}, []string{IDModuleProviderConfig}, SetTerraform},
		// input/tags-unused counts a reference inside a nested module that
		// receives captf_tags through its module call.
		{"tags forwarded to and used by a nested module", contract.RoleMachine, func(f, _ map[string]string) {
			delete(f, "main.tf")
			f["nested.tf"] = "module \"child\" {\n  source = \"./child\"\n  resource_tags = var.captf_tags\n}\n"
			f["child/main.tf"] = "variable \"resource_tags\" {\n  type = any\n}\nresource \"null_resource\" \"x\" {\n  triggers = var.resource_tags\n}\n"
		}, nil, SetTerraform},
		{"tags forwarded but not used by a nested module", contract.RoleMachine, func(f, _ map[string]string) {
			delete(f, "main.tf")
			f["nested.tf"] = "module \"child\" {\n  source = \"./child\"\n  resource_tags = var.captf_tags\n}\n"
			f["child/main.tf"] = "variable \"resource_tags\" {\n  type = any\n}\nresource \"null_resource\" \"x\" {}\n"
		}, []string{IDInputTagsUnused}, SetTerraform},
	} {
		m := module(t, tt.role, tt.edit)
		r, err := Lint(m, tt.role)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, f := range r.Findings {
			got = append(got, f.ID)
		}
		slices.Sort(got)
		if !slices.Equal(got, tt.want) || r.FileSet != tt.set {
			t.Errorf("%s: findings %v (set %s), want %v (set %s)", tt.name, r.Findings, r.FileSet, tt.want, tt.set)
		}
	}
}

// TestBrokenTofuFile proves LoadModule returns ErrParse for a module whose
// only file is a .tofu file that fails to parse, since tfconfig never
// opens .tofu files itself.
func TestBrokenTofuFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tofu"), []byte("variable \"x\" {\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadModule(dir); !errors.Is(err, ErrParse) {
		t.Errorf("a broken .tofu file loaded: %v", err)
	}
}

// poolModule uses t to write files as a module directory's contents and
// load it, failing t on a write or load error. It returns the loaded
// Module.
func poolModule(t *testing.T, files map[string]string) *Module {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m, err := LoadModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestPoolExpressions: the pool checks' expression-level fixtures, called
// directly (TestPoolAutoscalingCheck and TestPoolRole in lint_test.go
// exercise them through Checks(machinepool) and Lint end to end).
func TestPoolExpressions(t *testing.T) {
	t.Parallel()
	const asg = "variable \"autoscaling\" {}\nresource \"aws_autoscaling_group\" \"g\" {\n  min_size = var.autoscaling.min\n%s}\n"
	for _, tt := range []struct {
		name      string
		lifecycle string
		want      int
	}{
		{"no ignore_changes", "", 1},
		{"desired_capacity ignored", "  lifecycle {\n    ignore_changes = [desired_capacity]\n  }\n", 0},
		{"sku capacity ignored", "  lifecycle {\n    ignore_changes = [sku[0].capacity]\n  }\n", 0},
		{"ignore all", "  lifecycle {\n    ignore_changes = all\n  }\n", 0},
		{"something else ignored", "  lifecycle {\n    ignore_changes = [tags]\n  }\n", 1},
	} {
		m := poolModule(t, map[string]string{"main.tf": strings.Replace(asg, "%s", tt.lifecycle, 1)})
		if got := poolAutoscaling(m); len(got) != tt.want {
			t.Errorf("%s: %+v", tt.name, got)
		}
	}
	if got := poolAutoscaling(poolModule(t, map[string]string{"main.tf": "resource \"x\" \"y\" {}\n"})); len(got) != 0 {
		t.Errorf("no autoscaling: %+v", got)
	}

	for _, tt := range []struct {
		name, value string
		want        int
	}{
		{"filtered and unsorted", "[for i in data.x.y.instances : i.id if i.state == \"running\"]", 2},
		{"sorted, unfiltered", "sort([for i in data.x.y.instances : i.id])", 0},
		{"distinct alone", "distinct(data.x.y.ids)", 1},
		{"distinct of sort", "distinct(sort(data.x.y.ids))", 0},
		{"unsorted", "data.x.y.ids", 1},
		{"sorted but filtered on health", "sort([for i in data.x.y.instances : i.id if i.healthy])", 1},
	} {
		m := poolModule(t, map[string]string{"main.tf": "output \"provider_id_list\" {\n  value = " + tt.value + "\n}\n"})
		if got := outputProviderIDShape(m); len(got) != tt.want {
			t.Errorf("%s: %+v", tt.name, got)
		}
	}
	// JSON syntax is not scanned; no output is no finding here.
	if got := outputProviderIDShape(poolModule(t, map[string]string{"main.tf.json": `{"output": {"provider_id_list": {"value": "${data.x.ids}"}}}`})); len(got) != 0 {
		t.Errorf("JSON: %+v", got)
	}
	if got := outputProviderIDShape(poolModule(t, map[string]string{"main.tf": "locals {}\n"})); len(got) != 0 {
		t.Errorf("no output: %+v", got)
	}
}
