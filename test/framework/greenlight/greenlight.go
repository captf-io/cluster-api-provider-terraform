/*
Copyright 2026 The cluster-api-provider-terraform Authors.

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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/env"
)

const (
	// RecordVersion is the layout version Write stamps and Validate
	// accepts.
	RecordVersion = 1
	// DefaultMaxAge is how old a record may be when neither Check.MaxAge
	// nor the environment says otherwise.
	DefaultMaxAge = 24 * time.Hour
	// MaxAgeEnv is the environment variable that overrides the maximum
	// age, as a Go duration such as "6h".
	MaxAgeEnv = "CAPTF_E2E_GREENLIGHT_MAX_AGE"
	// fileName is the record's file name inside the work directory.
	fileName = "greenlight.json"
	// remedy is the instruction every Require failure ends with.
	remedy = "run `make e2e-foundation` to green-light the cluster"
)

// Stage is one stage of the foundation suite.
type Stage struct {
	// Name is the stage name.
	Name string `json:"name"`
	// Duration is how long the stage took.
	Duration time.Duration `json:"duration"`
	// Passed is true when the stage passed.
	Passed bool `json:"passed"`
}

// Record is what the foundation suite writes when it passes.
type Record struct {
	// Version is the file layout version (RecordVersion).
	Version int `json:"version"`
	// Cluster is the kind cluster name.
	Cluster string `json:"cluster"`
	// TreeID is the working-tree identity the manager image was built
	// from.
	TreeID string `json:"treeID"`
	// ManagerRef is the manager image reference the suite ran.
	ManagerRef string `json:"managerRef"`
	// KubernetesVersion is the cluster's Kubernetes version.
	KubernetesVersion string `json:"kubernetesVersion"`
	// Pins are the versions the environment was built from.
	Pins env.Pins `json:"pins"`
	// Stages are the suite's stages in order.
	Stages []Stage `json:"stages"`
	// StabilityWindow is how long the stability stage watched.
	StabilityWindow time.Duration `json:"stabilityWindow"`
	// CreatedAt is when the record was written.
	CreatedAt time.Time `json:"createdAt"`
}

// Path returns where the record lives for the work directory workDir:
// workDir + "/greenlight.json".
func Path(workDir string) string {
	return workDir + "/" + fileName
}

// Write encodes r as indented JSON and writes it to path atomically, via a
// temporary file in the same directory renamed over path. It stamps
// Version with RecordVersion in the file. It returns a wrapped error when
// encoding or any file step fails, leaving no temporary file behind.
func Write(path string, r Record) error {
	r.Version = RecordVersion
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("greenlight: encode record: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("greenlight: write record: %w", err)
	}
	name := tmp.Name()
	_, err = tmp.Write(append(data, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, 0o644)
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("greenlight: write record %s: %w", path, err)
	}
	return nil
}

// Read decodes the record at path. It returns a wrapped error (matching
// os.ErrNotExist when the file is missing) when the file is unreadable or
// malformed. It does not check the version; Validate does.
func Read(path string) (Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, fmt.Errorf("greenlight: read record: %w", err)
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, fmt.Errorf("greenlight: decode record %s: %w", path, err)
	}
	return r, nil
}

// Check holds what Require compares a record against: what is true now.
type Check struct {
	// Pins are the current pins (env.CurrentPins()).
	Pins env.Pins
	// ManagerRef is the current manager image reference, from state.json.
	ManagerRef string
	// ClusterExists reports whether the record's cluster still exists. A
	// nil function fails the check.
	ClusterExists func(cluster string) (bool, error)
	// MaxAge is how old the record may be; zero means DefaultMaxAge.
	MaxAge time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// MaxAgeFromEnv returns the maximum record age: the duration in
// CAPTF_E2E_GREENLIGHT_MAX_AGE as read by getenv, or DefaultMaxAge when it
// is unset. It returns an error when the value is not a positive Go
// duration.
func MaxAgeFromEnv(getenv func(string) string) (time.Duration, error) {
	v := getenv(MaxAgeEnv)
	if v == "" {
		return DefaultMaxAge, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("greenlight: %s=%q is not a positive duration", MaxAgeEnv, v)
	}
	return d, nil
}

// CheckFromState builds a Check for the environment in workDir: the pins
// of this build, the manager reference in workDir/state.json, MaxAge from
// the environment (see MaxAgeFromEnv, read with os.Getenv) and exists as
// the cluster test. It returns an error when state.json cannot be read or
// the age variable is invalid.
func CheckFromState(workDir string, exists func(cluster string) (bool, error)) (Check, error) {
	st, err := env.ReadState(workDir)
	if err != nil {
		return Check{}, fmt.Errorf("greenlight: current state: %w", err)
	}
	age, err := MaxAgeFromEnv(os.Getenv)
	if err != nil {
		return Check{}, err
	}
	return Check{Pins: env.CurrentPins(), ManagerRef: st.ManagerRef, ClusterExists: exists, MaxAge: age}, nil
}

// Validate checks r against c without side effects. It returns nil when
// the record version is known, the cluster exists, the pins and manager
// reference equal the current ones, the record is younger than the maximum
// age and every stage passed; otherwise one error listing every failure.
func Validate(r Record, c Check) error {
	var out []string
	if r.Version != RecordVersion {
		out = append(out, fmt.Sprintf("record version %d, want %d", r.Version, RecordVersion))
	}
	if c.ClusterExists == nil {
		out = append(out, "no cluster check configured")
	} else if ok, err := c.ClusterExists(r.Cluster); err != nil {
		out = append(out, fmt.Sprintf("cannot tell whether cluster %q exists: %v", r.Cluster, err))
	} else if !ok {
		out = append(out, fmt.Sprintf("cluster %q does not exist", r.Cluster))
	}
	out = append(out, diffPins(r.Pins, c.Pins)...)
	if r.ManagerRef != c.ManagerRef {
		out = append(out, fmt.Sprintf("manager ref %q, current %q", r.ManagerRef, c.ManagerRef))
	}
	maxAge := c.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	if age := now().Sub(r.CreatedAt); age > maxAge {
		out = append(out, fmt.Sprintf("record is %s old, max %s", age.Round(time.Second), maxAge))
	}
	if len(r.Stages) == 0 {
		out = append(out, "record has no stages")
	}
	for _, s := range r.Stages {
		if !s.Passed {
			out = append(out, fmt.Sprintf("stage %q did not pass", s.Name))
		}
	}
	if len(out) > 0 {
		return fmt.Errorf("greenlight: record not valid:\n  %s", strings.Join(out, "\n  "))
	}
	return nil
}

// diffPins returns one message per field of the recorded pins rec that
// differs from the current pins cur, nil when equal.
func diffPins(rec, cur env.Pins) []string {
	var out []string
	field := func(name, a, b string) {
		if a != b {
			out = append(out, fmt.Sprintf("pin %s: record %q, current %q", name, a, b))
		}
	}
	field("kindVersion", rec.KindVersion, cur.KindVersion)
	field("kindNodeImage", rec.KindNodeImage, cur.KindNodeImage)
	field("capiVersion", rec.CAPIVersion, cur.CAPIVersion)
	field("certManagerVersion", rec.CertManagerVersion, cur.CertManagerVersion)
	field("captfVersion", rec.CAPTFVersion, cur.CAPTFVersion)
	if !slices.Equal(rec.NoopImages, cur.NoopImages) {
		out = append(out, fmt.Sprintf("pin noopImages: record %v, current %v", rec.NoopImages, cur.NoopImages))
	}
	return out
}

// Require reads the record at path and validates it against c, calling
// t.Fatalf with what failed and "run `make e2e-foundation` to green-light
// the cluster" when the record is missing, unreadable or invalid. It
// never skips: a missing green light fails the test.
func Require(t testing.TB, path string, c Check) {
	t.Helper()
	r, err := Read(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Fatalf("no green-light record at %s: %v; %s", path, err, remedy)
			return
		}
		t.Fatalf("green-light record unusable: %v; %s", err, remedy)
		return
	}
	if err := Validate(r, c); err != nil {
		t.Fatalf("cluster is not green-lit: %v\n%s", err, remedy)
	}
}
