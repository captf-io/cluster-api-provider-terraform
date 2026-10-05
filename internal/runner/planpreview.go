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

package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/captf-io/cluster-api-provider-terraform/internal/strutil"
)

// MaxPlanResources caps the "<address> (<action>)" entries a plan summary
// keeps (status.plan.resources); Plan.Truncated says when there were more.
const MaxPlanResources = 50

// PlanHashPrefix versions the plan fingerprint, as h1:/h2: version the
// inputs hash: a changed algorithm can never match an old approval. p2:
// keys each change's effect (what it sets, not the attributes it leaves
// alone), its output changes, and what it imports or moves; an approval of
// a p1: plan (addresses and actions only) no longer matches, so such a
// plan is planned and approved again.
const PlanHashPrefix = "p2:"

// EmptyPlanHash is PlanHash of a plan that changes, imports and moves no
// resource and changes no output. Such a plan never waits for an approval.
const EmptyPlanHash = PlanHashPrefix + "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Plan summarizes a plan for review (applyPolicy Manual): the counts, the
// address and action of every resource it changes (at most
// MaxPlanResources), and its fingerprint. Never a value: the plan document
// the summary is read from stays in the runner.
type Plan struct {
	// Hash is PlanHash of the plan's changes.
	Hash    string `json:"hash"`
	Add     int    `json:"add"`
	Change  int    `json:"change"`
	Destroy int    `json:"destroy"`
	// Outputs is the number of root module outputs the plan changes.
	Outputs int `json:"outputs,omitempty"`
	// Import and Move are the numbers of resources the plan imports into
	// the state and moves to a new address (a moved block). Neither counts
	// in Add, Change or Destroy; a resource imported or moved and also
	// changed counts there by its action as well.
	Import int `json:"import,omitempty"`
	Move   int `json:"move,omitempty"`
	// Resources are "<address> (<labels>)", sorted by address: the action
	// (create, update, delete, replace, read or forget) unless a no-op,
	// then "import" and "move" when the plan imports or moves the
	// resource, comma-separated: "a.b (import)", "a.c (update, move)".
	Resources []string `json:"resources,omitempty"`
	// Truncated is true when Resources lists fewer changes than the plan
	// has.
	Truncated bool `json:"truncated,omitempty"`

	// sensitive is SensitiveValues' list; it is never encoded.
	sensitive []string
}

// SensitiveValues returns, sorted and without duplicates, the non-empty
// string values the plan marks sensitive (before_sensitive or
// after_sensitive), of every resource change and of every data source the
// plan read (sensitive_values in the prior state), but not of output
// changes (see planSensitive), for the runner to redact from what it
// reports. Never log, emit or encode them.
func (p *Plan) SensitiveValues() []string {
	if p == nil {
		return nil
	}
	return slices.Clone(p.sensitive)
}

// EmptyPlan returns the summary of a plan that changes nothing (the plan
// step exited 0, so there is no plan to show).
func EmptyPlan() *Plan {
	return &Plan{Hash: EmptyPlanHash}
}

// PlanHash returns the plan fingerprint an approval names, of changes:
// PlanHashPrefix + hex(sha256) of the sorted lines, newline-joined. ParsePlan
// makes one line per resource or output change that is not a no-op, and per
// resource the plan imports or moves even when its action is a no-op:
// "<address>|<actions>|<mac>" or "output.<name>|<actions>|<mac>", where
// actions are comma-joined in the plan's order, so the two replace orders
// differ, and mac is the hex HMAC-SHA256, under the object's plan key, of
// the change's effect: for an update or a replacement, every path whose
// value changes (before and after), becomes unknown or changes
// sensitivity; for a create or a read, the planned object with its unknown
// and sensitive markers; for a delete or a forget, nothing more; and in
// every case what it imports (the import ID or identity) and the address
// it moves from. The plan Job and the approved apply both compute it here:
// an approval matches only the same changes, setting the same values, of
// the same resources and outputs, whatever order the runtime listed them
// in, and however the attributes a change leaves alone moved in between.
// The key keeps a reader of the hash from testing guesses of a value
// against it.
func PlanHash(changes []string) string {
	sorted := slices.Sorted(slices.Values(changes))
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return PlanHashPrefix + hex.EncodeToString(sum[:])
}

// planAction returns actions named the way the CLI does: create, update,
// delete, replace (either order), read or forget; anything else is
// actions comma-joined.
func planAction(actions []string) string {
	switch {
	case slices.Contains(actions, "delete") && slices.Contains(actions, "create"):
		return "replace"
	case len(actions) == 1:
		return actions[0]
	}
	return strings.Join(actions, ",")
}

// noOp reports whether actions does nothing.
func noOp(actions []string) bool {
	return len(actions) == 0 || (len(actions) == 1 && actions[0] == "no-op")
}

// imports reports whether c imports its resource into the state: its
// importing object is present and not null.
func (c planChange) imports() bool {
	return len(c.Importing) > 0 && string(c.Importing) != "null"
}

// changeLabels returns what rc does, as status.plan.resources names it:
// its action (planAction) unless it is a no-op, then "import" when it
// imports the resource and "move" when a moved block moves it. A resource
// imported or moved without other change has only that label; nil means rc
// does nothing.
func changeLabels(rc resourceChange) []string {
	var labels []string
	if !noOp(rc.Change.Actions) {
		labels = append(labels, planAction(rc.Change.Actions))
	}
	if rc.Change.imports() {
		labels = append(labels, "import")
	}
	if rc.PreviousAddress != "" {
		labels = append(labels, "move")
	}
	return labels
}

// ParsePlan summarizes the `show -json` plan planJSON for review, and
// returns the resulting Plan: counts as ParseDrift counts them, the numbers
// of changed outputs and of imported and moved resources, the changed,
// imported and moved resources, the sensitive values and
// PlanHash, keyed with key. It returns a non-nil error when key is shorter
// than a plan key (a fingerprint is never computed unkeyed) or planJSON is
// not valid JSON.
func ParsePlan(planJSON, key []byte) (*Plan, error) {
	if len(key) < planKeySize {
		return nil, errNoPlanKey
	}
	plan, err := parsePlan(planJSON)
	if err != nil {
		return nil, err
	}
	d, err := ParseDrift(planJSON)
	if err != nil {
		return nil, err
	}
	p := &Plan{Add: d.Add, Change: d.Change, Destroy: d.Destroy}
	f := &fingerprinter{key: key}
	var entries []string
	type listed struct{ address, line string }
	var all []listed
	for _, rc := range plan.ResourceChanges {
		labels := changeLabels(rc)
		if len(labels) == 0 {
			continue
		}
		e, err := f.line(rc.Address, rc.Change, rc.PreviousAddress)
		if err != nil {
			return nil, err
		}
		if rc.Change.imports() {
			p.Import++
		}
		if rc.PreviousAddress != "" {
			p.Move++
		}
		entries = append(entries, e)
		all = append(all, listed{rc.Address, rc.Address + " (" + strings.Join(labels, ", ") + ")"})
	}
	for name, oc := range plan.OutputChanges {
		if noOp(oc.Actions) {
			continue
		}
		e, err := f.line("output."+name, oc, "")
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
		p.Outputs++
	}
	p.Hash = PlanHash(entries)
	if p.sensitive, err = planSensitive(plan); err != nil {
		return nil, err
	}
	slices.SortFunc(all, func(a, b listed) int { return strings.Compare(a.address, b.address) })
	for i, l := range all {
		if i == MaxPlanResources {
			p.Truncated = true
			break
		}
		p.Resources = append(p.Resources, l.line)
	}
	return p, nil
}

// planChangedSummary returns the failure summary of an approved apply whose
// plan p is no longer the one named by approved.
func planChangedSummary(approved string, p *Plan) string {
	return strutil.Truncate(fmt.Sprintf("the plan changed since it was approved: approved %s, now %s (%s); nothing was applied",
		approved, p.Hash, p.counts()), MaxSummary)
}

// counts returns p's counts as "<n> to add, <n> to change, <n> to
// destroy", followed by ", <n> to import", ", <n> to move" and ", <n>
// output(s) to change" for those it has.
func (p *Plan) counts() string {
	s := fmt.Sprintf("%d to add, %d to change, %d to destroy", p.Add, p.Change, p.Destroy)
	if p.Import > 0 {
		s += fmt.Sprintf(", %d to import", p.Import)
	}
	if p.Move > 0 {
		s += fmt.Sprintf(", %d to move", p.Move)
	}
	if p.Outputs > 0 {
		s += fmt.Sprintf(", %d output(s) to change", p.Outputs)
	}
	return s
}
