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
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// writeTree uses t to write files (name -> content, "/"-separated for a
// nested path) under dir, creating parent directories as needed.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestNestedModules: a relative-path module call loads the child, and its
// findings and Rel are reachable from the root.
func TestNestedModules(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"main.tf":       "module \"child\" {\n  source = \"./child\"\n}\n",
		"child/main.tf": "resource \"null_resource\" \"x\" {}\n",
	})
	m, err := LoadModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Nested) != 1 {
		t.Fatalf("Nested = %+v, want 1 entry", m.Nested)
	}
	n := m.Nested[0]
	if n.Name != "child" || n.Rel != "child" {
		t.Errorf("nested = %+v", n)
	}
	if n.Module == nil || n.Dir != filepath.Join(dir, "child") {
		t.Errorf("nested.Dir = %q", n.Dir)
	}
}

// TestNestedModuleRemoteSource: a registry or git source is never
// fetched; only a relative path (./ or ../) is followed.
func TestNestedModuleRemoteSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"main.tf": "module \"vpc\" {\n  source = \"terraform-aws-modules/vpc/aws\"\n  version = \"5.0.0\"\n}\n" +
			"module \"other\" {\n  source = \"git::https://example.com/vpc.git\"\n}\n",
	})
	m, err := LoadModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Nested) != 0 {
		t.Errorf("Nested = %+v, want none", m.Nested)
	}
}

// TestNestedModuleCycle: A calls B, B calls back to A: LoadModule must not
// recurse forever, and must still succeed.
func TestNestedModuleCycle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"main.tf":   "module \"b\" {\n  source = \"./b\"\n}\n",
		"b/main.tf": "module \"back\" {\n  source = \"../\"\n}\n",
	})
	m, err := LoadModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Nested) != 1 || m.Nested[0].Name != "b" {
		t.Fatalf("Nested = %+v", m.Nested)
	}
	b := m.Nested[0]
	if len(b.Nested) != 0 {
		t.Errorf("the cycle back to the root was followed: %+v", b.Nested)
	}
}

// TestNestedModuleEscape: a module call whose relative path resolves
// outside the root module directory is never loaded.
func TestNestedModuleEscape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "module")
	writeTree(t, dir, map[string]string{
		// Escapes dir (the root passed to LoadModule) into root's parent,
		// a directory that does not even exist: if this were ever
		// resolved, loading would fail instead of just being skipped.
		"main.tf": "module \"outside\" {\n  source = \"../../does-not-exist\"\n}\n",
	})
	m, err := LoadModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Nested) != 0 {
		t.Errorf("an escaping module call was followed: %+v", m.Nested)
	}
	wantEscape(t, nested(moduleSourceEscape)(m), "main.tf")
}

// wantEscape fails t unless fs holds exactly one module/source-escape
// finding, in file.
func wantEscape(t *testing.T, fs []Finding, file string) {
	t.Helper()
	if len(fs) != 1 || fs[0].ID != IDModuleSourceEscape || fs[0].Severity != SeverityError || fs[0].File != file {
		t.Fatalf("findings = %+v, want one %s in %s", fs, IDModuleSourceEscape, file)
	}
}

// TestNestedModuleSymlinkDirEscape: a nested module directory that is a
// symlink to somewhere outside the root is not loaded and is reported.
func TestNestedModuleSymlinkDirEscape(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	writeTree(t, base, map[string]string{
		"root/main.tf":    "module \"link\" {\n  source = \"./link\"\n}\n",
		"outside/main.tf": "terraform {\n  backend \"s3\" {}\n}\n",
	})
	if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(base, "root", "link")); err != nil {
		t.Fatal(err)
	}
	m, err := LoadModule(filepath.Join(base, "root"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Nested) != 0 {
		t.Errorf("a symlinked escape was followed: %+v", m.Nested)
	}
	wantEscape(t, moduleSourceEscape(m), "main.tf")
}

// TestNestedModuleSymlinkFileEscape: a module file that is a symlink to a
// file outside the root is reported; one inside the root is not.
func TestNestedModuleSymlinkFileEscape(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	writeTree(t, base, map[string]string{
		"root/real.tf":   "variable \"a\" {}\n",
		"outside/out.tf": "variable \"b\" {}\n",
	})
	if err := os.Symlink(filepath.Join(base, "outside", "out.tf"), filepath.Join(base, "root", "out.tf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.tf", filepath.Join(base, "root", "alias.tf")); err != nil {
		t.Fatal(err)
	}
	m, err := LoadModule(filepath.Join(base, "root"))
	if err != nil {
		t.Fatal(err)
	}
	wantEscape(t, moduleSourceEscape(m), "out.tf")
}

// TestNestedModuleSymlinkInTreeOK: symlinks that stay inside the root
// (a module directory, a module file) produce no finding and the module
// is still loaded.
func TestNestedModuleSymlinkInTreeOK(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"main.tf":        "module \"link\" {\n  source = \"./link\"\n}\n",
		"shared/main.tf": "variable \"a\" {}\n",
	})
	if err := os.Symlink("shared", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("shared/main.tf", filepath.Join(root, "extra.tf")); err != nil {
		t.Fatal(err)
	}
	m, err := LoadModule(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Nested) != 1 {
		t.Errorf("Nested = %+v, want the in-tree symlinked module", m.Nested)
	}
	if fs := nested(moduleSourceEscape)(m); len(fs) != 0 {
		t.Errorf("findings = %+v, want none", fs)
	}
}

// TestNestedModuleMissingSource: a local source whose directory does not
// exist is reported rather than skipped or failing the load.
func TestNestedModuleMissingSource(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"main.tf": "module \"gone\" {\n  source = \"./gone\"\n}\n",
	})
	m, err := LoadModule(root)
	if err != nil {
		t.Fatal(err)
	}
	wantEscape(t, moduleSourceEscape(m), "main.tf")
}

// TestNestedModuleEscapeNestedFile: an escape found in a nested module is
// reported with its File prefixed by the child's path.
func TestNestedModuleEscapeNestedFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"main.tf":       "module \"child\" {\n  source = \"./child\"\n}\n",
		"child/main.tf": "module \"up\" {\n  source = \"../../x\"\n}\n",
	})
	m, err := LoadModule(root)
	if err != nil {
		t.Fatal(err)
	}
	wantEscape(t, nested(moduleSourceEscape)(m), "child/main.tf")
}

// TestNestedFindingPrefixed: a finding from a nested module's check
// ("defense in depth") carries a File prefixed with the child's path
// from the root, so it is still locatable.
func TestNestedFindingPrefixed(t *testing.T) {
	t.Parallel()
	m := module(t, contract.RoleMachine, func(f, _ map[string]string) {
		f["nested.tf"] = "module \"child\" {\n  source = \"./child\"\n}\n"
		f["child/provider.tf"] = "provider \"aws\" {\n  access_key = \"AKIAEXAMPLE\"\n}\n"
	})
	r, err := Lint(m, contract.RoleMachine)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range r.Findings {
		if f.ID != IDModuleProviderConfig {
			continue
		}
		found = true
		if f.File != "child/provider.tf" {
			t.Errorf("File = %q, want %q", f.File, "child/provider.tf")
		}
	}
	if !found {
		t.Fatalf("no %s finding: %+v", IDModuleProviderConfig, r.Findings)
	}
}

// TestRequiredProvidersAggregated: image/providers-complete's aggregation
// sees a nested module's required_providers even though the root never
// declares it, de-duplicated by local name and source.
func TestRequiredProvidersAggregated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"main.tf": "terraform {\n  required_providers {\n    aws = {\n      source = \"hashicorp/aws\"\n    }\n  }\n}\n" +
			"module \"child\" {\n  source = \"./child\"\n}\n" +
			"module \"twin\" {\n  source = \"./child\"\n}\n",
		"child/main.tf": "terraform {\n  required_providers {\n    tls = {\n      source = \"hashicorp/tls\"\n    }\n  }\n}\n",
	})
	m, err := LoadModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, rp := range m.RequiredProviders() {
		got = append(got, rp.Local+"="+rp.Req.Source)
	}
	slices.Sort(got)
	if want := []string{"aws=hashicorp/aws", "tls=hashicorp/tls"}; !slices.Equal(got, want) {
		t.Errorf("required providers = %v, want %v", got, want)
	}
}
