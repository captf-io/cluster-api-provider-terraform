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

package app_test

import (
	"slices"
	"testing"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/cmd/runner/app"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
)

// runFlagsOwner is the fixture regarding object of TestParseRunFlagsEvents,
// matching internal/runner's own event fixtures.
var runFlagsOwner = runner.ObjectRef{
	APIVersion: "infrastructure.cluster.x-k8s.io/v1alpha1", Kind: "TerraformMachine",
	Namespace: "team-a", Name: "m1", UID: "m1-uid",
}

// TestParseRunFlags checks ParseRunFlags' repeatable flags, defaults,
// guard-deletes flags and rejection of a stray positional argument. None of
// these calls touch component-base/version/verflag's shared flag, so unlike
// the command-line tests in app_test.go it is safe to run in parallel.
func TestParseRunFlags(t *testing.T) {
	t.Parallel()
	o, result, err := app.ParseRunFlags([]string{"--op=drift", "--bin=/opt/w", "--bin=tofu", "--backend-config=a=b", "--backend-config=labels={\"x\"=\"y\"}", "--lock-timeout=60s", "--force-unlock=id"})
	if err != nil {
		t.Fatalf("ParseRunFlags: %v", err)
	}
	if !slices.Equal(o.Bin, []string{"/opt/w", "tofu"}) || len(o.BackendConfig) != 2 || o.LockTimeout != time.Minute || o.ForceUnlockID != "id" ||
		result != app.DefaultResultPath || o.ModuleDir != "/captf/module" || o.ConfigDir != "/captf/config" {
		t.Errorf("options = %+v, result %s", o, result)
	}
	def, _, err := app.ParseRunFlags([]string{"--op=apply"})
	if err != nil || !slices.Equal(def.Bin, []string{"/captf/runtime"}) || def.GuardDeletes || def.InputsHash != "" || def.AllowDeletesHash != "" {
		t.Errorf("defaults = %+v, %v", def, err)
	}
	g, _, err := app.ParseRunFlags([]string{"--op=apply", "--guard-deletes", "--inputs-hash=h1:a", "--allow-deletes-hash=h1:b"})
	if err != nil || !g.GuardDeletes || g.InputsHash != "h1:a" || g.AllowDeletesHash != "h1:b" {
		t.Errorf("guard flags = %+v, %v", g, err)
	}
	if _, _, err := app.ParseRunFlags([]string{"--op=apply", "extra"}); err == nil {
		t.Error("positional argument accepted")
	}
}

// TestParseRunFlagsEvents checks that ParseRunFlags carries --event-object
// and --job-name into the parsed runner.Options.
func TestParseRunFlagsEvents(t *testing.T) {
	t.Parallel()
	o, _, err := app.ParseRunFlags([]string{"--op=apply", "--event-object=" + runFlagsOwner.String(), "--job-name=j1"})
	if err != nil || o.EventObject != runFlagsOwner.String() || o.JobName != "j1" {
		t.Errorf("options = %+v, %v", o, err)
	}
}

// TestParseExpectPlanFlag checks that ParseRunFlags accepts op "plan" with
// a lock timeout, and carries --expect-plan into the parsed runner.Options.
func TestParseExpectPlanFlag(t *testing.T) {
	t.Parallel()
	o, _, err := app.ParseRunFlags([]string{"--op=plan", "--lock-timeout=60s"})
	if err != nil || o.Op != runner.OpPlan || o.LockTimeout != time.Minute {
		t.Errorf("plan flags = %+v, %v", o, err)
	}
	a, _, err := app.ParseRunFlags([]string{"--op=apply", "--expect-plan=" + runner.EmptyPlanHash})
	if err != nil || a.ExpectPlan != runner.EmptyPlanHash {
		t.Errorf("expect-plan = %+v, %v", a, err)
	}
}

// TestParsePlanKeyFileFlag checks that --plan-key-file defaults to the
// key's mount and carries an explicit path into the parsed runner.Options.
func TestParsePlanKeyFileFlag(t *testing.T) {
	t.Parallel()
	def, _, err := app.ParseRunFlags([]string{"--op=plan"})
	if err != nil || def.PlanKeyFile != runner.DefaultPlanKeyFile {
		t.Errorf("default plan key file = %q, %v", def.PlanKeyFile, err)
	}
	o, _, err := app.ParseRunFlags([]string{"--op=apply", "--expect-plan=p2:x", "--plan-key-file=/k/key"})
	if err != nil || o.PlanKeyFile != "/k/key" {
		t.Errorf("plan key file = %q, %v", o.PlanKeyFile, err)
	}
}
