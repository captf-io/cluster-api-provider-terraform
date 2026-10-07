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
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
)

// exportsHashOf returns hash.Exports of raw, failing t on error.
func exportsHashOf(t *testing.T, raw string) string {
	t.Helper()
	h, err := hash.Exports(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestRecordClusterOutputs proves the recorded exports are read back, kept
// by the next WriteAttempt, replaced by the next record, and that a record
// removes a pending change; exports that do not fit next to the files
// are not recorded and drop an older record, but their hash is; a
// missing Secret is ErrNotFound.
func TestRecordClusterOutputs(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	m := machine("m1")
	if _, err := RecordClusterOutputs(ctx, c, m, json.RawMessage(`{}`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("record without a Secret: %v, want ErrNotFound", err)
	}
	files := machineFiles(t, "a")
	if err := WriteAttempt(ctx, c, m, Record{Files: files, Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if err := SetPending(ctx, c, m, Pending{ExportsHash: "h2:new", ApprovalHash: "h2:a", Job: "j1", Summary: "the plan replaces x"}); err != nil {
		t.Fatal(err)
	}
	d, err := Read(ctx, c, ns, "m", "m1")
	if err != nil || d.Pending == nil || d.Pending.Job != "j1" || d.Pending.Summary != "the plan replaces x" || d.AppliedClusterOutputs != nil {
		t.Fatalf("after SetPending: %+v, %v", d, err)
	}

	ok, err := RecordClusterOutputs(ctx, c, m, json.RawMessage(`{"net":"n-1"}`))
	if err != nil || !ok {
		t.Fatalf("record: %v, %v", ok, err)
	}
	d, err = Read(ctx, c, ns, "m", "m1")
	if err != nil || string(d.AppliedClusterOutputs) != `{"net":"n-1"}` || d.AppliedExportsHash != exportsHashOf(t, `{"net":"n-1"}`) || d.Pending != nil {
		t.Fatalf("after record: applied %s (%s), pending %+v, %v", d.AppliedClusterOutputs, d.AppliedExportsHash, d.Pending, err)
	}
	if !bytes.Equal(d.Attempt.Files.TFVars, files.TFVars) {
		t.Error("recording the exports changed the rendered files")
	}

	// The next apply's WriteAttempt keeps the record; any other key goes.
	if err := WriteAttempt(ctx, c, m, Record{Files: machineFiles(t, "b"), Image: "img"}); err != nil {
		t.Fatal(err)
	}
	s := getSecret(t, c, Name("m", "m1"))
	if !slices.Equal(dataKeys(s), []string{AppliedClusterOutputsKey, MainTFKey, TFVarsKey}) || string(s.Data[AppliedClusterOutputsKey]) != `{"net":"n-1"}` {
		t.Errorf("after WriteAttempt: keys %v, applied %s", dataKeys(s), s.Data[AppliedClusterOutputsKey])
	}

	// Recording the same value again is a no-op that reports it recorded.
	if ok, err := RecordClusterOutputs(ctx, c, m, json.RawMessage(`{"net":"n-1"}`)); err != nil || !ok {
		t.Errorf("same record: %v, %v", ok, err)
	}

	// Exports too large to fit next to the files drop the record.
	huge := json.RawMessage(`"` + strings.Repeat("x", maxDataBytes) + `"`)
	if ok, err := RecordClusterOutputs(ctx, c, m, huge); err != nil || ok {
		t.Fatalf("oversize record: %v, %v; want not recorded", ok, err)
	}
	if d, err := Read(ctx, c, ns, "m", "m1"); err != nil || d.AppliedClusterOutputs != nil || d.AppliedExportsHash != exportsHashOf(t, string(huge)) {
		t.Errorf("an oversize record kept %d bytes, hash %s, %v", len(d.AppliedClusterOutputs), d.AppliedExportsHash, err)
	}
}

// TestWriteAttemptDropsRecordThatNoLongerFits proves WriteAttempt drops the recorded
// exports when the new rendered files leave no room for them, so a write
// never fails on the Secret's size, and keeps their hash.
func TestWriteAttemptDropsRecordThatNoLongerFits(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	m := machine("m1")
	if err := WriteAttempt(ctx, c, m, Record{Files: machineFiles(t, "a"), Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := RecordClusterOutputs(ctx, c, m, json.RawMessage(`{"net":"n-1"}`)); err != nil || !ok {
		t.Fatalf("record: %v, %v", ok, err)
	}
	big := render.Files{MainTF: []byte("{}"), TFVars: bytes.Repeat([]byte("x"), maxDataBytes-4)}
	if err := WriteAttempt(ctx, c, m, Record{Files: big, Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if s := getSecret(t, c, Name("m", "m1")); s.Data[AppliedClusterOutputsKey] != nil {
		t.Errorf("WriteAttempt kept a record that no longer fits: keys %v", dataKeys(s))
	}
	if d, err := Read(ctx, c, ns, "m", "m1"); err != nil || d.AppliedExportsHash != exportsHashOf(t, `{"net":"n-1"}`) {
		t.Errorf("the hash of the dropped record: %q, %v", d.AppliedExportsHash, err)
	}
}

// TestAppliedExports proves how a durable Secret's record of the applied
// exports reads: the hash annotation alone (a record dropped for size), a
// record from before the annotation (hashed), a record the annotation
// does not name or that is not JSON (no value to hold), and neither.
func TestAppliedExports(t *testing.T) {
	t.Parallel()
	h1, h2 := exportsHashOf(t, `{"net":"n-1"}`), exportsHashOf(t, `{"net":"n-2"}`)
	for _, tt := range []struct {
		name      string
		ann, data string
		wantHash  string
		wantData  bool
	}{
		{name: "hash and record", ann: h1, data: `{"net":"n-1"}`, wantHash: h1, wantData: true},
		{name: "hash only", ann: h1, wantHash: h1},
		{name: "record before the hash", data: `{"net":"n-1"}`, wantHash: h1, wantData: true},
		{name: "record of other exports", ann: h2, data: `{"net":"n-1"}`, wantHash: h2},
		{name: "unreadable record", ann: h1, data: `{`, wantHash: h1},
		{name: "unreadable record before the hash", data: `{`},
		{name: "neither"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}, Data: map[string][]byte{}}
			if tt.ann != "" {
				s.Annotations[AppliedClusterOutputsHashAnnotation] = tt.ann
			}
			if tt.data != "" {
				s.Data[AppliedClusterOutputsKey] = []byte(tt.data)
			}
			h, raw := appliedExports(s)
			if h != tt.wantHash || (raw != nil) != tt.wantData {
				t.Errorf("appliedExports = %q, %s; want %q, record %v", h, raw, tt.wantHash, tt.wantData)
			}
		})
	}
}

// TestPendingRoundTrip proves SetPending reports ErrNotFound without a
// Secret, that a WriteAttempt keeps a pending change, and that an unparsable or
// empty annotation reads as no pending change.
func TestPendingRoundTrip(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	m := machine("m1")
	if err := SetPending(ctx, c, m, Pending{ExportsHash: "h"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetPending without a Secret: %v", err)
	}
	if err := WriteAttempt(ctx, c, m, Record{Files: machineFiles(t, "a"), Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if err := SetPending(ctx, c, m, Pending{ExportsHash: "h", Job: "j"}); err != nil {
		t.Fatal(err)
	}
	// A WriteAttempt (the next apply) keeps it.
	if err := WriteAttempt(ctx, c, m, Record{Files: machineFiles(t, "b"), Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if d, err := Read(ctx, c, ns, "m", "m1"); err != nil || d.Pending == nil || d.Pending.ExportsHash != "h" {
		t.Fatalf("pending after WriteAttempt: %+v, %v", d.Pending, err)
	}
	for _, raw := range []string{"", "{", `{"job":"j"}`} {
		if p := parsePending(raw); p != nil {
			t.Errorf("parsePending(%q) = %+v, want nil", raw, p)
		}
	}
}

// TestPartialRoundTrip proves SetPartial reports ErrNotFound without a
// Secret, that a WriteAttempt keeps the record, that RecordClusterOutputs
// removes it even when the exports it records are already recorded, and
// that an unparsable annotation still reads as a partial change.
func TestPartialRoundTrip(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	m := machine("m1")
	if err := SetPartial(ctx, c, m, Partial{ExportsHash: "h", Job: "j"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetPartial without a Secret: %v", err)
	}
	if err := WriteAttempt(ctx, c, m, Record{Files: machineFiles(t, "a"), Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := RecordClusterOutputs(ctx, c, m, json.RawMessage(`{"net":"n-1"}`)); err != nil || !ok {
		t.Fatalf("record: %v, %v", ok, err)
	}
	if err := SetPartial(ctx, c, m, Partial{ExportsHash: "h", Job: "j"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteAttempt(ctx, c, m, Record{Files: machineFiles(t, "b"), Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if d, err := Read(ctx, c, ns, "m", "m1"); err != nil || d.Partial == nil || *d.Partial != (Partial{ExportsHash: "h", Job: "j"}) {
		t.Fatalf("partial after WriteAttempt: %+v, %v", d.Partial, err)
	}
	if ok, err := RecordClusterOutputs(ctx, c, m, json.RawMessage(`{"net":"n-1"}`)); err != nil || !ok {
		t.Fatalf("record again: %v, %v", ok, err)
	}
	if d, err := Read(ctx, c, ns, "m", "m1"); err != nil || d.Partial != nil {
		t.Errorf("partial after a record of the same exports: %+v, %v", d.Partial, err)
	}
	if p := parsePartial(""); p != nil {
		t.Errorf("parsePartial(\"\") = %+v, want nil", p)
	}
	if p := parsePartial("{"); p == nil {
		t.Error("an unparsable partial change reads as none")
	}
}

// TestLastClusterOutputs proves LastClusterOutputs returns the rendered
// captf_cluster_outputs, and nil for a nil Record, unparsable tfvars or a
// root without the key.
func TestLastClusterOutputs(t *testing.T) {
	t.Parallel()
	d := &Record{Files: render.Files{TFVars: []byte(`{"captf_cluster_outputs":{"net":"n-1"},"replicas":1}`)}}
	if got := LastClusterOutputs(d); string(got) != `{"net":"n-1"}` {
		t.Errorf("LastClusterOutputs = %s", got)
	}
	for name, d := range map[string]*Record{
		"nil":          nil,
		"unparsable":   {Files: render.Files{TFVars: []byte("{")}},
		"cluster role": {Files: clusterFiles(t, false)},
	} {
		if got := LastClusterOutputs(d); got != nil {
			t.Errorf("%s: LastClusterOutputs = %s, want nil", name, got)
		}
	}
}
