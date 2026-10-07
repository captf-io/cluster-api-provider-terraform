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

package app

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/captf-io/cluster-api-provider-terraform/cmd/manager/app/options"
	"github.com/captf-io/cluster-api-provider-terraform/internal/imageinspect"
	captfmanager "github.com/captf-io/cluster-api-provider-terraform/internal/manager"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
)

// restoreMetricsRegistry saves ctrlmetrics.Registry and registers a cleanup
// on t that restores it. metrics.Install (which setup calls on every
// attempt that gets far enough) replaces that package-level global with a
// Bridge wrapping its previous value; left unrestored, a second Install in
// this binary would nest Bridges around the same legacyregistry.
// DefaultGatherer and make any later Gather (see
// TestMetricsBridgeGathersCleanInThisBinary) see every component-base
// series twice.
//
// The global is shared by the whole test binary, so a test that calls this
// (or setup) must not call t.Parallel, and neither may a test that reads
// ctrlmetrics.Registry: a parallel test would see another's Bridge.
func restoreMetricsRegistry(t *testing.T) {
	t.Helper()
	saved := ctrlmetrics.Registry
	t.Cleanup(func() { ctrlmetrics.Registry = saved })
}

// testOpts returns manager options with every flag's default value set (as
// Flags would from an empty command line) and a valid RunnerImage, ready
// for setup or newDeps. HealthAddr is "0", which disables the health probe
// listener: ctrl.NewManager binds it immediately, before Start, and an
// unstarted manager never closes it. t reports a fatal error if the
// resulting options don't validate.
func testOpts(t *testing.T) *options.Options {
	t.Helper()
	opts := options.NewOptions()
	opts.Flags()
	opts.RunnerImage = "example.com/runner:v1"
	opts.HealthAddr = "0"
	if err := opts.Validate(); err != nil {
		t.Fatalf("options: %v", err)
	}
	return opts
}

// unreachableConfig returns a *rest.Config whose Host resolves but refuses
// the connection, so any code path that dials it fails fast instead of
// hanging or reaching a real cluster.
func unreachableConfig() *rest.Config {
	return &rest.Config{Host: "https://127.0.0.1:1"}
}

// staticMapper builds a RESTMapper that resolves every type
// captfmanager.NewScheme registers, so a manager and its controllers built
// against it never perform API discovery. t reports a fatal error if the
// scheme cannot be built.
func staticMapper(t *testing.T) meta.RESTMapper {
	t.Helper()
	scheme, err := captfmanager.NewScheme()
	if err != nil {
		t.Fatalf("scheme: %v", err)
	}
	rm := meta.NewDefaultRESTMapper(scheme.PrioritizedVersionsAllGroups())
	for gvk := range scheme.AllKnownTypes() {
		rm.Add(gvk, meta.RESTScopeNamespace)
	}
	return rm
}

// testConfigure returns a setup configure hook that installs a static,
// offline RESTMapper (see staticMapper) and sets Controller.
// SkipNameValidation so repeated calls in the same test binary don't trip
// controller-runtime's process-global controller-name registry (pkg/
// controller/name.go), which TestSetup would otherwise collide with under
// go test -count>1 or a second happy-path test. t reports a fatal error if
// the scheme cannot be built.
func testConfigure(t *testing.T) func(*ctrl.Options) {
	t.Helper()
	rm := staticMapper(t)
	return func(o *ctrl.Options) {
		o.MapperProvider = func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return rm, nil }
		o.Controller.SkipNameValidation = ptr.To(true)
	}
}

// TestSetup runs setup to completion against an unreachable *rest.Config
// and a static, offline RESTMapper. testConfigure also sets Controller.
// SkipNameValidation, so this is safe under go test -count>1 and next to
// any future full-success test: without it, controller-runtime's
// process-global controller-name registry (pkg/controller/name.go) would
// reject the second and later registrations of, e.g., "terraformcluster".
func TestSetup(t *testing.T) {
	restoreMetricsRegistry(t)
	opts := testOpts(t)
	mgr, err := setup(t.Context(), unreachableConfig, opts, testConfigure(t))
	if err != nil {
		t.Fatalf("setup() error = %v, want nil", err)
	}
	if mgr == nil {
		t.Fatal("setup() manager = nil, want a configured manager")
	}
}

// TestSetupManagerOptionsError proves setup surfaces an invalid diagnostics
// flag from opts.ManagerOptions. That failure happens before setup calls
// getConfig or touches the metrics globals or the controller registry.
func TestSetupManagerOptionsError(t *testing.T) {
	opts := testOpts(t)
	opts.Diagnostics.TLSMinVersion = "not-a-version"
	_, err := setup(t.Context(), unreachableConfig, opts, testConfigure(t))
	if err == nil {
		t.Fatal("setup() error = nil, want an error for an invalid --tls-min-version")
	}
}

// TestSetupCreateManagerError proves setup wraps ctrl.NewManager's error
// with "create manager". A configure hook that installs a failing
// MapperProvider makes ctrl.NewManager fail deterministically, without any
// reconciler ever touching the controller registry.
func TestSetupCreateManagerError(t *testing.T) {
	restoreMetricsRegistry(t)
	opts := testOpts(t)
	wantErr := errors.New("boom")
	configure := func(o *ctrl.Options) {
		o.MapperProvider = func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return nil, wantErr }
	}
	_, err := setup(t.Context(), unreachableConfig, opts, configure)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("setup() error = %v, want it to wrap %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "create manager") {
		t.Errorf("setup() error = %q, want it to say create manager", err)
	}
}

// TestSetupReconcilerError proves setup surfaces a reconciler's
// SetupWithManager error: a RESTMapper with no default GroupVersions can
// still resolve a fully-qualified GroupVersionKind (letting ctrl.NewManager
// and terraformcluster's SetupWithManager succeed), but fails the
// versionless GroupKind lookup CAPI's isAPINamespaced does for
// terraformmachine's cluster watch, deep inside
// terraformmachine.ClusterToMachines. SkipNameValidation is still required
// because terraformcluster registers successfully before the failure.
func TestSetupReconcilerError(t *testing.T) {
	restoreMetricsRegistry(t)
	opts := testOpts(t)
	scheme, err := captfmanager.NewScheme()
	if err != nil {
		t.Fatalf("scheme: %v", err)
	}
	rm := meta.NewDefaultRESTMapper(nil)
	for gvk := range scheme.AllKnownTypes() {
		rm.Add(gvk, meta.RESTScopeNamespace)
	}
	configure := func(o *ctrl.Options) {
		o.MapperProvider = func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return rm, nil }
		o.Controller.SkipNameValidation = ptr.To(true)
	}
	_, err = setup(t.Context(), unreachableConfig, opts, configure)
	var nkm *meta.NoKindMatchError
	if !errors.As(err, &nkm) {
		t.Fatalf("setup() error = %v, want it to wrap a *meta.NoKindMatchError", err)
	}
}

// TestNewDeps proves newDeps copies opts' runner and behavior fields onto
// the returned shared.Deps and wires Client, APIReader, Scheme, Recorder,
// Jobs and State from mgr. mgr is built directly with ctrl.NewManager, not
// through setup, so this test carries none of the controller-registry risk
// TestSetup documents.
func TestNewDeps(t *testing.T) {
	opts := testOpts(t)
	scheme, err := captfmanager.NewScheme()
	if err != nil {
		t.Fatalf("scheme: %v", err)
	}
	mgrOpts, err := opts.ManagerOptions(scheme)
	if err != nil {
		t.Fatalf("manager options: %v", err)
	}
	testConfigure(t)(&mgrOpts)
	mgr, err := ctrl.NewManager(unreachableConfig(), mgrOpts)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	rec := metrics.New()
	deps := newDeps(mgr, opts, rec)

	if deps.Client == nil || deps.APIReader == nil || deps.Scheme == nil || deps.Recorder == nil ||
		deps.Jobs == nil || deps.State == nil || deps.Clock == nil || deps.Inspector == nil {
		t.Fatalf("newDeps left a field nil: %+v", deps)
	}
	if deps.Metrics != rec {
		t.Error("newDeps: Metrics != rec")
	}
	if deps.RunnerImage != opts.RunnerImage {
		t.Errorf("RunnerImage = %q, want %q", deps.RunnerImage, opts.RunnerImage)
	}
	if deps.RunnerEvents != opts.RunnerEvents {
		t.Errorf("RunnerEvents = %v, want %v", deps.RunnerEvents, opts.RunnerEvents)
	}
	if deps.DriftDefault != opts.DriftDefaultInterval {
		t.Errorf("DriftDefault = %v, want %v", deps.DriftDefault, opts.DriftDefaultInterval)
	}
	if deps.WatchFilter != opts.WatchFilter {
		t.Errorf("WatchFilter = %q, want %q", deps.WatchFilter, opts.WatchFilter)
	}
	if deps.ClusterOperationGate != opts.ClusterOperationGate {
		t.Errorf("ClusterOperationGate = %v, want %v", deps.ClusterOperationGate, opts.ClusterOperationGate)
	}
	if deps.StateBackups != opts.StateBackups {
		t.Errorf("StateBackups = %d, want %d", deps.StateBackups, opts.StateBackups)
	}
	if fb, ok := deps.Inspector.(imageinspect.FallbackInspector); !ok || fb.Inner.(imageinspect.Remote).AllowPrivate != opts.ImageInspectAllowPrivate {
		t.Errorf("Inspector = %#v, want a Remote with AllowPrivate %v", deps.Inspector, opts.ImageInspectAllowPrivate)
	}
	if deps.MaxActiveJobs != opts.MaxActiveJobs || deps.ClusterMaxActiveJobs != opts.ClusterMaxActiveJobs {
		t.Errorf("Job limits = %d/%d, want %d/%d", deps.MaxActiveJobs, deps.ClusterMaxActiveJobs, opts.MaxActiveJobs, opts.ClusterMaxActiveJobs)
	}
}
