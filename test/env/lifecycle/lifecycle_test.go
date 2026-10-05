//go:build e2e

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

package lifecycle

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/env"
)

// opTimeout bounds one operation. It is below the make targets' 45m
// `go test -timeout`, so a slow Up fails with diagnostics instead of a
// test-binary panic.
const opTimeout = 40 * time.Minute

// TestMain refuses to run unless -run selects an operation: the tests
// change shared host state and must never run together (TestUp followed
// by TestDown, say). It returns through os.Exit with m.Run's code.
func TestMain(m *testing.M) {
	flag.Parse()
	if f := flag.Lookup("test.run"); f == nil || f.Value.String() == "" {
		fmt.Fprintln(os.Stderr, "lifecycle: select one operation with -run, for example -run '^TestUp$'; see the testenv-* make targets")
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// newEnv returns the Env for the configuration in the environment, with
// progress streamed to stderr and a context bounded by opTimeout, failing
// t when the configuration is invalid.
func newEnv(t *testing.T) (*env.Env, context.Context) {
	t.Helper()
	cfg, err := env.ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	e, err := env.New(cfg, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	t.Cleanup(cancel)
	return e, ctx
}

// The tests deliberately do not call t.Parallel: each one is a whole
// operation on shared host state, run alone (see TestMain).

// TestUp brings the environment up (make testenv-up).
func TestUp(t *testing.T) {
	e, ctx := newEnv(t)
	if err := e.Up(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestDown tears the environment down (make testenv-down).
func TestDown(t *testing.T) {
	e, ctx := newEnv(t)
	if err := e.Down(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestStatus reports on the environment (make testenv-status).
func TestStatus(t *testing.T) {
	e, ctx := newEnv(t)
	if _, err := e.Status(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestCollect writes a diagnostics bundle (make testenv-logs).
func TestCollect(t *testing.T) {
	e, ctx := newEnv(t)
	dir, err := e.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("artifacts: %s", dir)
}

// TestReload rebuilds and rolls out the manager image (make
// testenv-reload).
func TestReload(t *testing.T) {
	e, ctx := newEnv(t)
	if err := e.Reload(ctx); err != nil {
		t.Fatal(err)
	}
}
