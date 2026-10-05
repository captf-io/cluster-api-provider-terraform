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

package greenlight

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/env"
)

// t0 is the fixed "now" of the tests.
var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// testPins returns a fixed set of pins.
func testPins() env.Pins {
	return env.Pins{KindVersion: "v0.33.0", KindNodeImage: "kindest/node:v1.36.0", CAPIVersion: "v1.13.0",
		CertManagerVersion: "v1.20.0", CAPTFVersion: "v0.1.0", NoopImages: []string{"a@sha256:1", "b@sha256:2"}}
}

// goodRecord returns a record that Validate accepts with goodCheck.
func goodRecord() Record {
	return Record{
		Version: RecordVersion, Cluster: "captf-test-e2e", TreeID: "tree-1", ManagerRef: "mgr:1", KubernetesVersion: "v1.36.0",
		Pins:            testPins(),
		Stages:          []Stage{{Name: "cluster-build", Duration: time.Minute, Passed: true}, {Name: "together", Duration: 2 * time.Minute, Passed: true}},
		StabilityWindow: 2 * time.Minute,
		CreatedAt:       t0.Add(-time.Hour),
	}
}

// goodCheck returns a check that accepts goodRecord.
func goodCheck() Check {
	return Check{
		Pins: testPins(), ManagerRef: "mgr:1",
		ClusterExists: func(string) (bool, error) { return true, nil },
		MaxAge:        24 * time.Hour,
		Now:           func() time.Time { return t0 },
	}
}

// TestPath checks the record's location.
func TestPath(t *testing.T) {
	t.Parallel()
	if got := Path("/w/captf"); got != "/w/captf/greenlight.json" {
		t.Fatalf("Path = %q", got)
	}
}

// TestRoundTrip writes and reads a record and checks the version stamp
// and the absence of temporary files.
func TestRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := Path(dir)
	want := goodRecord()
	want.Version = 0 // Write stamps it.
	if err := Write(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	want.Version = RecordVersion
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("want only greenlight.json, found %v", entries)
	}
}

// TestWriteAtomic checks that a failed write leaves the old file intact
// and no temporary file behind, and that a write replaces atomically.
func TestWriteAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := Path(dir)
	first := goodRecord()
	if err := Write(path, first); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)

	// Make the rename fail: the target is a non-empty directory.
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Write(blocked, first); err == nil {
		t.Fatal("write over a directory must fail")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Error("existing record changed by a failed write of another path")
	}

	second := first
	second.Cluster = "other"
	if err := Write(path, second); err != nil {
		t.Fatal(err)
	}
	got, _ := Read(path)
	if got.Cluster != "other" {
		t.Errorf("record not replaced: %+v", got)
	}

	if err := Write(filepath.Join(dir, "no", "such", "dir", "g.json"), first); err == nil {
		t.Fatal("write into a missing directory must fail")
	}
}

// TestReadErrors checks the missing and malformed cases.
func TestReadErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := Read(Path(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	bad := Path(dir)
	if err := os.WriteFile(bad, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(bad); err == nil || !strings.Contains(err.Error(), "decode record") {
		t.Fatalf("malformed file: %v", err)
	}
}

// TestValidate covers the accepted record and every failure mode.
func TestValidate(t *testing.T) {
	t.Parallel()
	if err := Validate(goodRecord(), goodCheck()); err != nil {
		t.Fatalf("good record rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Record, *Check)
		want   []string
	}{
		{"version", func(r *Record, _ *Check) { r.Version = 99 }, []string{"record version 99, want 1"}},
		{"cluster missing", func(_ *Record, c *Check) { c.ClusterExists = func(string) (bool, error) { return false, nil } }, []string{`cluster "captf-test-e2e" does not exist`}},
		{"cluster error", func(_ *Record, c *Check) {
			c.ClusterExists = func(string) (bool, error) { return false, errors.New("engine down") }
		}, []string{"cannot tell", "engine down"}},
		{"no cluster func", func(_ *Record, c *Check) { c.ClusterExists = nil }, []string{"no cluster check configured"}},
		{"pins all differ", func(r *Record, _ *Check) {
			r.Pins = env.Pins{KindVersion: "x", KindNodeImage: "x", CAPIVersion: "x", CertManagerVersion: "x", CAPTFVersion: "x", NoopImages: []string{"z"}}
		}, []string{"pin kindVersion", "pin kindNodeImage", "pin capiVersion", "pin certManagerVersion", "pin captfVersion", "pin noopImages"}},
		{"manager ref", func(r *Record, _ *Check) { r.ManagerRef = "mgr:0" }, []string{`manager ref "mgr:0", current "mgr:1"`}},
		{"too old", func(r *Record, _ *Check) { r.CreatedAt = t0.Add(-25 * time.Hour) }, []string{"record is 25h0m0s old, max 24h0m0s"}},
		{"default max age", func(r *Record, c *Check) { c.MaxAge = 0; r.CreatedAt = t0.Add(-25 * time.Hour) }, []string{"max 24h0m0s"}},
		{"stage failed", func(r *Record, _ *Check) { r.Stages[1].Passed = false }, []string{`stage "together" did not pass`}},
		{"no stages", func(r *Record, _ *Check) { r.Stages = nil }, []string{"no stages"}},
		{"several at once", func(r *Record, _ *Check) {
			r.ManagerRef = "x"
			r.Pins.CAPIVersion = "x"
			r.Stages[0].Passed = false
		}, []string{"manager ref", "pin capiVersion", `stage "cluster-build"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, c := goodRecord(), goodCheck()
			tc.mutate(&r, &c)
			err := Validate(r, c)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

// TestValidateDefaultClock checks that a nil Now falls back to the real
// clock.
func TestValidateDefaultClock(t *testing.T) {
	t.Parallel()
	r, c := goodRecord(), goodCheck()
	c.Now = nil
	r.CreatedAt = time.Now()
	if err := Validate(r, c); err != nil {
		t.Fatalf("fresh record with the real clock: %v", err)
	}
}

// fakeTB is a testing.TB that records Fatalf instead of stopping.
type fakeTB struct {
	testing.TB
	// fatal holds the formatted Fatalf messages.
	fatal []string
}

// Helper does nothing; the fake has no caller to attribute.
func (f *fakeTB) Helper() {}

// Fatalf records the formatted message in f and returns; format and args
// are formatted as fmt.Sprintf would.
func (f *fakeTB) Fatalf(format string, args ...any) {
	f.fatal = append(f.fatal, fmt.Sprintf(format, args...))
}

// TestRequire checks that Require passes a valid record and calls Fatalf
// with the remedy for a missing, malformed and invalid one.
func TestRequire(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := Path(dir)

	missing := &fakeTB{}
	Require(missing, path, goodCheck())
	if len(missing.fatal) != 1 || !strings.Contains(missing.fatal[0], "no green-light record") || !strings.Contains(missing.fatal[0], "make e2e-foundation") {
		t.Fatalf("missing record: %q", missing.fatal)
	}

	if err := os.WriteFile(path, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	broken := &fakeTB{}
	Require(broken, path, goodCheck())
	if len(broken.fatal) != 1 || !strings.Contains(broken.fatal[0], "unusable") || !strings.Contains(broken.fatal[0], "make e2e-foundation") {
		t.Fatalf("malformed record: %q", broken.fatal)
	}

	if err := Write(path, goodRecord()); err != nil {
		t.Fatal(err)
	}
	ok := &fakeTB{}
	Require(ok, path, goodCheck())
	if len(ok.fatal) != 0 {
		t.Fatalf("valid record: %q", ok.fatal)
	}

	c := goodCheck()
	c.ManagerRef = "mgr:2"
	stale := &fakeTB{}
	Require(stale, path, c)
	if len(stale.fatal) != 1 || !strings.Contains(stale.fatal[0], "manager ref") || !strings.Contains(stale.fatal[0], "run `make e2e-foundation` to green-light the cluster") {
		t.Fatalf("invalid record: %q", stale.fatal)
	}
}

// TestMaxAgeFromEnv checks the default, an override and bad values.
func TestMaxAgeFromEnv(t *testing.T) {
	t.Parallel()
	get := func(v string) func(string) string {
		return func(k string) string {
			if k != MaxAgeEnv {
				t.Errorf("read unexpected variable %q", k)
			}
			return v
		}
	}
	if d, err := MaxAgeFromEnv(get("")); err != nil || d != DefaultMaxAge {
		t.Fatalf("default: %v %v", d, err)
	}
	if d, err := MaxAgeFromEnv(get("6h")); err != nil || d != 6*time.Hour {
		t.Fatalf("override: %v %v", d, err)
	}
	for _, bad := range []string{"soon", "-1h", "0s"} {
		if _, err := MaxAgeFromEnv(get(bad)); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

// TestCheckFromState builds a Check from a real state.json.
func TestCheckFromState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := CheckFromState(dir, nil); err == nil || !strings.Contains(err.Error(), "current state") {
		t.Fatalf("no state.json: %v", err)
	}
	if err := env.WriteState(dir, &env.State{Cluster: "c", ManagerRef: "mgr:7"}); err != nil {
		t.Fatal(err)
	}
	exists := func(string) (bool, error) { return true, nil }
	c, err := CheckFromState(dir, exists)
	if err != nil {
		t.Fatal(err)
	}
	if c.ManagerRef != "mgr:7" || !c.Pins.Equal(env.CurrentPins()) || c.MaxAge != DefaultMaxAge || c.ClusterExists == nil {
		t.Fatalf("check: %+v", c)
	}
}
