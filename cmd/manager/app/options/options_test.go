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

package options

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	captfmanager "github.com/captf-io/cluster-api-provider-terraform/internal/manager"
)

// testImage is a syntactically valid image reference used wherever a test
// needs one, for --runner-image or $CAPTF_MANAGER_IMAGE.
const testImage = "ghcr.io/captf-io/cluster-api-provider-terraform:v0.1.0"

// parse builds a fresh Options, registers every named flag set it returns
// on a new FlagSet and parses args against it, failing test t if parsing
// errors. It returns the parsed Options.
func parse(t *testing.T, args ...string) *Options {
	t.Helper()
	o := NewOptions()
	nfs := o.Flags()
	fs := pflag.NewFlagSet("manager", pflag.ContinueOnError)
	for _, name := range nfs.Order {
		fs.AddFlagSet(nfs.FlagSet(name))
	}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return o
}

// noEnv is a lookupEnv that reports every environment variable as unset.
func noEnv(string) (string, bool) { return "", false }

// envWith returns a lookupEnv that reports v for ManagerImageEnv and every
// other variable as unset.
func envWith(v string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		if k == ManagerImageEnv {
			return v, true
		}
		return "", false
	}
}

// TestFlagSet is the --help acceptance: every flag of the task is present and
// none of the kubebuilder scaffold's.
// It does not run t.Parallel: Flags calls controller-runtime's
// client/config.RegisterFlags, which writes a package-level global.
func TestFlagSet(t *testing.T) {
	o := NewOptions()
	nfs := o.Flags()
	fs := pflag.NewFlagSet("manager", pflag.ContinueOnError)
	for _, name := range nfs.Order {
		fs.AddFlagSet(nfs.FlagSet(name))
	}
	for _, name := range []string{
		"namespace", "watch-filter", "leader-elect", "leader-elect-lease-duration",
		"leader-elect-renew-deadline", "leader-elect-retry-period", "sync-period",
		"terraformcluster-concurrency", "terraformmachine-concurrency",
		"terraformmachinetemplate-concurrency", "terraformmachinepool-concurrency", "webhook-port", "webhook-cert-dir",
		"health-addr", "profiler-address", "diagnostics-address", "insecure-diagnostics",
		"feature-gates", "runner-image", "runner-events", "drift-default-interval", "cluster-operation-gate", "state-backups", "logging-format", "v", "kubeconfig",
	} {
		if fs.Lookup(name) == nil {
			t.Errorf("flag --%s is missing", name)
		}
	}
	for _, name := range []string{"metrics-bind-address", "health-probe-bind-address", "webhook-cert-path", "metrics-secure", "version"} {
		if fs.Lookup(name) != nil {
			t.Errorf("flag --%s must not be registered by Options.Flags", name)
		}
	}
}

// TestDefaults proves every flag's parsed default matches the value
// Flags registered for it, including the default log verbosity NewOptions
// sets. It does not run t.Parallel, for the reason TestFlagSet gives.
func TestDefaults(t *testing.T) {
	o := parse(t)
	checks := []struct {
		name      string
		got, want any
	}{
		{"namespace", o.Namespace, ""},
		{"watch-filter", o.WatchFilter, ""},
		{"leader-elect", o.LeaderElect, false},
		{"sync-period", o.SyncPeriod, 10 * time.Minute},
		{"terraformcluster-concurrency", o.TerraformClusterConcurrency, 10},
		{"terraformmachine-concurrency", o.TerraformMachineConcurrency, 10},
		{"terraformmachinetemplate-concurrency", o.TerraformMachineTemplateConcurrency, 10},
		{"terraformmachinepool-concurrency", o.TerraformMachinePoolConcurrency, 10},
		{"webhook-port", o.WebhookPort, 9443},
		{"webhook-cert-dir", o.WebhookCertDir, DefaultWebhookCertDir},
		{"health-addr", o.HealthAddr, ":9440"},
		{"diagnostics-address", o.Diagnostics.DiagnosticsAddress, ":8443"},
		{"insecure-diagnostics", o.Diagnostics.InsecureDiagnostics, false},
		{"drift-default-interval", o.DriftDefaultInterval, 30 * time.Minute},
		{"runner-image", o.RunnerImage, ""},
		{"runner-events", o.RunnerEvents, true},
		{"cluster-operation-gate", o.ClusterOperationGate, true},
		{"state-backups", o.StateBackups, 5},
		{"verbosity", int(o.Logs.Verbosity), 2},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("--%s default = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestRunnerImage proves Complete fills RunnerImage from the environment
// only when no flag set it, and that Validate rejects an empty or
// syntactically invalid image reference. Neither it nor its subtests run
// t.Parallel, for the reason TestFlagSet gives.
func TestRunnerImage(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     func(string) (string, bool)
		want    string
		wantErr string
	}{
		{name: "from env", env: envWith(testImage), want: testImage},
		{name: "flag wins over env", args: []string{"--runner-image=ghcr.io/x/runner:v2"}, env: envWith(testImage), want: "ghcr.io/x/runner:v2"},
		{name: "neither", env: noEnv, wantErr: "--runner-image is empty"},
		{name: "invalid", args: []string{"--runner-image=Not A Ref"}, env: noEnv, want: "Not A Ref", wantErr: "--runner-image"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := parse(t, tt.args...)
			o.Complete(tt.env)
			if o.RunnerImage != tt.want {
				t.Errorf("RunnerImage = %q, want %q", o.RunnerImage, tt.want)
			}
			err := o.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestValidateRanges proves Validate rejects an out-of-range concurrency,
// sync period, drift interval and negative state-backups count, each with
// an error that names the offending flag. It does not run t.Parallel, for
// the reason TestFlagSet gives.
func TestValidateRanges(t *testing.T) {
	o := parse(t, "--terraformmachine-concurrency=0", "--terraformmachinepool-concurrency=0", "--sync-period=0s", "--drift-default-interval=-1m", "--state-backups=-1")
	o.Complete(envWith(testImage))
	err := o.Validate()
	for _, want := range []string{
		"--terraformmachine-concurrency must be at least 1", "--terraformmachinepool-concurrency must be at least 1", "--sync-period must be positive", "--drift-default-interval must be positive",
		"--state-backups must not be negative",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate error = %v, want it to mention %q", err, want)
		}
	}
}

// TestManagerOptions proves ManagerOptions wires the parsed flags into
// controller-runtime's ctrl.Options: scheme, leader election, health and
// metrics addresses, cache scoping and selectors, the uncached-object list,
// the webhook server, and that it surfaces an invalid diagnostics flag as
// an error. It does not run t.Parallel, for the reason TestFlagSet gives.
func TestManagerOptions(t *testing.T) {
	scheme := runtime.NewScheme()
	o := parse(t, "--namespace=tenant-a", "--leader-elect", "--webhook-port=9555", "--sync-period=5m")
	m, err := o.ManagerOptions(scheme)
	if err != nil {
		t.Fatalf("ManagerOptions: %v", err)
	}
	if m.Scheme != scheme || !m.LeaderElection || m.LeaderElectionID != LeaderElectionID || m.LeaderElectionResourceLock != "leases" {
		t.Errorf("leader election/scheme not wired: %+v", m)
	}
	if m.HealthProbeBindAddress != ":9440" || m.Metrics.BindAddress != ":8443" || !m.Metrics.SecureServing || m.Metrics.FilterProvider == nil {
		t.Errorf("diagnostics not secured by default: health=%q metrics=%+v", m.HealthProbeBindAddress, m.Metrics)
	}
	if _, ok := m.Cache.DefaultNamespaces["tenant-a"]; !ok || len(m.Cache.DefaultNamespaces) != 1 {
		t.Errorf("DefaultNamespaces = %v, want only tenant-a", m.Cache.DefaultNamespaces)
	}
	if m.Cache.SyncPeriod == nil || *m.Cache.SyncPeriod != 5*time.Minute {
		t.Errorf("SyncPeriod = %v, want 5m", m.Cache.SyncPeriod)
	}

	selectors := map[string]labels.Selector{}
	for obj, by := range m.Cache.ByObject {
		switch obj.(type) {
		case *corev1.Secret:
			selectors["secret"] = by.Label
		case *batchv1.Job:
			selectors["job"] = by.Label
		default:
			t.Errorf("unexpected ByObject entry %T", obj)
		}
	}
	if s := selectors["secret"]; s == nil || !s.Matches(labels.Set{captfmanager.ManagedSecretLabel: "true"}) || s.Matches(labels.Set{}) {
		t.Errorf("Secret selector = %v, want %s=true", s, captfmanager.ManagedSecretLabel)
	}
	if s := selectors["job"]; s == nil || !s.Matches(labels.Set{captfmanager.JobOwnerKindLabel: "TerraformMachine", captfmanager.ManagedSecretLabel: "true"}) ||
		s.Matches(labels.Set{captfmanager.JobOwnerKindLabel: "TerraformMachine"}) || s.Matches(labels.Set{}) {
		t.Errorf("Job selector = %v, want %s present and %s=true", s, captfmanager.JobOwnerKindLabel, captfmanager.ManagedSecretLabel)
	}

	if got := len(m.Client.Cache.DisableFor); got != len(captfmanager.UncachedObjects()) {
		t.Errorf("DisableFor has %d types, want UncachedObjects (%d)", got, len(captfmanager.UncachedObjects()))
	}

	if m.WebhookServer == nil {
		t.Fatal("WebhookServer is nil")
	}

	// All namespaces when --namespace is unset.
	all, err := parse(t).ManagerOptions(scheme)
	if err != nil {
		t.Fatalf("ManagerOptions: %v", err)
	}
	if all.Cache.DefaultNamespaces != nil {
		t.Errorf("DefaultNamespaces = %v, want nil (all namespaces)", all.Cache.DefaultNamespaces)
	}

	// Invalid TLS flags surface as an error.
	if _, err := parse(t, "--tls-min-version=VersionTLS99").ManagerOptions(scheme); err == nil {
		t.Error("ManagerOptions accepted an invalid --tls-min-version")
	}
	insecure, err := parse(t, "--insecure-diagnostics").ManagerOptions(scheme)
	if err != nil {
		t.Fatalf("ManagerOptions: %v", err)
	}
	if insecure.Metrics.SecureServing {
		t.Error("--insecure-diagnostics still serves securely")
	}
}

// TestManagerUser proves Complete builds the manager's ServiceAccount
// username from the downward-API variables, and leaves it empty when either
// is missing.
func TestManagerUser(t *testing.T) {
	env := func(ns, sa string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			switch k {
			case PodNamespaceEnv:
				return ns, ns != ""
			case ServiceAccountEnv:
				return sa, sa != ""
			}
			return "", false
		}
	}
	tests := []struct {
		name, ns, sa, want string
	}{
		{name: "both set", ns: "captf-system", sa: "captf-manager", want: "system:serviceaccount:captf-system:captf-manager"},
		{name: "no namespace", sa: "captf-manager"},
		{name: "no service account", ns: "captf-system"},
		{name: "neither"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := parse(t)
			o.Complete(env(tt.ns, tt.sa))
			if o.ManagerUser != tt.want {
				t.Errorf("ManagerUser = %q, want %q", o.ManagerUser, tt.want)
			}
		})
	}
}
