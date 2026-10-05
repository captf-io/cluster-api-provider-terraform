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
	"os"
)

// Encode marshals r into at most MaxResultBytes, shrinking in stages until
// it fits: the resource changes (only a metric), then the error tail, then
// the plan's resources (halved until it fits, marked truncated), then the
// drift resources, then the steps (keeping the first and the last), then
// everything but version, op, error kind and the plan's hash and counts
// (resources, outputs, imports and moves): without the hash a plan Job's
// result is lost, and the controller could only plan again; without the
// output count an output-only plan would read as changing nothing. It
// returns the resulting JSON bytes.
func Encode(r Result) []byte {
	fits := func(r Result) ([]byte, bool) {
		b, err := json.Marshal(r)
		return b, err == nil && len(b) <= MaxResultBytes
	}
	if b, ok := fits(r); ok {
		return b
	}
	if r.Changes != nil {
		r.Changes = nil
		if b, ok := fits(r); ok {
			return b
		}
	}
	if r.Error != nil {
		e := *r.Error
		r.Error = &e
		for r.Error.Tail != "" {
			r.Error.Tail = r.Error.Tail[len(r.Error.Tail)/2:]
			if len(r.Error.Tail) < 16 {
				r.Error.Tail = ""
			}
			if b, ok := fits(r); ok {
				return b
			}
		}
	}
	if r.Plan != nil {
		p := *r.Plan
		r.Plan = &p
		for len(p.Resources) > 0 {
			p.Resources, p.Truncated = p.Resources[:len(p.Resources)/2], true
			if len(p.Resources) == 0 {
				p.Resources = nil
			}
			if b, ok := fits(r); ok {
				return b
			}
		}
	}
	if r.Drift != nil {
		d := *r.Drift
		d.Resources = nil
		r.Drift = &d
		if b, ok := fits(r); ok {
			return b
		}
	}
	for len(r.Steps) > 2 {
		r.Steps = append(r.Steps[:1:1], r.Steps[2:]...)
		if b, ok := fits(r); ok {
			return b
		}
	}
	minimal := Result{Version: ResultVersion, Op: r.Op}
	if r.Error != nil {
		minimal.Error = &Error{Kind: r.Error.Kind}
	}
	if r.Plan != nil {
		minimal.Plan = &Plan{
			Hash: r.Plan.Hash, Add: r.Plan.Add, Change: r.Plan.Change, Destroy: r.Plan.Destroy,
			Outputs: r.Plan.Outputs, Import: r.Plan.Import, Move: r.Plan.Move, Truncated: r.Plan.Truncated,
		}
	}
	b, _ := json.Marshal(minimal)
	return b
}

// Write writes r, encoded, to path (the termination log). It returns a
// non-nil error only when the write itself fails.
func Write(path string, r Result) error {
	if err := os.WriteFile(path, Encode(r), 0o600); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return nil
}
