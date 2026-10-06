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
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	"github.com/captf-io/cluster-api-provider-terraform/internal/strutil"
)

// MaxDriftResources caps the resource addresses kept in the result.
const MaxDriftResources = 20

// planChange is the part of a `show -json` plan's change object the runner
// reads: the actions, the values before and after the change with their
// unknown and sensitive markers, and, for a resource the plan imports
// (Terraform and OpenTofu 1.5 and later), what it imports. The values only
// feed the keyed plan fingerprint and the list of sensitive values to
// redact; none of them is ever logged, emitted as an event or written to
// the result.
type planChange struct {
	Actions         []string        `json:"actions"`
	Importing       json.RawMessage `json:"importing"`
	Before          json.RawMessage `json:"before"`
	After           json.RawMessage `json:"after"`
	AfterUnknown    json.RawMessage `json:"after_unknown"`
	BeforeSensitive json.RawMessage `json:"before_sensitive"`
	AfterSensitive  json.RawMessage `json:"after_sensitive"`
}

// resourceChange is a `show -json` plan's resource_changes[] entry, as far
// as the runner reads it. PreviousAddress is set when a moved block moves
// the resource from that address.
type resourceChange struct {
	Address         string     `json:"address"`
	PreviousAddress string     `json:"previous_address"`
	Change          planChange `json:"change"`
}

// planDoc is the part of a `show -json` plan the runner reads: its
// resource changes, its output changes by output name, and the prior
// state, whose data resources hold what the plan read from data sources.
type planDoc struct {
	ResourceChanges []resourceChange      `json:"resource_changes"`
	OutputChanges   map[string]planChange `json:"output_changes"`
	PriorState      struct {
		Values struct {
			RootModule stateModule `json:"root_module"`
		} `json:"values"`
	} `json:"prior_state"`
}

// stateModule is a module of a `show -json` state's values: its resources
// and its child modules.
type stateModule struct {
	Resources    []stateResource `json:"resources"`
	ChildModules []stateModule   `json:"child_modules"`
}

// stateResource is a resource of a stateModule, as far as the runner reads
// it: its mode ("managed" or "data"), its values and their sensitive
// markers, left raw until planSensitive reads them. Like a planChange's,
// the values are never logged, emitted or written to the result.
type stateResource struct {
	Mode            string          `json:"mode"`
	Values          json.RawMessage `json:"values"`
	SensitiveValues json.RawMessage `json:"sensitive_values"`
}

// parsePlan decodes the resource and output changes of the `show -json`
// plan planJSON and returns them, or a non-nil error when planJSON is not
// valid JSON.
func parsePlan(planJSON []byte) (planDoc, error) {
	var plan planDoc
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return planDoc{}, fmt.Errorf("parse plan: %w", err)
	}
	return plan, nil
}

// ParseDrift summarizes the `show -json` plan planJSON and returns the
// resulting Drift: counts of resources to create, update, replace (delete
// and create, in either order) and delete, and up to MaxDriftResources
// addresses. Output changes are not drift, nor are imports and moves: they
// come from the configuration (an import or a moved block), not from a
// change made outside it, so a resource the plan imports or moves counts
// only by the action it also takes, if any. It returns a non-nil error when
// planJSON is not valid JSON.
func ParseDrift(planJSON []byte) (*Drift, error) {
	plan, err := parsePlan(planJSON)
	if err != nil {
		return nil, err
	}
	d := &Drift{Resources: []string{}}
	for _, rc := range plan.ResourceChanges {
		a := rc.Change.Actions
		create, del := slices.Contains(a, "create"), slices.Contains(a, "delete")
		changed := true
		switch {
		case create && del:
			d.Replace++
		case create:
			d.Create++
		case del:
			d.Delete++
		case slices.Contains(a, "update"):
			d.Update++
		default:
			changed = false
		}
		if changed && len(d.Resources) < MaxDriftResources {
			d.Resources = append(d.Resources, rc.Address)
		}
	}
	// A plan can exit 2 for output-only changes or other actions this parser
	// does not count (e.g. a data source read); that is not drift.
	d.Detected = d.Create+d.Update+d.Replace+d.Delete > 0
	return d, nil
}

// DestructiveChanges returns the resources the `show -json` plan planJSON
// deletes, as "<address> (delete)" or "<address> (replace)": any change
// whose actions contain "delete", which covers a plain delete and both
// replace orders (["delete","create"] and ["create","delete"]). It returns
// a non-nil error when planJSON is not valid JSON.
func DestructiveChanges(planJSON []byte) ([]string, error) {
	plan, err := parsePlan(planJSON)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, rc := range plan.ResourceChanges {
		a := rc.Change.Actions
		if !slices.Contains(a, "delete") {
			continue
		}
		action := "delete"
		if slices.Contains(a, "create") {
			action = "replace"
		}
		out = append(out, rc.Address+" ("+action+")")
	}
	return out, nil
}

// blockedPrefix starts the summary of a blocked apply; the controller shows
// the summary in ApplyJobSucceeded's message.
const blockedPrefix = "the plan deletes or replaces "

// blockedSummary returns the failure summary of a blocked apply over
// destructive: the count and as many addresses as fit in MaxSummary bytes,
// then "and N more". Only addresses and actions, never plan values.
func blockedSummary(destructive []string) string {
	s := blockedPrefix + strconv.Itoa(len(destructive)) + " resource(s): "
	for i, addr := range destructive {
		sep := ""
		if i > 0 {
			sep = ", "
		}
		// Room for this address and, if more follow, the " and N more" tail.
		tail := ""
		if rest := len(destructive) - i - 1; rest > 0 {
			tail = " and " + strconv.Itoa(rest) + " more"
		}
		if len(s)+len(sep)+len(addr)+len(tail) > MaxSummary {
			more := " and " + strconv.Itoa(len(destructive)-i) + " more"
			return strutil.Truncate(s, MaxSummary-len(more)) + more
		}
		s += sep + addr
	}
	return s
}
