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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/terraform-config-inspect/tfconfig"
)

// ErrParse is returned for a module that cannot be parsed; tfcapi-lint
// exits 2 on it rather than reporting findings.
var ErrParse = errors.New("lint: module does not parse")

// IsModuleFile reports whether name is part of a module: Terraform's .tf
// and .tf.json, OpenTofu's .tofu and .tofu.json. terraform-config-inspect
// reads only the first two; the .tofu files are for the hcl/v2 pass. It
// applies terraform-config-inspect's own ignore rules (tfconfig's
// unexported isIgnoredFile): a name starting with ".",
// ending with "~", or wrapped in "#…#" is never a module file, so an
// editor's dangling lock file or backup copy next to a real one is
// skipped rather than parsed.
func IsModuleFile(name string) bool {
	if ignoredModuleFile(name) {
		return false
	}
	for _, s := range []string{".tf", ".tf.json", ".tofu", ".tofu.json"} {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// ignoredModuleFile mirrors terraform-config-inspect's isIgnoredFile
// (tfconfig/load.go): a Unix-style hidden file, a vim backup ("~") or an
// Emacs lock/autosave ("#…#"). It reports whether name is one of these.
func ignoredModuleFile(name string) bool {
	return strings.HasPrefix(name, ".") ||
		strings.HasSuffix(name, "~") ||
		(strings.HasPrefix(name, "#") && strings.HasSuffix(name, "#"))
}

// Module is a loaded module directory.
type Module struct {
	// Dir is the module directory (absolute).
	Dir string
	// Config is the module's declared names and type expressions, no
	// evaluated values: Terraform's file set's declarations (every .tf and
	// .tf.json file, exactly what terraform-config-inspect itself would
	// read), unless the module ships no .tf/.tf.json file at all, in
	// which case it is OpenTofu's file set's declarations
	// instead, built the same way by the hcl/v2 pass
	// (tfconfig.LoadModuleFromFile) — otherwise a .tofu-only module would
	// have no declarations to check at all. module/tofu-shadow (below)
	// is what flags a difference between the two sets for a module that
	// ships both.
	Config *tfconfig.Module
	// Files are the module's .tf, .tf.json, .tofu and .tofu.json files,
	// relative to Dir, sorted.
	Files []string
	// Nested are the local modules reached from this one by a
	// relative-path module call (./ or ../), loaded recursively within
	// the root module's directory.
	Nested []*Nested
	// escapes are the module/source-escape findings found while loading
	// this module: a local module call that could not be followed, or a
	// module file that is a symlink out of the root. moduleSourceEscape
	// reports them.
	escapes []Finding

	// tf and tofu are the hcl/v2 pass over Terraform's and OpenTofu's file
	// sets; tofu is nil when the module ships no .tofu file.
	tf, tofu *hclModel
	// hasTF records whether the module has any .tf or .tf.json file at
	// all. module/tofu-shadow reads it: a .tofu-only module isn't
	// shadowing anything Terraform would otherwise load, so
	// nothing there is a difference worth flagging.
	hasTF bool
}

// Set returns the file set the hcl/v2 checks read: OpenTofu's when the
// module ships a .tofu file, else Terraform's. The tfconfig checks always
// read Terraform's set; module/tofu-shadow flags where the two differ.
func (m *Module) Set() string {
	if m.tofu != nil {
		return SetOpenTofu
	}
	return SetTerraform
}

// model returns the hcl/v2 model of the file set Set names: m.tofu when
// the module ships a .tofu file, else m.tf.
func (m *Module) model() *hclModel {
	if m.tofu != nil {
		return m.tofu
	}
	return m.tf
}

// LoadModule reads dir and, recursively, every local module it calls (a
// "module" block whose source is a relative path, within dir). A parse
// error, anywhere in the tree, is ErrParse. It returns the
// loaded Module tree.
func LoadModule(dir string) (*Module, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("lint: read module: %w", err)
	}
	return loadTree(abs, abs, map[string]struct{}{})
}

// loadOne reads one module directory (dir, already absolute): its file
// set, the hcl/v2 pass over Terraform's and OpenTofu's file sets, and
// Config from whichever set has declarations (see Module.Config).
// Nested is left empty; LoadModule (through loadTree) fills it in. A
// module file that is a symlink whose target lies outside rootReal (the
// symlink-resolved root module directory) is still parsed, but recorded
// as a module/source-escape finding. It returns the loaded Module, or an
// error when dir cannot be read or a file fails to parse.
func loadOne(dir, rootReal string) (*Module, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("lint: read module: %w", err)
	}
	m := &Module{Dir: dir}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if IsModuleFile(e.Name()) {
			m.Files = append(m.Files, e.Name())
			if e.Type()&os.ModeSymlink != 0 {
				if f, ok := linkEscape(filepath.Join(dir, e.Name()), e.Name(), rootReal); ok {
					m.escapes = append(m.escapes, f)
				}
			}
		}
	}
	slices.Sort(m.Files)

	// The hcl/v2 pass; tfconfig never opens .tofu files, so a broken one
	// is first seen here and is a parse error too.
	p := hclparse.NewParser()
	tofuFiles, tfFiles := FileSets(m.Files)
	if m.tf, err = parseSet(p, dir, tfFiles); err != nil {
		return nil, err
	}
	if slices.ContainsFunc(m.Files, func(f string) bool { return strings.Contains(f, ".tofu") }) {
		if m.tofu, err = parseSet(p, dir, tofuFiles); err != nil {
			return nil, err
		}
	}
	m.hasTF = len(tfFiles) > 0
	if m.hasTF || m.tofu == nil {
		m.Config = m.tf.config
	} else {
		m.Config = m.tofu.config
	}
	return m, nil
}

// atRange is at for r, an hcl/v2 range, and returns the same file and line
// pair.
func (m *Module) atRange(r hcl.Range) (string, int) {
	return m.at(tfconfig.SourcePos{Filename: r.Filename, Line: r.Start.Line})
}

// at returns pos, a finding's position, as a file relative to the module
// directory and its line.
func (m *Module) at(pos tfconfig.SourcePos) (string, int) {
	if pos.Filename == "" {
		return "", 0
	}
	if rel, err := filepath.Rel(m.Dir, pos.Filename); err == nil && !strings.HasPrefix(rel, "..") {
		return rel, pos.Line
	}
	return pos.Filename, pos.Line
}
