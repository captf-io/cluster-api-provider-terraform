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
	"maps"
	"reflect"
	"slices"
)

// changeEffect is the canonical form of what one change does, which the
// plan fingerprint keys. encoding/json writes struct fields in this order
// and map keys sorted, and empty fields are omitted, so equal effects
// encode alike.
//
// It holds the effect, not the whole change: an attribute the change
// leaves as it is never enters it, so a computed attribute that moves
// between the plan Job and the approved apply's own plan (an autoscaled
// size under ignore_changes, a Kubernetes resourceVersion) does not change
// the fingerprint, while every value the change sets does.
type changeEffect struct {
	// Importing is what an import imports (its ID or identity), and
	// PreviousAddress the address a moved block moves the resource from.
	Importing       any    `json:"importing,omitempty"`
	PreviousAddress string `json:"previous_address,omitempty"`
	// After, AfterUnknown and AfterSensitive are the whole planned object
	// and its markers, for a change that creates or reads it.
	After          any `json:"after,omitempty"`
	AfterUnknown   any `json:"after_unknown,omitempty"`
	AfterSensitive any `json:"after_sensitive,omitempty"`
	// Paths are the values a change that keeps or replaces the object sets.
	Paths []pathEffect `json:"paths,omitempty"`
}

// pathEffect is one value an update or a replacement sets: the value at
// Path before and after it, whether it is unknown until applied, and
// which parts of it are sensitive before and after. Before is omitted for
// an unknown value: what the apply computes does not depend on it.
// BeforeAbsent and AfterAbsent say the value has no such member or element
// at all, which an explicit null (Before or After nil) is not. The markers
// are normalized (normMarker).
type pathEffect struct {
	Path            []any `json:"path"`
	Before          any   `json:"before"`
	After           any   `json:"after"`
	BeforeAbsent    bool  `json:"before_absent,omitempty"`
	AfterAbsent     bool  `json:"after_absent,omitempty"`
	Unknown         any   `json:"unknown,omitempty"`
	BeforeSensitive any   `json:"before_sensitive,omitempty"`
	AfterSensitive  any   `json:"after_sensitive,omitempty"`
}

// absent stands for an object member or a list element a value does not
// have, so walkDiff tells a member added or removed apart from one that
// is, or becomes, an explicit null.
type absent struct{}

// effectOf returns the effect of a change with actions and values v,
// without what it imports or where from it moves:
//
//   - create and read: the planned object (after) with its unknown and
//     sensitive markers;
//   - delete and forget: nothing, the address and actions say it all;
//   - anything else (update, either replace order, a no-op that imports or
//     moves): the paths whose value changes, becomes unknown or changes
//     sensitivity (diffPaths).
func effectOf(actions []string, v changeValues) changeEffect {
	switch {
	case len(actions) == 1 && (actions[0] == "create" || actions[0] == "read"):
		return changeEffect{After: v.After, AfterUnknown: normMarker(v.AfterUnknown), AfterSensitive: normMarker(v.AfterSensitive)}
	case len(actions) == 1 && (actions[0] == "delete" || actions[0] == "forget"):
		return changeEffect{}
	}
	return changeEffect{Paths: diffPaths(v)}
}

// diffPaths returns, ordered by path, the leaf paths of v where before and
// after differ, the value is unknown (after_unknown) or its sensitivity
// differs (before_sensitive and after_sensitive). A path is a list of
// tokens ([] for the whole value): an object key as a string, a list index
// as a number, so a key "0" and an index 0 never read alike, nor a key
// holding "/" as a nested path. An object or list both before and after
// holds is compared member by member, a member only one side has as
// absent on the other; anything else is a leaf, a whole object included
// when it appears, disappears or becomes unknown as a whole.
func diffPaths(v changeValues) []pathEffect {
	var out []pathEffect
	walkDiff([]any{}, v.Before, v.After, v.AfterUnknown, v.BeforeSensitive, v.AfterSensitive, &out)
	return out
}

// walkDiff appends to out the pathEffect of every leaf path under path
// that diffPaths keeps, of before b and after a with the unknown marker u
// and sensitive markers bs and as at path. b or a is absent{} where that
// side has no such member or element. Paths come out in a deterministic
// order: object keys sorted, list indexes ascending.
func walkDiff(path []any, b, a, u, bs, as any, out *[]pathEffect) {
	if unknown, ok := u.(bool); ok && unknown {
		*out = append(*out, pathEffect{Path: path, Unknown: true, BeforeSensitive: normMarker(bs), AfterSensitive: normMarker(as)})
		return
	}
	if bm, ok := b.(map[string]any); ok {
		if am, ok := a.(map[string]any); ok && (len(bm) > 0 || len(am) > 0) {
			keys := slices.AppendSeq(slices.Collect(maps.Keys(bm)), maps.Keys(am))
			if um, ok := u.(map[string]any); ok {
				keys = slices.AppendSeq(keys, maps.Keys(um))
			}
			slices.Sort(keys)
			for _, k := range slices.Compact(keys) {
				walkDiff(slices.Concat(path, []any{k}), field(bm, k), field(am, k), member(u, k), member(bs, k), member(as, k), out)
			}
			return
		}
	}
	if bl, ok := b.([]any); ok {
		if al, ok := a.([]any); ok && (len(bl) > 0 || len(al) > 0) {
			n := max(len(bl), len(al))
			if ul, ok := u.([]any); ok {
				n = max(n, len(ul))
			}
			for i := range n {
				walkDiff(slices.Concat(path, []any{i}), item(bl, i), item(al, i), element(u, i), element(bs, i), element(as, i), out)
			}
			return
		}
	}
	_, bAbsent := b.(absent)
	_, aAbsent := a.(absent)
	if bAbsent {
		b = nil
	}
	if aAbsent {
		a = nil
	}
	nu, nbs, nas := normMarker(u), normMarker(bs), normMarker(as)
	if nu != nil || bAbsent != aAbsent || !reflect.DeepEqual(b, a) || !reflect.DeepEqual(nbs, nas) {
		*out = append(*out, pathEffect{
			Path: path, Before: b, After: a, BeforeAbsent: bAbsent, AfterAbsent: aAbsent,
			Unknown: nu, BeforeSensitive: nbs, AfterSensitive: nas,
		})
	}
}

// field returns the member k of the object o, or absent{} when o has none.
func field(o map[string]any, k string) any {
	if v, ok := o[k]; ok {
		return v
	}
	return absent{}
}

// item returns l[i], or absent{} when l is shorter.
func item(l []any, i int) any {
	if i < len(l) {
		return l[i]
	}
	return absent{}
}

// member returns the marker of key k inside marker m: m itself when it is
// a boolean (true marks everything under it), its member k when it is an
// object, else nil.
func member(m any, k string) any {
	switch x := m.(type) {
	case bool:
		return x
	case map[string]any:
		return x[k]
	}
	return nil
}

// element returns the marker of index i inside marker m: m itself when it
// is a boolean, its element i when it is a list that long, else nil.
func element(m any, i int) any {
	switch x := m.(type) {
	case bool:
		return x
	case []any:
		return index(x, i)
	}
	return nil
}

// index returns l[i], or nil when l is shorter.
func index(l []any, i int) any {
	if i < len(l) {
		return l[i]
	}
	return nil
}

// normMarker returns marker m with everything that marks nothing removed:
// true stays true, an object or list keeps its members that mark something
// (a list keeps its length, nil for the others), and a marker that marks
// nothing at all (false, {}, [false], absent) is nil. So two markers that
// mark the same values are equal, whichever way the runtime spelled them.
func normMarker(m any) any {
	switch x := m.(type) {
	case bool:
		if x {
			return true
		}
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			if n := normMarker(e); n != nil {
				out[k] = n
			}
		}
		if len(out) > 0 {
			return out
		}
	case []any:
		out := make([]any, len(x))
		marks := false
		for i, e := range x {
			out[i] = normMarker(e)
			marks = marks || out[i] != nil
		}
		if marks {
			return out
		}
	}
	return nil
}
