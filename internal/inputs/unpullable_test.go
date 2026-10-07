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
)

// TestUnpullable proves AddUnpullable appends an image once, keeps only
// the newest MaxUnpullable, and survives WriteAttempt; Read returns the
// list, an unparsable value reads as none, ClearUnpullable removes it,
// and a missing durable Secret is ErrNotFound.
func TestUnpullable(t *testing.T) {
	t.Parallel()
	owner := machine("m")
	c := newClient(t, owner)
	if _, _, err := AddUnpullable(ctx, c, owner, nil, "r@sha256:a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no durable Secret: err = %v, want ErrNotFound", err)
	}
	files := machineFiles(t, "#cloud-config\n")
	if err := WriteAttempt(ctx, c, owner, attempt(files, "j1")); err != nil {
		t.Fatal(err)
	}
	var list []string
	for _, ref := range []string{"r@sha256:a", "r@sha256:a", "r:v1", "r:v2", "r:v3"} {
		got, added, err := AddUnpullable(ctx, c, owner, list, ref)
		if err != nil {
			t.Fatal(err)
		}
		if added == slices.Contains(list, ref) {
			t.Errorf("%s: added = %v with list %v", ref, added, list)
		}
		list = got
	}
	if want := []string{"r:v1", "r:v2", "r:v3"}; !slices.Equal(list, want) {
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
