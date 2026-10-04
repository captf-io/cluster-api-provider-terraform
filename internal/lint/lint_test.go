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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// module uses t to write a module whose variables follow the contract of
// role exactly, applies edit (when non-nil) to the generated file set,
// and loads it. The good modules are generated here rather than read from
// disk. It returns the loaded Module.
func module(t *testing.T, role contract.Role, edit func(files map[string]string, vars map[string]string)) *Module {
	t.Helper()
	return buildModule(t, role, ".tf", edit)
}

// buildModule is module, using t and role as module does, with the
// generated file set written with ext instead of ".tf" (ext ".tofu"
// builds a module tfconfig itself never reads a single declaration of,
// exercising the hcl/v2 pass's own Config). edit may
// still add files under any extension of its choosing. It returns the
// loaded Module.
func buildModule(t *testing.T, role contract.Role, ext string, edit func(files map[string]string, vars map[string]string)) *Module {
	t.Helper()
	specs, err := contract.InputSpecs(role)
	if err != nil {
		t.Fatal(err)
	}
	outputs, err := contract.RequiredOutputs(role)
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{}
	for name, s := range specs {
		body := "  type = " + s.Type + "\n"
		if s.Sensitive {
			body += "  sensitive = true\n"
		}
		if s.Nullable {
			body += "  default = null\n"
		}
		vars[name] = body
	}
	var outs strings.Builder
	for _, o := range outputs {
		value := "null"
		if o == "control_plane_endpoint" {
			value = "var.control_plane_endpoint"
		}
		fmt.Fprintf(&outs, "output %q {\n  value = %s\n}\n", o, value)
	}
	files := map[string]string{
		"outputs" + ext:  outs.String(),
		"versions" + ext: "terraform {\n  required_version = \">= 1.10\"\n}\n",
		"main" + ext:     "locals {\n  tags = var.captf_tags\n}\n",
	}
	if edit != nil {
		edit(files, vars)
	}
	var vb strings.Builder
	for _, name := range slices.Sorted(maps.Keys(vars)) {
		fmt.Fprintf(&vb, "variable %q {\n%s}\n", name, vars[name])
	}
	files["variables"+ext] = vb.String()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m, err := LoadModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// ids uses t to run Lint on m for role, failing t on error, and returns
// the resulting findings' IDs.
func ids(t *testing.T, m *Module, role contract.Role) []string {
	t.Helper()
	r, err := Lint(m, role)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range r.Findings {
		out = append(out, f.ID)
	}
	return out
}

// TestGoodModules proves a module generated to follow the contract exactly
// has no findings, for the cluster, machine and machinepool roles. The
// machinepool role needs its provider_id_list output shaped correctly
// (sort()-wrapped), which the generic outputs generator does not produce.
func TestGoodModules(t *testing.T) {
	t.Parallel()
	for _, role := range []contract.Role{contract.RoleCluster, contract.RoleMachine, contract.RoleMachinePool} {
		var edit func(files, vars map[string]string)
		if role == contract.RoleMachinePool {
			edit = poolOutputsEdit
		}
		if got := ids(t, module(t, role, edit), role); len(got) != 0 {
			t.Errorf("%s: findings %v on a conforming module", role, got)
		}
	}
}

// poolOutputsEdit overrides files' generic outputs file with one that
// declares provider_id_list as a sort()-wrapped list, satisfying
// output/provider-id-list-shape; it matches the edit func(files, vars
// map[string]string) signature buildModule takes, ignoring vars.
func poolOutputsEdit(files, _ map[string]string) {
	files["outputs.tf"] = `output "provider_id" {
  value = null
}
output "provider_id_list" {
  value = sort([])
}
output "replicas" {
  value = null
}
output "instances" {
  value = null
}
output "health" {
  value = null
}
`
}

// TestAcceptance: the three cases of the task's acceptance, each with
// exactly its one finding.
func TestAcceptance(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		edit func(files, vars map[string]string)
		want []string
	}{
		{"missing bootstrap_format", func(_, v map[string]string) { delete(v, "bootstrap_format") }, []string{IDInputRequired}},
		{"user variable without default", func(_, v map[string]string) { v["foo"] = "" }, []string{IDInputUserVariableDefault}},
		{"reserved prefix", func(_, v map[string]string) { v["captf_x"] = "  type = string\n" }, []string{IDInputReserved}},
	} {
		if got := ids(t, module(t, contract.RoleMachine, tt.edit), contract.RoleMachine); !slices.Equal(got, tt.want) {
			t.Errorf("%s: findings %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestChecks: one failing fixture per check ID, asserting the exact IDs.
func TestChecks(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		role contract.Role
		edit func(files, vars map[string]string)
		want []string
	}{
		{"extra variable with a default is fine", contract.RoleMachine, func(_, v map[string]string) { v["region"] = "  default = \"x\"\n" }, nil},
		{"captf_tags missing", contract.RoleCluster, func(_, v map[string]string) { delete(v, "captf_tags") }, []string{IDInputTagsDeclared}},
		{"wrong type", contract.RoleCluster, func(_, v map[string]string) {
			v["control_plane_initialized"] = "  type = string\n"
		}, []string{IDInputType}},
		{"object missing a required attribute", contract.RoleCluster, func(_, v map[string]string) {
			v["control_plane_endpoint"] = "  type = object({host=string, port=number, zone=string})\n  default = null\n"
		}, []string{IDInputType}},
		{"no type", contract.RoleMachine, func(_, v map[string]string) { v["machine_name"] = "" }, []string{IDInputType}},
		{"bootstrap_data not sensitive", contract.RoleMachine, func(_, v map[string]string) {
			v["bootstrap_data"] = "  type = string\n"
		}, []string{IDInputSensitive}},
		{"default on a non-nullable input", contract.RoleMachine, func(_, v map[string]string) {
			v["machine_name"] = "  type = string\n  default = \"m\"\n"
		}, []string{IDInputDefault}},
		// The contract skeletons declare captf_cluster_outputs with
		// default = null for every role.
		{"captf_cluster_outputs defaulted on a machine", contract.RoleMachine, func(_, v map[string]string) {
			v["captf_cluster_outputs"] = "  type = any\n  default = null\n"
		}, nil},
		{"captf_cluster_outputs defaulted on a cluster", contract.RoleCluster, func(_, v map[string]string) {
			v["captf_cluster_outputs"] = "  type = any\n  default = null\n"
		}, nil},
		{"captf_cluster_outputs required on a cluster", contract.RoleCluster, func(_, v map[string]string) {
			v["captf_cluster_outputs"] = "  type = any\n"
		}, []string{IDInputReserved}},
		{"output missing", contract.RoleMachine, func(f, _ map[string]string) {
			f["outputs.tf"] = strings.Replace(f["outputs.tf"], "output \"provider_id\"", "output \"renamed\"", 1)
		}, []string{IDOutputRequired}},
		{"health missing", contract.RoleCluster, func(f, _ map[string]string) {
			f["outputs.tf"] = strings.Replace(f["outputs.tf"], "output \"health\"", "output \"status\"", 1)
		}, []string{IDOutputHealth}},
		{"reserved output", contract.RoleCluster, func(f, _ map[string]string) {
			f["extra.tf"] = "output \"captf_debug\" {\n  value = 1\n}\n"
		}, []string{IDOutputReserved}},
		{"no required_version", contract.RoleCluster, func(f, _ map[string]string) {
			delete(f, "versions.tf")
		}, []string{IDModuleVersion}},
	} {
		if got := ids(t, module(t, tt.role, tt.edit), tt.role); !slices.Equal(got, tt.want) {
			t.Errorf("%s: findings %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestFindingPositions proves a finding tied to a declaration carries its
// file (relative to the module directory), a non-zero line and the
// check's severity.
func TestFindingPositions(t *testing.T) {
	t.Parallel()
	m := module(t, contract.RoleMachine, func(_, v map[string]string) { v["foo"] = "" })
	r, err := Lint(m, contract.RoleMachine)
	if err != nil {
		t.Fatal(err)
	}
	if f := r.Findings[0]; f.File != "variables.tf" || f.Line == 0 || f.Severity != SeverityWarning {
		t.Errorf("finding = %+v", f)
	}
}

// TestUserVariables: a variable outside the contract is a user variable. It
// is never an error; without a default it is input/user-variable-default,
// a warning, because an object that does not set it cannot apply; with a
// default, or typed and sensitive, it has no finding at all.
func TestUserVariables(t *testing.T) {
	t.Parallel()
	for _, role := range []contract.Role{contract.RoleCluster, contract.RoleMachine} {
		m := module(t, role, func(_, v map[string]string) {
			v["instance_type"] = "  type = string\n"
			v["disk_gib"] = "  type = number\n  default = 20\n"
			v["db_password"] = "  type = string\n  sensitive = true\n  default = null\n"
		})
		r, err := Lint(m, role)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Findings) != 1 {
			t.Fatalf("%s: findings %+v, want one", role, r.Findings)
		}
		f := r.Findings[0]
		if f.ID != IDInputUserVariableDefault || f.Severity != SeverityWarning || !strings.Contains(f.Message, "instance_type") {
			t.Errorf("%s: finding %+v", role, f)
		}
	}
	checks, err := Checks(contract.RoleMachine)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if c.ID == IDInputUserVariableDefault && c.Severity != SeverityWarning {
			t.Errorf("registry severity = %v", c.Severity)
		}
	}
}

// TestReportStable: the same module gives byte-identical JSON across runs,
// ordered by file, line and ID, with a summary.
func TestReportStable(t *testing.T) {
	t.Parallel()
	edit := func(f, v map[string]string) {
		v["foo"], v["captf_x"], v["bar"] = "", "", ""
		delete(v, "captf_tags")
		delete(f, "versions.tf")
	}
	var first []byte
	for i := range 5 {
		r, err := Lint(module(t, contract.RoleMachine, edit), contract.RoleMachine)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = b
			if r.Summary != (Summary{Errors: 2, Warnings: 2, Infos: 1}) {
				t.Errorf("summary = %+v in %s", r.Summary, b)
			}
			continue
		}
		if !bytes.Equal(b, first) {
			t.Fatalf("run %d differs:\n%s\n%s", i, first, b)
		}
	}
	if !strings.Contains(string(first), `"severity":"error"`) {
		t.Errorf("severity is not written by name: %s", first)
	}
}

// TestLoadModuleErrors proves LoadModule errors on a missing directory and
// returns ErrParse for a module that fails to parse.
func TestLoadModuleErrors(t *testing.T) {
	t.Parallel()
	if _, err := LoadModule(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing directory loaded")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte("variable \"x\" {\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadModule(dir); !errors.Is(err, ErrParse) {
		t.Errorf("unparsable module: %v", err)
	}
}

// TestModuleFiles proves LoadModule collects a module's .tf, .tf.json,
// .tofu and .tofu.json files, sorted, and skips other extensions and a
// directory that merely has a module-file-like name.
func TestModuleFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"b.tf", "a.tf.json", "c.tofu", "d.tofu.json", "README.md", "e.tfvars"} {
		content := ""
		if strings.HasSuffix(name, ".json") {
			content = "{}"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.tf"), 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := LoadModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.tf.json", "b.tf", "c.tofu", "d.tofu.json"}; !reflect.DeepEqual(m.Files, want) {
		t.Errorf("files = %v, want %v", m.Files, want)
	}
}

// TestPoolRole proves Checks(machinepool) returns the pool-specific
// checks, and that outputProviderIDs, the declaration half of
// output/provider-id-list-shape, fires only when provider_id_list is not
// declared.
func TestPoolRole(t *testing.T) {
	t.Parallel()
	checks, err := Checks(contract.RoleMachinePool)
	if err != nil {
		t.Fatalf("Checks(machinepool): %v", err)
	}
	for _, id := range []string{IDOutputProviderIDs, IDPoolAutoscaling} {
		if !slices.ContainsFunc(checks, func(c Check) bool { return c.ID == id }) {
			t.Errorf("Checks(machinepool) does not include %s", id)
		}
	}
	m := module(t, contract.RoleMachine, nil)
	if got := outputProviderIDs(m); len(got) != 1 || got[0].ID != IDOutputProviderIDs {
		t.Errorf("findings = %+v", got)
	}
	m = module(t, contract.RoleMachine, func(f, _ map[string]string) {
		f["pool.tf"] = "output \"provider_id_list\" {\n  value = []\n}\n"
	})
	if got := outputProviderIDs(m); len(got) != 0 {
		t.Errorf("findings = %+v", got)
	}
}

// TestPoolAutoscalingCheck proves the machinepool role's Lint run flags a
// module that references var.autoscaling without ignoring the group's
// desired capacity, and accepts one that does.
func TestPoolAutoscalingCheck(t *testing.T) {
	t.Parallel()
	m := module(t, contract.RoleMachinePool, func(f, _ map[string]string) {
		poolOutputsEdit(f, nil)
		f["autoscale.tf"] = `resource "terraform_data" "asg" {
  input = var.autoscaling
}
`
	})
	if got := ids(t, m, contract.RoleMachinePool); !slices.Contains(got, IDPoolAutoscaling) {
		t.Errorf("findings %v missing %s", got, IDPoolAutoscaling)
	}
	m = module(t, contract.RoleMachinePool, func(f, _ map[string]string) {
		poolOutputsEdit(f, nil)
		f["autoscale.tf"] = `resource "terraform_data" "asg" {
  input = var.autoscaling
  lifecycle {
    ignore_changes = [desired_capacity]
  }
}
`
	})
	if got := ids(t, m, contract.RoleMachinePool); slices.Contains(got, IDPoolAutoscaling) {
		t.Errorf("findings %v: want no %s", got, IDPoolAutoscaling)
	}
}

// TestTofuOnlyModule: a module written only in .tofu files (item 1) is
// invisible to terraform-config-inspect (it never opens .tofu files), so
// it must not fail every input/* and output/* check.
func TestTofuOnlyModule(t *testing.T) {
	t.Parallel()
	for _, role := range []contract.Role{contract.RoleCluster, contract.RoleMachine} {
		m := buildModule(t, role, ".tofu", nil)
		if m.Set() != SetOpenTofu {
			t.Errorf("%s: file set = %s, want %s", role, m.Set(), SetOpenTofu)
		}
		if got := ids(t, m, role); len(got) != 0 {
			t.Errorf("%s: findings %v on a .tofu-only module", role, got)
		}
	}
}

// TestIgnoredModuleFiles: a dangling editor lock file next to real module
// files is skipped, not parsed (item 4; terraform-config-inspect's own
// ignore rules, tfconfig/load.go isIgnoredFile).
func TestIgnoredModuleFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte("variable \"x\" {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A dangling Emacs lock file: a symlink to a target that does not
	// exist. isModuleFile must never even try to open it.
	if err := os.Symlink("nobody@nowhere.1234:5678", filepath.Join(dir, ".#main.tf")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"main.tf.bak~", "#main.tf#"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not valid hcl {{{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m, err := LoadModule(dir)
	if err != nil {
		t.Fatalf("a module with editor artifacts did not load: %v", err)
	}
	if want := []string{"main.tf"}; !slices.Equal(m.Files, want) {
		t.Errorf("files = %v, want %v", m.Files, want)
	}
}

// TestSeverity proves Severity.String names each known severity and falls
// back to "severity(N)" for an out-of-range value.
func TestSeverity(t *testing.T) {
	t.Parallel()
	for s, want := range map[Severity]string{SeverityInfo: "info", SeverityWarning: "warning", SeverityError: "error", Severity(9): "severity(9)"} {
		if s.String() != want {
			t.Errorf("%d = %s", int(s), s)
		}
	}
}
