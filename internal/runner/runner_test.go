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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"
)

// TestMain silences klog's global fallback logger for m's tests: a
// Run(context.Background(), ...) call without a logger in its context would
// otherwise print every diagnostic line to the test binary's own stderr.
// Tests that assert on log lines attach their own logger with logContext.
// It reports m's exit code to the process.
func TestMain(m *testing.M) {
	klog.SetLogger(textlogger.NewLogger(textlogger.NewConfig(textlogger.Output(io.Discard))))
	os.Exit(m.Run())
}

// TestHelperProcess is the fake runtime: the test binary re-executes itself
// with GO_FAKE_RUNTIME=1. The subcommand's behavior comes from the
// environment: FAKE_EXIT_<KEY>, FAKE_STDOUT_<KEY>, FAKE_STDERR_<KEY>, where
// KEY is the upper-cased subcommand (APPLY_REFRESH_ONLY for
// `apply -refresh-only`). Every invocation is appended to FAKE_LOG.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_FAKE_RUNTIME") != "1" {
		t.Skip("helper process")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	key := strings.ToUpper(strings.ReplaceAll(args[0], "-", "_"))
	if args[0] == "apply" && slices.Contains(args, "-refresh-only") {
		key = "APPLY_REFRESH_ONLY"
	}
	if args[0] == "state" && len(args) > 1 {
		key = "STATE_" + strings.ToUpper(args[1])
	}
	if f, err := os.OpenFile(os.Getenv("FAKE_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintf(f, "%s %s\n", key, strings.Join(args[1:], " "))
		_ = f.Close()
	}
	fake := fakeBehavior()
	fmt.Fprint(os.Stdout, fake["FAKE_STDOUT_"+key])
	fmt.Fprint(os.Stderr, fake["FAKE_STDERR_"+key])
	code, _ := strconv.Atoi(fake["FAKE_EXIT_"+key])
	os.Exit(code)
}

// fakeBehavior returns the FAKE_* settings of the fake runtime, read from
// the JSON file named by FAKE_CONFIG: newFixture keeps them out of the
// step environment, because the runner redacts every value in it.
func fakeBehavior() map[string]string {
	fake := map[string]string{}
	if raw, err := os.ReadFile(os.Getenv("FAKE_CONFIG")); err == nil {
		_ = json.Unmarshal(raw, &fake)
	}
	return fake
}

// logContext returns, for t, a context carrying a klog.Logger that writes
// to the returned buffer, for tests that assert on the runner's structured
// log lines (which replaced its old hand-rolled stderr lines).
func logContext(t *testing.T) (context.Context, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	cfg := textlogger.NewConfig(textlogger.Output(buf))
	return klog.NewContext(context.Background(), textlogger.NewLogger(cfg)), buf
}

// fixture is a runnable Options against the fake runtime (TestHelperProcess)
// and the paths to read its log and captured stdout back from.
type fixture struct {
	opts   Options
	log    string
	stdout *bytes.Buffer
}

// newFixture returns a fixture for op, with a module, a rendered config
// directory (kubelet-style Secret mount, with bootstrap_data set) and a
// work directory and a plan key file (testPlanKey) under t.TempDir(), and env appended to the fake runtime's
// environment, except its FAKE_* settings, which go to the file FAKE_CONFIG names. It fails t on any setup error.
func newFixture(t *testing.T, op string, env ...string) fixture {
	t.Helper()
	dir := t.TempDir()
	module := filepath.Join(dir, "module")
	config := filepath.Join(dir, "config")
	work := filepath.Join(dir, "work")
	for _, d := range []string{module, config, filepath.Join(config, "..2026_09_25"), work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(module, "main.tf"):                               "",
		filepath.Join(config, "..2026_09_25", "main.tf.json"):          `{}`,
		filepath.Join(config, "..2026_09_25", "terraform.tfvars.json"): `{"bootstrap_data":"c2VjcmV0"}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// A kubelet-style Secret mount: ..data -> ..<ts>, files -> ..data/<file>.
	if err := os.Symlink("..2026_09_25", filepath.Join(config, "..data")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	for _, f := range []string{"main.tf.json", "terraform.tfvars.json"} {
		if err := os.Symlink(filepath.Join("..data", f), filepath.Join(config, f)); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}
	key := filepath.Join(dir, "plan-key")
	if err := os.WriteFile(key, []byte(testPlanKey), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	log := filepath.Join(dir, "fake.log")
	fake := map[string]string{}
	var jobEnv []string
	for _, kv := range env {
		if name, value, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "FAKE_") {
			fake[name] = value
		} else {
			jobEnv = append(jobEnv, kv)
		}
	}
	rawFake, err := json.Marshal(fake)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fakeConfig := filepath.Join(dir, "fake.json")
	if err := os.WriteFile(fakeConfig, rawFake, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	stdout := &bytes.Buffer{}
	return fixture{
		opts: Options{
			Op: op, Bin: []string{os.Args[0], "-test.run=^TestHelperProcess$", "--"}, Image: "r:v1",
			ModuleDir: module, ProvidersDir: filepath.Join(dir, "providers"), WorkDir: work, ConfigDir: config,
			BackendConfig: []string{"secret_suffix=abc-m", `labels={"a"="b"}`}, LockTimeout: 90 * time.Second, PlanKeyFile: key,
			Env:    append([]string{"GO_FAKE_RUNTIME=1", "FAKE_LOG=" + log, "FAKE_CONFIG=" + fakeConfig, "PATH=" + os.Getenv("PATH")}, jobEnv...),
			Stdout: stdout, Stderr: &bytes.Buffer{},
		},
		log: log, stdout: stdout,
	}
}

// calls reads f's fake log and returns the subcommand key of each recorded
// invocation, in order; it fails t if the log cannot be read.
func (f fixture) calls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatalf("read fake log: %v", err)
	}
	var keys []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		keys = append(keys, strings.Fields(line)[0])
	}
	return keys
}

// stepNames returns the Name of each of r's Steps, in order.
func stepNames(r Result) []string {
	var out []string
	for _, s := range r.Steps {
		out = append(out, s.Name)
	}
	return out
}

// TestRunSequences checks Run's resulting steps, exit code, calls and
// result across apply, force-unlock and related cases.
func TestRunSequences(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		op    string
		env   []string
		force bool
		code  int
		steps []string
		calls []string
		check func(t *testing.T, r Result, f fixture)
	}{
		{name: "apply", op: OpApply, steps: []string{"init", "validate", "apply"}, calls: []string{"VERSION", "INIT", "VALIDATE", "APPLY"},
			env: []string{`FAKE_STDOUT_VERSION={"terraform_version":"1.12.6"}`},
			check: func(t *testing.T, r Result, _ fixture) {
				if r.Error != nil || r.Runtime.Version != "1.12.6" || r.Image.Ref != "r:v1" || r.Image.ProvidersMirror {
					t.Errorf("result = %+v", r)
				}
			}},
		{name: "force-unlock after init", op: OpApply, force: true, steps: []string{"init", "force-unlock", "validate", "apply"}, calls: []string{"VERSION", "INIT", "FORCE_UNLOCK", "VALIDATE", "APPLY"}},
		// A retried pod finds the lock released, or held under
		// another ID; both are the stale lock gone, not a failure.
		{name: "force-unlock of a released lock", op: OpApply, force: true,
			env:   []string{"FAKE_EXIT_FORCE_UNLOCK=1", "FAKE_STDERR_FORCE_UNLOCK=Failed to unlock state: state is already unlocked"},
			steps: []string{"init", "force-unlock", "validate", "apply"}, calls: []string{"VERSION", "INIT", "FORCE_UNLOCK", "VALIDATE", "APPLY"}},
		{name: "force-unlock of a relocked state", op: OpApply, force: true,
			env:   []string{"FAKE_EXIT_FORCE_UNLOCK=1", `FAKE_STDERR_FORCE_UNLOCK=Failed to unlock state: lock id "lock-1" does not match existing lock`},
			steps: []string{"init", "force-unlock", "validate", "apply"}, calls: []string{"VERSION", "INIT", "FORCE_UNLOCK", "VALIDATE", "APPLY"}},
		{name: "force-unlock fails", op: OpApply, force: true, code: 1,
			env:   []string{"FAKE_EXIT_FORCE_UNLOCK=1", "FAKE_STDERR_FORCE_UNLOCK=Failed to unlock state: connection refused"},
			steps: []string{"init", "force-unlock"}, calls: []string{"VERSION", "INIT", "FORCE_UNLOCK"}},
		{name: "destroy", op: OpDestroy, steps: []string{"init", "destroy"}, calls: []string{"VERSION", "INIT", "DESTROY"}},
		{name: "refresh", op: OpRefresh, steps: []string{"init", "apply-refresh-only"}, calls: []string{"VERSION", "INIT", "APPLY_REFRESH_ONLY"}},
		{name: "drift without changes", op: OpDrift, steps: []string{"init", "apply-refresh-only", "plan"}, calls: []string{"VERSION", "INIT", "APPLY_REFRESH_ONLY", "PLAN"},
			check: func(t *testing.T, r Result, _ fixture) {
				if r.Drift == nil || r.Drift.Detected || r.Error != nil {
					t.Errorf("drift = %+v", r.Drift)
				}
			}},
		{name: "drift with changes", op: OpDrift, code: 0,
			env: []string{"FAKE_EXIT_PLAN=2",
				`FAKE_STDOUT_SHOW={"variables":{"bootstrap_data":{"value":"c2VjcmV0"}},"resource_changes":[` +
					`{"address":"module.role.a","change":{"actions":["update"]}},` +
					`{"address":"module.role.b","change":{"actions":["delete","create"]}},` +
					`{"address":"module.role.c","change":{"actions":["no-op"]}}]}`},
			steps: []string{"init", "apply-refresh-only", "plan", "show-json"}, calls: []string{"VERSION", "INIT", "APPLY_REFRESH_ONLY", "PLAN", "SHOW"},
			check: func(t *testing.T, r Result, f fixture) {
				d := r.Drift
				if d == nil || !d.Detected || d.Create != 0 || d.Update != 1 || d.Replace != 1 || d.Delete != 0 || !slices.Equal(d.Resources, []string{"module.role.a", "module.role.b"}) {
					t.Errorf("drift = %+v", d)
				}
				if r.Steps[2].Exit != 2 {
					t.Errorf("plan exit = %d, want 2 recorded", r.Steps[2].Exit)
				}
				// The plan JSON carries inputs: it never reaches the log.
				if strings.Contains(f.stdout.String(), "c2VjcmV0") {
					t.Error("show -json output leaked into the log")
				}
			}},
		// A plan exits 2 for output-only changes too; that is not drift.
		{name: "drift with only output changes", op: OpDrift, code: 0,
			env:   []string{"FAKE_EXIT_PLAN=2", `FAKE_STDOUT_SHOW={"output_changes":{"x":{"actions":["update"]}},"resource_changes":[]}`},
			steps: []string{"init", "apply-refresh-only", "plan", "show-json"}, calls: []string{"VERSION", "INIT", "APPLY_REFRESH_ONLY", "PLAN", "SHOW"},
			check: func(t *testing.T, r Result, _ fixture) {
				d := r.Drift
				if d == nil || d.Detected || d.Create != 0 || d.Update != 0 || d.Replace != 0 || d.Delete != 0 {
					t.Errorf("drift = %+v, want no drift detected", d)
				}
			}},
		{name: "validate fails with JSON diagnostics on stdout", op: OpApply, code: 1,
			env:   []string{"FAKE_EXIT_VALIDATE=1", `FAKE_STDOUT_VALIDATE={"diagnostics":[{"severity":"error","summary":"Unsupported attribute","detail":"This object has no argument, nested block, or exported attribute named \"health\".\nmore context"}]}`},
			steps: []string{"init", "validate"}, calls: []string{"VERSION", "INIT", "VALIDATE"},
			check: func(t *testing.T, r Result, f fixture) {
				if r.Error == nil || r.Error.Kind != ErrorKindStep || *r.Error.Step != "validate" ||
					r.Error.Tail != `Error: Unsupported attribute: This object has no argument, nested block, or exported attribute named "health".` {
					t.Errorf("error = %+v", r.Error)
				}
				// validate's JSON carries no input values: it reaches the log.
				if !strings.Contains(f.stdout.String(), "Unsupported attribute") {
					t.Error("validate diagnostics did not reach the log")
				}
			}},
		{name: "validate fails with only provider noise on stderr", op: OpApply, code: 1,
			env:   []string{"FAKE_EXIT_VALIDATE=1", "FAKE_STDERR_VALIDATE=2026-09-27T00:00:00.000Z [DEBUG] provider: some noise\nmore noise\nError: Unsupported attribute\n\n  on main.tf.json line 3:\nmore trailing context\n"},
			steps: []string{"init", "validate"}, calls: []string{"VERSION", "INIT", "VALIDATE"},
			check: func(t *testing.T, r Result, _ fixture) {
				if r.Error == nil || r.Error.Kind != ErrorKindStep || *r.Error.Step != "validate" || r.Error.Tail != "Error: Unsupported attribute" {
					t.Errorf("error = %+v, want only the Error: line", r.Error)
				}
			}},
		// A non-validate step's stderr is the primary source, not a fallback:
		// provider debug noise around the diagnostic yields only its
		// "Error: " line.
		{name: "apply fails with provider noise on stderr", op: OpApply, code: 1,
			env:   []string{"FAKE_EXIT_APPLY=1", "FAKE_STDERR_APPLY=2026-09-27T00:00:00.000Z [DEBUG] provider.aws: request: {\"AccessKeyId\":\"noise\"}\nmore request/response noise\nError: creating instance: InvalidParameterValue\n\n  with aws_instance.example,\n  on main.tf line 12, in resource \"aws_instance\" \"example\":\n  12: resource \"aws_instance\" \"example\" {\n"},
			steps: []string{"init", "validate", "apply"}, calls: []string{"VERSION", "INIT", "VALIDATE", "APPLY"},
			check: func(t *testing.T, r Result, _ fixture) {
				if r.Error == nil || r.Error.Kind != ErrorKindStep || *r.Error.Step != "apply" || r.Error.Tail != "Error: creating instance: InvalidParameterValue" {
					t.Errorf("error = %+v, want only the Error: line", r.Error)
				}
			}},
		{name: "plan error in drift", op: OpDrift, code: 1, env: []string{"FAKE_EXIT_PLAN=1"}, steps: []string{"init", "apply-refresh-only", "plan"}, calls: []string{"VERSION", "INIT", "APPLY_REFRESH_ONLY", "PLAN"}},
		// A failing step's own code stays in its Step; the process exits
		// ExitFailure, so a runtime panic (exit 2) never reads as
		// ExitUsage.
		{name: "a step's exit code stays in its step", op: OpDestroy, code: ExitFailure, env: []string{"FAKE_EXIT_DESTROY=3"}, steps: []string{"init", "destroy"}, calls: []string{"VERSION", "INIT", "DESTROY"},
			check: func(t *testing.T, r Result, _ fixture) {
				if r.Steps[1].Exit != 3 {
					t.Errorf("destroy step exit = %d, want 3", r.Steps[1].Exit)
				}
			}},
		{name: "a runtime panic is a failure, not a usage error", op: OpApply, code: ExitFailure, env: []string{"FAKE_EXIT_APPLY=2"}, steps: []string{"init", "validate", "apply"}, calls: []string{"VERSION", "INIT", "VALIDATE", "APPLY"},
			check: func(t *testing.T, r Result, _ fixture) {
				if r.Steps[2].Exit != 2 || r.Error == nil || r.Error.Kind != ErrorKindStep {
					t.Errorf("apply step = %+v, error %+v", r.Steps[2], r.Error)
				}
			}},
		{name: "bad plan JSON", op: OpDrift, code: 1, env: []string{"FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW=not json"}, steps: []string{"init", "apply-refresh-only", "plan", "show-json"}, calls: []string{"VERSION", "INIT", "APPLY_REFRESH_ONLY", "PLAN", "SHOW"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, c.op, c.env...)
			if c.force {
				f.opts.ForceUnlockID = "lock-1"
			}
			r, code := Run(context.Background(), f.opts)
			if code != c.code {
				t.Errorf("exit code = %d, want %d (%+v)", code, c.code, r.Error)
			}
			if got := stepNames(r); !slices.Equal(got, c.steps) {
				t.Errorf("steps = %v, want %v", got, c.steps)
			}
			if got := f.calls(t); !slices.Equal(got, c.calls) {
				t.Errorf("calls = %v, want %v", got, c.calls)
			}
			if c.check != nil {
				c.check(t, r, f)
			}
			// The rendered root was copied; the Secret mount's dot entries were not.
			root := filepath.Join(f.opts.WorkDir, "root")
			entries, _ := os.ReadDir(root)
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			if !slices.Equal(names, []string{"main.tf.json", "terraform.tfvars.json"}) {
				t.Errorf("root = %v", names)
			}
		})
	}
}

// destructivePlan is a `show -json` plan with a replace, a
// create-before-destroy replace, a delete, an update and an input value
// that must never reach the log or the result.
const destructivePlan = `{"variables":{"bootstrap_data":{"value":"c2VjcmV0"}},"resource_changes":[` +
	`{"address":"module.role.lb","change":{"actions":["delete","create"],"after":{"name":"c2VjcmV0"}}},` +
	`{"address":"module.role.net","change":{"actions":["create","delete"]}},` +
	`{"address":"module.role.old","change":{"actions":["delete"]}},` +
	`{"address":"module.role.tags","change":{"actions":["update"]}}]}`

// TestRunGuardedApply: a cluster apply (--guard-deletes) plans to a file,
// shows it and applies that saved plan, unless the plan deletes or
// replaces something and the approval is not this run's inputs hash.
func TestRunGuardedApply(t *testing.T) {
	t.Parallel()
	const h = "h1:current"
	guarded := []string{"VERSION", "INIT", "VALIDATE", "PLAN", "SHOW", "APPLY"}
	blocked := []string{"VERSION", "INIT", "VALIDATE", "PLAN", "SHOW"}
	cases := []struct {
		name    string
		env     []string
		allow   string
		inputs  string
		code    int
		calls   []string
		blocked bool
	}{
		{name: "no changes skips show and the apply", calls: []string{"VERSION", "INIT", "VALIDATE", "PLAN"}},
		{name: "only creates and updates apply", env: []string{"FAKE_EXIT_PLAN=2",
			`FAKE_STDOUT_SHOW={"resource_changes":[{"address":"a","change":{"actions":["create"]}},{"address":"b","change":{"actions":["update"]}},{"address":"c","change":{"actions":["no-op"]}}]}`},
			calls: guarded},
		{name: "deletes without approval block", env: []string{"FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW=" + destructivePlan}, code: 1, calls: blocked, blocked: true},
		{name: "deletes approved for another hash block", env: []string{"FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW=" + destructivePlan}, allow: "h1:previous", code: 1, calls: blocked, blocked: true},
		{name: "an empty hash approves nothing", env: []string{"FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW=" + destructivePlan}, inputs: "-", code: 1, calls: blocked, blocked: true},
		{name: "deletes approved for this hash apply", env: []string{"FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW=" + destructivePlan}, allow: h, calls: guarded},
		{name: "a replace alone blocks", env: []string{"FAKE_EXIT_PLAN=2", `FAKE_STDOUT_SHOW={"resource_changes":[{"address":"module.role.lb","change":{"actions":["create","delete"]}}]}`},
			code: 1, calls: blocked, blocked: true},
		{name: "unparseable plan fails as a step", env: []string{"FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW=not json"}, code: 1, calls: blocked},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, OpApply, c.env...)
			stderr := &bytes.Buffer{}
			f.opts.Stderr = stderr
			f.opts.GuardDeletes, f.opts.InputsHash, f.opts.AllowDeletesHash = true, h, c.allow
			if c.inputs == "-" {
				f.opts.InputsHash = ""
			}
			ctx, log := logContext(t)
			r, code := Run(ctx, f.opts)
			if code != c.code {
				t.Errorf("exit code = %d, want %d (%+v)", code, c.code, r.Error)
			}
			if got := f.calls(t); !slices.Equal(got, c.calls) {
				t.Errorf("calls = %v, want %v", got, c.calls)
			}
			if r.Drift != nil {
				t.Errorf("an apply result carries drift %+v", r.Drift)
			}
			if got := r.Error != nil && r.Error.Kind == ErrorKindBlocked; got != c.blocked {
				t.Fatalf("blocked = %v (%+v), want %v", got, r.Error, c.blocked)
			}
			for _, out := range []string{stderr.String(), f.stdout.String(), log.String(), string(Encode(r))} {
				if strings.Contains(out, "c2VjcmV0") {
					t.Errorf("a plan value leaked: %s", out)
				}
			}
			if !c.blocked {
				return
			}
			if r.Error.Step != nil {
				t.Errorf("blocked step = %q, want none", *r.Error.Step)
			}
			if !strings.HasPrefix(r.Error.Tail, blockedPrefix) || strings.Contains(r.Error.Tail, "module.role.tags") {
				t.Errorf("summary = %q", r.Error.Tail)
			}
			if !strings.Contains(log.String(), `"Apply blocked"`) {
				t.Errorf("the log does not say the apply was blocked: %s", log)
			}
		})
	}
	// The summary names each destructive resource and its action.
	f := newFixture(t, OpApply, "FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW="+destructivePlan)
	f.opts.GuardDeletes, f.opts.InputsHash = true, h
	r, _ := Run(context.Background(), f.opts)
	want := blockedPrefix + "3 resource(s): module.role.lb (replace), module.role.net (replace), module.role.old (delete)"
	if r.Error == nil || r.Error.Tail != want {
		t.Errorf("summary = %+v, want %q", r.Error, want)
	}
}

// TestRunNoChangeApply: a guarded apply (--guard-deletes) or an approved
// empty plan (--expect-plan) whose plan exits 0 ends after the plan: no
// show, no apply step, exit 0, zero changes and a log line saying so. An
// unguarded apply has no plan step and still applies.
func TestRunNoChangeApply(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name          string
		guard, expect bool
	}{
		{"guarded", true, false},
		{"approved empty plan", true, true},
		{"expect-plan alone", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, OpApply)
			f.opts.GuardDeletes, f.opts.InputsHash = c.guard, "h1:x"
			if c.expect {
				f.opts.ExpectPlan = EmptyPlanHash
			}
			ctx, log := logContext(t)
			r, code := Run(ctx, f.opts)
			if code != 0 || r.Error != nil {
				t.Fatalf("run = %d %+v", code, r.Error)
			}
			if got := stepNames(r); !slices.Equal(got, []string{"init", "validate", "plan"}) {
				t.Errorf("steps = %v, want init, validate, plan", got)
			}
			if got := f.calls(t); !slices.Equal(got, []string{"VERSION", "INIT", "VALIDATE", "PLAN"}) {
				t.Errorf("calls = %v", got)
			}
			if r.Changes == nil || *r.Changes != (Changes{}) || r.Plan != nil || r.Drift != nil {
				t.Errorf("changes %+v, plan %+v, drift %+v; want zero changes only", r.Changes, r.Plan, r.Drift)
			}
			if !strings.Contains(log.String(), "Plan has no changes; skipping the apply step") {
				t.Errorf("the log does not say the apply was skipped: %s", log)
			}
			var back Result
			if err := json.Unmarshal(Encode(r), &back); err != nil || back.Changes == nil || len(back.Steps) != 3 {
				t.Errorf("encoded result = %+v, %v", back, err)
			}
		})
	}
}

// TestUnguardedApplyUnchanged: without --guard-deletes (machines) apply
// runs as before, whatever the plan would delete.
func TestUnguardedApplyUnchanged(t *testing.T) {
	t.Parallel()
	f := newFixture(t, OpApply, "FAKE_STDOUT_SHOW="+destructivePlan)
	f.opts.InputsHash = "h1:x"
	if r, code := Run(context.Background(), f.opts); code != 0 || r.Error != nil {
		t.Fatalf("run = %d %+v", code, r.Error)
	}
	if got := f.calls(t); !slices.Equal(got, []string{"VERSION", "INIT", "VALIDATE", "APPLY"}) {
		t.Errorf("calls = %v", got)
	}
}

// TestBlockedSummaryCap checks that blockedSummary stays within MaxSummary
// and valid UTF-8, cutting a huge address on a rune boundary, and that a
// small list is not truncated.
func TestBlockedSummaryCap(t *testing.T) {
	t.Parallel()
	var many []string
	for i := range 100 {
		many = append(many, fmt.Sprintf("module.role.aws_lb_target_group_attachment.é%03d (replace)", i))
	}
	s := blockedSummary(many)
	if len(s) > MaxSummary || !utf8.ValidString(s) {
		t.Fatalf("summary is %d bytes, valid UTF-8 %v", len(s), utf8.ValidString(s))
	}
	listed := strings.Count(s, "(replace)")
	if want := fmt.Sprintf(" and %d more", 100-listed); listed == 0 || !strings.HasSuffix(s, want) {
		t.Errorf("summary %q lists %d, want suffix %q", s, listed, want)
	}
	// One huge address is cut on a rune boundary and still counted.
	huge := blockedSummary([]string{strings.Repeat("é", 400), "b (delete)"})
	if len(huge) > MaxSummary || !utf8.ValidString(huge) || !strings.HasSuffix(huge, " and 2 more") {
		t.Errorf("huge = %q (%d bytes)", huge, len(huge))
	}
	if got := blockedSummary([]string{"a (delete)"}); got != blockedPrefix+"1 resource(s): a (delete)" {
		t.Errorf("single = %q", got)
	}
}

// TestDestructiveChanges checks DestructiveChanges against a plan with
// deletes and replaces, one with only creates, and invalid JSON.
func TestDestructiveChanges(t *testing.T) {
	t.Parallel()
	got, err := DestructiveChanges([]byte(destructivePlan))
	if err != nil || !slices.Equal(got, []string{"module.role.lb (replace)", "module.role.net (replace)", "module.role.old (delete)"}) {
		t.Errorf("DestructiveChanges = %v, %v", got, err)
	}
	if got, err := DestructiveChanges([]byte(`{"resource_changes":[{"address":"a","change":{"actions":["create"]}}]}`)); err != nil || len(got) != 0 {
		t.Errorf("creates only = %v, %v", got, err)
	}
	if _, err := DestructiveChanges([]byte("{")); err == nil {
		t.Error("bad JSON accepted")
	}
}

// TestRunLogLines: the runner logs one structured line before and after
// each step, with the exit code, and never the environment or the tfvars.
func TestRunLogLines(t *testing.T) {
	t.Parallel()
	const marker = "captf-secret-env-value"
	f := newFixture(t, OpRefresh, "AWS_SECRET_ACCESS_KEY="+marker)
	stderr := &bytes.Buffer{}
	f.opts.Stderr = stderr
	ctx, log := logContext(t)
	if _, code := Run(ctx, f.opts); code != 0 {
		t.Fatalf("run = %d: %s", code, log)
	}
	var lines []string
	for l := range strings.Lines(log.String()) {
		if strings.Contains(l, `"Step started"`) || strings.Contains(l, `"Step finished"`) {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	if len(lines) == 0 || len(lines)%2 != 0 {
		t.Fatalf("step lines = %q", lines)
	}
	for i, l := range lines {
		want := []string{`"Step started"`, `"Step finished"`}[i%2]
		if !strings.Contains(l, want) {
			t.Errorf("line %q, want %q", l, want)
		}
		if !strings.Contains(l, `step="`) {
			t.Errorf("line %q lacks the step name", l)
		}
		if i%2 == 1 && !strings.Contains(l, "exit=0") {
			t.Errorf("line %q lacks exit=0", l)
		}
	}
	for _, out := range []string{log.String(), stderr.String(), f.stdout.String()} {
		if strings.Contains(out, marker) || strings.Contains(out, "c2VjcmV0") {
			t.Errorf("the runner's output leaks the environment or the tfvars:\n%s", out)
		}
	}
}

// TestRunEnvAndMirror checks that Run and Prepare drop hostile TF_* and
// KUBE_* environment variables, force TF_DATA_DIR and HOME, keep or default
// TMPDIR, and write the CLI config only when a provider mirror exists.
func TestRunEnvAndMirror(t *testing.T) {
	t.Parallel()
	// An identity Secret or image ENV must not steer the runtime.
	hostile := []string{"TF_WORKSPACE=elsewhere", "TF_CLI_ARGS=-lock=false", "TF_CLI_ARGS_apply=-parallelism=1", "TF_LOG=trace",
		"TF_VAR_captf_name=x", "KUBE_HOST=https://evil", "KUBE_TOKEN=t", "KUBE_CONFIG_PATH=/k"}
	kept := []string{"TF_IN_AUTOMATION=1", "TF_INPUT=0", "KUBE_NAMESPACE=ns", "AWS_REGION=us-east-1", "TFX=1"}
	env := append([]string{"TF_DATA_DIR=/wrong", "TF_CLI_CONFIG_FILE=/wrong", "TMPDIR=/tmp"}, hostile...)
	f := newFixture(t, OpRefresh, append(env, kept...)...)
	if err := os.MkdirAll(f.opts.ProvidersDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	r, code := Run(context.Background(), f.opts)
	if code != 0 || !r.Image.ProvidersMirror {
		t.Fatalf("run = %d, mirror %v", code, r.Image.ProvidersMirror)
	}
	cfg, err := os.ReadFile(filepath.Join(f.opts.WorkDir, "cli.tfrc"))
	if err != nil || !strings.Contains(string(cfg), `"*/*/*"`) || !strings.Contains(string(cfg), f.opts.ProvidersDir) {
		t.Errorf("cli.tfrc = %s (err %v)", cfg, err)
	}
	p, err := Prepare(f.opts.ConfigDir, f.opts.WorkDir, f.opts.ProvidersDir, f.opts.Env)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := map[string]string{
		"TF_DATA_DIR":        filepath.Join(f.opts.WorkDir, ".terraform"),
		"HOME":               f.opts.WorkDir,
		"TMPDIR":             "/tmp",
		"TF_CLI_CONFIG_FILE": filepath.Join(f.opts.WorkDir, "cli.tfrc"),
	}
	for k, v := range want {
		var got []string
		for _, kv := range p.Env {
			if name, val, _ := strings.Cut(kv, "="); name == k {
				got = append(got, val)
			}
		}
		if !slices.Equal(got, []string{v}) {
			t.Errorf("%s = %v, want exactly [%s]", k, got, v)
		}
	}
	for _, kv := range hostile {
		if slices.Contains(p.Env, kv) {
			t.Errorf("Env keeps %s", kv)
		}
	}
	for _, kv := range kept {
		if !slices.Contains(p.Env, kv) {
			t.Errorf("Env lacks %s", kv)
		}
	}
	if len(p.Dropped) != len(hostile)+2 { // plus TF_DATA_DIR and TF_CLI_CONFIG_FILE
		t.Errorf("Dropped = %v", p.Dropped)
	}
	// Without TMPDIR the work directory provides one.
	p2, _ := Prepare(f.opts.ConfigDir, t.TempDir(), "/nonexistent", nil)
	if !slices.ContainsFunc(p2.Env, func(kv string) bool { return strings.HasPrefix(kv, "TMPDIR=") }) || p2.ProvidersMirror {
		t.Errorf("env without TMPDIR = %v", p2.Env)
	}
}

// TestRunDefaultEnvNoWarning: the Job's actual default env (internal/jobs.Build,
// without TF_DATA_DIR: the runner always forces its own) produces no
// "ignoring environment variables" line. Build used to set TF_DATA_DIR
// itself, which Prepare always drops and re-sets, so every Job logged the
// warning regardless of the identity Secret or the image's ENV.
func TestRunDefaultEnvNoWarning(t *testing.T) {
	t.Parallel()
	defaultJobEnv := []string{
		"TF_IN_AUTOMATION=1", "TF_INPUT=0", "HOME=/captf/work", "TMPDIR=/tmp",
		"KUBE_NAMESPACE=team-a", "CHECKPOINT_DISABLE=1",
	}
	f := newFixture(t, OpRefresh, defaultJobEnv...)
	ctx, log := logContext(t)
	if _, code := Run(ctx, f.opts); code != 0 {
		t.Fatalf("run = %d: %s", code, log)
	}
	if strings.Contains(log.String(), "Ignoring environment variables") {
		t.Errorf("the Job's default env produced a warning: %s", log)
	}
}

// TestRunInterrupted: item 6. After SIGTERM, Terraform/OpenTofu finish their
// own graceful shutdown and exit non-zero on their own accord, a normal
// process exit indistinguishable from a module failure except by the
// canceled context.
func TestRunInterrupted(t *testing.T) {
	t.Parallel()
	f := newFixture(t, OpApply)
	// version returns fast; every other step blocks until SIGTERM, then exits
	// 1 via its own trap, exactly like the real runtimes.
	script := `case "$1" in
version) echo '{"terraform_version":"1.12.6"}'; exit 0 ;;
esac
trap 'exit 1' TERM
while :; do sleep 0.02; done`
	f.opts.Bin = []string{"sh", "-c", script, "--"}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(40*time.Millisecond, cancel)
	r, code := Run(ctx, f.opts)
	if code != 1 || r.Error == nil || r.Error.Kind != ErrorKindInterrupted || r.Error.Step == nil || *r.Error.Step != StepInit {
		t.Errorf("interrupted run = %d %+v", code, r.Error)
	}
}

// TestSteps checks Steps' argument sequences for drift, apply, a guarded
// apply, an approved apply and every op under GuardDeletes/ExpectPlan, and
// that an unknown op errors.
func TestSteps(t *testing.T) {
	t.Parallel()
	steps, err := Steps(OpDrift, PlanOptions{BackendConfig: []string{"namespace=ns"}, LockTimeout: 5 * time.Minute, WorkDir: "/captf/work"})
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	want := [][]string{
		{"init", "-input=false", "-no-color", "-lock-timeout=300s", "-backend-config=namespace=ns"},
		{"apply", "-input=false", "-no-color", "-lock-timeout=300s", "-refresh-only", "-auto-approve", "-var-file=terraform.tfvars.json"},
		{"plan", "-input=false", "-no-color", "-lock-timeout=300s", "-detailed-exitcode", "-refresh=false", "-var-file=terraform.tfvars.json", "-out=/captf/work/plan.tfplan"},
		{"show", "-json", "-no-color", "/captf/work/plan.tfplan"},
	}
	for i, s := range steps {
		if !slices.Equal(s.Args, want[i]) {
			t.Errorf("step %d = %v, want %v", i, s.Args, want[i])
		}
	}
	apply, _ := Steps(OpApply, PlanOptions{LockTimeout: time.Minute})
	if !slices.Equal(apply[1].Args, []string{"validate", "-json", "-no-color"}) ||
		!slices.Equal(apply[2].Args, []string{"apply", "-input=false", "-no-color", "-lock-timeout=60s", "-auto-approve", "-var-file=terraform.tfvars.json"}) {
		t.Errorf("apply = %v", apply)
	}
	guarded, _ := Steps(OpApply, PlanOptions{LockTimeout: time.Minute, WorkDir: "/captf/work", GuardDeletes: true})
	wantGuarded := [][]string{
		{"init", "-input=false", "-no-color", "-lock-timeout=60s"},
		{"validate", "-json", "-no-color"},
		{"plan", "-input=false", "-no-color", "-lock-timeout=60s", "-detailed-exitcode", "-var-file=terraform.tfvars.json", "-out=/captf/work/apply.tfplan"},
		{"show", "-json", "-no-color", "/captf/work/apply.tfplan"},
		{"apply", "-input=false", "-no-color", "-lock-timeout=60s", "/captf/work/apply.tfplan"},
	}
	if len(guarded) != len(wantGuarded) || !guarded[3].Capture || guarded[3].LogCapture {
		t.Fatalf("guarded apply = %+v", guarded)
	}
	for i, s := range guarded {
		if !slices.Equal(s.Args, wantGuarded[i]) {
			t.Errorf("guarded step %d = %v, want %v", i, s.Args, wantGuarded[i])
		}
	}
	// GuardDeletes changes only apply.
	for _, op := range []string{OpDestroy, OpRefresh, OpDrift, OpPlan} {
		a, _ := Steps(op, PlanOptions{WorkDir: "/w"})
		b, _ := Steps(op, PlanOptions{WorkDir: "/w", GuardDeletes: true, ExpectPlan: EmptyPlanHash})
		if len(a) != len(b) {
			t.Errorf("%s: GuardDeletes or ExpectPlan changed the sequence", op)
		}
	}
	// An approved apply runs the guarded sequence.
	approved, _ := Steps(OpApply, PlanOptions{LockTimeout: time.Minute, WorkDir: "/captf/work", ExpectPlan: EmptyPlanHash})
	for i, s := range approved {
		if !slices.Equal(s.Args, wantGuarded[i]) {
			t.Errorf("approved step %d = %v, want %v", i, s.Args, wantGuarded[i])
		}
	}
	if _, err := Steps("bogus", PlanOptions{}); err == nil {
		t.Error("unknown op accepted")
	}
}

// TestPreflight checks Preflight against module directories and runtime
// commands that should and should not pass, and that a preflight or
// prepare failure surfaces through Run with the right error kind and step.
func TestPreflight(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mod := func(files ...string) string {
		d, _ := os.MkdirTemp(dir, "m")
		for _, f := range files {
			_ = os.WriteFile(filepath.Join(d, f), nil, 0o600)
		}
		return d
	}
	exe := filepath.Join(dir, "tofu")
	_ = os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755)
	plain := filepath.Join(dir, "plain")
	_ = os.WriteFile(plain, nil, 0o600)
	cases := []struct {
		name   string
		module string
		bin    []string
		ok     bool
	}{
		{"tf", mod("main.tf"), []string{exe}, true},
		{"tofu json", mod("x.tofu.json"), []string{exe, "tofu"}, true},
		{"on PATH", mod("x.tf.json"), []string{"sh"}, true},
		{"no module files", mod("README.md"), []string{exe}, false},
		{"no module dir", filepath.Join(dir, "missing"), []string{exe}, false},
		{"not executable", mod("main.tf"), []string{plain}, false},
		{"directory", mod("main.tf"), []string{dir}, false},
		{"missing binary", mod("main.tf"), []string{filepath.Join(dir, "nope")}, false},
		{"not on PATH", mod("main.tf"), []string{"no-such-runtime-xyz"}, false},
		{"empty", mod("main.tf"), nil, false},
	}
	for _, c := range cases {
		err := Preflight(c.module, c.bin)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrImageLayout)) {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
	// A preflight failure is an image-layout result with exit 1.
	f := newFixture(t, OpApply)
	f.opts.ModuleDir = mod()
	r, code := Run(context.Background(), f.opts)
	if code != 1 || r.Error == nil || r.Error.Kind != ErrorKindImageLayout || r.Error.Step != nil || len(r.Steps) != 0 {
		t.Errorf("preflight failure = %d %+v", code, r)
	}
	// So is a prepare failure (kind step, step "prepare").
	f = newFixture(t, OpApply)
	f.opts.ConfigDir = filepath.Join(dir, "no-config")
	if r, code := Run(context.Background(), f.opts); code != 1 || r.Error == nil || *r.Error.Step != "prepare" {
		t.Errorf("prepare failure = %d %+v", code, r.Error)
	}
	if r, code := Run(context.Background(), Options{Op: "bogus"}); code != 2 || r.Error == nil {
		t.Errorf("unknown op = %d %+v", code, r.Error)
	}
}

// TestEncodeSize: a 50-step run with long names, a full tail and drift
// stays within the termination message cap, in stages.
func TestEncodeSize(t *testing.T) {
	t.Parallel()
	big := Result{Version: ResultVersion, Op: OpDrift, Image: ResultImage{Ref: strings.Repeat("r", 200)},
		Drift: &Drift{Detected: true, Resources: slices.Repeat([]string{strings.Repeat("module.role.x", 10)}, MaxDriftResources)},
		Error: &Error{Kind: ErrorKindStep, Step: new("plan"), Tail: strings.Repeat("e", MaxTail)}}
	for i := range 50 {
		big.Steps = append(big.Steps, Step{Name: fmt.Sprintf("step-%02d-%s", i, strings.Repeat("n", 40)), Exit: i % 3, Seconds: 12.3})
	}
	b := Encode(big)
	var r Result
	if len(b) > MaxResultBytes || json.Unmarshal(b, &r) != nil {
		t.Fatalf("encoded %d bytes, valid %v", len(b), json.Valid(b))
	}
	if r.Version != 1 || r.Op != OpDrift || r.Error == nil || r.Error.Kind != ErrorKindStep {
		t.Errorf("essentials lost: %+v", r)
	}
	if len(r.Steps) < 2 || r.Steps[0].Name != big.Steps[0].Name || r.Steps[len(r.Steps)-1].Name != big.Steps[49].Name {
		t.Errorf("first and last step not kept: %d steps", len(r.Steps))
	}
	// Small results are untouched.
	small := Result{Version: 1, Op: OpApply, Steps: []Step{{Name: "init"}}}
	if got := Encode(small); !json.Valid(got) || strings.Contains(string(got), `"steps":null`) {
		t.Errorf("small = %s", got)
	}
	// Even an image reference too big for anything yields a minimal document.
	huge := Result{Version: 1, Op: OpApply, Image: ResultImage{Ref: strings.Repeat("r", 5000)}, Error: &Error{Kind: ErrorKindImageLayout}}
	if got := Encode(huge); len(got) > MaxResultBytes || !strings.Contains(string(got), "image-layout") {
		t.Errorf("minimal = %s", got)
	}
	path := filepath.Join(t.TempDir(), "r.json")
	if err := Write(path, big); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := Write(filepath.Join(t.TempDir(), "missing", "r.json"), big); err == nil {
		t.Error("Write to a missing directory succeeded")
	}
}

// TestParseDrift checks that ParseDrift counts changes and caps its
// resource list at MaxDriftResources, and rejects invalid JSON.
func TestParseDrift(t *testing.T) {
	t.Parallel()
	var changes []string
	for i := range 30 {
		changes = append(changes, fmt.Sprintf(`{"address":"r.%d","change":{"actions":["create"]}}`, i))
	}
	d, err := ParseDrift([]byte(`{"resource_changes":[` + strings.Join(changes, ",") + `]}`))
	if err != nil || d.Create != 30 || len(d.Resources) != MaxDriftResources {
		t.Errorf("drift = %+v (err %v)", d, err)
	}
	if _, err := ParseDrift([]byte("{")); err == nil {
		t.Error("bad JSON accepted")
	}
}

// TestExecStopTimeout: an interrupted step gets SIGTERM and the whole
// stop timeout to finish (Terraform waiting on in-flight provider calls);
// SIGKILL comes only after it.
func TestExecStopTimeout(t *testing.T) {
	t.Parallel()
	// The trap outlives a short stop timeout but not a long one.
	script := `trap 'sleep 1; exit 3' TERM; echo ready >&2; while :; do sleep 0.05; done`
	for _, tt := range []struct {
		stop   time.Duration
		exit   int
		killed bool
	}{
		{stop: 10 * time.Second, exit: 3},
		{stop: 200 * time.Millisecond, exit: 1, killed: true},
	} {
		t.Run(tt.stop.String(), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			time.AfterFunc(300*time.Millisecond, cancel)
			res := Exec(ctx, []string{"sh", "-c", script}, Invocation{Name: "x"}, nil, t.TempDir(), &bytes.Buffer{}, &bytes.Buffer{}, tt.stop)
			if res.Exit != tt.exit || (res.Err != nil) != tt.killed {
				t.Errorf("stop %s: exit %d err %v, want exit %d killed %v", tt.stop, res.Exit, res.Err, tt.exit, tt.killed)
			}
		})
	}
}

// TestExecKilledByContext checks that Exec on an already-canceled ctx
// reports a failing StepResult, and that tailWriter keeps only its last
// max bytes.
func TestExecKilledByContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := Exec(ctx, []string{"sleep"}, Invocation{Name: "x", Args: []string{"5"}}, nil, t.TempDir(), &bytes.Buffer{}, &bytes.Buffer{}, 0)
	if res.Err == nil || !res.Failed(Invocation{}) {
		t.Errorf("canceled exec = %+v", res)
	}
	var w tailWriter
	w.max = 4
	_, _ = w.Write([]byte("abcdef"))
	_, _ = w.Write([]byte("gh"))
	if w.String() != "efgh" {
		t.Errorf("tail = %q", w.String())
	}
}
