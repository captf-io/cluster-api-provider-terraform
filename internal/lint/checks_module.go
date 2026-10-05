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
	"maps"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// Module check IDs; module/version is in checks_inputs.go.
const (
	// IDModuleBackend: the root or a nested module declares a
	// terraform { backend } block; the generated root owns the backend.
	IDModuleBackend = "module/backend"
	// IDModuleSourceEscape: a local module call resolves outside the root
	// module directory (directly or through a symlink) or does not
	// resolve, or a module file is a symlink out of the root. Such code is
	// never linted, so a backend or cloud block there would go unseen.
	IDModuleSourceEscape = "module/source-escape"
	// IDModuleCloud: the root or a nested module declares a
	// terraform { cloud } block; the generated root owns it, the same as
	// module/backend.
	IDModuleCloud = "module/cloud"
	// IDModuleProviderConfig: a provider block in the root or a nested
	// module sets a credential-named argument (see credentialNames) to a
	// string literal, at the top level of the block or inside any nested
	// block or object. A reference such as var.token is fine; credentials
	// come from the identity, never from the module source.
	IDModuleProviderConfig = "module/provider-config"
	// IDModuleTofuShadow: a .tofu file shadows a .tf file, and the
	// declarations OpenTofu loads differ from Terraform's: a variable,
	// output, provider, backend, cloud block, resource, data source, local
	// value, module call, required_providers entry or required_version
	// present in only one set or with a different body.
	IDModuleTofuShadow = "module/tofu-shadow"
)

// moduleBackend checks m for a nested module declaring a backend; the
// generated root owns it. It returns an IDModuleBackend finding for each
// backend block found.
func moduleBackend(m *Module) []Finding {
	return m.blockFindings(m.model().backends, IDModuleBackend,
		"terraform { backend } in the module: the generated root owns the backend, a module must not declare one")
}

// moduleCloud checks m for a terraform { cloud } block, which is likewise
// forbidden. It returns an IDModuleCloud finding for each one found.
func moduleCloud(m *Module) []Finding {
	return m.blockFindings(m.model().clouds, IDModuleCloud,
		"terraform { cloud } in the module: state lives in the generated root's backend")
}

// blockFindings builds one Finding per range in ranges, each with id, m's
// file and line for that range, error severity and message msg, and
// returns them.
func (m *Module) blockFindings(ranges []hcl.Range, id, msg string) []Finding {
	out := make([]Finding, 0, len(ranges))
	for _, r := range ranges {
		file, line := m.atRange(r)
		out = append(out, Finding{ID: id, Severity: SeverityError, File: file, Line: line, Message: msg})
	}
	return out
}

// credentialNames are the argument names (compared case-insensitively) that
// moduleProviderConfig treats as credentials: the common secret, key and
// token arguments of the AWS, Azure, Google and generic
// providers. A name outside this list is not checked.
var credentialNames = map[string]bool{
	"access_key": true, "secret_key": true, "secret_access_key": true,
	"session_token": true, "token": true, "access_token": true,
	"auth_token": true, "bearer_token": true, "api_token": true,
	"api_key": true, "apikey": true, "password": true, "passphrase": true,
	"client_secret": true, "client_certificate": true,
	"client_certificate_password": true, "client_key": true,
	"private_key": true, "private_key_password": true,
	"ssh_private_key": true, "secret": true,
}

// moduleProviderConfig checks m's provider blocks for a credential-named
// argument (see credentialNames) set to a non-empty string literal, at the
// top level or inside any nested block or object (assume_role, JSON
// objects). A reference (token = var.token) is fine: credentials reach the
// Job as mounted files and environment. It returns the
// IDModuleProviderConfig finding for each literal credential found.
func moduleProviderConfig(m *Module) []Finding {
	var out []Finding
	for _, body := range m.model().providers {
		walkBody(body, func(name string, e hcl.Expression) {
			if !credentialNames[strings.ToLower(name)] || len(e.Variables()) > 0 {
				return
			}
			v, diags := e.Value(nil)
			if diags.HasErrors() || v.IsNull() || !v.IsKnown() || v.Type() != cty.String || v.AsString() == "" {
				return
			}
			file, line := m.atRange(e.Range())
			out = append(out, Finding{ID: IDModuleProviderConfig, Severity: SeverityWarning, File: file, Line: line,
				Message: "provider argument " + name + " is a literal: credentials come from the identity, never from the module"})
		})
	}
	return out
}

// walkBody calls visit with the name and expression of every attribute in
// body and, recursively, in its nested blocks and object or list values.
func walkBody(body hcl.Body, visit func(name string, e hcl.Expression)) {
	if sb, ok := body.(*hclsyntax.Body); ok {
		attrs := slices.SortedFunc(maps.Values(sb.Attributes), func(a, b *hclsyntax.Attribute) int {
			return a.SrcRange.Start.Byte - b.SrcRange.Start.Byte
		})
		for _, a := range attrs {
			walkExpr(a.Name, a.Expr, visit)
		}
		for _, blk := range sb.Blocks {
			walkBody(blk.Body, visit)
		}
		return
	}
	attrs, _ := body.JustAttributes()
	for _, name := range slices.Sorted(maps.Keys(attrs)) {
		walkExpr(name, attrs[name].Expr, visit)
	}
}

// walkExpr calls visit with name and e, unless e is an object (visiting
// each member under its own key) or a list (visiting each element under
// name), in which case it recurses instead.
func walkExpr(name string, e hcl.Expression, visit func(name string, e hcl.Expression)) {
	if kvs, diags := hcl.ExprMap(e); !diags.HasErrors() {
		for _, kv := range kvs {
			key := hcl.ExprAsKeyword(kv.Key)
			if key == "" {
				if v, d := kv.Key.Value(nil); !d.HasErrors() && v.Type() == cty.String && !v.IsNull() {
					key = v.AsString()
				}
			}
			walkExpr(key, kv.Value, visit)
		}
		return
	}
	if items, diags := hcl.ExprList(e); !diags.HasErrors() {
		for _, it := range items {
			walkExpr(name, it, visit)
		}
		return
	}
	visit(name, e)
}

// moduleTofuShadow checks m for a .tofu file shadowing a .tf file, where
// the declarations OpenTofu loads differ from Terraform's: variables,
// outputs, providers, backends, resources, data sources, locals, module
// calls, required_providers and required_version, each compared by its
// whole body (nested blocks included). Messages name the declaration, never
// a value. The contract
// checks read Terraform's set (terraform-config-inspect does not load
// .tofu), so this is where a difference in OpenTofu's shows up. A module
// with no .tf file at all isn't shadowing anything: it never runs
// under plain Terraform, by design, so nothing here is a difference worth
// flagging. It returns the IDModuleTofuShadow finding for each differing
// or one-sided declaration, or nil when m has no .tofu file or no .tf
// file.
func moduleTofuShadow(m *Module) []Finding {
	if m.tofu == nil || !m.hasTF {
		return nil
	}
	keys := slices.Sorted(maps.Keys(m.tf.decls))
	for k := range m.tofu.decls {
		if _, ok := m.tf.decls[k]; !ok {
			keys = append(keys, k)
		}
	}
	var out []Finding
	for _, k := range keys {
		tf, inTF := m.tf.decls[k]
		tofu, inTofu := m.tofu.decls[k]
		if inTF && inTofu && tf.sig == tofu.sig {
			continue
		}
		at, msg := tofu.rng, k+" differs between OpenTofu's files and Terraform's"
		switch {
		case !inTofu:
			at, msg = tf.rng, k+" is declared for Terraform, but a .tofu file shadows it and OpenTofu does not see it"
		case !inTF:
			msg = k + " is declared only in a .tofu file: Terraform does not see it"
		}
		file, line := m.atRange(at)
		out = append(out, Finding{ID: IDModuleTofuShadow, Severity: SeverityWarning, File: file, Line: line,
			Message: msg + "; the contract checks read Terraform's files"})
	}
	return out
}
