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

package labels

import (
	"errors"
	"strings"
	"testing"
)

// TestParseCapacity proves ParseCapacity decodes a valid capacity label
// into the expected resource list and returns ErrInvalidLabel for
// malformed JSON, an empty object, a non-string quantity, an invalid
// resource name or an unparsable quantity.
func TestParseCapacity(t *testing.T) {
	t.Parallel()
	c, err := ParseCapacity(`{"cpu":"4","memory":"16Gi","nvidia.com/gpu":"1"}`)
	if err != nil || c.Cpu().Value() != 4 || c.Memory().String() != "16Gi" || len(c) != 3 {
		t.Errorf("capacity = %v, %v", c, err)
	}
	for _, bad := range []string{
		`{"cpu":"four"}`, `{}`, `[]`, `{"cpu":4}`, `{"cpu":"1"} {}`, `{"":"1"}`, `nope`,
		// Not a Kubernetes resource name: a slash with no prefix, an
		// invalid prefix, and an invalid name segment.
		`{"/cpu":"1"}`, `{"Not A Prefix/cpu":"1"}`, `{"cpu name":"1"}`,
	} {
		if _, err := ParseCapacity(bad); !errors.Is(err, ErrInvalidLabel) {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

// TestParseCapacityMessageStable proves a label with several bad entries
// always reports the same one, and quotes a long key cut short.
func TestParseCapacityMessageStable(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("k", 40_000)
	label := `{"b b":"1","a a":"x","` + long + `":"1"}`
	_, first := ParseCapacity(label)
	for range 50 {
		if _, err := ParseCapacity(label); err == nil || first == nil || err.Error() != first.Error() {
			t.Fatalf("messages differ: %v / %v", err, first)
		}
	}
	if !strings.Contains(first.Error(), `"a a"`) {
		t.Errorf("message %q does not report the first key in order", first)
	}
	_, err := ParseCapacity(`{"` + long + `":"1"}`)
	if err == nil || len(err.Error()) > 512 {
		t.Errorf("long key message is %d bytes", len(err.Error()))
	}
}

// TestParseNodeInfo proves ParseNodeInfo decodes a valid node-info label
// into the expected NodeInfo and returns ErrInvalidLabel for an all-empty
// object, an unknown architecture, an unrecognized field or an
// operatingSystem over 64 characters.
func TestParseNodeInfo(t *testing.T) {
	t.Parallel()
	ni, err := ParseNodeInfo(`{"architecture":"arm64","operatingSystem":"linux"}`)
	if err != nil || ni.Architecture != "arm64" || ni.OperatingSystem != "linux" {
		t.Errorf("node info = %+v, %v", ni, err)
	}
	for _, bad := range []string{`{}`, `{"architecture":"mips"}`, `{"os":"linux"}`, `{"operatingSystem":"` + strings.Repeat("x", 65) + `"}`, `nope`} {
		if _, err := ParseNodeInfo(bad); !errors.Is(err, ErrInvalidLabel) {
			t.Errorf("%s: %v", bad, err)
		}
	}
}
