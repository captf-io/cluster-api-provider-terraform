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
	"compress/gzip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// restoreJSON is a fixture backup state document with one managed resource.
const restoreJSON = `{"version":4,"serial":5,"lineage":"l1","resources":[{"mode":"managed","type":"t","name":"a"}]}`

// gzipped returns s gzip-compressed, failing t if compression errors.
func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// writeChunks lays payload out in n chunks under config the way a
// projected Secret volume does: <config>/..<ts>/restore/<i>, reached
// through the restore -> ..data/restore symlink; it fails t on any
// filesystem error.
func writeChunks(t *testing.T, config string, payload []byte, n int) {
	t.Helper()
	dir := filepath.Join(config, "..2026_09_25", restoreChunkDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	size := (len(payload) + n - 1) / n
	for i := range n {
		part := payload[min(i*size, len(payload)):min((i+1)*size, len(payload))]
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(i)), part, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(filepath.Join(config, "..data")); os.IsNotExist(err) {
		if err := os.Symlink("..2026_09_25", filepath.Join(config, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("..data", restoreChunkDir), filepath.Join(config, restoreChunkDir)); err != nil {
		t.Fatal(err)
	}
}

// TestRunRestore: init, then state push -force of the reassembled,
// decompressed backup, then state list, whose managed addresses are the
// sanity check. The chunks never reach the root directory.
func TestRunRestore(t *testing.T) {
	t.Parallel()
	f := newFixture(t, OpRestore, "FAKE_STDOUT_STATE_LIST=data.t.x\nmodule.m[\"a.b\"].t.a\n")
	writeChunks(t, f.opts.ConfigDir, gzipped(t, restoreJSON), 2)
	f.opts.RestoreChunks, f.opts.RestoreResources = 2, 1
	r, code := Run(t.Context(), f.opts)
	if code != 0 || r.Error != nil {
		t.Fatalf("Run = %d, %+v", code, r.Error)
	}
	if got := stepNames(r); !slices.Equal(got, []string{StepInit, StepStatePush, StepStateList}) {
		t.Errorf("steps = %v", got)
	}
	if got := f.calls(t); !slices.Equal(got, []string{"VERSION", "INIT", "STATE_PUSH", "STATE_LIST"}) {
		t.Errorf("calls = %v", got)
	}
	raw, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	pushed := filepath.Join(f.opts.WorkDir, RestoreStateFile)
	if !strings.Contains(string(raw), "STATE_PUSH push -force -lock-timeout=90s "+pushed+"\n") {
		t.Errorf("state push call: %s", raw)
	}
	if got, err := os.ReadFile(pushed); err != nil || string(got) != restoreJSON {
		t.Errorf("pushed state = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(f.opts.WorkDir, "root", restoreChunkDir)); !os.IsNotExist(err) {
		t.Errorf("the chunks were copied into the root: %v", err)
	}
}

// TestRunRestoreFailures: a bad backup fails before any runtime step, and
// a state list without managed resources fails a backup that has some.
func TestRunRestoreFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		payload   []byte
		chunks    int
		resources int
		env       []string
		step      string
		calls     []string
	}{
		{name: "not gzip", payload: []byte("plain"), chunks: 1, step: StepPrepare, calls: []string{}},
		{name: "not JSON", payload: gzipped(t, "{"), chunks: 1, step: StepPrepare, calls: []string{}},
		{name: "chunk missing", payload: gzipped(t, restoreJSON), chunks: 1, step: StepPrepare, calls: []string{}},
		{name: "push fails", payload: gzipped(t, restoreJSON), chunks: 1, resources: 1, env: []string{"FAKE_EXIT_STATE_PUSH=1"},
			step: StepStatePush, calls: []string{"VERSION", "INIT", "STATE_PUSH"}},
		{name: "nothing listed", payload: gzipped(t, restoreJSON), chunks: 1, resources: 1, env: []string{"FAKE_STDOUT_STATE_LIST=data.t.x\n"},
			step: StepStateList, calls: []string{"VERSION", "INIT", "STATE_PUSH", "STATE_LIST"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, OpRestore, tc.env...)
			writeChunks(t, f.opts.ConfigDir, tc.payload, tc.chunks)
			f.opts.RestoreChunks, f.opts.RestoreResources = tc.chunks, tc.resources
			if tc.name == "chunk missing" {
				f.opts.RestoreChunks = 2
			}
			r, code := Run(t.Context(), f.opts)
			if code == 0 || r.Error == nil || r.Error.Step == nil || *r.Error.Step != tc.step {
				t.Fatalf("Run = %d, %+v; want a failure in %s", code, r.Error, tc.step)
			}
			var calls []string
			if _, err := os.Stat(f.log); err == nil {
				calls = f.calls(t)
			}
			// version runs before the preparation only when it succeeds.
			calls = slices.DeleteFunc(calls, func(c string) bool { return c == "VERSION" && len(tc.calls) == 0 })
			if len(calls) != len(tc.calls) || (len(calls) > 0 && !slices.Equal(calls, tc.calls)) {
				t.Errorf("calls = %v, want %v", calls, tc.calls)
			}
		})
	}
}

// TestAssembleRestoreLimits checks that AssembleRestore rejects a chunk
// count out of range and stops a gzip bomb at the decompressed-size cap,
// exercised through assembleRestore with a small cap.
func TestAssembleRestoreLimits(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, maxRestoreChunks + 1} {
		if err := AssembleRestore(t.TempDir(), t.TempDir(), n); err == nil {
			t.Errorf("%d chunks accepted", n)
		}
	}
	const limit = 1 << 10
	// State exactly at the cap is accepted.
	config := t.TempDir()
	writeChunks(t, config, gzipped(t, `"`+strings.Repeat("x", limit-2)+`"`), 1)
	if err := assembleRestore(config, t.TempDir(), 1, limit); err != nil {
		t.Errorf("state at the cap: %v", err)
	}
	// A gzip bomb stops at the cap.
	config = t.TempDir()
	writeChunks(t, config, gzipped(t, `"`+strings.Repeat("x", limit)+`"`), 1)
	if err := assembleRestore(config, t.TempDir(), 1, limit); err == nil || !strings.Contains(err.Error(), "decompressed") {
		t.Errorf("oversized state: %v", err)
	}
}

// TestManagedAddresses checks that ManagedAddresses counts managed
// resource instances, including ones under nested and indexed modules,
// while excluding data sources.
func TestManagedAddresses(t *testing.T) {
	t.Parallel()
	out := strings.Join([]string{
		"aws_instance.a",
		"aws_instance.b[0]",
		"data.aws_ami.x",
		"module.role.aws_vpc.main",
		"module.role.data.aws_region.current",
		`module.m["a.b"].data.x.y`,
		`module.m["a.b"].t.z["k"]`,
		`module.outer[1].module.inner.t.w`,
		"",
	}, "\n")
	if n := ManagedAddresses([]byte(out)); n != 5 {
		t.Errorf("ManagedAddresses = %d, want 5", n)
	}
}

// TestStepsRestore checks Steps' restore sequence (init, force-unlock,
// state push, state list) and that its own step names are metric labels.
func TestStepsRestore(t *testing.T) {
	t.Parallel()
	steps, err := Steps(OpRestore, PlanOptions{BackendConfig: []string{"namespace=ns"}, LockTimeout: time.Minute, WorkDir: "/captf/work", ForceUnlockID: "l1"})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"init", "-input=false", "-no-color", "-lock-timeout=60s", "-backend-config=namespace=ns"},
		{"force-unlock", "-force", "l1"},
		{"state", "push", "-force", "-lock-timeout=60s", "/captf/work/restore.tfstate"},
		{"state", "list"},
	}
	if len(steps) != len(want) || !steps[3].Capture {
		t.Fatalf("steps = %+v", steps)
	}
	for i, s := range steps {
		if !slices.Equal(s.Args, want[i]) {
			t.Errorf("step %d = %v, want %v", i, s.Args, want[i])
		}
	}
	if StepLabel(StepStatePush) != StepStatePush || StepLabel(StepStateList) != StepStateList {
		t.Error("the restore steps are not metric step labels")
	}
}
