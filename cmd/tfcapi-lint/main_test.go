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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/cmd/tfcapi-lint/app"
)

// update reports whether the golden files should be rewritten:
// UPDATE_SNAPSHOTS=1 (the house convention) or UPDATE_GOLDEN=1 (the
// task's).
func update() bool {
	return os.Getenv("UPDATE_SNAPSHOTS") == "1" || os.Getenv("UPDATE_GOLDEN") == "1"
}

// TestFixtures: every fixture's --json output equals its expected.json, and
// the exit code is 0 for good and 1 for bad ones under --strict.
func TestFixtures(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		dir, role string
		exit      int
	}{
		{"good/cluster", "cluster", app.ExitOK},
		{"good/machine", "machine", app.ExitOK},
		{"good/machine-tofu-only", "machine", app.ExitOK},
		{"good/machinepool", "machinepool", app.ExitOK},
		{"bad/missing-output", "machine", app.ExitFindings},
		{"bad/wrong-type", "cluster", app.ExitFindings},
		{"bad/has-backend", "machine", app.ExitFindings},
		{"bad/reserved-prefix", "machine", app.ExitFindings},
		{"bad/extra-input-no-default", "machine", app.ExitFindings},
		{"bad/tofu-shadow", "machine", app.ExitFindings},
		{"bad/no-tags", "cluster", app.ExitFindings},
	} {
		t.Run(tt.dir, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join("testdata", tt.dir)
			var stdout, stderr bytes.Buffer
			args := []string{"module", "--role", tt.role, "--strict", "--json", dir}
			if code := app.Run(context.Background(), args, &stdout, &stderr); code != tt.exit {
				t.Fatalf("exit %d, want %d; stderr: %s", code, tt.exit, stderr.String())
			}
			golden := filepath.Join(dir, "expected.json")
			if update() {
				if err := os.WriteFile(golden, stdout.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (UPDATE_SNAPSHOTS=1 writes it)", err)
			}
			if !bytes.Equal(stdout.Bytes(), want) {
				t.Errorf("output differs from %s:\n%s", golden, stdout.String())
			}
			// Good fixtures have no findings at all.
			var r struct {
				Findings []json.RawMessage `json:"findings"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(tt.dir, "good/") != (len(r.Findings) == 0) {
				t.Errorf("%d findings", len(r.Findings))
			}
		})
	}
}
