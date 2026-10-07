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

package inputs

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// TestUnpullable proves AddUnpullable appends an image once, keeps only
// the newest MaxUnpullable, and survives WriteAttempt; Read returns the
// list, an unparsable value reads as none, ClearUnpullable removes it,
// and a missing durable Secret is ErrNotFound.
func TestUnpullable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	owner := machine("m")
	c := newClient(t, owner)
	if _, _, err := AddUnpullable(ctx, c, owner, nil, "r@sha256:a", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no durable Secret: err = %v, want ErrNotFound", err)
	}
	files := machineFiles(t, "#cloud-config\n")
	if err := WriteAttempt(ctx, c, owner, attempt(files, "j1")); err != nil {
		t.Fatal(err)
	}
	var list []Unpullable
	for _, ref := range []string{"r@sha256:a", "r@sha256:a", "r:v1", "r:v2", "r:v3", "r:v4"} {
		got, added, err := AddUnpullable(ctx, c, owner, list, ref, now)
		if err != nil {
			t.Fatal(err)
		}
		if added == slices.Contains(UnpullableRefs(list, now), ref) {
			t.Errorf("%s: added = %v with list %v", ref, added, list)
		}
		list = got
	}
	if want := []string{"r:v1", "r:v2", "r:v3", "r:v4"}; !slices.Equal(UnpullableRefs(list, now), want) {
		t.Errorf("list = %v, want %v", list, want)
	}
	if err := WriteAttempt(ctx, c, owner, attempt(files, "j2")); err != nil {
		t.Fatal(err)
	}
	d, err := Read(ctx, c, ns, "m", "m")
	if err != nil || !slices.Equal(d.Unpullable, list) {
		t.Fatalf("Read after WriteAttempt: %v, %v; want %v", d, err, list)
	}
	if err := ClearUnpullable(ctx, c, owner); err != nil {
		t.Fatal(err)
	}
	if d, _ = Read(ctx, c, ns, "m", "m"); d.Unpullable != nil {
		t.Errorf("after ClearUnpullable: %v", d.Unpullable)
	}
	if got := parseUnpullable("{"); got != nil {
		t.Errorf("unparsable = %v", got)
	}
}

// TestUnpullableExpires proves an entry is passed over for UnpullableTTL
// only: after it, UnpullableRefs leaves it out and AddUnpullable records
// it again with the new time, dropping what expired; a list of bare
// references, as earlier releases wrote it, reads as expired.
func TestUnpullableExpires(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	owner := machine("m")
	c := newClient(t, owner)
	if err := WriteAttempt(ctx, c, owner, attempt(machineFiles(t, "#cloud-config\n"), "j1")); err != nil {
		t.Fatal(err)
	}
	list, _, err := AddUnpullable(ctx, c, owner, nil, "r@sha256:a", t0)
	if err != nil {
		t.Fatal(err)
	}
	if list, _, err = AddUnpullable(ctx, c, owner, list, "r:v1", t0.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	later := t0.Add(UnpullableTTL)
	if got := UnpullableRefs(list, later); !slices.Equal(got, []string{"r:v1"}) {
		t.Errorf("refs after the TTL = %v, want only the newer entry", got)
	}
	list, added, err := AddUnpullable(ctx, c, owner, list, "r@sha256:a", later)
	if err != nil || !added || len(list) != 2 || list[1].Ref != "r@sha256:a" || !list[1].At.Equal(later) {
		t.Errorf("re-recorded = %v, %v, %v", list, added, err)
	}
	if got := parseUnpullable(`["r:v1"]`); len(got) != 1 || got[0].Ref != "r:v1" || UnpullableRefs(got, t0) != nil {
		t.Errorf("bare list = %v, want one expired entry", got)
	}
}
