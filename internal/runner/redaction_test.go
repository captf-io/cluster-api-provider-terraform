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

package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeConfig overwrites the rendered main.tf.json (with mainTF) and
// terraform.tfvars.json (with tfvars) of f's kubelet-style config directory,
// failing t on error.
func writeConfig(t *testing.T, f fixture, mainTF, tfvars string) {
	t.Helper()
	dir := filepath.Join(f.opts.ConfigDir, "..2026_09_25")
	for name, content := range map[string]string{"main.tf.json": mainTF, "terraform.tfvars.json": tfvars} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// writeVars writes main.tf.json (mainTF) and terraform.tfvars.json
// (tfvars) into a new directory and returns it, failing t on error.
func writeVars(t *testing.T, mainTF, tfvars string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"main.tf.json": mainTF, "terraform.tfvars.json": tfvars} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestVarSecrets: bootstrap_data and every variable main.tf.json declares
// sensitive are secrets, a string whole and any other type by its string
// leaves; a variable not declared sensitive is not. bootstrap_data comes
// with the longer line minimum, declared sensitive or not, and is not
// decoded unless it is base64 of text.
func TestVarSecrets(t *testing.T) {
	t.Parallel()
	dir := writeVars(t, `{"variable":{"token":{"sensitive":true},"cfg":{"sensitive":true},"open":{},"bootstrap_data":{"sensitive":true}}}`,
		`{"bootstrap_data":"boot-data","token":"tok-value","open":"plain-value",`+
			`"cfg":{"a":["list-leaf",3],"b":{"c":"nested-leaf"},"d":true}}`)
	got := varSecrets(dir)
	slices.SortFunc(got, func(a, b secret) int { return strings.Compare(a.value, b.value) })
	want := []secret{{"boot-data", minBootstrapLine}, {"list-leaf", minRedactLen}, {"nested-leaf", minRedactLen}, {"tok-value", minRedactLen}}
	if !slices.Equal(got, want) {
		t.Errorf("varSecrets = %v, want %v", got, want)
	}
	if got := varSecrets(t.TempDir()); got != nil {
		t.Errorf("varSecrets of an empty directory = %v", got)
	}
	binary := base64.StdEncoding.EncodeToString([]byte{0x1f, 0x8b, 0xff, 0xfe})
	if got := varSecrets(writeVars(t, `{}`, `{"bootstrap_data":"`+binary+`"}`)); !slices.Equal(got, []secret{{binary, minBootstrapLine}}) {
		t.Errorf("varSecrets of binary bootstrap data = %v", got)
	}
}

// TestBootstrapRedaction: bootstrap data is redacted base64 and decoded,
// whole and by its lines of at least minBootstrapLine bytes; its short
// lines ("then", "fi") stay in ordinary text.
func TestBootstrapRedaction(t *testing.T) {
	t.Parallel()
	const (
		script = "#!/bin/sh\nif true; then\n  kubeadm join --token abcdef.0123456789abcdef\nfi\n"
		line   = "kubeadm join --token abcdef.0123456789abcdef"
	)
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	r := newRedactor(varSecrets(writeVars(t, `{}`, `{"bootstrap_data":"`+encoded+`"}`)))
	cases := []struct{ in, want string }{
		{"user data " + encoded, "user data (sensitive)"},
		{"script: " + script, "script: (sensitive)"},
		{"line 3: " + line + ": exit 1", "line 3: (sensitive): exit 1"},
		{"if the module fails then fix it", "if the module fails then fix it"},
		{"#!/bin/sh is missing", "#!/bin/sh is missing"},
	}
	for _, c := range cases {
		if got := r.Redact(c.in); got != c.want {
			t.Errorf("Redact(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRunRedactsGuardedPlanValue: a guarded apply, which fingerprints
// nothing, still redacts a value its plan marks sensitive from the failure
// of the apply step.
func TestRunRedactsGuardedPlanValue(t *testing.T) {
	t.Parallel()
	const secret = "guarded-sensitive-value"
	plan := `{"resource_changes":[{"address":"a.b","change":{"actions":["create"],` +
		`"after":{"password":"` + secret + `"},"after_sensitive":{"password":true}}}]}`
	f := newFixture(t, OpApply, "FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW="+plan, "FAKE_EXIT_APPLY=1",
		"FAKE_STDERR_APPLY=Error: setting password to "+secret+"\n")
	f.opts.GuardDeletes, f.opts.InputsHash = true, "h1:x"
	r, code := Run(context.Background(), f.opts)
	if code != ExitFailure || r.Error == nil || r.Error.Step == nil || *r.Error.Step != StepApply {
		t.Fatalf("run = %d, error %+v", code, r.Error)
	}
	if want := "Error: setting password to (sensitive)"; r.Error.Tail != want {
		t.Errorf("tail = %q, want %q", r.Error.Tail, want)
	}
}

// userDataScript is bootstrap data as a module decodes it into user_data:
// short ordinary lines ("then", "done", "#cloud-config") that occur in
// diagnostics ("authentication" holds "then"), and a long secret line.
const userDataScript = "#cloud-config\nruncmd:\n  - |\n    for i in 1 2 3\n    do\n      if test -f /run/ok\n      then\n        kubeadm join --token abcdef.0123456789abcdef\n      fi\n    done\n"

// userDataPlan returns a `show -json` plan creating an instance whose
// sensitive user_data is script, failing t on error.
func userDataPlan(t *testing.T, script string) string {
	t.Helper()
	raw, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	return `{"resource_changes":[{"address":"aws_instance.m","change":{"actions":["create"],` +
		`"after":{"user_data":` + string(raw) + `},"after_sensitive":{"user_data":true}}}]}`
}

// TestRedactorLargestLineMinimum: a value or line registered with several
// line minimums keeps the largest, whatever the order, so a second copy
// of a script with the default minimum does not redact its short lines.
func TestRedactorLargestLineMinimum(t *testing.T) {
	t.Parallel()
	const long = "kubeadm join --token abcdef.0123456789abcdef"
	for name, secrets := range map[string][]secret{
		"same value, large first":  {{userDataScript, minBootstrapLine}, {userDataScript, minRedactLen}},
		"same value, small first":  {{userDataScript, minRedactLen}, {userDataScript, minBootstrapLine}},
		"same line, other value":   {{userDataScript, minBootstrapLine}, {"then\n" + long + "\nextra", minRedactLen}},
		"plan value alone":         planSecrets([]string{userDataScript}),
		"bootstrap and plan value": append(varSecretsOf(t, userDataScript), planSecrets([]string{userDataScript})...),
	} {
		r := newRedactor(secrets)
		for in, want := range map[string]string{
			"Error: authentication failed":    "Error: authentication failed",
			"#cloud-config is missing; done":  "#cloud-config is missing; done",
			"line 5: " + long + ": exit 1":    "line 5: (sensitive): exit 1",
			"script: " + userDataScript + ".": "script: (sensitive).",
		} {
			if got := r.Redact(in); got != want {
				t.Errorf("%s: Redact(%q) = %q, want %q", name, in, got, want)
			}
		}
	}
}

// varSecretsOf returns varSecrets of a rendered root whose bootstrap_data
// is script, base64-encoded, failing t on error.
func varSecretsOf(t *testing.T, script string) []secret {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	return varSecrets(writeVars(t, `{}`, `{"bootstrap_data":"`+encoded+`"}`))
}

// TestRunGuardedUserDataRedaction: a guarded apply whose plan marks
// user_data, the decoded bootstrap data, sensitive keeps the ordinary
// words of a diagnostic intact and still redacts the script's long line.
func TestRunGuardedUserDataRedaction(t *testing.T) {
	t.Parallel()
	const long = "kubeadm join --token abcdef.0123456789abcdef"
	f := newFixture(t, OpApply, "FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW="+userDataPlan(t, userDataScript), "FAKE_EXIT_APPLY=1",
		"FAKE_STDERR_APPLY=Error: authentication failed running "+long+"\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(userDataScript))
	writeConfig(t, f, `{}`, `{"bootstrap_data":"`+encoded+`"}`)
	f.opts.GuardDeletes, f.opts.InputsHash = true, "h1:x"
	r, code := Run(context.Background(), f.opts)
	if code != ExitFailure || r.Error == nil || r.Error.Step == nil || *r.Error.Step != StepApply {
		t.Fatalf("run = %d, error %+v", code, r.Error)
	}
	if want := "Error: authentication failed running (sensitive)"; r.Error.Tail != want {
		t.Errorf("tail = %q, want %q", r.Error.Tail, want)
	}
}

// TestEnvSecrets: credentials in the step environment are secrets, by a
// credential-looking name whatever their length or by a long value
// whatever their name; the paths and flags the runner or the Job sets,
// cluster addresses and short values of other names (a region, a profile)
// are not.
func TestEnvSecrets(t *testing.T) {
	t.Parallel()
	got := envSecrets([]string{
		"AWS_SECRET_ACCESS_KEY=aws-secret", "TF_DATA_DIR=/work/.terraform/long/enough/path", "HOME=/work", "TMPDIR=/tmp",
		"TF_CLI_CONFIG_FILE=/work/cli.tfrc", "TF_IN_AUTOMATION=1234", "TF_INPUT=0000", "KUBE_NAMESPACE=team-a",
		"PATH=/usr/bin:/bin:/usr/local/bin", "KUBERNETES_SERVICE_HOST=10.96.0.1", "EMPTY=", "token=ghp_abcdef",
		"AWS_REGION=us-east-1", "AWS_PROFILE=default", "TF_VAR_zone=us-east-1a",
		"OS_PASSWORD=pw12", "AWS_SESSION_TOKEN=sess", "VSPHERE_USER=administrator", "Private_Data=pd-1", "TLS_CERT=c-1",
		"api_auth=a-1", "GOOGLE_CREDENTIALS=gc-1", "OPAQUE=0123456789abcdef",
	})
	want := []string{"aws-secret", "ghp_abcdef", "pw12", "sess", "pd-1", "c-1", "a-1", "gc-1", "0123456789abcdef"}
	if !slices.Equal(got, want) {
		t.Errorf("envSecrets = %v, want %v", got, want)
	}
}

// TestRunKeepsOutputValues: a failing apply whose diagnostic names values
// the plan's outputs mark sensitive (every re-exported output is) and a
// short region from the environment keeps them; a sensitive resource
// attribute is still redacted.
func TestRunKeepsOutputValues(t *testing.T) {
	t.Parallel()
	const secret = "attr-sensitive-value"
	plan := `{"resource_changes":[{"address":"a.b","change":{"actions":["create"],` +
		`"after":{"password":"` + secret + `"},"after_sensitive":{"password":true}}}],` +
		`"output_changes":{"failure_domains":{"actions":["create"],"after":["us-east-1a"],"after_sensitive":true},` +
		`"provider_id":{"actions":["create"],"after":"aws:///us-east-1a/i-0abc","after_sensitive":true}}}`
	f := newFixture(t, OpApply, "AWS_REGION=us-east-1", "FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW="+plan, "FAKE_EXIT_APPLY=1",
		"FAKE_STDERR_APPLY=Error: aws:///us-east-1a/i-0abc in us-east-1 rejected "+secret+"\n")
	f.opts.ExpectPlan = mustPlan(t, plan).Hash
	r, code := Run(context.Background(), f.opts)
	if code != ExitFailure || r.Error == nil {
		t.Fatalf("run = %d, error %+v", code, r.Error)
	}
	if want := "Error: aws:///us-east-1a/i-0abc in us-east-1 rejected (sensitive)"; r.Error.Tail != want {
		t.Errorf("tail = %q, want %q", r.Error.Tail, want)
	}
}

// TestRunRedactsFailure: a failing step whose stderr echoes a credential
// from the environment, a sensitive tfvar and bootstrap data produces a
// result, events, log and summary without them, and keeps the rest of the
// diagnostic.
func TestRunRedactsFailure(t *testing.T) {
	t.Parallel()
	const (
		cred      = "AKIA-credential-value"
		sensitive = "tfvar-sensitive-value"
		bootstrap = "c2VjcmV0"
	)
	f := newFixture(t, OpApply,
		"AWS_SECRET_ACCESS_KEY="+cred,
		"FAKE_EXIT_APPLY=1",
		"FAKE_STDERR_APPLY=noise "+bootstrap+"\nError: creating instance with "+cred+" and "+sensitive+": InvalidParameterValue\n")
	writeConfig(t, f, `{"variable":{"db_password":{"sensitive":true}}}`,
		`{"bootstrap_data":"`+bootstrap+`","db_password":"`+sensitive+`"}`)
	rec := &memRecorder{}
	f.opts.Events = rec
	stderr := &bytes.Buffer{}
	f.opts.Stderr = stderr
	ctx, log := logContext(t)
	r, code := Run(ctx, f.opts)
	if code != ExitFailure || r.Error == nil {
		t.Fatalf("run = %d, error %+v", code, r.Error)
	}
	if want := "Error: creating instance with (sensitive) and (sensitive): InvalidParameterValue"; r.Error.Tail != want {
		t.Errorf("tail = %q, want %q", r.Error.Tail, want)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	outputs := []string{string(raw), log.String()}
	for _, e := range rec.events {
		outputs = append(outputs, e.Note)
	}
	for _, out := range outputs {
		for _, v := range []string{cred, sensitive, bootstrap} {
			if strings.Contains(out, v) {
				t.Errorf("%q leaks %q", out, v)
			}
		}
	}
}

// TestRunRedactsPlanValue: a value the approved plan marks sensitive is
// redacted from the failure of the apply step that follows, which the
// environment and the tfvars do not name.
func TestRunRedactsPlanValue(t *testing.T) {
	t.Parallel()
	const secret = "plan-sensitive-value"
	plan := `{"resource_changes":[{"address":"a.b","change":{"actions":["create"],` +
		`"after":{"password":"` + secret + `","name":"visible-name"},"after_sensitive":{"password":true}}}]}`
	approved := mustPlan(t, plan)
	f := newFixture(t, OpApply, "FAKE_EXIT_PLAN=2", "FAKE_STDOUT_SHOW="+plan, "FAKE_EXIT_APPLY=1",
		"FAKE_STDERR_APPLY=Error: setting visible-name password to "+secret+"\n")
	f.opts.ExpectPlan = approved.Hash
	rec := &memRecorder{}
	f.opts.Events = rec
	ctx, log := logContext(t)
	r, code := Run(ctx, f.opts)
	if code != ExitFailure || r.Error == nil || r.Error.Step == nil || *r.Error.Step != StepApply {
		t.Fatalf("run = %d, error %+v", code, r.Error)
	}
	if want := "Error: setting visible-name password to (sensitive)"; r.Error.Tail != want {
		t.Errorf("tail = %q, want %q", r.Error.Tail, want)
	}
	for _, e := range rec.events {
		if strings.Contains(e.Note, secret) {
			t.Errorf("%s leaks the plan value: %q", e.Reason, e.Note)
		}
	}
	if strings.Contains(log.String(), secret) {
		t.Errorf("the log leaks the plan value: %s", log)
	}
}
