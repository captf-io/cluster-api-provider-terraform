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
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/terraform-config-inspect/tfconfig"
)

// File set names, as a Report names the set the module checks read.
const (
	SetTerraform = "terraform"
	SetOpenTofu  = "opentofu"
)

// FileSets splits a module's files (relative names) into what each runtime
// loads: Terraform reads *.tf and *.tf.json; OpenTofu reads the same plus
// *.tofu and *.tofu.json, but skips x.tf when x.tofu exists and x.tf.json
// when x.tofu.json exists (filterTfPathsWithTofuAlternatives). It returns
// the two sorted sets, tofu's files and tf's.
func FileSets(files []string) (tofu, tf []string) {
	has := map[string]bool{}
	for _, f := range files {
		has[f] = true
	}
	for _, f := range files {
		switch {
		case strings.HasSuffix(f, ".tofu"), strings.HasSuffix(f, ".tofu.json"):
			tofu = append(tofu, f)
		case strings.HasSuffix(f, ".tf.json"):
			tf = append(tf, f)
			if !has[strings.TrimSuffix(f, ".tf.json")+".tofu.json"] {
				tofu = append(tofu, f)
			}
		case strings.HasSuffix(f, ".tf"):
			tf = append(tf, f)
			if !has[strings.TrimSuffix(f, ".tf")+".tofu"] {
				tofu = append(tofu, f)
			}
		}
	}
	slices.Sort(tofu)
	slices.Sort(tf)
	return tofu, tf
}

// decl is one declaration as tofu-shadow compares it: a variable, output,
// provider, backend, cloud, resource, data source, local, module call,
// required_providers entry or required_version.
type decl struct {
	rng hcl.Range
	// sig is the declaration's body, attributes as name=source and nested
	// blocks recursively, with whitespace collapsed and parts sorted, so a
	// reformatted copy is the same declaration.
	sig string
}

// resourceDecl is a resource's lifecycle.ignore_changes, if any.
type resourceDecl struct {
	rng hcl.Range
	// ignoreChanges is the attribute's source text; ignoreAll is
	// ignore_changes = all.
	ignoreChanges string
	ignoreAll     bool
}

// hclModel is what the hcl/v2 pass reads from one file set: what
// terraform-config-inspect does not expose, plus a full tfconfig.Module
// built the same way
// terraform-config-inspect builds its own, over this file set, so a
// module whose declarations only tfconfig misses (a .tofu-only module,
// which terraform-config-inspect never reads) still has a config.
type hclModel struct {
	decls    map[string]decl
	outputs  map[string]*hcl.Attribute // each output's value
	backends []hcl.Range
	clouds   []hcl.Range
	// providers are every provider block's body, nested blocks included.
	providers []hcl.Body
	resources []resourceDecl
	// moduleCallArgs are each "module" block's arguments, keyed by its
	// label: input/tags-unused reads which argument forwards
	// var.captf_tags to a nested local module.
	moduleCallArgs map[string]hcl.Attributes
	// refs are every traversal the files reference.
	refs []hcl.Traversal
	// syntax are the native-syntax bodies, for expression scans the JSON
	// syntax does not support (lower fidelity, never an error).
	syntax []*hclsyntax.Body
	src    map[string][]byte
	// config is this file set's declarations, decoded by
	// terraform-config-inspect's own exported decoder
	// (tfconfig.LoadModuleFromFile) so a module read only through this
	// pass gets identical Variables/Outputs/RequiredProviders/
	// RequiredCore/ModuleCalls semantics to a module tfconfig.LoadModule
	// reads directly.
	config *tfconfig.Module
}

// parseSet parses files (relative to dir) with p, which caches by file
// name, so a file in both sets is read once. It returns the resulting
// hclModel, or an error wrapping ErrParse when a file fails to parse or
// load.
func parseSet(p *hclparse.Parser, dir string, files []string) (*hclModel, error) {
	m := &hclModel{decls: map[string]decl{}, outputs: map[string]*hcl.Attribute{}, moduleCallArgs: map[string]hcl.Attributes{}, config: tfconfig.NewModule(dir)}
	for _, name := range files {
		path := filepath.Join(dir, name)
		var (
			f     *hcl.File
			diags hcl.Diagnostics
		)
		if strings.HasSuffix(name, ".json") {
			f, diags = p.ParseJSONFile(path)
		} else {
			f, diags = p.ParseHCLFile(path)
		}
		if diags.HasErrors() {
			return nil, fmt.Errorf("%w: %w", ErrParse, diags)
		}
		if fdiags := tfconfig.LoadModuleFromFile(f, m.config); fdiags.HasErrors() {
			return nil, fmt.Errorf("%w: %w", ErrParse, fdiags)
		}
		if err := m.add(f); err != nil {
			return nil, err
		}
	}
	m.src = p.Sources()
	addImpliedProviders(m.config)
	return m, nil
}

// addImpliedProviders mirrors tfconfig's own (unexported) Module.init: it
// adds to cfg.RequiredProviders an empty requirement for every resource or
// data source in cfg with no explicit provider and no existing entry, so
// image/providers-complete sees a provider like a bare "tls_private_key"
// resource needs even without a required_providers entry naming it.
func addImpliedProviders(cfg *tfconfig.Module) {
	for _, r := range cfg.ManagedResources {
		if _, ok := cfg.RequiredProviders[r.Provider.Name]; !ok {
			cfg.RequiredProviders[r.Provider.Name] = &tfconfig.ProviderRequirement{}
		}
	}
	for _, r := range cfg.DataResources {
		if _, ok := cfg.RequiredProviders[r.Provider.Name]; !ok {
			cfg.RequiredProviders[r.Provider.Name] = &tfconfig.ProviderRequirement{}
		}
	}
}

// add reads f's terraform, variable, output, provider, resource and
// module blocks into m: declarations for tofu-shadow, output values,
// backend and cloud ranges, provider attributes, resource lifecycle
// blocks, module call arguments, and every traversal the file references.
// It returns an error wrapping ErrParse when f's body does not match the
// expected block schema.
func (m *hclModel) add(f *hcl.File) error {
	content, _, diags := f.Body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{
		{Type: "terraform"},
		{Type: "variable", LabelNames: []string{"name"}},
		{Type: "output", LabelNames: []string{"name"}},
		{Type: "provider", LabelNames: []string{"name"}},
		{Type: "resource", LabelNames: []string{"type", "name"}},
		{Type: "data", LabelNames: []string{"type", "name"}},
		{Type: "locals"},
		{Type: "module", LabelNames: []string{"name"}},
	}})
	if diags.HasErrors() {
		return fmt.Errorf("%w: %w", ErrParse, diags)
	}
	src := f.Bytes
	for _, b := range content.Blocks {
		m.addBlock(b, src)
	}
	if body, ok := f.Body.(*hclsyntax.Body); ok {
		m.syntax = append(m.syntax, body)
		// Every block except "module": a module call's own arguments are
		// moduleCallArgs's (input/tags-unused's nested-forwarding check),
		// not the generic references() scan — forwarding a value
		// into a nested module is not, by itself, using it.
		for _, blk := range body.Blocks {
			if blk.Type == "module" {
				continue
			}
			_ = hclsyntax.VisitAll(blk, func(n hclsyntax.Node) hcl.Diagnostics {
				if t, ok := n.(*hclsyntax.ScopeTraversalExpr); ok {
					m.refs = append(m.refs, t.Traversal)
				}
				return nil
			})
		}
	} else {
		// JSON: every nested value is an attribute expression, and its
		// Variables walks nested objects and template strings. "module" is
		// excluded for the same reason as the native-syntax pass above.
		attrs, _ := f.Body.JustAttributes()
		for name, a := range attrs {
			if name == "module" {
				continue
			}
			m.refs = append(m.refs, a.Expr.Variables()...)
		}
	}
	return nil
}

// addBlock records the top-level block b, whose file source is src, into
// m: its tofu-shadow declaration and whatever else the checks read from
// its type.
func (m *hclModel) addBlock(b *hcl.Block, src []byte) {
	switch b.Type {
	case "terraform":
		m.addTerraform(b, src)
	case "variable", "output":
		m.decls[b.Type+"."+b.Labels[0]] = decl{rng: b.DefRange, sig: bodySig(b.Body, src)}
		if b.Type == "output" {
			attrs, _ := b.Body.JustAttributes() // nested blocks are not the value
			m.outputs[b.Labels[0]] = attrs["value"]
		}
	case "provider":
		attrs, _ := b.Body.JustAttributes() // for the alias only; nested blocks are in the signature
		key := "provider." + b.Labels[0]
		if a, ok := attrs["alias"]; ok {
			key += "." + collapse(text(src, a.Expr.Range()))
		}
		m.decls[key] = decl{rng: b.DefRange, sig: bodySig(b.Body, src)}
		m.providers = append(m.providers, b.Body)
	case "resource":
		m.decls["resource."+b.Labels[0]+"."+b.Labels[1]] = decl{rng: b.DefRange, sig: bodySig(b.Body, src)}
		m.resources = append(m.resources, lifecycleOf(b, src))
	case "data":
		m.decls["data."+b.Labels[0]+"."+b.Labels[1]] = decl{rng: b.DefRange, sig: bodySig(b.Body, src)}
	case "locals":
		attrs, _ := b.Body.JustAttributes()
		for name, a := range attrs {
			m.decls["local."+name] = decl{rng: a.NameRange, sig: collapse(text(src, a.Expr.Range()))}
		}
	case "module":
		m.decls["module."+b.Labels[0]] = decl{rng: b.DefRange, sig: bodySig(b.Body, src)}
		attrs, _ := b.Body.JustAttributes() // count, for_each, providers etc. are fine
		m.moduleCallArgs[b.Labels[0]] = attrs
	}
}

// addTerraform reads the backend, cloud and required_providers blocks and
// the required_version attribute of b, a "terraform" block whose source is
// src, into m.backends, m.clouds and m.decls.
func (m *hclModel) addTerraform(b *hcl.Block, src []byte) {
	content, _, _ := b.Body.PartialContent(&hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "required_version"}},
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "backend", LabelNames: []string{"type"}},
			{Type: "cloud"},
			{Type: "required_providers"},
		},
	})
	if a, ok := content.Attributes["required_version"]; ok {
		m.decls["required_version"] = decl{rng: a.NameRange, sig: collapse(text(src, a.Expr.Range()))}
	}
	for _, nb := range content.Blocks {
		switch nb.Type {
		case "backend":
			m.backends = append(m.backends, nb.DefRange)
			m.decls["backend."+nb.Labels[0]] = decl{rng: nb.DefRange, sig: bodySig(nb.Body, src)}
		case "cloud":
			m.clouds = append(m.clouds, nb.DefRange)
			m.decls["cloud"] = decl{rng: nb.DefRange, sig: bodySig(nb.Body, src)}
		case "required_providers":
			attrs, _ := nb.Body.JustAttributes()
			for name, a := range attrs {
				m.decls["required_providers."+name] = decl{rng: a.NameRange, sig: collapse(text(src, a.Expr.Range()))}
			}
		}
	}
}

// lifecycleOf reads resource block b's lifecycle.ignore_changes, whose
// source is src, and returns the resourceDecl describing it.
func lifecycleOf(b *hcl.Block, src []byte) resourceDecl {
	r := resourceDecl{rng: b.DefRange}
	content, _, _ := b.Body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "lifecycle"}}})
	for _, lb := range content.Blocks {
		attrs, _ := lb.Body.JustAttributes()
		if a, ok := attrs["ignore_changes"]; ok {
			r.ignoreChanges = text(src, a.Expr.Range())
			r.ignoreAll = hcl.ExprAsKeyword(a.Expr) == "all"
		}
	}
	return r
}

// references reports whether any traversal starts root.attr.
func (m *hclModel) references(root, attr string) bool {
	for _, t := range m.refs {
		if t.RootName() != root || len(t) < 2 {
			continue
		}
		if a, ok := t[1].(hcl.TraverseAttr); ok && a.Name == attr {
			return true
		}
	}
	return false
}

// exprReferences reports whether a's expression traverses root.attr; nil
// (the argument was never set) never references anything. Unlike
// hclModel.references, this reads one attribute's expression directly
// (via hcl.Expression.Variables, which every syntax implements), so it
// also works on a JSON module call.
func exprReferences(a *hcl.Attribute, root, attr string) bool {
	if a == nil {
		return false
	}
	for _, t := range a.Expr.Variables() {
		if t.RootName() != root || len(t) < 2 {
			continue
		}
		if at, ok := t[1].(hcl.TraverseAttr); ok && at.Name == attr {
			return true
		}
	}
	return false
}

// bodySig returns a declaration body's comparable signature, read from src,
// the file's source: its
// attributes' names and whitespace-collapsed source expressions and, for a
// native-syntax body, every nested block (type, labels and body, recursively),
// sorted so a reformatted copy of the same declaration compares equal. A
// JSON body has no block/attribute distinction, so its nested objects are
// attribute expressions whose source text is already the whole value.
func bodySig(body hcl.Body, src []byte) string {
	var parts []string
	if sb, ok := body.(*hclsyntax.Body); ok {
		for name, a := range sb.Attributes {
			parts = append(parts, name+"="+collapse(text(src, a.Expr.Range())))
		}
		for _, blk := range sb.Blocks {
			parts = append(parts, blk.Type+" "+strings.Join(blk.Labels, " ")+"{"+bodySig(blk.Body, src)+"}")
		}
	} else {
		attrs, _ := body.JustAttributes()
		for name, a := range attrs {
			parts = append(parts, name+"="+collapse(text(src, a.Expr.Range())))
		}
	}
	slices.Sort(parts)
	return strings.Join(parts, ";")
}

// text returns the source text of src covered by range r, or "" when r is
// invalid or out of bounds.
func text(src []byte, r hcl.Range) string {
	if r.Start.Byte < 0 || r.End.Byte > len(src) || r.Start.Byte > r.End.Byte {
		return ""
	}
	return string(src[r.Start.Byte:r.End.Byte])
}

// collapse returns s with every run of whitespace replaced by a single
// space, so two differently formatted copies of the same source compare
// equal.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }
