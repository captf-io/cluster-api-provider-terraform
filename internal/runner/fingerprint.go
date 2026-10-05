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
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DefaultPlanKeyFile is where plan and apply Jobs mount the plan key:
// internal/plankey's MountPath joined with its KeyFile. The runner does not
// import internal/plankey, which pulls in the Kubernetes client the runner
// binary otherwise does without; a test keeps the two in step.
const DefaultPlanKeyFile = "/captf/plan-key/key"

// planKeySize is the shortest plan key accepted, internal/plankey.KeySize.
const planKeySize = 32

// errNoPlanKey is returned when a plan is fingerprinted without a key.
var errNoPlanKey = errors.New("runner: no plan key: a plan fingerprint is always keyed")

// loadPlanKey reads the plan key from the file at path, as
// internal/plankey.Load does. It returns an error when path is empty, the
// file cannot be read or it holds fewer than planKeySize bytes. The error
// names the path and the length, never the key.
func loadPlanKey(path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("%w (no --plan-key-file)", errNoPlanKey)
	}
	k, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("runner: read plan key: %w", err)
	}
	if len(k) < planKeySize {
		return nil, fmt.Errorf("runner: plan key %s has %d bytes, want at least %d", path, len(k), planKeySize)
	}
	return k, nil
}

// changeValues are a change's values decoded with UseNumber, so numbers
// keep their literal text: before and after, the unknown and sensitive
// markers, and the importing object of a resource the plan imports.
type changeValues struct {
	Before          any
	After           any
	AfterUnknown    any
	BeforeSensitive any
	AfterSensitive  any
	Importing       any
}

// decodeValues returns c's values decoded, or a non-nil error when one is
// not valid JSON. An absent value decodes as null, like an explicit null.
func decodeValues(c planChange) (changeValues, error) {
	var v changeValues
	for _, f := range []struct {
		raw json.RawMessage
		dst *any
	}{
		{c.Before, &v.Before}, {c.After, &v.After}, {c.AfterUnknown, &v.AfterUnknown},
		{c.BeforeSensitive, &v.BeforeSensitive}, {c.AfterSensitive, &v.AfterSensitive},
		{c.Importing, &v.Importing},
	} {
		if len(f.raw) == 0 {
			continue
		}
		d := json.NewDecoder(bytes.NewReader(f.raw))
		d.UseNumber()
		if err := d.Decode(f.dst); err != nil {
			return changeValues{}, fmt.Errorf("parse plan values: %w", err)
		}
	}
	return v, nil
}

// fingerprinter builds the fingerprint lines of one plan under key.
type fingerprinter struct {
	key []byte
}

// line returns the fingerprint line of the change c named name (a resource
// address, or "output.<name>"), which a moved block moves from the address
// from when that is not "": "<name>|<actions>|<mac>", where mac is the hex
// HMAC-SHA256 under f.key of the canonical JSON of c's effect (effectOf),
// what c imports and from. The values, sensitive or not, enter only the
// HMAC.
func (f *fingerprinter) line(name string, c planChange, from string) (string, error) {
	v, err := decodeValues(c)
	if err != nil {
		return "", err
	}
	e := effectOf(c.Actions, v)
	e.Importing, e.PreviousAddress = v.Importing, from
	canonical, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("canonicalize plan values: %w", err)
	}
	mac := hmac.New(sha256.New, f.key)
	mac.Write(canonical)
	return name + "|" + strings.Join(c.Actions, ",") + "|" + hex.EncodeToString(mac.Sum(nil)), nil
}

// planSensitiveValues returns planSensitive of the `show -json` plan
// planJSON, without a key: a run that fingerprints nothing (a guarded
// apply) still redacts them. It returns a non-nil error when planJSON is
// not valid JSON.
func planSensitiveValues(planJSON []byte) ([]string, error) {
	plan, err := parsePlan(planJSON)
	if err != nil {
		return nil, err
	}
	return planSensitive(plan)
}

// planSensitive returns, sorted and without duplicates, the non-empty
// string values plan marks sensitive: those of its resource changes, no-ops
// included (before under before_sensitive, after under after_sensitive),
// and those of the data resources its prior state holds, in the root
// module and every child module (values under sensitive_values). A data
// source read during the plan appears only there, not among the resource
// changes. It returns a non-nil error when a change's or a data
// resource's values are not valid JSON.
//
// Output changes are left out. The rendered root re-exports every contract
// output as sensitive, so that a module may mark any of them sensitive;
// collecting them would make every provider ID, address, hostname and
// failure domain a redaction target and mangle the diagnostics that name
// them. An output that is truly sensitive takes its value from a sensitive
// resource attribute, data source attribute or variable, which are
// redacted already.
func planSensitive(plan planDoc) ([]string, error) {
	into := map[string]struct{}{}
	for _, rc := range plan.ResourceChanges {
		v, err := decodeValues(rc.Change)
		if err != nil {
			return nil, err
		}
		collectSensitive(v.Before, v.BeforeSensitive, into)
		collectSensitive(v.After, v.AfterSensitive, into)
	}
	if err := collectDataSensitive(plan.PriorState.Values.RootModule, into); err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(into)), nil
}

// collectDataSensitive adds to into every non-empty string leaf the data
// resources of the state module m and of its child modules, at any depth,
// mark sensitive (sensitive_values). Managed resources are left out: their
// values are a resource change's before already. It returns a non-nil
// error when a data resource's values are not valid JSON.
func collectDataSensitive(m stateModule, into map[string]struct{}) error {
	for _, r := range m.Resources {
		if r.Mode != "data" || len(r.SensitiveValues) == 0 {
			continue
		}
		var values, marker any
		if err := json.Unmarshal(r.SensitiveValues, &marker); err != nil {
			return fmt.Errorf("parse plan values: %w", err)
		}
		if len(r.Values) > 0 {
			if err := json.Unmarshal(r.Values, &values); err != nil {
				return fmt.Errorf("parse plan values: %w", err)
			}
		}
		collectSensitive(values, marker, into)
	}
	for _, c := range m.ChildModules {
		if err := collectDataSensitive(c, into); err != nil {
			return err
		}
	}
	return nil
}

// collectSensitive adds to into every non-empty string leaf of v that
// marker marks sensitive. A marker is true for a sensitive value (and all
// it contains), or an object or array of markers mirroring v's shape.
func collectSensitive(v, marker any, into map[string]struct{}) {
	switch m := marker.(type) {
	case bool:
		if m {
			collectStrings(v, into)
		}
	case map[string]any:
		obj, _ := v.(map[string]any)
		for k, mk := range m {
			collectSensitive(obj[k], mk, into)
		}
	case []any:
		arr, _ := v.([]any)
		for i, mk := range m {
			if i < len(arr) {
				collectSensitive(arr[i], mk, into)
			}
		}
	}
}

// collectStrings adds to into every non-empty string leaf of v.
func collectStrings(v any, into map[string]struct{}) {
	switch x := v.(type) {
	case string:
		if x != "" {
			into[x] = struct{}{}
		}
	case map[string]any:
		for _, e := range x {
			collectStrings(e, into)
		}
	case []any:
		for _, e := range x {
			collectStrings(e, into)
		}
	}
}
