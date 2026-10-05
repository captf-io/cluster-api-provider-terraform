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
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-config-inspect/tfconfig"
)

// Nested is one local module reached from its parent by a module call
// whose source is a relative path (./ or ../), loaded recursively within
// the root module's own directory: the image contract treats
// /captf/module as including its nested local modules.
type Nested struct {
	// Name is the module call's label ("module \"name\" { … }").
	Name string
	// Rel is this module's directory, relative to the root module's, with
	// "/" separators: how a nested finding's File is prefixed, so it is
	// still locatable.
	Rel string
	*Module
}

// isLocalModuleSource reports whether src is a relative-path module call,
// the only kind LoadModule follows; a registry, git, HTTP or other remote
// source is never fetched. This is Terraform's own rule for what counts
// as a local path:
// https://developer.hashicorp.com/terraform/language/modules/sources#local-paths
func isLocalModuleSource(src string) bool {
	return src == "." || src == ".." ||
		strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../")
}

// within reports whether path is dir or lies beneath it; both must be
// absolute and already cleaned (or symlink-resolved) the same way.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// linkEscape checks path, a module file named name that is a symlink: it
// returns a module/source-escape finding and true when the link target
// cannot be resolved or lies outside rootReal. A link that stays inside
// the root is fine.
func linkEscape(path, name, rootReal string) (Finding, bool) {
	target, err := filepath.EvalSymlinks(path)
	switch {
	case err != nil:
		return Finding{ID: IDModuleSourceEscape, Severity: SeverityError, File: name,
			Message: fmt.Sprintf("module file %s is a symlink that cannot be resolved: %v", name, err)}, true
	case !within(rootReal, target):
		return Finding{ID: IDModuleSourceEscape, Severity: SeverityError, File: name,
			Message: fmt.Sprintf("module file %s is a symlink to %s, outside the module directory; it would not ship with the module", name, target)}, true
	}
	return Finding{}, false
}

// moduleSourceEscape returns m's own module/source-escape findings
// (recorded while loading); nested wraps it to cover the whole tree.
func moduleSourceEscape(m *Module) []Finding {
	return slices.Clone(m.escapes)
}

// canonicalDir resolves symlinks in dir for cycle detection. A directory
// that cannot be resolved (already gone, a permission error — it will
// fail again, informatively, when loadOne opens it) is used as given,
// which still catches a literal repeat. It returns the resolved (or
// given) directory.
func canonicalDir(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

// loadTree loads dir (already absolute) as one module of the tree rooted
// at rootAbs (also absolute), then recursively loads its local module
// calls, guarding against a call that escapes rootAbs and against a
// cycle (visiting holds every directory on the current path, root
// included). It returns the loaded Module, with Nested populated, or an
// error when dir or a descendant fails to load.
func loadTree(rootAbs, dir string, visiting map[string]struct{}) (*Module, error) {
	rootReal := canonicalDir(rootAbs)
	m, err := loadOne(dir, rootReal)
	if err != nil {
		return nil, err
	}
	key := canonicalDir(dir)
	visiting[key] = struct{}{}
	defer delete(visiting, key)

	if m.Config == nil {
		return m, nil
	}
	for _, name := range slices.Sorted(maps.Keys(m.Config.ModuleCalls)) {
		call := m.Config.ModuleCalls[name]
		if !isLocalModuleSource(call.Source) {
			continue // a registry, git or other remote source: not fetched
		}
		file, line := m.at(call.Pos)
		escape := func(format string, args ...any) {
			m.escapes = append(m.escapes, Finding{ID: IDModuleSourceEscape, Severity: SeverityError, File: file, Line: line,
				Message: fmt.Sprintf("module %q source %q ", name, call.Source) + fmt.Sprintf(format, args...)})
		}
		childAbs, err := filepath.Abs(filepath.Join(dir, filepath.FromSlash(call.Source)))
		if err != nil {
			escape("cannot be resolved: %v", err)
			continue
		}
		if !within(rootAbs, childAbs) {
			escape("escapes the module directory; it is never linted and would not ship with the module")
			continue
		}
		childReal, err := filepath.EvalSymlinks(childAbs)
		if err != nil {
			escape("cannot be resolved: %v", err)
			continue
		}
		if !within(rootReal, childReal) {
			escape("is a symlink to %s, outside the module directory; it is never linted and would not ship with the module", childReal)
			continue
		}
		rel, err := filepath.Rel(rootAbs, childAbs)
		if err != nil {
			escape("cannot be resolved: %v", err)
			continue
		}
		// A call back onto a directory already on this path is a cycle:
		// that module is being linted already, so it is skipped silently.
		if _, cyc := visiting[childReal]; cyc {
			continue
		}
		child, err := loadTree(rootAbs, childAbs, visiting)
		if err != nil {
			return nil, fmt.Errorf("lint: nested module %q (%s): %w", name, call.Source, err)
		}
		m.Nested = append(m.Nested, &Nested{Name: name, Rel: filepath.ToSlash(rel), Module: child})
	}
	return m, nil
}

// nested wraps check to also run over every module in m's local tree
// (module/backend, module/cloud and module/provider-config are defense
// in depth against a nested module hiding a backend, a cloud block or a
// credential literal), prefixing each nested finding's File
// with the child's path from the root. It returns the wrapped check
// function.
func nested(check func(*Module) []Finding) func(*Module) []Finding {
	var run func(*Module) []Finding
	run = func(m *Module) []Finding {
		out := check(m)
		for _, n := range m.Nested {
			for _, f := range run(n.Module) {
				if f.File == "" {
					f.File = n.Rel
				} else {
					f.File = path.Join(n.Rel, f.File)
				}
				out = append(out, f)
			}
		}
		return out
	}
	return run
}

// RequiredProvider is one required_providers entry, as
// Module.RequiredProviders aggregates it across a module and its local
// tree.
type RequiredProvider struct {
	// Local is the required_providers key (the module's local name for
	// the provider).
	Local string
	Req   *tfconfig.ProviderRequirement
}

// RequiredProviders returns every required_providers entry of m and of
// every module in its local tree, de-duplicated by local name and
// source. A provider only a nested module requires is otherwise
// invisible to image/providers-complete, so the mirror can lack it, lint
// passes, and the Job's init fails.
func (m *Module) RequiredProviders() []RequiredProvider {
	seen := map[string]bool{}
	var out []RequiredProvider
	var walk func(*Module)
	walk = func(mod *Module) {
		if mod.Config != nil {
			for _, local := range slices.Sorted(maps.Keys(mod.Config.RequiredProviders)) {
				req := mod.Config.RequiredProviders[local]
				key := local
				if req != nil {
					key += "|" + req.Source
				}
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, RequiredProvider{Local: local, Req: req})
			}
		}
		for _, n := range mod.Nested {
			walk(n.Module)
		}
	}
	walk(m)
	return out
}
