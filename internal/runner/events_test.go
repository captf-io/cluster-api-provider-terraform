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
	"context"
	"encoding/json"
	"encoding/pem"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// TestAPIRecorderLongNameValid: a name cut at maxNamePrefix on a separator
// still yields a valid DNS-1123 subdomain Event name.
func TestAPIRecorderLongNameValid(t *testing.T) {
	t.Parallel()
	s := newAPIServer(t, false)
	rec := testRecorder(s)
	rec.Regarding.Name = strings.Repeat("a", maxNamePrefix-1) + "-b"
	ctx, _ := logContext(t)
	rec.Event(ctx, EventTypeNormal, EventStepStarted, StepInit, "x")
	if len(s.bodies) != 1 {
		t.Fatalf("received %d events", len(s.bodies))
	}
	name := s.bodies[0].Metadata.Name
	if errs := validation.IsDNS1123Subdomain(name); len(errs) != 0 {
		t.Errorf("name %q invalid: %v", name, errs)
	}
}

// event is one recorded event.
type event struct{ Type, Reason, Action, Note string }

// memRecorder records events in memory.
type memRecorder struct {
	mu     sync.Mutex
	events []event
}

// Event records eventType, reason, action and note as one event.
func (m *memRecorder) Event(_ context.Context, eventType, reason, action, note string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, event{eventType, reason, action, note})
}

// reasons returns each recorded event's "reason/action".
func (m *memRecorder) reasons() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.events))
	for _, e := range m.events {
		out = append(out, e.Reason+"/"+e.Action)
	}
	return out
}

// applySummaryLine is a fixture apply summary line with a known Changes.
const applySummaryLine = "Apply complete! Resources: 1 added, 2 changed, 0 destroyed.\n"

// TestRunEvents: the event sequence of each kind of run, and that no
// event carries a tfvars value or raw stderr.
func TestRunEvents(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		op    string
		env   []string
		guard bool
		want  []string
		// warn is the reasons emitted as Warning.
		warn []string
		note map[string]string
	}{
		{
			name: "apply", op: OpApply, env: []string{"FAKE_STDOUT_APPLY=" + applySummaryLine},
			want: []string{
				"RunStarted/apply", "StepStarted/init", "StepSucceeded/init", "StepStarted/validate", "StepSucceeded/validate",
				"StepStarted/apply", "StepSucceeded/apply", "ResourcesChanged/apply", "RunFinished/apply",
			},
			note: map[string]string{
				EventResourcesChanged: "apply changed resources: 1 added, 2 changed, 0 destroyed, 0 imported",
				EventRunStarted:       "apply started: image r:v1, runtime version unknown",
			},
		},
		{
			name: "drift with changes", op: OpDrift, env: []string{"FAKE_EXIT_PLAN=2", `FAKE_STDOUT_SHOW={"resource_changes":[{"address":"a","change":{"actions":["update"]}}]}`},
			want: []string{
				"RunStarted/drift", "StepStarted/init", "StepSucceeded/init", "StepStarted/apply-refresh-only", "StepSucceeded/apply-refresh-only",
				"StepStarted/plan", "StepSucceeded/plan", "StepStarted/show-json", "StepSucceeded/show-json", "PlanSummary/show-json", "RunFinished/drift",
			},
			note: map[string]string{EventPlanSummary: "plan: 0 to add, 1 to change, 0 to destroy"},
		},
		{
			name: "drift without changes", op: OpDrift,
			want: []string{
				"RunStarted/drift", "StepStarted/init", "StepSucceeded/init", "StepStarted/apply-refresh-only", "StepSucceeded/apply-refresh-only",
				"StepStarted/plan", "StepSucceeded/plan", "PlanSummary/plan", "RunFinished/drift",
			},
			note: map[string]string{EventPlanSummary: "plan: no changes"},
		},
		{
			name: "failing step", op: OpDestroy, env: []string{"FAKE_EXIT_DESTROY=1", "FAKE_STDERR_DESTROY=noise c2VjcmV0\nError: boom\n"},
			want: []string{"RunStarted/destroy", "StepStarted/init", "StepSucceeded/init", "StepStarted/destroy", "StepFailed/destroy", "RunFinished/destroy"},
			warn: []string{EventStepFailed, EventRunFinished},
			note: map[string]string{EventRunFinished: "destroy failed in step destroy after "},
		},
		{
			name: "unparseable plan", op: OpDrift, env: []string{"FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW=not json"},
			want: []string{
				"RunStarted/drift", "StepStarted/init", "StepSucceeded/init", "StepStarted/apply-refresh-only", "StepSucceeded/apply-refresh-only",
				"StepStarted/plan", "StepSucceeded/plan", "StepStarted/show-json", "StepFailed/show-json", "RunFinished/drift",
			},
			warn: []string{EventStepFailed, EventRunFinished},
			note: map[string]string{EventStepFailed: "step show-json failed (exit 0)"},
		},
		{
			// Nothing to apply: no apply step and no ResourcesChanged; the
			// run succeeds right after the plan.
			name: "guarded apply without changes", op: OpApply, guard: true,
			want: []string{
				"RunStarted/apply", "StepStarted/init", "StepSucceeded/init", "StepStarted/validate", "StepSucceeded/validate",
				"StepStarted/plan", "StepSucceeded/plan", "PlanSummary/plan", "RunFinished/apply",
			},
			note: map[string]string{EventPlanSummary: "plan: no changes", EventRunFinished: "apply succeeded in "},
		},
		{
			name: "blocked guarded apply", op: OpApply, guard: true, env: []string{"FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW=" + destructivePlan},
			want: []string{
				"RunStarted/apply", "StepStarted/init", "StepSucceeded/init", "StepStarted/validate", "StepSucceeded/validate",
				"StepStarted/plan", "StepSucceeded/plan", "StepStarted/show-json", "StepSucceeded/show-json", "PlanSummary/show-json", "RunFinished/apply",
			},
			warn: []string{EventRunFinished},
			note: map[string]string{EventPlanSummary: "plan: 2 to add, 1 to change, 3 to destroy"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, c.op, c.env...)
			rec := &memRecorder{}
			f.opts.Events = rec
			f.opts.GuardDeletes, f.opts.InputsHash = c.guard, "h1:x"
			Run(context.Background(), f.opts)
			if got := rec.reasons(); !slices.Equal(got, c.want) {
				t.Errorf("events = %v\nwant     %v", got, c.want)
			}
			for _, e := range rec.events {
				if want := slices.Contains(c.warn, e.Reason); want != (e.Type == EventTypeWarning) {
					t.Errorf("%s is %s", e.Reason, e.Type)
				}
				if strings.Contains(e.Note, "c2VjcmV0") || strings.Contains(e.Note, "noise") {
					t.Errorf("%s leaks a value or raw stderr: %q", e.Reason, e.Note)
				}
				if n, ok := c.note[e.Reason]; ok && !strings.HasPrefix(e.Note, n) {
					t.Errorf("%s note = %q, want prefix %q", e.Reason, e.Note, n)
				}
				if e.Reason == EventStepFailed && e.Action == StepDestroy && !strings.Contains(e.Note, "boom") {
					t.Errorf("StepFailed lacks the summary: %q", e.Note)
				}
			}
		})
	}
}

// TestRunEventsEarlyFailure: a run that fails before the runtime is ready
// emits only RunFinished.
func TestRunEventsEarlyFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t, OpApply)
	f.opts.ModuleDir = filepath.Join(t.TempDir(), "missing")
	rec := &memRecorder{}
	f.opts.Events = rec
	if _, code := Run(context.Background(), f.opts); code == 0 {
		t.Fatal("a missing module succeeded")
	}
	if got := rec.reasons(); !slices.Equal(got, []string{"RunFinished/apply"}) || rec.events[0].Type != EventTypeWarning {
		t.Errorf("events = %v", rec.events)
	}
}

// apiServer is a fake events API.
type apiServer struct {
	*httptest.Server
	status  atomic.Int32
	delay   time.Duration
	mu      sync.Mutex
	bodies  []eventBody
	paths   []string
	auths   []string
	started atomic.Int32
}

// newAPIServer returns a running fake events API, over TLS when tls is
// true, that records every request it receives and answers s.status
// (created 201 by default); t.Cleanup closes it.
func newAPIServer(t *testing.T, tls bool) *apiServer {
	t.Helper()
	s := &apiServer{}
	s.status.Store(http.StatusCreated)
	unblock := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.started.Add(1)
		if s.delay > 0 {
			select {
			case <-time.After(s.delay):
			case <-r.Context().Done():
				return
			case <-unblock:
				return
			}
		}
		var b eventBody
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &b)
		s.mu.Lock()
		s.bodies = append(s.bodies, b)
		s.paths = append(s.paths, r.Method+" "+r.URL.Path)
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.mu.Unlock()
		w.WriteHeader(int(s.status.Load()))
	})
	if tls {
		s.Server = httptest.NewTLSServer(h)
	} else {
		s.Server = httptest.NewServer(h)
	}
	t.Cleanup(s.Close)
	// Runs before Close (cleanups run last-in first-out): Close waits for
	// the stalled handlers.
	t.Cleanup(func() { close(unblock) })
	return s
}

// owner is the fixture regarding object of every APIRecorder test.
var owner = ObjectRef{
	APIVersion: "infrastructure.cluster.x-k8s.io/v1alpha1", Kind: "TerraformMachine",
	Namespace: "team-a", Name: "m1", UID: "m1-uid",
}

// testRecorder returns an APIRecorder posting to s, regarding owner.
func testRecorder(s *apiServer) *APIRecorder {
	return &APIRecorder{
		BaseURL: s.URL, Client: s.Client(), Token: func() (string, error) { return "tok", nil },
		Regarding: owner, Related: &ObjectRef{APIVersion: "batch/v1", Kind: "Job", Namespace: "team-a", Name: "captf-m-m1-apply-1-abc"},
		Instance: "captf-m-m1-apply-1-abc-xyz",
	}
}

// TestAPIRecorder: the Event the API server receives.
func TestAPIRecorder(t *testing.T) {
	t.Parallel()
	s := newAPIServer(t, false)
	rec := testRecorder(s)
	ctx, log := logContext(t)
	rec.Event(ctx, EventTypeNormal, EventStepStarted, StepInit, "step init started")
	rec.Event(ctx, EventTypeWarning, EventStepFailed, StepApply, strings.Repeat("x", 2000))
	if len(s.bodies) != 2 || log.Len() != 0 {
		t.Fatalf("received %d events; log %q", len(s.bodies), log)
	}
	b := s.bodies[0]
	if s.paths[0] != "POST /apis/events.k8s.io/v1/namespaces/team-a/events" || s.auths[0] != "Bearer tok" {
		t.Errorf("request = %s, auth %q", s.paths[0], s.auths[0])
	}
	if b.APIVersion != "events.k8s.io/v1" || b.Kind != "Event" || b.Regarding != owner || b.Related == nil || b.Related.Kind != "Job" ||
		b.Related.Name != "captf-m-m1-apply-1-abc" || b.Action != StepInit || b.Reason != EventStepStarted || b.Type != EventTypeNormal ||
		b.ReportingController != ReportingController || b.ReportingInstance != "captf-m-m1-apply-1-abc-xyz" || b.Note != "step init started" {
		t.Errorf("event = %+v", b)
	}
	if b.Metadata.Namespace != "team-a" || !strings.HasPrefix(b.Metadata.Name, "m1.") || b.Metadata.Name == s.bodies[1].Metadata.Name {
		t.Errorf("names = %q, %q", b.Metadata.Name, s.bodies[1].Metadata.Name)
	}
	if _, err := time.Parse(microTime, b.EventTime); err != nil {
		t.Errorf("eventTime %q: %v", b.EventTime, err)
	}
	if n := len(s.bodies[1].Note); n != maxNote {
		t.Errorf("note length = %d, want %d", n, maxNote)
	}
}

// TestAPIRecorderFailures: failures are reported once and stop emission
// after MaxEventFailures in a row; a slow API server costs at most the
// timeout per event.
func TestAPIRecorderFailures(t *testing.T) {
	t.Parallel()
	t.Run("errors", func(t *testing.T) {
		t.Parallel()
		s := newAPIServer(t, false)
		s.status.Store(http.StatusForbidden)
		rec := testRecorder(s)
		ctx, log := logContext(t)
		for range 10 {
			rec.Event(ctx, EventTypeNormal, EventStepStarted, StepInit, "x")
		}
		if n := s.started.Load(); n != MaxEventFailures {
			t.Errorf("requests = %d, want %d", n, MaxEventFailures)
		}
		if n := strings.Count(log.String(), "Emitting event failed"); n != 1 || !strings.Contains(log.String(), "Events failed repeatedly") {
			t.Errorf("log = %q", log)
		}
	})
	t.Run("a success resets the count", func(t *testing.T) {
		t.Parallel()
		s := newAPIServer(t, false)
		rec := testRecorder(s)
		for i := range 10 {
			s.status.Store(int32(map[bool]int{true: http.StatusInternalServerError, false: http.StatusCreated}[i%2 == 0]))
			rec.Event(context.Background(), EventTypeNormal, EventStepStarted, StepInit, "x")
		}
		if n := s.started.Load(); n != 10 {
			t.Errorf("requests = %d, want 10", n)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		s := newAPIServer(t, false)
		s.delay = 5 * time.Second
		rec := testRecorder(s)
		rec.Timeout = 50 * time.Millisecond
		start := time.Now()
		for range 5 {
			rec.Event(context.Background(), EventTypeNormal, EventStepStarted, StepInit, "x")
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("five events against a stalled API took %s", took)
		}
		// A request may time out before the server counts it; the recorder
		// must have given up after MaxEventFailures either way.
		rec.mu.Lock()
		failures, disabled := rec.failures, rec.disabled
		rec.mu.Unlock()
		if failures != MaxEventFailures || !disabled {
			t.Errorf("failures = %d, disabled %v; want %d, true", failures, disabled, MaxEventFailures)
		}
	})
	t.Run("a canceled run still reports", func(t *testing.T) {
		t.Parallel()
		s := newAPIServer(t, false)
		rec := testRecorder(s)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec.Event(ctx, EventTypeWarning, EventRunFinished, OpApply, "interrupted")
		if len(s.bodies) != 1 {
			t.Errorf("received %d events after cancel, want 1", len(s.bodies))
		}
	})
}

// TestRunWithFailingEvents: a run whose events all fail still succeeds with
// the same result.
func TestRunWithFailingEvents(t *testing.T) {
	t.Parallel()
	s := newAPIServer(t, false)
	s.status.Store(http.StatusForbidden)
	f := newFixture(t, OpApply)
	f.opts.Events = testRecorder(s)
	ctx, log := logContext(t)
	r, code := Run(ctx, f.opts)
	if code != 0 || r.Error != nil || !slices.Equal(stepNames(r), []string{StepInit, StepValidate, StepApply}) {
		t.Errorf("run = %d %+v", code, r)
	}
	if !strings.Contains(log.String(), "Events failed repeatedly") {
		t.Errorf("log = %q", log)
	}
}

// TestNewEventRecorder: no events without --event-object or in-cluster
// configuration; with both, events reach the API server over TLS with the
// token file's content.
func TestNewEventRecorder(t *testing.T) {
	t.Parallel()
	obj := owner.String()
	ctx, log := logContext(t)
	if rec := NewEventRecorder(ctx, Options{}, InCluster{Host: "h", Port: "1"}, "p"); rec != nil || log.Len() != 0 {
		t.Errorf("without --event-object: %v, %q", rec, log)
	}
	for name, o := range map[string]Options{
		"bad reference":  {EventObject: "v1/Kind/ns"},
		"not in cluster": {EventObject: obj},
	} {
		ctx, log := logContext(t)
		if rec := NewEventRecorder(ctx, o, InCluster{}, "p"); rec != nil || !strings.Contains(log.String(), "Events disabled") {
			t.Errorf("%s: %v, %q", name, rec, log)
		}
	}

	s := newAPIServer(t, true)
	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
	ic := InCluster{TokenFile: filepath.Join(dir, "token"), CAFile: filepath.Join(dir, "ca.crt")}
	if err := os.WriteFile(ic.CAFile, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ic.TokenFile, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	ic.Host, ic.Port = u.Hostname(), u.Port()
	rec := NewEventRecorder(context.Background(), Options{EventObject: obj, JobName: "j1"}, ic, "pod-1")
	if rec == nil {
		t.Fatal("no recorder")
	}
	rec.Event(context.Background(), EventTypeNormal, EventRunStarted, OpApply, "started")
	if len(s.bodies) != 1 || s.auths[0] != "Bearer sa-token" || s.bodies[0].Related.Name != "j1" || s.bodies[0].Regarding != owner {
		t.Errorf("received %+v, auth %v", s.bodies, s.auths)
	}
}

// TestParseObjectRef checks ParseObjectRef's round trip with String, a
// group/version apiVersion, and rejection of malformed forms.
func TestParseObjectRef(t *testing.T) {
	t.Parallel()
	if got, err := ParseObjectRef(owner.String()); err != nil || got != owner {
		t.Errorf("round trip = %+v, %v", got, err)
	}
	core, err := ParseObjectRef("v1/ConfigMap/ns/cm/uid")
	if err != nil || core.APIVersion != "v1" || core.Kind != "ConfigMap" {
		t.Errorf("core = %+v, %v", core, err)
	}
	for _, bad := range []string{"", "a/b/c/d", "g/v/K/ns//uid", "/K/ns/n/u"} {
		if _, err := ParseObjectRef(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestRunnerEventsEmitted: every runner event reason is used outside its
// declaration and DocumentedEvents in the runner's non-test code.
func TestRunnerEventsEmitted(t *testing.T) {
	t.Parallel()
	used := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			if g, ok := decl.(*ast.GenDecl); ok && g.Tok == token.CONST {
				continue
			}
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "DocumentedEvents" {
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && strings.HasPrefix(id.Name, "Event") {
					used[id.Name] = true
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range DocumentedEvents() {
		if !used["Event"+reason] {
			t.Errorf("runner event reason %s is never emitted", reason)
		}
	}
}
