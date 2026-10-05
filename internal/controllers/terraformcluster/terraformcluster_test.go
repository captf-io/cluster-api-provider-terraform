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

package terraformcluster

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/clock"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// ns is the namespace every test fixture uses.
const ns = "team-a"

var (
	// valid is a well-formed API endpoint fixture.
	valid = clusterv1.APIEndpoint{Host: "cp.example", Port: 6443}
	// tcValid is a well-formed API endpoint fixture distinct from valid, for
	// TerraformCluster.spec.controlPlaneEndpoint cases.
	tcValid = &clusterv1.APIEndpoint{Host: "tc.example", Port: 443}
	// half is an API endpoint fixture with only the host set, so IsValid is
	// false.
	half = clusterv1.APIEndpoint{Host: "cp.example"}
)

// TestEndpointInput proves EndpointInput picks the Cluster's endpoint over
// the TerraformCluster's, falls back correctly, records the source on the
// first apply, and returns null forever once the source is module.
func TestEndpointInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		source     string
		firstApply bool
		cluster    clusterv1.APIEndpoint
		tc         *clusterv1.APIEndpoint
		want       *contract.Endpoint
		wantSet    string
	}{
		{"user-first: Cluster endpoint before the first apply", "", true, valid, nil, &contract.Endpoint{Host: "cp.example", Port: 6443}, EndpointSourceUser},
		{"user-first: TerraformCluster fallback", "", true, clusterv1.APIEndpoint{}, tcValid, &contract.Endpoint{Host: "tc.example", Port: 443}, EndpointSourceUser},
		{"the Cluster's endpoint wins over the TerraformCluster's", "", true, valid, tcValid, &contract.Endpoint{Host: "cp.example", Port: 6443}, EndpointSourceUser},
		{"a half-set Cluster endpoint is not valid", "", true, half, nil, nil, ""},
		{"module-first: no endpoint anywhere", "", true, clusterv1.APIEndpoint{}, nil, nil, ""},
		{"CP provider sets the Cluster endpoint later: input, source unchanged", "", false, valid, nil, &contract.Endpoint{Host: "cp.example", Port: 6443}, ""},
		{"user source keeps feeding the endpoint", EndpointSourceUser, false, valid, nil, &contract.Endpoint{Host: "cp.example", Port: 6443}, ""},
		{"module source: null forever, even after CAPI's copy-back", EndpointSourceModule, false, valid, tcValid, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, set := EndpointInput(tt.source, tt.firstApply, tt.cluster, tt.tc)
			if set != tt.wantSet || (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Errorf("EndpointInput = %+v, %q; want %+v, %q", got, set, tt.want, tt.wantSet)
			}
		})
	}
}

// TestModuleEndpoint proves ModuleEndpoint writes the module's output to
// spec only once, and only when no source is recorded, the last render
// was null, the output is fully set, and spec has no valid endpoint
// already.
func TestModuleEndpoint(t *testing.T) {
	t.Parallel()
	out := &contract.Endpoint{Host: "lb.example", Port: 6443}
	tests := []struct {
		name       string
		source     string
		lastNull   bool
		output     *contract.Endpoint
		spec       *clusterv1.APIEndpoint
		wantWrites bool
	}{
		{"module-first: rendered null, output valid, spec empty", "", true, out, nil, true},
		{"spec holds an invalid endpoint: written", "", true, out, &clusterv1.APIEndpoint{}, true},
		{"already recorded: written once only", EndpointSourceModule, true, out, nil, false},
		{"user source: never written", EndpointSourceUser, true, out, nil, false},
		{"rendered input was an endpoint", "", false, out, nil, false},
		{"output null", "", true, nil, nil, false},
		{"output half-set", "", true, &contract.Endpoint{Host: "lb.example"}, nil, false},
		{"spec already valid", "", true, out, tcValid, false},
	}
	for _, tt := range tests {
		got := ModuleEndpoint(tt.source, tt.lastNull, tt.output, tt.spec)
		if (got != nil) != tt.wantWrites || (got != nil && (got.Host != "lb.example" || got.Port != 6443)) {
			t.Errorf("%s: ModuleEndpoint = %+v", tt.name, got)
		}
	}
}

// testCluster returns a Cluster fixture named "c1" in ns, with
// spec.infrastructureRef naming testTC's TerraformCluster back, and each
// mut applied in order; it returns the built cluster.
func testCluster(mut ...func(*clusterv1.Cluster)) *clusterv1.Cluster {
	c := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c1", UID: "cluster-uid"}}
	c.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{
		APIGroup: infrav1.GroupVersion.Group, Kind: "TerraformCluster", Name: "c1",
	}
	for _, f := range mut {
		f(c)
	}
	return c
}

// testTC returns a TerraformCluster fixture named "c1" in ns, owned by
// cluster "c1" (via the cluster-name label), with each mut applied in
// order; it returns the built object.
func testTC(mut ...func(*infrav1.TerraformCluster)) *infrav1.TerraformCluster {
	tc := &infrav1.TerraformCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c1", UID: "tc-uid", Labels: map[string]string{clusterv1.ClusterNameLabel: "c1"}},
		Spec:       infrav1.TerraformClusterSpec{WorkspaceSpec: infrav1.WorkspaceSpec{Source: infrav1.Source{Image: "registry.example/cluster:1.0"}, IdentityRef: infrav1.IdentityReference{Name: "aws"}}},
	}
	for _, f := range mut {
		f(tc)
	}
	return tc
}

// TestCopyBackKeepsHash proves that once the module owns the endpoint, CAPI
// copying it to the Cluster changes neither the input nor the hash.
func TestCopyBackKeepsHash(t *testing.T) {
	t.Parallel()
	tc := testTC()
	before := testCluster()
	after := testCluster(func(c *clusterv1.Cluster) { c.Spec.ControlPlaneEndpoint = valid })
	h := func(c *clusterv1.Cluster) string {
		t.Helper()
		ep, _ := EndpointInput(EndpointSourceModule, false, c.Spec.ControlPlaneEndpoint, tc.Spec.ControlPlaneEndpoint)
		sum, err := hash.Inputs(contract.RoleCluster, tc.Spec.Source.Image, ClusterInputs(c, tc, false, ep))
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	if h(before) != h(after) {
		t.Error("the copied-back endpoint changed the inputs hash")
	}
}

// TestClusterInputs proves ClusterInputs maps a bare, a partially set and
// a fully set Cluster into the expected common fields, cluster network and
// Kubernetes version, and that control_plane_initialized comes from either
// the Cluster's field or the latched value.
func TestClusterInputs(t *testing.T) {
	t.Parallel()
	tc := testTC(func(tc *infrav1.TerraformCluster) {
		tc.Annotations = map[string]string{clusterv1.TemplateClonedFromNameAnnotation: "tpl"}
	})

	bare := ClusterInputs(testCluster(), tc, false, nil)
	if bare.ClusterNetwork != nil || bare.KubernetesVersion != nil || bare.ControlPlaneInitialized || bare.ControlPlaneEndpoint != nil {
		t.Errorf("bare = %+v", bare)
	}
	if bare.Cluster.Name != "c1" || bare.Object.Kind != "TerraformCluster" || bare.Tags["captf.io/template"] != "tpl" || bare.Contract != contract.Version {
		t.Errorf("common = %+v", bare.CommonInputs)
	}

	partial := ClusterInputs(testCluster(func(c *clusterv1.Cluster) {
		c.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{"10.0.0.0/16"}
		c.Spec.Topology.Version = "v1.36.2"
	}), tc, false, nil)
	raw, err := json.Marshal(partial.ClusterNetwork)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"pods":["10.0.0.0/16"],"services":[],"service_domain":null,"api_server_port":null}` {
		t.Errorf("partial network = %s", raw)
	}
	if partial.KubernetesVersion == nil || *partial.KubernetesVersion != "v1.36.2" {
		t.Errorf("version = %v", partial.KubernetesVersion)
	}

	full := ClusterInputs(testCluster(func(c *clusterv1.Cluster) {
		c.Spec.ClusterNetwork = clusterv1.ClusterNetwork{APIServerPort: 6443, ServiceDomain: "cluster.local"}
	}), tc, false, nil)
	if full.ClusterNetwork == nil || *full.ClusterNetwork.APIServerPort != 6443 || *full.ClusterNetwork.ServiceDomain != "cluster.local" {
		t.Errorf("full network = %+v", full.ClusterNetwork)
	}

	// control_plane_initialized: the Cluster field, or the latched value.
	cpInit := testCluster(func(c *clusterv1.Cluster) { c.Status.Initialization.ControlPlaneInitialized = new(true) })
	if !ClusterInputs(cpInit, tc, false, nil).ControlPlaneInitialized {
		t.Error("Cluster controlPlaneInitialized ignored")
	}
	if !ClusterInputs(testCluster(), tc, true, nil).ControlPlaneInitialized {
		t.Error("latch ignored: a post-move false would destroy gated resources")
	}
}

// --- adapter ----------------------------------------------------------------

// scheme builds a runtime.Scheme with the core, clusterv1 and infrav1
// types this package's tests need, failing t if any fails to register; it
// returns the built scheme.
func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, clusterv1.AddToScheme, infrav1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// newTestAdapter builds an adapter for tc, backed by a fake client seeded
// with tc and objs, failing t on setup error. It returns the adapter and
// its client.
func newTestAdapter(t *testing.T, tc *infrav1.TerraformCluster, objs ...client.Object) (*adapter, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(append([]client.Object{tc}, objs...)...).Build()
	return newAdapter(shared.Deps{Client: c, APIReader: c}, tc), c
}

// owned sets an ownerRef on tc pointing at the fixture Cluster "c1".
func owned(tc *infrav1.TerraformCluster) {
	tc.OwnerReferences = []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster", Name: "c1", UID: "cluster-uid"}}
}

// TestOwner proves adapter.Owner resolves an existing owner Cluster, tells
// a dangling ownerRef apart as owner-gone, and reports no owner at all for
// an object with no ownerRef.
func TestOwner(t *testing.T) {
	t.Parallel()
	a, _ := newTestAdapter(t, testTC(owned), testCluster())
	o, err := a.Owner(t.Context())
	if err != nil || !o.HasOwnerRef || o.Cluster == nil || o.OwnerGone || o.InfraCluster != a.obj {
		t.Errorf("owned: %+v, %v", o, err)
	}
	gone, _ := newTestAdapter(t, testTC(owned))
	if o, err := gone.Owner(t.Context()); err != nil || !o.HasOwnerRef || !o.OwnerGone || o.Cluster != nil {
		t.Errorf("gone: %+v, %v", o, err)
	}
	none, _ := newTestAdapter(t, testTC())
	if o, err := none.Owner(t.Context()); err != nil || o.HasOwnerRef {
		t.Errorf("none: %+v, %v", o, err)
	}
}

// checkMismatch asserts, failing t otherwise, that o carries the
// OwnerMismatch gate and no Cluster: a forged or stale Cluster ownerRef
// must never resolve to a usable owner.
func checkMismatch(t *testing.T, o shared.OwnerInfo) {
	t.Helper()
	if o.Gate == nil || o.Gate.Reason != infrav1.OwnerMismatchReason || o.Gate.Status != metav1.ConditionFalse {
		t.Errorf("owner = %+v, want OwnerMismatch gate", o)
	}
	if o.Cluster != nil {
		t.Errorf("owner = %+v, want no Cluster", o)
	}
}

// TestOwnerForged proves adapter.Owner rejects a Cluster ownerRef whose
// Cluster does not reference this TerraformCluster back: a wrong Kind,
// Name or APIGroup in the Cluster's own infrastructureRef, a UID mismatch
// on the ownerRef, or a cluster-name label that disagrees with the
// resolved Cluster's name. An attacker who can create a TerraformCluster
// can otherwise forge an ownerRef at any existing Cluster in the
// namespace, gaining the manager's own privileges over that Cluster and
// its machines.
func TestOwnerForged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tc   *infrav1.TerraformCluster
		objs []client.Object
	}{
		{"wrong Kind in Cluster.infrastructureRef", testTC(owned), []client.Object{testCluster(func(c *clusterv1.Cluster) {
			c.Spec.InfrastructureRef.Kind = "AWSCluster"
		})}},
		{"wrong Name in Cluster.infrastructureRef", testTC(owned), []client.Object{testCluster(func(c *clusterv1.Cluster) {
			c.Spec.InfrastructureRef.Name = "someone-elses-tc"
		})}},
		{"wrong APIGroup in Cluster.infrastructureRef", testTC(owned), []client.Object{testCluster(func(c *clusterv1.Cluster) {
			c.Spec.InfrastructureRef.APIGroup = "infrastructure.example.com"
		})}},
		{"UID mismatch on the Cluster ownerRef", testTC(func(tc *infrav1.TerraformCluster) {
			tc.OwnerReferences = []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster", Name: "c1", UID: "some-other-cluster-uid"}}
		}), []client.Object{testCluster()}},
		{"cluster-name label disagrees with the owner Cluster's name", testTC(owned, func(tc *infrav1.TerraformCluster) {
			tc.Labels[clusterv1.ClusterNameLabel] = "someone-elses-cluster"
		}), []client.Object{testCluster()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, _ := newTestAdapter(t, tt.tc, tt.objs...)
			o, err := a.Owner(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			checkMismatch(t, o)
		})
	}
}

// TestAdapterContract pins what the cluster adapter tells the shared core:
// mutable (spec/input changes re-apply) and not refreshed faster than the
// normal cadence, unlike the machine adapter.
func TestAdapterContract(t *testing.T) {
	t.Parallel()
	tc := testTC()
	a := newAdapter(shared.Deps{}, tc)
	if a.Kind() != state.KindTerraformCluster || a.Role() != contract.RoleCluster || a.Finalizer() != Finalizer ||
		!a.Mutable() || a.RefreshAfterApply() || a.Object() != tc {
		t.Error("adapter identity")
	}
	if a.Spec().Source.Image != tc.Spec.Source.Image || a.Spec().IdentityRef.Name != "aws" {
		t.Errorf("spec view = %+v", a.Spec())
	}
	st := a.Status()
	st.StateSecretSuffix = "sfx"
	st.ActiveJob.Name = "j"
	if tc.Status.StateSecretSuffix != "sfx" || tc.Status.ActiveJob.Name != "j" {
		t.Error("status pointers do not point into the object")
	}
}

// TestBuildInputsGatesAndUserSource proves adapter.BuildInputs gates on a
// gone or missing owner, and otherwise builds inputs carrying the owner
// Cluster's endpoint while recording the endpoint source as user.
func TestBuildInputsGatesAndUserSource(t *testing.T) {
	t.Parallel()
	a, _ := newTestAdapter(t, testTC())
	if _, gate, _ := a.BuildInputs(t.Context(), shared.OwnerInfo{OwnerGone: true}, nil); gate == nil || gate.Reason != infrav1.OwnerNotFoundReason {
		t.Errorf("owner gone gate = %+v", gate)
	}
	if _, gate, _ := a.BuildInputs(t.Context(), shared.OwnerInfo{}, nil); gate == nil || gate.Reason != infrav1.WaitingForOwnerReason {
		t.Errorf("no owner gate = %+v", gate)
	}

	cl := testCluster(func(c *clusterv1.Cluster) { c.Spec.ControlPlaneEndpoint = valid })
	in, gate, err := a.BuildInputs(t.Context(), shared.OwnerInfo{HasOwnerRef: true, Cluster: cl}, nil)
	if err != nil || gate != nil {
		t.Fatalf("BuildInputs: %v, %+v", err, gate)
	}
	ci, ok := in.(contract.ClusterInputs)
	if !ok || ci.ControlPlaneEndpoint == nil || a.obj.Annotations[EndpointSourceAnnotation] != EndpointSourceUser {
		t.Errorf("inputs %+v, annotations %v", in, a.obj.Annotations)
	}
}

// clusterState returns a cluster state.State with outs as raw JSON output
// values, and an InputsHash set when hashed is true (simulating a
// completed apply).
func clusterState(hashed bool, outs map[string]string) *state.State {
	st := &state.State{Outputs: map[string]state.Output{}}
	if hashed {
		st.InputsHash = "h1:x"
	}
	for k, v := range outs {
		st.Outputs[k] = state.Output{Value: json.RawMessage(v)}
	}
	return st
}

// healthy is a raw health output JSON value describing a running, healthy
// instance.
const healthy = `{"state":"running","healthy":true,"message":null,"reasons":[]}`

// writeDurable writes, through client c, the durable Secret an apply of tc
// that rendered ep leaves, failing t on any error, and returns it as the
// reconcile reads it.
func writeDurable(t *testing.T, c client.Client, tc *infrav1.TerraformCluster, ep *contract.Endpoint) *inputs.Durable {
	t.Helper()
	files, err := render.Root(contract.RoleCluster, ClusterInputs(testCluster(), tc, false, ep))
	if err != nil {
		t.Fatal(err)
	}
	if err := inputs.Write(t.Context(), c, tc, files, inputs.Meta{Image: tc.Spec.Source.Image}); err != nil {
		t.Fatal(err)
	}
	d, err := inputs.Read(t.Context(), c, tc.Namespace, "c", tc.Name)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// recorder records event reasons.
type recorder struct {
	mu      sync.Mutex
	reasons []string
}

// Eventf records reason from an events.EventRecorder.Eventf call.
func (r *recorder) Eventf(_, _ runtime.Object, _, reason, _, _ string, _ ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reasons = append(r.reasons, reason)
}

// TestApplyOutputsModuleEndpoint proves adapter.ApplyOutputs writes the
// module's endpoint to spec once, maps failure_domains (keeping the
// previous list on a violation), sets EndpointAvailable, and emits
// exactly one event per actual change across repeated calls.
func TestApplyOutputsModuleEndpoint(t *testing.T) {
	t.Parallel()
	a, c := newTestAdapter(t, testTC())
	rec := &recorder{}
	a.d.Recorder = rec
	durable := writeDurable(t, c, a.obj, nil) // the apply rendered a null endpoint
	st := clusterState(true, map[string]string{
		"control_plane_endpoint": `{"host":"lb.example","port":6443}`,
		"failure_domains":        `[{"name":"az-1"}]`,
		"health":                 healthy,
	})
	res, health, err := a.ApplyOutputs(t.Context(), shared.OwnerInfo{}, st, durable)
	if err != nil || !res.Valid() || health == nil {
		t.Fatalf("ApplyOutputs: %+v, %v, %v", res, health, err)
	}
	if ep := a.obj.Spec.ControlPlaneEndpoint; ep == nil || ep.Host != "lb.example" || a.obj.Annotations[EndpointSourceAnnotation] != EndpointSourceModule {
		t.Errorf("endpoint %+v, annotations %v", ep, a.obj.Annotations)
	}
	if fds := a.obj.Status.FailureDomains; len(fds) != 1 || fds[0].Name != "az-1" || fds[0].ControlPlane == nil || !*fds[0].ControlPlane {
		t.Errorf("failureDomains = %+v", fds)
	}
	if cond := conditions.Get(a.obj, infrav1.EndpointAvailableCondition); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("EndpointAvailable = %+v", cond)
	}

	// A bad failure_domains output keeps the previous list.
	bad := clusterState(true, map[string]string{"failure_domains": `[{"name":""}]`, "health": healthy})
	if res, _, _ := a.ApplyOutputs(t.Context(), shared.OwnerInfo{}, bad, durable); res.Valid() || len(a.obj.Status.FailureDomains) != 1 {
		t.Errorf("invalid failure domains: valid=%v list=%+v", res.Valid(), a.obj.Status.FailureDomains)
	}
	// The same outputs again: the endpoint is written once, the list is
	// unchanged, so no further event.
	if _, _, err := a.ApplyOutputs(t.Context(), shared.OwnerInfo{}, st, durable); err != nil {
		t.Fatal(err)
	}
	want := []string{shared.EventControlPlaneEndpointSet, shared.EventFailureDomainsChanged}
	if !slices.Equal(rec.reasons, want) {
		t.Errorf("events = %v, want %v", rec.reasons, want)
	}
	two := clusterState(true, map[string]string{"failure_domains": `[{"name":"az-1"},{"name":"az-2"}]`, "health": healthy})
	if _, _, err := a.ApplyOutputs(t.Context(), shared.OwnerInfo{}, two, durable); err != nil {
		t.Fatal(err)
	}
	if n := len(rec.reasons); n != 3 || rec.reasons[2] != shared.EventFailureDomainsChanged {
		t.Errorf("events after a new failure domain = %v", rec.reasons)
	}
}

// TestApplyOutputsExports proves adapter.ApplyOutputs publishes the
// exports output compactly in status.exports (any JSON value, null or
// absent as {}), keeps the previous value on a violation, leaves an
// unchanged value alone, and clears it with one warning when it is over
// the publish limit.
func TestApplyOutputsExports(t *testing.T) {
	t.Parallel()
	a, c := newTestAdapter(t, testTC())
	rec := &recorder{}
	a.d.Recorder = rec
	durable := writeDurable(t, c, a.obj, nil)
	apply := func(exports string) {
		t.Helper()
		outs := map[string]string{"health": healthy}
		if exports != "" {
			outs["exports"] = exports
		}
		if _, _, err := a.ApplyOutputs(t.Context(), shared.OwnerInfo{}, clusterState(true, outs), durable); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want string) {
		t.Helper()
		if got := string(a.obj.Status.Exports.Raw); got != want {
			t.Errorf("status.exports = %q, want %q", got, want)
		}
	}

	apply("{ \"a\": 1,\n \"b\": [true, null] }")
	check(`{"a":1,"b":[true,null]}`)
	apply(`["x", 2]`)
	check(`["x",2]`)
	apply(`"s"`)
	check(`"s"`)
	apply(`null`)
	check(`{}`)
	apply("")
	check(`{}`)
	apply(`{"k":"v"}`)
	check(`{"k":"v"}`)

	// A fractional number violates the contract: the previous value stays.
	if res, _, _ := a.ApplyOutputs(t.Context(), shared.OwnerInfo{}, clusterState(true, map[string]string{"exports": `{"k":1.5}`, "health": healthy}), durable); res.Valid() {
		t.Error("a fractional exports output is valid")
	}
	check(`{"k":"v"}`)

	// An unchanged value keeps the same bytes and emits nothing.
	before := a.obj.Status.Exports.Raw
	apply(`{ "k": "v" }`)
	if &before[0] != &a.obj.Status.Exports.Raw[0] {
		t.Error("an unchanged exports value was rewritten")
	}
	if len(rec.reasons) != 0 {
		t.Errorf("events = %v, want none", rec.reasons)
	}

	// Over the limit: cleared, one warning, none on the next pass.
	big, err := json.Marshal(map[string]string{"blob": strings.Repeat("x", infrav1.MaxPublishedExportsBytes)})
	if err != nil {
		t.Fatal(err)
	}
	apply(string(big))
	if len(a.obj.Status.Exports.Raw) != 0 {
		t.Errorf("oversize exports published: %d bytes", len(a.obj.Status.Exports.Raw))
	}
	apply(string(big))
	if want := []string{shared.EventExportsNotPublished}; !slices.Equal(rec.reasons, want) {
		t.Errorf("events = %v, want %v", rec.reasons, want)
	}
}

// TestApplyOutputsEndpointAvailable proves adapter.ApplyOutputs sets
// EndpointAvailable False/WaitingForEndpoint when provisioned with no
// endpoint, True when the owner Cluster's endpoint is valid, and leaves
// the condition unset before provisioning.
func TestApplyOutputsEndpointAvailable(t *testing.T) {
	t.Parallel()
	// Provisioned without any endpoint: WaitingForEndpoint.
	a, c := newTestAdapter(t, testTC())
	durable := writeDurable(t, c, a.obj, nil)
	if _, _, err := a.ApplyOutputs(t.Context(), shared.OwnerInfo{}, clusterState(true, map[string]string{"health": healthy}), durable); err != nil {
		t.Fatal(err)
	}
	if cond := conditions.Get(a.obj, infrav1.EndpointAvailableCondition); cond == nil || cond.Reason != infrav1.WaitingForEndpointReason {
		t.Errorf("EndpointAvailable = %+v", cond)
	}
	// The owner Cluster's valid endpoint counts, passed in rather than
	// remembered from an earlier Owner call.
	withEP := shared.OwnerInfo{Cluster: testCluster(func(c *clusterv1.Cluster) { c.Spec.ControlPlaneEndpoint = valid })}
	if _, _, err := a.ApplyOutputs(t.Context(), withEP, clusterState(true, map[string]string{"health": healthy}), durable); err != nil {
		t.Fatal(err)
	}
	if cond := conditions.Get(a.obj, infrav1.EndpointAvailableCondition); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("EndpointAvailable with the Cluster's endpoint = %+v", cond)
	}
	// Not provisioned (no hash) and no endpoint: unset.
	b, _ := newTestAdapter(t, testTC())
	res, health, err := b.ApplyOutputs(t.Context(), shared.OwnerInfo{}, clusterState(false, map[string]string{}), nil)
	if err != nil || health != nil || res.Valid() {
		t.Fatalf("empty state: %+v, %v, %v", res, health, err)
	}
	if conditions.Has(b.obj, infrav1.EndpointAvailableCondition) {
		t.Error("EndpointAvailable set before provisioning")
	}
}

// TestDeletionBlocked proves adapter.DeletionBlocked blocks while a
// same-cluster TerraformMachine exists, unblocks once it is gone, and
// never blocks a TerraformCluster with no cluster name to match against.
func TestDeletionBlocked(t *testing.T) {
	t.Parallel()
	machine := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: "m1", Labels: map[string]string{clusterv1.ClusterNameLabel: "c1"},
	}}
	other := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: "m2", Labels: map[string]string{clusterv1.ClusterNameLabel: "other"},
	}}
	a, c := newTestAdapter(t, testTC(), machine, other)
	blocked, err := a.DeletionBlocked(t.Context(), shared.OwnerInfo{})
	if err != nil || !blocked {
		t.Fatalf("with a machine: %v, %v", blocked, err)
	}
	if cond := conditions.Get(a.obj, infrav1.DeletionBlockedCondition); cond == nil || cond.Reason != infrav1.DependentsExistReason {
		t.Errorf("DeletionBlocked = %+v", cond)
	}
	if err := c.Delete(t.Context(), machine); err != nil {
		t.Fatal(err)
	}
	if blocked, err := a.DeletionBlocked(t.Context(), shared.OwnerInfo{}); err != nil || blocked {
		t.Errorf("after the machine is gone: %v, %v", blocked, err)
	}
	if cond := conditions.Get(a.obj, infrav1.DeletionBlockedCondition); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("DeletionBlocked = %+v", cond)
	}
	// No cluster name at all: nothing can reference the cluster.
	nameless, _ := newTestAdapter(t, testTC(func(tc *infrav1.TerraformCluster) { tc.Labels = nil }), machine)
	if blocked, _ := nameless.DeletionBlocked(t.Context(), shared.OwnerInfo{}); blocked {
		t.Error("blocked without a cluster name")
	}
}

// TestDeletionBlockedPools proves adapter.DeletionBlocked also blocks on a
// same-cluster TerraformMachinePool, counts and names machines and pools
// apart in the message (sorted, at most three names per kind), and
// ignores pools of another cluster.
func TestDeletionBlockedPools(t *testing.T) {
	t.Parallel()
	lbl := func(cluster string) map[string]string { return map[string]string{clusterv1.ClusterNameLabel: cluster} }
	machine := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "m1", Labels: lbl("c1")}}
	pool := &infrav1.TerraformMachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p1", Labels: lbl("c1")}}
	otherPool := &infrav1.TerraformMachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p9", Labels: lbl("other")}}
	// named returns a copy of o named name.
	named := func(o client.Object, name string) client.Object {
		c, _ := o.DeepCopyObject().(client.Object)
		c.SetName(name)
		return c
	}
	tests := []struct {
		name    string
		objs    []client.Object
		blocked bool
		msg     string
	}{
		{"a pool alone blocks", []client.Object{pool, otherPool}, true, "1 TerraformMachinePool(s) (p1) of cluster c1 still exist"},
		{"machines and pools are counted", []client.Object{machine, pool}, true, "1 TerraformMachine(s) (m1) and 1 TerraformMachinePool(s) (p1) of cluster c1 still exist"},
		{
			"names are sorted, three per kind", []client.Object{
				named(machine, "m4"), named(machine, "m2"), machine, named(machine, "m5"), named(machine, "m3"),
				named(pool, "p3"), pool, named(pool, "p2"),
			}, true,
			"5 TerraformMachine(s) (m1, m2, m3 and 2 more) and 3 TerraformMachinePool(s) (p1, p2, p3) of cluster c1 still exist",
		},
		{"another cluster's pool does not block", []client.Object{otherPool}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// The fake client writes to its objects: give each subtest copies.
			objs := make([]client.Object, 0, len(tt.objs))
			for _, o := range tt.objs {
				objs = append(objs, o.DeepCopyObject().(client.Object))
			}
			a, _ := newTestAdapter(t, testTC(), objs...)
			blocked, err := a.DeletionBlocked(t.Context(), shared.OwnerInfo{})
			if err != nil || blocked != tt.blocked {
				t.Fatalf("DeletionBlocked = %v, %v; want %v", blocked, err, tt.blocked)
			}
			if cond := conditions.Get(a.obj, infrav1.DeletionBlockedCondition); cond == nil || cond.Message != tt.msg {
				t.Errorf("DeletionBlocked condition = %+v, want message %q", cond, tt.msg)
			}
		})
	}
	boom := errors.New("boom")
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(testTC()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			if _, ok := l.(*infrav1.TerraformMachinePoolList); ok {
				return boom
			}
			return c.List(ctx, l, opts...)
		},
	}).Build()
	a := newAdapter(shared.Deps{Client: c, APIReader: c}, testTC())
	if _, err := a.DeletionBlocked(t.Context(), shared.OwnerInfo{}); !errors.Is(err, boom) {
		t.Errorf("pool list error = %v, want boom", err)
	}
}

// TestDeletionBlockedClusterNameFallback: without the cluster-name label,
// the cluster name comes from the owner Cluster (delete.go's fallback), and
// deletion blocks exactly on TerraformMachines labelled with that name.
func TestDeletionBlockedClusterNameFallback(t *testing.T) {
	t.Parallel()
	nameless := func(tc *infrav1.TerraformCluster) { tc.Labels = nil }
	machine := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: "m1", Labels: map[string]string{clusterv1.ClusterNameLabel: "c1"},
	}}

	a, _ := newTestAdapter(t, testTC(nameless), machine)
	owner := shared.OwnerInfo{Cluster: testCluster()} // Name "c1"
	blocked, err := a.DeletionBlocked(t.Context(), owner)
	if err != nil || !blocked {
		t.Errorf("fallback to owner.Cluster.Name: blocked=%v, err=%v", blocked, err)
	}
	if cond := conditions.Get(a.obj, infrav1.DeletionBlockedCondition); cond == nil || cond.Reason != infrav1.DependentsExistReason {
		t.Errorf("DeletionBlocked condition = %+v", cond)
	}

	b, _ := newTestAdapter(t, testTC(nameless))
	if blocked, err := b.DeletionBlocked(t.Context(), owner); err != nil || blocked {
		t.Errorf("no matching machines: blocked=%v, err=%v", blocked, err)
	}
}

// TestAdapterReadsNoSecret: the adapter works from the durable Secret the
// reconcile read once and hands in; it never reads the Secret itself (the
// client fails every Secret read). A durable Secret means an apply ran, so
// an endpoint present now is not recorded as the user's.
func TestAdapterReadsNoSecret(t *testing.T) {
	t.Parallel()
	tc := testTC()
	durable := writeDurable(t, fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(tc.DeepCopy()).Build(), tc.DeepCopy(), nil)
	failing := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(tc, testCluster()).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return errors.New("etcd unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	a := newAdapter(shared.Deps{Client: failing, APIReader: failing}, tc)

	cl := testCluster(func(c *clusterv1.Cluster) { c.Spec.ControlPlaneEndpoint = valid })
	if _, gate, err := a.BuildInputs(t.Context(), shared.OwnerInfo{HasOwnerRef: true, Cluster: cl}, durable); err != nil || gate != nil {
		t.Errorf("BuildInputs = %+v, %v", gate, err)
	}
	if src := a.obj.Annotations[EndpointSourceAnnotation]; src != "" {
		t.Errorf("endpoint source %q recorded after the first apply", src)
	}
	if _, _, err := a.ApplyOutputs(t.Context(), shared.OwnerInfo{}, clusterState(true, map[string]string{"health": healthy}), durable); err != nil {
		t.Errorf("ApplyOutputs = %v", err)
	}
}

// TestReconcileNotFound proves Reconcile returns no error and no requeue
// for a request naming a TerraformCluster that does not exist.
func TestReconcileNotFound(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).Build()
	r := &Reconciler{Deps: shared.Deps{Client: c}}
	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "gone"}})
	if err != nil || res.RequeueAfter != 0 {
		t.Errorf("Reconcile = %+v, %v", res, err)
	}
}

// TestReconcileFirstVisit runs the whole flow once on a fresh object: the
// finalizer is added and the reconcile stops there.
func TestReconcileFirstVisit(t *testing.T) {
	t.Parallel()
	tc := testTC(owned)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(tc, testCluster(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}).WithStatusSubresource(tc).Build()
	r := &Reconciler{Deps: shared.Deps{Client: c, APIReader: c, Clock: clock.RealClock{}, DriftDefault: time.Hour}}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tc)}); err != nil {
		t.Fatal(err)
	}
	got := &infrav1.TerraformCluster{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(tc), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 1 || got.Finalizers[0] != Finalizer {
		t.Errorf("finalizers = %v", got.Finalizers)
	}
}

// TestBuildInputsEndpointSourceTiming proves a gated pass does not record
// the user endpoint source, the pass that builds the first-apply inputs
// does, and a user source whose endpoint is gone before any apply stored
// it is dropped so the module's endpoint is adopted.
func TestBuildInputsEndpointSourceTiming(t *testing.T) {
	t.Parallel()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "net", Labels: map[string]string{infrav1.VariablesSourceLabel: "true"}},
		Data:       map[string]string{"control_plane_initialized": "true"},
	}
	tc := testTC(func(tc *infrav1.TerraformCluster) {
		tc.Spec.VariablesFrom = []infrav1.VariablesSource{{ConfigMapRef: infrav1.VariablesSourceReference{Name: "net"}}}
	})
	withEP := shared.OwnerInfo{HasOwnerRef: true, Cluster: testCluster(func(c *clusterv1.Cluster) { c.Spec.ControlPlaneEndpoint = valid })}

	gated, _ := newTestAdapter(t, tc, cm)
	if _, gate, _ := gated.BuildInputs(t.Context(), withEP, nil); gate == nil || gated.obj.Annotations[EndpointSourceAnnotation] != "" {
		t.Errorf("gated pass: gate %+v, annotations %v", gate, gated.obj.Annotations)
	}

	ok, _ := newTestAdapter(t, testTC())
	if _, gate, err := ok.BuildInputs(t.Context(), withEP, nil); gate != nil || err != nil || ok.obj.Annotations[EndpointSourceAnnotation] != EndpointSourceUser {
		t.Errorf("first-apply pass: %+v, %v, annotations %v", gate, err, ok.obj.Annotations)
	}

	stale, _ := newTestAdapter(t, testTC(func(tc *infrav1.TerraformCluster) {
		tc.Annotations = map[string]string{EndpointSourceAnnotation: EndpointSourceUser}
	}))
	in, gate, err := stale.BuildInputs(t.Context(), shared.OwnerInfo{HasOwnerRef: true, Cluster: testCluster()}, nil)
	if gate != nil || err != nil {
		t.Fatalf("BuildInputs: %+v, %v", gate, err)
	}
	if ci := in.(contract.ClusterInputs); ci.ControlPlaneEndpoint != nil || stale.obj.Annotations[EndpointSourceAnnotation] != "" {
		t.Errorf("stale user source kept: %+v, %v", ci.ControlPlaneEndpoint, stale.obj.Annotations)
	}
	out := &contract.Endpoint{Host: "m.example", Port: 6443}
	if ep := ModuleEndpoint(stale.obj.Annotations[EndpointSourceAnnotation], true, out, nil); ep == nil || ep.Host != "m.example" {
		t.Errorf("module endpoint not adopted: %+v", ep)
	}
}
