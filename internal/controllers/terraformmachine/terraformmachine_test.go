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

package terraformmachine

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
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

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// ns is the namespace every test fixture uses.
const ns = "team-a"

// gzipped gzip-compresses s, failing t on any error; it returns the
// compressed bytes.
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

// testCluster returns a provisioned Cluster fixture named "c1" in ns,
// pointing its infrastructureRef at a TerraformCluster, with each mut
// applied in order; it returns the built cluster.
func testCluster(mut ...func(*clusterv1.Cluster)) *clusterv1.Cluster {
	c := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c1"},
		Spec: clusterv1.ClusterSpec{InfrastructureRef: clusterv1.ContractVersionedObjectReference{
			APIGroup: infrav1.GroupVersion.Group, Kind: "TerraformCluster", Name: "c1",
		}},
	}
	c.Status.Initialization.InfrastructureProvisioned = new(true)
	for _, f := range mut {
		f(c)
	}
	return c
}

// testMachine returns a Machine fixture in ns, on cluster "c1", with a
// bootstrap data Secret name set and spec.infrastructureRef naming testTM's
// TerraformMachine back, and each mut applied in order; it returns the
// built machine.
func testMachine(mut ...func(*clusterv1.Machine)) *clusterv1.Machine {
	m := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c1-md-0-abc", UID: "machine-uid"}}
	m.Spec.ClusterName = "c1"
	m.Spec.Bootstrap.DataSecretName = new("bootstrap-abc")
	m.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{
		APIGroup: infrav1.GroupVersion.Group, Kind: "TerraformMachine", Name: "c1-md-0-xyz",
	}
	for _, f := range mut {
		f(m)
	}
	return m
}

// testTM returns a TerraformMachine fixture in ns, owned by the fixture
// Machine, with each mut applied in order; it returns the built object.
func testTM(mut ...func(*infrav1.TerraformMachine)) *infrav1.TerraformMachine {
	tm := &infrav1.TerraformMachine{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "c1-md-0-xyz", UID: "tm-uid",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "c1"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: "c1-md-0-abc", UID: "machine-uid",
			}},
		},
		Spec: infrav1.TerraformMachineSpec{WorkspaceSpec: infrav1.WorkspaceSpec{Source: infrav1.Source{Image: "registry.example/machine:1.0"}, IdentityRef: infrav1.IdentityReference{Name: "aws"}}},
	}
	for _, f := range mut {
		f(tm)
	}
	return tm
}

// bootstrapSecret returns a bootstrap data Secret fixture in ns, named to
// match testMachine, holding data.
func bootstrapSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bootstrap-abc"}, Data: data}
}

// TestMachineInputs proves MachineInputs base64-encodes the raw bootstrap
// value, defaults bootstrap_data's format and control_plane, maps the
// common fields and cluster outputs verbatim, and derives control_plane,
// bootstrap format, failure domain and Kubernetes version correctly for a
// control-plane machine; and that BootstrapReady requires a non-empty
// value.
func TestMachineInputs(t *testing.T) {
	t.Parallel()
	payload := gzipped(t, "#cloud-config\nruncmd: [echo hi]\n")
	secret := bootstrapSecret(map[string][]byte{"value": payload})
	tm := testTM(func(tm *infrav1.TerraformMachine) {
		tm.Annotations = map[string]string{clusterv1.TemplateClonedFromNameAnnotation: "tpl"}
	})
	in := MachineInputs(testCluster(), testMachine(), tm, json.RawMessage(`{"net":"n-1"}`), secret)
	raw, err := base64.StdEncoding.DecodeString(in.BootstrapData)
	if err != nil || !bytes.Equal(raw, payload) {
		t.Errorf("bootstrap_data does not round-trip the gzip bytes: %v", err)
	}
	if in.BootstrapFormat != "cloud-config" || in.ControlPlane || in.FailureDomain != nil || in.KubernetesVersion != nil {
		t.Errorf("defaults = %+v", in)
	}
	if string(in.ClusterOutputs) != `{"net":"n-1"}` || in.MachineName != "c1-md-0-abc" || in.Tags["captf.io/template"] != "tpl" ||
		in.Object.Kind != "TerraformMachine" || in.Cluster.Name != "c1" {
		t.Errorf("inputs = %+v", in)
	}

	cp := testMachine(func(m *clusterv1.Machine) {
		m.Labels = map[string]string{clusterv1.MachineControlPlaneLabel: ""}
		m.Spec.FailureDomain = "az-1"
		m.Spec.Version = "v1.36.2"
	})
	ign := bootstrapSecret(map[string][]byte{"value": []byte("{}"), "format": []byte("ignition")})
	in = MachineInputs(testCluster(), cp, tm, json.RawMessage(`{}`), ign)
	if !in.ControlPlane || in.BootstrapFormat != "ignition" || *in.FailureDomain != "az-1" || *in.KubernetesVersion != "v1.36.2" {
		t.Errorf("control-plane inputs = %+v", in)
	}
	if shared.BootstrapReady(bootstrapSecret(map[string][]byte{"format": []byte("x")})) || shared.BootstrapReady(nil) {
		t.Error("a Secret without value is ready")
	}
}

// TestCheckGates proves CheckGates returns nil when every gate is met,
// returns the first unmet gate's reason in declared precedence order
// otherwise, and reports OwnerNotFound as ConditionFalse.
func TestCheckGates(t *testing.T) {
	t.Parallel()
	all := GateInput{ClusterFound: true, InfrastructureProvisioned: true, InfraClusterFound: true, ExportsReady: true, BootstrapReady: true}
	with := func(f func(*GateInput)) GateInput { in := all; f(&in); return in }
	tests := []struct {
		name   string
		in     GateInput
		reason string
	}{
		{"all met", all, ""},
		{"Machine gone", with(func(in *GateInput) { in.MachineGone = true }), infrav1.OwnerNotFoundReason},
		{"no Cluster", with(func(in *GateInput) { in.ClusterFound = false }), infrav1.WaitingForOwnerReason},
		{"infrastructure not provisioned", with(func(in *GateInput) { in.InfrastructureProvisioned = false }), infrav1.WaitingForClusterInfrastructureReason},
		{"TerraformCluster missing", with(func(in *GateInput) { in.InfraClusterFound = false }), infrav1.WaitingForClusterInfrastructureReason},
		{"exports unreadable", with(func(in *GateInput) { in.ExportsReady = false }), infrav1.WaitingForClusterExportsReason},
		{"no bootstrap data", with(func(in *GateInput) { in.BootstrapReady = false }), infrav1.WaitingForBootstrapDataReason},
		{"order: infrastructure before bootstrap", with(func(in *GateInput) { in.InfrastructureProvisioned, in.BootstrapReady = false, false }), infrav1.WaitingForClusterInfrastructureReason},
	}
	for _, tt := range tests {
		g := CheckGates(tt.in)
		switch {
		case tt.reason == "" && g != nil:
			t.Errorf("%s: gate %+v", tt.name, g)
		case tt.reason != "" && (g == nil || g.Reason != tt.reason):
			t.Errorf("%s: gate %+v, want %s", tt.name, g, tt.reason)
		}
	}
	if g := CheckGates(with(func(in *GateInput) { in.MachineGone = true })); g.Status != metav1.ConditionFalse {
		t.Errorf("OwnerNotFound status = %s", g.Status)
	}
}

// --- adapter ----------------------------------------------------------------

// stateReader is a state.Reader stub returning a fixed state or error, or
// state.ErrNoState when neither is set.
type stateReader struct {
	st  *state.State
	err error
}

// Read returns r's fixed state and error, or state.ErrNoState if r has
// neither.
func (r stateReader) Read(context.Context, string, string) (*state.State, error) {
	if r.st == nil && r.err == nil {
		return nil, state.ErrNoState
	}
	return r.st, r.err
}

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

// testTC returns a TerraformCluster fixture named "c1" in ns.
func testTC() *infrav1.TerraformCluster {
	return &infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c1"}}
}

// newTestAdapter builds an adapter for tm, backed by a fake client seeded
// with tm and objs and a state reader sr, failing t on setup error. It
// returns the built adapter.
func newTestAdapter(t *testing.T, tm *infrav1.TerraformMachine, sr stateReader, objs ...client.Object) *adapter {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(append([]client.Object{tm}, objs...)...).Build()
	return newAdapter(shared.Deps{Client: c, APIReader: c, State: sr}, tm)
}

// checkMismatch asserts, failing t otherwise, that o carries the
// OwnerMismatch gate and no Machine, Cluster or InfraCluster: a forged or
// stale Machine ownerRef must never resolve to a usable owner.
func checkMismatch(t *testing.T, o shared.OwnerInfo) {
	t.Helper()
	if o.Gate == nil || o.Gate.Reason != infrav1.OwnerMismatchReason || o.Gate.Status != metav1.ConditionFalse {
		t.Errorf("owner = %+v, want OwnerMismatch gate", o)
	}
	if o.Machine != nil || o.Cluster != nil || o.InfraCluster != nil {
		t.Errorf("owner = %+v, want no Machine/Cluster/InfraCluster", o)
	}
}

// TestOwner proves adapter.Owner resolves the full Machine/Cluster/
// TerraformCluster chain, and tells apart every gated or partial case:
// a control-plane-only ownerRef, no ownerRef, a gone Machine, a missing
// cluster-name label, a missing Cluster, a non-TerraformCluster
// infrastructureRef, a missing TerraformCluster, and an unset
// infrastructureRef.
func TestOwner(t *testing.T) {
	t.Parallel()
	foreign := testCluster(func(c *clusterv1.Cluster) {
		c.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "AWSCluster", Name: "c1"}
	})
	cpOnly := func(tm *infrav1.TerraformMachine) {
		tm.OwnerReferences = []metav1.OwnerReference{{APIVersion: "controlplane.cluster.x-k8s.io/v1beta2", Kind: "KubeadmControlPlane", Name: "kcp", UID: "kcp-uid"}}
	}
	tests := []struct {
		name  string
		tm    *infrav1.TerraformMachine
		objs  []client.Object
		check func(*testing.T, shared.OwnerInfo)
	}{
		{"full chain", testTM(), []client.Object{testMachine(), testCluster(), testTC()}, func(t *testing.T, o shared.OwnerInfo) {
			if !o.HasOwnerRef || o.Machine == nil || o.Cluster == nil || o.InfraCluster == nil || o.Gate != nil {
				t.Errorf("owner = %+v", o)
			}
		}},
		{"KCP-only ownerRef: WaitingForOwnerMachine", testTM(cpOnly), nil, func(t *testing.T, o shared.OwnerInfo) {
			if o.HasOwnerRef || o.Gate == nil || o.Gate.Reason != infrav1.WaitingForOwnerMachineReason || o.Gate.Status != metav1.ConditionFalse {
				t.Errorf("owner = %+v", o)
			}
		}},
		{"no ownerRef at all", testTM(func(tm *infrav1.TerraformMachine) { tm.OwnerReferences = nil }), nil, func(t *testing.T, o shared.OwnerInfo) {
			if o.HasOwnerRef || o.Gate != nil {
				t.Errorf("owner = %+v", o)
			}
		}},
		{"Machine gone: owner gone, Cluster still resolved", testTM(), []client.Object{testCluster(), testTC()}, func(t *testing.T, o shared.OwnerInfo) {
			if !o.HasOwnerRef || !o.OwnerGone || o.Machine != nil || o.Cluster == nil || o.InfraCluster == nil {
				t.Errorf("owner = %+v", o)
			}
		}},
		{"no cluster-name label yet", testTM(func(tm *infrav1.TerraformMachine) { tm.Labels = nil }), []client.Object{testMachine()}, func(t *testing.T, o shared.OwnerInfo) {
			if o.Machine == nil || o.Cluster != nil {
				t.Errorf("owner = %+v", o)
			}
		}},
		{"Cluster missing", testTM(), []client.Object{testMachine()}, func(t *testing.T, o shared.OwnerInfo) {
			if o.Cluster != nil {
				t.Errorf("owner = %+v", o)
			}
		}},
		{"foreign InfraCluster: ClusterNotTerraform", testTM(), []client.Object{testMachine(), foreign}, func(t *testing.T, o shared.OwnerInfo) {
			if o.Gate == nil || o.Gate.Reason != infrav1.ClusterNotTerraformReason || !o.HasOwnerRef {
				t.Errorf("owner = %+v", o)
			}
		}},
		{"TerraformCluster missing", testTM(), []client.Object{testMachine(), testCluster()}, func(t *testing.T, o shared.OwnerInfo) {
			if o.Cluster == nil || o.InfraCluster != nil || o.Gate != nil {
				t.Errorf("owner = %+v", o)
			}
		}},
		{"infrastructureRef not set yet", testTM(), []client.Object{testMachine(), testCluster(func(c *clusterv1.Cluster) {
			c.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{}
		})}, func(t *testing.T, o shared.OwnerInfo) {
			if o.Cluster == nil || o.InfraCluster != nil || o.Gate != nil {
				t.Errorf("owner = %+v", o)
			}
		}},
		{"forged: wrong Kind in Machine.infrastructureRef", testTM(), []client.Object{testMachine(func(m *clusterv1.Machine) {
			m.Spec.InfrastructureRef.Kind = "AWSMachine"
		}), testCluster(), testTC()}, checkMismatch},
		{"forged: wrong Name in Machine.infrastructureRef", testTM(), []client.Object{testMachine(func(m *clusterv1.Machine) {
			m.Spec.InfrastructureRef.Name = "someone-elses-tm"
		}), testCluster(), testTC()}, checkMismatch},
		{"forged: wrong APIGroup in Machine.infrastructureRef", testTM(), []client.Object{testMachine(func(m *clusterv1.Machine) {
			m.Spec.InfrastructureRef.APIGroup = "infrastructure.example.com"
		}), testCluster(), testTC()}, checkMismatch},
		{"forged: UID mismatch on the Machine ownerRef", testTM(func(tm *infrav1.TerraformMachine) {
			tm.OwnerReferences[0].UID = "some-other-machine-uid"
		}), []client.Object{testMachine(), testCluster(), testTC()}, checkMismatch},
		{"forged: cluster-name label disagrees with the Machine's", testTM(func(tm *infrav1.TerraformMachine) {
			tm.Labels[clusterv1.ClusterNameLabel] = "someone-elses-cluster"
		}), []client.Object{testMachine(), testCluster(), testTC()}, checkMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := newTestAdapter(t, tt.tm, stateReader{}, tt.objs...)
			o, err := a.Owner(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, o)
		})
	}
}

// TestExportsNilCluster: no TerraformCluster yet (owner.InfraCluster is
// nil) reads as nil exports, no error — a wait, not a failure.
func TestExportsNilCluster(t *testing.T) {
	t.Parallel()
	a := newTestAdapter(t, testTM(), stateReader{})
	out, err := a.exports(t.Context(), nil)
	if err != nil || out != nil {
		t.Errorf("exports(nil) = %v, %v; want nil, nil", out, err)
	}
}

// clusterState returns a cluster state.State fixture with exports as the
// raw exports output and a fixed healthy health output.
func clusterState(exports string) *state.State {
	return &state.State{InputsHash: "h1:c", Outputs: map[string]state.Output{
		"exports": {Value: json.RawMessage(exports)},
		"health":  {Value: json.RawMessage(`{"state":"running","healthy":true,"message":null,"reasons":[]}`)},
	}}
}

// TestBuildInputs proves adapter.BuildInputs builds machine inputs
// carrying the cluster's exports (or {} for an externally managed
// TerraformCluster) once every gate is met, and gates correctly on an
// unread or unhashable cluster state, a missing or empty bootstrap
// Secret, an unprovisioned cluster, a gone Machine, or no Cluster.
func TestBuildInputs(t *testing.T) {
	t.Parallel()
	owner := func() shared.OwnerInfo {
		return shared.OwnerInfo{HasOwnerRef: true, Machine: testMachine(), Cluster: testCluster(), InfraCluster: testTC()}
	}
	ready := bootstrapSecret(map[string][]byte{"value": []byte("#cloud-config\n")})
	tests := []struct {
		name   string
		owner  shared.OwnerInfo
		sr     stateReader
		objs   []client.Object
		reason string
		check  func(*testing.T, contract.MachineInputs)
	}{
		{"full chain", owner(), stateReader{st: clusterState(`{"vpc":"v-1"}`)}, []client.Object{ready}, "", func(t *testing.T, in contract.MachineInputs) {
			if string(in.ClusterOutputs) != `{"vpc":"v-1"}` {
				t.Errorf("captf_cluster_outputs = %s", in.ClusterOutputs)
			}
		}},
		{"externally managed TerraformCluster: exports {}", func() shared.OwnerInfo {
			o := owner()
			o.InfraCluster.Annotations = map[string]string{clusterv1.ManagedByAnnotation: "x"}
			return o
		}(), stateReader{}, []client.Object{ready}, "", func(t *testing.T, in contract.MachineInputs) {
			if string(in.ClusterOutputs) != `{}` {
				t.Errorf("captf_cluster_outputs = %s", in.ClusterOutputs)
			}
		}},
		{"cluster state not written yet", owner(), stateReader{}, []client.Object{ready}, infrav1.WaitingForClusterExportsReason, nil},
		{"cluster state mid-write", owner(), stateReader{err: state.ErrStateInconsistent}, []client.Object{ready}, infrav1.WaitingForClusterExportsReason, nil},
		{"exports not hashable", owner(), stateReader{st: clusterState(`{"ratio":0.5}`)}, []client.Object{ready}, infrav1.WaitingForClusterExportsReason, nil},
		{"bootstrap Secret missing", owner(), stateReader{st: clusterState(`{}`)}, nil, infrav1.WaitingForBootstrapDataReason, nil},
		{"dataSecretName empty", func() shared.OwnerInfo {
			o := owner()
			o.Machine.Spec.Bootstrap.DataSecretName = new("")
			return o
		}(), stateReader{st: clusterState(`{}`)}, []client.Object{ready}, infrav1.WaitingForBootstrapDataReason, nil},
		{"infrastructure not provisioned", func() shared.OwnerInfo {
			o := owner()
			o.Cluster.Status.Initialization.InfrastructureProvisioned = nil
			return o
		}(), stateReader{st: clusterState(`{}`)}, []client.Object{ready}, infrav1.WaitingForClusterInfrastructureReason, nil},
		{"Machine gone", shared.OwnerInfo{HasOwnerRef: true, OwnerGone: true, Cluster: testCluster()}, stateReader{}, nil, infrav1.OwnerNotFoundReason, nil},
		{"no Cluster", shared.OwnerInfo{HasOwnerRef: true, Machine: testMachine()}, stateReader{}, nil, infrav1.WaitingForOwnerReason, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			objs := make([]client.Object, 0, len(tt.objs))
			for _, o := range tt.objs {
				objs = append(objs, o.DeepCopyObject().(client.Object))
			}
			a := newTestAdapter(t, testTM(), tt.sr, objs...)
			in, gate, err := a.BuildInputs(t.Context(), tt.owner, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.reason != "" {
				if gate == nil || gate.Reason != tt.reason {
					t.Errorf("gate = %+v, want %s", gate, tt.reason)
				}
				return
			}
			mi, ok := in.(contract.MachineInputs)
			if gate != nil || !ok {
				t.Fatalf("gate %+v, inputs %T", gate, in)
			}
			tt.check(t, mi)
		})
	}
}

// TestBuildInputsClusterStateReadFails: a genuine cluster state read
// failure (not the "not written yet"/"mid-write" waits) propagates as a
// BuildInputs error instead of a gate.
func TestBuildInputsClusterStateReadFails(t *testing.T) {
	t.Parallel()
	owner := shared.OwnerInfo{HasOwnerRef: true, Machine: testMachine(), Cluster: testCluster(), InfraCluster: testTC()}
	ready := bootstrapSecret(map[string][]byte{"value": []byte("#cloud-config\n")})
	a := newTestAdapter(t, testTM(), stateReader{err: errors.New("etcd unavailable")}, ready)
	if _, _, err := a.BuildInputs(t.Context(), owner, nil); err == nil || !strings.Contains(err.Error(), "etcd unavailable") {
		t.Errorf("BuildInputs = %v, want it to propagate a genuine cluster state read error", err)
	}
}

// machineState returns a machine state.State with outs as raw output
// values and a fixed InputsHash marking it as applied.
func machineState(outs map[string]string) *state.State {
	st := &state.State{InputsHash: "h1:m", Outputs: map[string]state.Output{}}
	for k, v := range outs {
		st.Outputs[k] = state.Output{Value: json.RawMessage(v)}
	}
	return st
}

// healthy is a raw health output JSON value describing a running, healthy
// instance.
const healthy = `{"state":"running","healthy":true,"message":null,"reasons":[]}`

// TestApplyOutputs proves adapter.ApplyOutputs sets spec.providerID once
// and flags a later change as a violation, maps addresses, failure domain
// and interruptible (keeping the previous value on a placement or
// addresses violation), reports a terminated instance only for a null
// provider_id after provisioning (never for a malformed one), and never
// blocks its own deletion.
func TestApplyOutputs(t *testing.T) {
	t.Parallel()
	full := map[string]string{
		"provider_id":    `"aws:///us-east-1a/i-1"`,
		"addresses":      `[{"type":"ExternalIP","address":"1.2.3.4"},{"type":"InternalIP","address":"10.0.0.1"}]`,
		"failure_domain": `"az-1"`,
		"health":         healthy,
	}
	a := newTestAdapter(t, testTM(), stateReader{})
	rec := &recorder{}
	a.d.Recorder = rec
	owner := shared.OwnerInfo{Machine: testMachine(func(m *clusterv1.Machine) { m.Spec.FailureDomain = "az-1" })}
	res, health, err := a.ApplyOutputs(t.Context(), owner, machineState(full), nil)
	if err != nil || !res.Valid() || health == nil || health.State != contract.HealthRunning {
		t.Fatalf("ApplyOutputs = %+v, %+v, %v", res, health, err)
	}
	// ProviderIDSet once: the same output again writes nothing.
	if _, _, err := a.ApplyOutputs(t.Context(), owner, machineState(full), nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.reasons, []string{shared.EventProviderIDSet}) {
		t.Errorf("events = %v, want one ProviderIDSet", rec.reasons)
	}
	st := a.obj.Status
	if a.obj.Spec.ProviderID != "aws:///us-east-1a/i-1" || len(st.Addresses) != 2 || st.Addresses[0].Type != clusterv1.MachineInternalIP ||
		st.FailureDomain != "az-1" || st.Interruptible == nil || *st.Interruptible {
		t.Errorf("mapped spec %+v status %+v", a.obj.Spec, st)
	}

	// A different provider_id later is a violation; spec keeps the first.
	changed := machineState(map[string]string{"provider_id": `"aws:///us-east-1a/i-2"`, "failure_domain": `"az-1"`, "health": healthy})
	res, _, _ = a.ApplyOutputs(t.Context(), owner, changed, nil)
	if res.Reason() != infrav1.ProviderIDChangedReason || a.obj.Spec.ProviderID != "aws:///us-east-1a/i-1" {
		t.Errorf("changed provider_id: %s, spec %q", res.Reason(), a.obj.Spec.ProviderID)
	}

	// Placement differs from the Machine's failure domain: a violation,
	// and status keeps the previous failure domain, as for addresses.
	wrong := machineState(map[string]string{"provider_id": `"aws:///us-east-1a/i-1"`, "failure_domain": `"az-2"`, "health": healthy})
	if res, _, _ = a.ApplyOutputs(t.Context(), owner, wrong, nil); res.Reason() != infrav1.FailureDomainMismatchReason {
		t.Errorf("placement: %s", res.Reason())
	}
	if fd := a.obj.Status.FailureDomain; fd != "az-1" {
		t.Errorf("status.failureDomain = %q after a violation, want the previous az-1", fd)
	}

	// After provisioning the first null provider_id is only Unknown: it
	// must not terminate the instance, and spec stays.
	a.obj.Status.Initialization.Provisioned = new(true)
	lost := machineState(map[string]string{"health": healthy, "interruptible": `true`})
	_, health, _ = a.ApplyOutputs(t.Context(), owner, lost, nil)
	if health == nil || health.State != contract.HealthProviderIDMissing || a.obj.Spec.ProviderID != "aws:///us-east-1a/i-1" || !*a.obj.Status.Interruptible {
		t.Errorf("lost: health %+v, spec %q", health, a.obj.Spec.ProviderID)
	}

	// A malformed provider_id after provisioning is a violation, never a
	// terminated instance.
	for _, bad := range []string{`"not a provider id"`, `42`} {
		malformed := machineState(map[string]string{"provider_id": bad, "health": healthy})
		res, health, _ = a.ApplyOutputs(t.Context(), owner, malformed, nil)
		if res.Valid() || (health != nil && health.State == contract.HealthTerminated) || a.obj.Spec.ProviderID != "aws:///us-east-1a/i-1" {
			t.Errorf("malformed provider_id %s: %s, health %+v", bad, res.Reason(), health)
		}
	}

	// Before provisioning a null provider_id is only pending.
	b := newTestAdapter(t, testTM(), stateReader{})
	res, health, _ = b.ApplyOutputs(t.Context(), shared.OwnerInfo{}, machineState(map[string]string{"health": healthy}), nil)
	if res.Reason() != infrav1.OutputsPendingReason || health == nil || health.State != contract.HealthRunning || b.obj.Spec.ProviderID != "" {
		t.Errorf("pending: %s, %+v", res.Reason(), health)
	}
	if blocked, err := b.DeletionBlocked(t.Context(), shared.OwnerInfo{}); blocked || err != nil {
		t.Error("a machine blocked its own deletion")
	}
}

// TestReconcile proves Reconcile returns no error for a request naming a
// TerraformMachine that does not exist, and that a control-plane clone
// waiting for its Machine ownerRef gets no finalizer and a
// WaitingForOwnerMachine/Ready-False status at the gate requeue interval.
func TestReconcile(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).Build()
	r := &Reconciler{Deps: shared.Deps{Client: c}}
	if res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "gone"}}); err != nil || res.RequeueAfter != 0 {
		t.Errorf("NotFound: %+v, %v", res, err)
	}

	// A control-plane clone waits for its Machine ownerRef: no finalizer.
	tm := testTM(func(tm *infrav1.TerraformMachine) {
		tm.OwnerReferences = []metav1.OwnerReference{{APIVersion: "controlplane.cluster.x-k8s.io/v1beta2", Kind: "KubeadmControlPlane", Name: "kcp", UID: "kcp-uid"}}
	})
	c = fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(tm).WithStatusSubresource(tm).Build()
	r = &Reconciler{Deps: shared.Deps{Client: c, APIReader: c, Clock: clock.RealClock{}, DriftDefault: time.Hour}}
	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tm)})
	if err != nil || res.RequeueAfter != shared.GateRequeue {
		t.Fatalf("Reconcile = %+v, %v", res, err)
	}
	got := &infrav1.TerraformMachine{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(tm), got); err != nil {
		t.Fatal(err)
	}
	deps := conditions.Get(got, infrav1.DependenciesReadyCondition)
	ready := conditions.Get(got, infrav1.ReadyCondition)
	if len(got.Finalizers) != 0 || deps == nil || deps.Reason != infrav1.WaitingForOwnerMachineReason ||
		ready == nil || ready.Status != metav1.ConditionFalse {
		t.Errorf("finalizers %v, conditions %+v", got.Finalizers, got.Status.Conditions)
	}
}
