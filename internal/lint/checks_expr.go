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
	"regexp"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// Expression check IDs. These are best-effort: they read expressions
// without evaluating them.
const (
	// IDInputTagsUnused: captf_tags is declared but never referenced,
	// directly or forwarded into a nested local module that references it.
	IDInputTagsUnused = "input/tags-unused"
	// IDOutputEndpointNeverSet: the control_plane_endpoint output is a
	// literal null, so a KubeadmControlPlane cluster with no user-set
	// endpoint would wait forever.
	IDOutputEndpointNeverSet = "output/endpoint-never-set"
	// IDPoolAutoscaling: the module uses var.autoscaling but no resource
	// ignores changes to its desired capacity, so every apply resets the
	// cloud autoscaler's decision.
	IDPoolAutoscaling = "pool/autoscaling-ignore-changes"
)

// desiredCapacity matches the desired-count attributes of the common
// scaling groups: ASG, EKS node group, GCE MIG, VMSS, OCI instance pool.
var desiredCapacity = regexp.MustCompile(`\b(desired_capacity|desired_size|target_size|instance_count)\b|\bsku(\[\d+\])?\.capacity\b`)

// inputTagsUnused checks m for captf_tags declared but never referenced,
// directly or by forwarding it into a nested local module that references
// it. It cannot prove the tags reach every resource. A missing
// captf_tags is input/tags-declared. It returns the IDInputTagsUnused
// finding when the variable is unreferenced, else nil.
func inputTagsUnused(m *Module) []Finding {
	v, ok := m.Config.Variables[tagsInput]
	if !ok || tagsReferenced(m, tagsInput) {
		return nil
	}
	file, line := m.at(v.Pos)
	return []Finding{{ID: IDInputTagsUnused, Severity: SeverityWarning, File: file, Line: line,
		Message: "variable " + tagsInput + " is never referenced: the module should tag what it creates with it"}}
}

// tagsReferenced reports whether m references var.<name> itself, or
// forwards it (as any argument of a nested local module's call) into a
// module whose own copy of that argument is referenced there, checked
// recursively down the local module tree.
func tagsReferenced(m *Module, name string) bool {
	if m.model().references("var", name) {
		return true
	}
	for _, n := range m.Nested {
		for arg, a := range m.model().moduleCallArgs[n.Name] {
			if exprReferences(a, "var", name) && tagsReferenced(n.Module, arg) {
				return true
			}
		}
	}
	return false
}

// outputEndpointNeverSet checks whether m's control_plane_endpoint output
// is a literal null. With KCP and no user endpoint the cluster then waits
// forever. A conditional that is always false at runtime is not caught. It
// returns the IDOutputEndpointNeverSet finding when the output is always
// null, else nil.
func outputEndpointNeverSet(m *Module) []Finding {
	a := m.model().outputs["control_plane_endpoint"]
	if a == nil || len(a.Expr.Variables()) > 0 {
		return nil
	}
	if v, diags := a.Expr.Value(nil); diags.HasErrors() || !v.IsNull() {
		return nil
	}
	file, line := m.atRange(a.Range)
	return []Finding{{ID: IDOutputEndpointNeverSet, Severity: SeverityWarning, File: file, Line: line,
		Message: "output control_plane_endpoint is always null: without a user-set endpoint the cluster never gets one"}}
}

// poolAutoscaling checks that, when a module uses var.autoscaling, a
// resource that itself references var.autoscaling (the scaling group)
// keeps its desired count out of the apply with lifecycle ignore_changes;
// otherwise every apply resets the cloud autoscaler's decision. An
// ignore_changes on an unrelated resource does not count. It runs over m
// and every module in its local tree. Checks lists it for the machinepool
// role only. It returns the IDPoolAutoscaling finding for each module
// that uses var.autoscaling without such a resource, else nil.
//
// Limits: a scaling group that reaches var.autoscaling only through a
// local value is not recognized as one, and in .tf.json (where per
// resource references are not scanned) any resource that ignores the
// desired count is accepted.
func poolAutoscaling(m *Module) []Finding { return nested(poolAutoscalingOne)(m) }

// poolAutoscalingOne is poolAutoscaling for m alone, without its nested
// modules. It returns the IDPoolAutoscaling finding, else nil.
func poolAutoscalingOne(m *Module) []Finding {
	model := m.model()
	if !model.references("var", "autoscaling") {
		return nil
	}
	for _, r := range model.resources {
		if !r.ignoreAll && !desiredCapacity.MatchString(r.ignoreChanges) {
			continue
		}
		if usesAutoscaling, known := model.resourceReferences(r, "autoscaling"); usesAutoscaling || !known {
			return nil
		}
	}
	return []Finding{{ID: IDPoolAutoscaling, Severity: SeverityWarning,
		Message: "the module uses var.autoscaling, but no resource that uses it ignores changes to its desired capacity: every apply resets the autoscaler"}}
}

// resourceReferences reports whether resource r's block references
// var.<name>. known is false when r's block is not native syntax (JSON),
// where the answer cannot be read.
func (m *hclModel) resourceReferences(r resourceDecl, name string) (referenced, known bool) {
	for _, body := range m.syntax {
		for _, blk := range body.Blocks {
			if blk.Type != "resource" || blk.DefRange() != r.rng {
				continue
			}
			_ = hclsyntax.VisitAll(blk, func(n hclsyntax.Node) hcl.Diagnostics {
				if t, ok := n.(*hclsyntax.ScopeTraversalExpr); ok && len(t.Traversal) > 1 && t.Traversal.RootName() == "var" {
					if a, ok := t.Traversal[1].(hcl.TraverseAttr); ok && a.Name == name {
						referenced = true
					}
				}
				return nil
			})
			return referenced, true
		}
	}
	return false, false
}

// statusWords are the state and health words a provider_id_list filter
// must not test.
var statusWords = []string{"running", "healthy"}

// isStatusWord reports whether s is exactly a status word, ignoring case
// and surrounding space: "RUNNING" and "Healthy" are, "healthy-group" and
// "unhealthy_threshold" are not.
func isStatusWord(s string) bool {
	s = strings.TrimSpace(s)
	for _, w := range statusWords {
		if strings.EqualFold(s, w) {
			return true
		}
	}
	return false
}

// filtersOnStatus reports whether the for-expression condition cond
// compares against a status word: a string literal that is one, or an
// attribute or index step named for one (i.healthy, i["running"]).
func filtersOnStatus(cond hclsyntax.Expression) bool {
	found := false
	_ = hclsyntax.VisitAll(cond, func(n hclsyntax.Node) hcl.Diagnostics {
		switch e := n.(type) {
		case *hclsyntax.LiteralValueExpr:
			if e.Val.Type() == cty.String && !e.Val.IsNull() && isStatusWord(e.Val.AsString()) {
				found = true
			}
		case *hclsyntax.ScopeTraversalExpr:
			found = found || traversalHasStatus(e.Traversal)
		case *hclsyntax.RelativeTraversalExpr:
			found = found || traversalHasStatus(e.Traversal)
		}
		return nil
	})
	return found
}

// traversalHasStatus reports whether any step of t is an attribute or
// string index that is a status word.
func traversalHasStatus(t hcl.Traversal) bool {
	for _, step := range t {
		switch s := step.(type) {
		case hcl.TraverseAttr:
			if isStatusWord(s.Name) {
				return true
			}
		case hcl.TraverseIndex:
			if s.Key.Type() == cty.String && !s.Key.IsNull() && isStatusWord(s.Key.AsString()) {
				return true
			}
		}
	}
	return false
}

// isSorted reports whether e yields a list in a deterministic order: it is
// sort(...), or a paren, tolist(...) or distinct(...) around one, a
// concat of only such lists, or a conditional whose both branches are.
// distinct alone does not count, as it keeps the input's order.
func isSorted(e hclsyntax.Expression) bool {
	switch x := e.(type) {
	case *hclsyntax.ParenthesesExpr:
		return isSorted(x.Expression)
	case *hclsyntax.ConditionalExpr:
		return isSorted(x.TrueResult) && isSorted(x.FalseResult)
	case *hclsyntax.FunctionCallExpr:
		switch x.Name {
		case "sort":
			return true
		case "tolist", "distinct":
			return len(x.Args) == 1 && isSorted(x.Args[0])
		case "concat":
			for _, a := range x.Args {
				if !isSorted(a) {
					return false
				}
			}
			return len(x.Args) > 0
		}
	}
	return false
}

// outputProviderIDShape is the expression half of
// output/provider-id-list-shape, checked against m, native syntax only:
// provider_id_list must list every non-terminated member, so a for
// filtered on a running/healthy status is wrong, and unless the list is
// sort()ed (directly, or through parentheses, tolist, distinct, concat or
// both branches of a conditional) its order churns; distinct() alone does
// not fix the order. Like poolAutoscaling, Checks lists it for the
// machinepool role only. It returns the IDOutputProviderIDs findings for
// each problem found, or nil for JSON syntax (not scanned: the check
// cannot read expressions there) or a compliant expression.
func outputProviderIDShape(m *Module) []Finding {
	a := m.model().outputs[providerIDList]
	if a == nil {
		return nil
	}
	expr, ok := a.Expr.(hclsyntax.Expression)
	if !ok {
		return nil // JSON syntax: not scanned
	}
	file, line := m.atRange(a.Range)
	var out []Finding
	_ = hclsyntax.VisitAll(expr, func(n hclsyntax.Node) hcl.Diagnostics {
		if f, ok := n.(*hclsyntax.ForExpr); ok && f.CondExpr != nil && filtersOnStatus(f.CondExpr) {
			out = append(out, Finding{ID: IDOutputProviderIDs, Severity: SeverityWarning, File: file, Line: line,
				Message: "output " + providerIDList + " filters on health or state: it must list every non-terminated member"})
		}
		return nil
	})
	if !isSorted(expr) {
		out = append(out, Finding{ID: IDOutputProviderIDs, Severity: SeverityWarning, File: file, Line: line,
			Message: "output " + providerIDList + " is not sorted with sort(): its order churns between runs"})
	}
	return out
}
