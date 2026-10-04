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

package terraformmachinepool

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
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

// testMP returns a MachinePool fixture in ns, on cluster "c1", with a
// bootstrap data Secret name set and spec.template.spec.infrastructureRef
// naming testTMP's TerraformMachinePool back, and each mut applied in
// order; it returns the built pool.
func testMP(mut ...func(*clusterv1.MachinePool)) *clusterv1.MachinePool {
	mp := &clusterv1.MachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c1-mp-0", UID: "mp-uid"}}
	mp.Spec.ClusterName = "c1"
	mp.Spec.Template.Spec.Bootstrap.DataSecretName = new("bootstrap-mp")
	mp.Spec.Template.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{
		APIGroup: infrav1.GroupVersion.Group, Kind: "TerraformMachinePool", Name: "c1-mp-0-xyz",
	}
	for _, f := range mut {
		f(mp)
	}
	return mp
}

// testTMP returns a TerraformMachinePool fixture in ns, owned by the
// fixture MachinePool, with each mut applied in order; it returns the
// built object.
func testTMP(mut ...func(*infrav1.TerraformMachinePool)) *infrav1.TerraformMachinePool {
	tmp := &infrav1.TerraformMachinePool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "c1-mp-0-xyz", UID: "tmp-uid",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "c1"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(), Kind: "MachinePool", Name: "c1-mp-0", UID: "mp-uid",
			}},
		},
		Spec: infrav1.TerraformMachinePoolSpec{WorkspaceSpec: infrav1.WorkspaceSpec{
			Source: infrav1.Source{Image: "registry.example/pool:1.0"}, IdentityRef: infrav1.IdentityReference{Name: "aws"},
		}},
	}
	for _, f := range mut {
		f(tmp)
	}
	return tmp
}

// bootstrapSecret returns a bootstrap data Secret fixture in ns, named to
// match testMP, holding data.
func bootstrapSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bootstrap-mp"}, Data: data}
}

// TestMachinePoolInputs proves MachinePoolInputs defaults replicas to 1
// and keeps an explicit 0, maps labels, failure domains, the Kubernetes
// version (nil when empty) and the cluster's outputs, base64-encodes a
// gzip bootstrap value, and leaves autoscaling disabled.
func TestMachinePoolInputs(t *testing.T) {
	t.Parallel()
	payload := gzipped(t, "#cloud-config\nruncmd: [echo hi]\n")
	secret := bootstrapSecret(map[string][]byte{"value": payload})
	tmp := testTMP(func(p *infrav1.TerraformMachinePool) {
		p.Annotations = map[string]string{clusterv1.TemplateClonedFromNameAnnotation: "tpl"}
	})
	tests := []struct {
		name   string
		mp     *clusterv1.MachinePool
		tmpMut func(*infrav1.TerraformMachinePool)
		check  func(*testing.T, contract.MachinePoolInputs)
	}{
		{"defaults", testMP(), nil, func(t *testing.T, in contract.MachinePoolInputs) {
			raw, err := base64.StdEncoding.DecodeString(in.BootstrapData)
			if err != nil || !bytes.Equal(raw, payload) {
				t.Errorf("bootstrap_data does not round-trip the gzip bytes: %v", err)
			}
			if in.Replicas != 1 || in.KubernetesVersion != nil || in.NodeLabels != nil || in.FailureDomains != nil ||
				in.BootstrapFormat != "cloud-config" || in.Autoscaling != (contract.Autoscaling{}) {
				t.Errorf("defaults = %+v", in)
			}
			if string(in.ClusterOutputs) != `{"net":"n-1"}` || !slices.Equal(in.ClusterFailureDomains, []string{"az-1", "az-2"}) ||
				in.MachinePoolName != "c1-mp-0" || in.Object.Kind != "TerraformMachinePool" || in.Cluster.Name != "c1" ||
				in.Tags["captf.io/template"] != "tpl" {
				t.Errorf("inputs = %+v", in)
			}
			b, err := json.Marshal(in)
			if err != nil || !strings.Contains(string(b), `"node_labels":{}`) || !strings.Contains(string(b), `"failure_domains":[]`) {
				t.Errorf("rendered %s, %v", b, err)
			}
		}},
		{"explicit 0 replicas", testMP(func(mp *clusterv1.MachinePool) { mp.Spec.Replicas = new(int32(0)) }), nil, func(t *testing.T, in contract.MachinePoolInputs) {
			if in.Replicas != 0 {
				t.Errorf("replicas = %d, want 0", in.Replicas)
			}
		}},
		{"labels, failure domains, version", testMP(func(mp *clusterv1.MachinePool) {
			mp.Spec.Replicas = new(int32(3))
			mp.Spec.Template.ObjectMeta.Labels = map[string]string{"role": "worker"}
			mp.Spec.FailureDomains = []string{"az-2", "az-1"}
			mp.Spec.Template.Spec.Version = "v1.36.2"
		}), nil, func(t *testing.T, in contract.MachinePoolInputs) {
			if in.Replicas != 3 || in.NodeLabels["role"] != "worker" || !slices.Equal(in.FailureDomains, []string{"az-2", "az-1"}) ||
				in.KubernetesVersion == nil || *in.KubernetesVersion != "v1.36.2" {
				t.Errorf("inputs = %+v", in)
			}
		}},
		{"autoscaling enabled: first apply (no status yet) uses spec.replicas, clamped", testMP(func(mp *clusterv1.MachinePool) {
			mp.Annotations = map[string]string{
				clusterv1.AutoscalerMinSizeAnnotation: "2", clusterv1.AutoscalerMaxSizeAnnotation: "5",
			}
			mp.Spec.Replicas = new(int32(1)) // below Min: clamped up.
		}), nil, func(t *testing.T, in contract.MachinePoolInputs) {
			if in.Autoscaling != (contract.Autoscaling{Enabled: true, Min: 2, Max: 5}) || in.Replicas != 2 {
				t.Errorf("autoscaling = %+v, replicas = %d, want {true 2 5}, 2", in.Autoscaling, in.Replicas)
			}
		}},
		{"autoscaling enabled: later apply uses the observed status.replicas", testMP(func(mp *clusterv1.MachinePool) {
			mp.Annotations = map[string]string{
				clusterv1.AutoscalerMinSizeAnnotation: "1", clusterv1.AutoscalerMaxSizeAnnotation: "10",
			}
			mp.Spec.Replicas = new(int32(1))
		}), func(p *infrav1.TerraformMachinePool) { p.Status.Replicas = new(int32(7)) }, func(t *testing.T, in contract.MachinePoolInputs) {
			if in.Replicas != 7 {
				t.Errorf("replicas = %d, want the observed 7, not spec.replicas", in.Replicas)
			}
		}},
		{"autoscaling enabled: observed status.replicas pointer-to-0 with min 0 renders 0", testMP(func(mp *clusterv1.MachinePool) {
			mp.Annotations = map[string]string{
				clusterv1.AutoscalerMinSizeAnnotation: "0", clusterv1.AutoscalerMaxSizeAnnotation: "10",
			}
			mp.Spec.Replicas = new(int32(5))
		}), func(p *infrav1.TerraformMachinePool) { p.Status.Replicas = new(int32(0)) }, func(t *testing.T, in contract.MachinePoolInputs) {
			if in.Replicas != 0 {
				t.Errorf("replicas = %d, want 0 (a real observed 0, not treated as unset)", in.Replicas)
			}
		}},
		{"autoscaling enabled: observed above max is clamped down", testMP(func(mp *clusterv1.MachinePool) {
			mp.Annotations = map[string]string{
				clusterv1.AutoscalerMinSizeAnnotation: "1", clusterv1.AutoscalerMaxSizeAnnotation: "5",
			}
		}), func(p *infrav1.TerraformMachinePool) { p.Status.Replicas = new(int32(9)) }, func(t *testing.T, in contract.MachinePoolInputs) {
			if in.Replicas != 5 {
				t.Errorf("replicas = %d, want clamped to Max 5", in.Replicas)
			}
		}},
		{"autoscaling enabled: observed below min is clamped up", testMP(func(mp *clusterv1.MachinePool) {
			mp.Annotations = map[string]string{
				clusterv1.AutoscalerMinSizeAnnotation: "3", clusterv1.AutoscalerMaxSizeAnnotation: "5",
			}
		}), func(p *infrav1.TerraformMachinePool) { p.Status.Replicas = new(int32(1)) }, func(t *testing.T, in contract.MachinePoolInputs) {
			if in.Replicas != 3 {
				t.Errorf("replicas = %d, want clamped to Min 3", in.Replicas)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rowTMP := tmp.DeepCopy()
			if tt.tmpMut != nil {
				tt.tmpMut(rowTMP)
			}
			in := MachinePoolInputs(testCluster(), tt.mp, rowTMP, json.RawMessage(`{"net":"n-1"}`), []string{"az-1", "az-2"}, secret)
			tt.check(t, in)
		})
	}
	// The inputs share no memory with the MachinePool.
	mp := testMP(func(mp *clusterv1.MachinePool) { mp.Spec.Template.ObjectMeta.Labels = map[string]string{"a": "b"} })
	in := MachinePoolInputs(testCluster(), mp, tmp, nil, nil, secret)
	in.NodeLabels["a"] = "mutated"
	if mp.Spec.Template.ObjectMeta.Labels["a"] != "b" {
		t.Error("node_labels aliases the MachinePool's labels")
	}
}

// TestParseAutoscaling proves ParseAutoscaling enables autoscaling only
// when both autoscaler annotations are present, parse as non-negative
// base-10 32-bit integers and satisfy min <= max, and returns the right
// reason (and, when invalid, a message naming the problem) for every other
// case.
func TestParseAutoscaling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		annotations map[string]string
		want        contract.Autoscaling
		reason      string
	}{
		{"neither annotation", nil, contract.Autoscaling{}, infrav1.AutoscalingDisabledReason},
		{"both valid", map[string]string{
			clusterv1.AutoscalerMinSizeAnnotation: "1", clusterv1.AutoscalerMaxSizeAnnotation: "5",
		}, contract.Autoscaling{Enabled: true, Min: 1, Max: 5}, infrav1.ReplicasManagedByModuleReason},
		{"both valid, min == max", map[string]string{
			clusterv1.AutoscalerMinSizeAnnotation: "3", clusterv1.AutoscalerMaxSizeAnnotation: "3",
		}, contract.Autoscaling{Enabled: true, Min: 3, Max: 3}, infrav1.ReplicasManagedByModuleReason},
		{"both valid, min 0", map[string]string{
			clusterv1.AutoscalerMinSizeAnnotation: "0", clusterv1.AutoscalerMaxSizeAnnotation: "5",
		}, contract.Autoscaling{Enabled: true, Min: 0, Max: 5}, infrav1.ReplicasManagedByModuleReason},
		{"min > max", map[string]string{
			clusterv1.AutoscalerMinSizeAnnotation: "6", clusterv1.AutoscalerMaxSizeAnnotation: "5",
		}, contract.Autoscaling{}, infrav1.AutoscalingAnnotationsInvalidReason},
		{"min unparsable", map[string]string{
			clusterv1.AutoscalerMinSizeAnnotation: "abc", clusterv1.AutoscalerMaxSizeAnnotation: "5",
		}, contract.Autoscaling{}, infrav1.AutoscalingAnnotationsInvalidReason},
		{"max unparsable", map[string]string{
			clusterv1.AutoscalerMinSizeAnnotation: "1", clusterv1.AutoscalerMaxSizeAnnotation: "abc",
		}, contract.Autoscaling{}, infrav1.AutoscalingAnnotationsInvalidReason},
		{"min negative", map[string]string{
			clusterv1.AutoscalerMinSizeAnnotation: "-1", clusterv1.AutoscalerMaxSizeAnnotation: "5",
		}, contract.Autoscaling{}, infrav1.AutoscalingAnnotationsInvalidReason},
		{"max negative", map[string]string{
			clusterv1.AutoscalerMinSizeAnnotation: "1", clusterv1.AutoscalerMaxSizeAnnotation: "-5",
		}, contract.Autoscaling{}, infrav1.AutoscalingAnnotationsInvalidReason},
		{"only min", map[string]string{
			clusterv1.AutoscalerMinSizeAnnotation: "1",
		}, contract.Autoscaling{}, infrav1.AutoscalingAnnotationsInvalidReason},
		{"only max", map[string]string{
			clusterv1.AutoscalerMaxSizeAnnotation: "5",
		}, contract.Autoscaling{}, infrav1.AutoscalingAnnotationsInvalidReason},
		{"overflows int32", map[string]string{
			clusterv1.AutoscalerMinSizeAnnotation: "1", clusterv1.AutoscalerMaxSizeAnnotation: "99999999999",
		}, contract.Autoscaling{}, infrav1.AutoscalingAnnotationsInvalidReason},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mp := testMP(func(mp *clusterv1.MachinePool) { mp.Annotations = tt.annotations })
			got, reason, message := ParseAutoscaling(mp)
			if got != tt.want || reason != tt.reason {
				t.Errorf("ParseAutoscaling = %+v, %q, %q; want %+v, %q", got, reason, message, tt.want, tt.reason)
			}
			if reason == infrav1.AutoscalingAnnotationsInvalidReason && message == "" {
				t.Error("AutoscalingAnnotationsInvalid must carry a message naming the problem")
			}
			if reason != infrav1.AutoscalingAnnotationsInvalidReason && message != "" {
				t.Errorf("unexpected message %q for reason %s", message, reason)
			}
		})
	}
}

// TestCheckGates proves CheckGates returns nil when every gate is met and
// the first unmet gate's reason in precedence order otherwise.
func TestCheckGates(t *testing.T) {
	t.Parallel()
	all := GateInput{ClusterFound: true, InfrastructureProvisioned: true, InfraClusterFound: true, ClusterOutputsReady: true, BootstrapReady: true}
	with := func(f func(*GateInput)) GateInput { in := all; f(&in); return in }
	tests := []struct {
		name   string
		in     GateInput
		reason string
		status metav1.ConditionStatus
	}{
		{"all met", all, "", ""},
		{"MachinePool gone", with(func(in *GateInput) { in.MachinePoolGone = true }), infrav1.OwnerNotFoundReason, metav1.ConditionFalse},
		{"no Cluster", with(func(in *GateInput) { in.ClusterFound = false }), infrav1.WaitingForOwnerReason, metav1.ConditionUnknown},
		{"infrastructure not provisioned", with(func(in *GateInput) { in.InfrastructureProvisioned = false }), infrav1.WaitingForClusterInfrastructureReason, metav1.ConditionUnknown},
		{"TerraformCluster missing", with(func(in *GateInput) { in.InfraClusterFound = false }), infrav1.WaitingForClusterInfrastructureReason, metav1.ConditionUnknown},
		{"cluster outputs unreadable", with(func(in *GateInput) { in.ClusterOutputsReady = false }), infrav1.WaitingForClusterExportsReason, metav1.ConditionUnknown},
		{"no bootstrap data", with(func(in *GateInput) { in.BootstrapReady = false }), infrav1.WaitingForBootstrapDataReason, metav1.ConditionUnknown},
		{"order: outputs before bootstrap", with(func(in *GateInput) { in.ClusterOutputsReady, in.BootstrapReady = false, false }), infrav1.WaitingForClusterExportsReason, metav1.ConditionUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := CheckGates(tt.in)
			switch {
			case tt.reason == "" && g != nil:
				t.Errorf("gate %+v", g)
			case tt.reason != "" && (g == nil || g.Reason != tt.reason || g.Status != tt.status):
				t.Errorf("gate %+v, want %s/%s", g, tt.status, tt.reason)
			}
		})
	}
}

// --- adapter ----------------------------------------------------------------

// stateReader is a state.Reader stub serving the state of a suffix from
// bySuffix (state.ErrNoState when absent), or err for every read when set.
type stateReader struct {
	mu       sync.Mutex
	bySuffix map[string]*state.State
	err      error
}

// Read returns r.err when set, else the state recorded for suffix, or
// state.ErrNoState.
func (r *stateReader) Read(_ context.Context, _, suffix string) (*state.State, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	if st, ok := r.bySuffix[suffix]; ok {
		return st, nil
	}
	return nil, state.ErrNoState
}

// set records st as the state of the object kind/name, failing t on error.
func (r *stateReader) set(t *testing.T, kind, name string, st *state.State) {
	t.Helper()
	suffix, err := state.Suffix(ns, kind, name)
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bySuffix == nil {
		r.bySuffix = map[string]*state.State{}
	}
	r.bySuffix[suffix] = st
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

// copies returns deep copies of objs: the fake client writes to the
// objects it is seeded with, and table rows are shared by parallel
// subtests.
func copies(objs []client.Object) []client.Object {
	out := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.DeepCopyObject().(client.Object))
	}
	return out
}

// newTestAdapter builds an adapter for tmp, backed by a fake client seeded
// with tmp and objs and the state reader sr, failing t on setup error. It
// returns the built adapter.
func newTestAdapter(t *testing.T, tmp *infrav1.TerraformMachinePool, sr *stateReader, objs ...client.Object) *adapter {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(append([]client.Object{tmp}, objs...)...).Build()
	return newAdapter(shared.Deps{Client: c, APIReader: c, State: sr}, tmp)
}

// checkMismatch asserts, failing t otherwise, that o carries the
// OwnerMismatch gate and no MachinePool, Cluster or InfraCluster: a forged
// or stale MachinePool ownerRef must never resolve to a usable owner.
func checkMismatch(t *testing.T, o shared.OwnerInfo) {
	t.Helper()
	if o.Gate == nil || o.Gate.Reason != infrav1.OwnerMismatchReason || o.Gate.Status != metav1.ConditionFalse {
		t.Errorf("owner = %+v, want OwnerMismatch gate", o)
	}
	if o.MachinePool != nil || o.Cluster != nil || o.InfraCluster != nil {
		t.Errorf("owner = %+v, want no MachinePool/Cluster/InfraCluster", o)
	}
}

// TestOwner proves adapter.Owner resolves the MachinePool/Cluster/
// TerraformCluster chain and tells apart every gated or partial case.
func TestOwner(t *testing.T) {
	t.Parallel()
	foreign := testCluster(func(c *clusterv1.Cluster) {
		c.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "AWSCluster", Name: "c1"}
	})
	tests := []struct {
		name  string
		tmp   *infrav1.TerraformMachinePool
		objs  []client.Object
		check func(*testing.T, shared.OwnerInfo)
	}{
		{"full chain", testTMP(), []client.Object{testMP(), testCluster(), testTC()}, func(t *testing.T, o shared.OwnerInfo) {
			if !o.HasOwnerRef || o.MachinePool == nil || o.Cluster == nil || o.InfraCluster == nil || o.Gate != nil {
				t.Errorf("owner = %+v", o)
			}
		}},
		{"no ownerRef at all", testTMP(func(p *infrav1.TerraformMachinePool) { p.OwnerReferences = nil }), nil, func(t *testing.T, o shared.OwnerInfo) {
			if o.HasOwnerRef || o.Gate != nil || o.Cluster != nil {
				t.Errorf("owner = %+v", o)
			}
		}},
		{"foreign ownerRef only: WaitingForOwnerMachinePool", testTMP(func(p *infrav1.TerraformMachinePool) {
			p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "example.com/v1", Kind: "Other", Name: "x", UID: "x-uid"}}
		}), nil, func(t *testing.T, o shared.OwnerInfo) {
			if o.HasOwnerRef || o.Gate == nil || o.Gate.Reason != infrav1.WaitingForOwnerMachinePoolReason || o.Gate.Status != metav1.ConditionFalse {
				t.Errorf("owner = %+v", o)
			}
		}},
		{"MachinePool gone: owner gone, Cluster still resolved", testTMP(), []client.Object{testCluster(), testTC()}, func(t *testing.T, o shared.OwnerInfo) {
			if !o.HasOwnerRef || !o.OwnerGone || o.MachinePool != nil || o.Cluster == nil || o.InfraCluster == nil {
				t.Errorf("owner = %+v", o)
			}
		}},
		{"non-Terraform cluster: ClusterNotTerraform", testTMP(), []client.Object{testMP(), foreign}, func(t *testing.T, o shared.OwnerInfo) {
			if o.Gate == nil || o.Gate.Reason != infrav1.ClusterNotTerraformReason || !o.HasOwnerRef {
				t.Errorf("owner = %+v", o)
			}
		}},
		{"TerraformCluster missing", testTMP(), []client.Object{testMP(), testCluster()}, func(t *testing.T, o shared.OwnerInfo) {
			if o.Cluster == nil || o.InfraCluster != nil || o.Gate != nil {
				t.Errorf("owner = %+v", o)
			}
		}},
		{"forged: wrong Kind in MachinePool.infrastructureRef", testTMP(), []client.Object{testMP(func(mp *clusterv1.MachinePool) {
			mp.Spec.Template.Spec.InfrastructureRef.Kind = "AWSMachinePool"
		}), testCluster(), testTC()}, checkMismatch},
		{"forged: wrong Name in MachinePool.infrastructureRef", testTMP(), []client.Object{testMP(func(mp *clusterv1.MachinePool) {
			mp.Spec.Template.Spec.InfrastructureRef.Name = "someone-elses-tmp"
		}), testCluster(), testTC()}, checkMismatch},
		{"forged: wrong APIGroup in MachinePool.infrastructureRef", testTMP(), []client.Object{testMP(func(mp *clusterv1.MachinePool) {
			mp.Spec.Template.Spec.InfrastructureRef.APIGroup = "infrastructure.example.com"
		}), testCluster(), testTC()}, checkMismatch},
		{"forged: UID mismatch on the MachinePool ownerRef", testTMP(func(p *infrav1.TerraformMachinePool) {
			p.OwnerReferences[0].UID = "some-other-mp-uid"
		}), []client.Object{testMP(), testCluster(), testTC()}, checkMismatch},
		{"forged: cluster-name label disagrees with the MachinePool's", testTMP(func(p *infrav1.TerraformMachinePool) {
			p.Labels[clusterv1.ClusterNameLabel] = "someone-elses-cluster"
		}), []client.Object{testMP(), testCluster(), testTC()}, checkMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := newTestAdapter(t, tt.tmp, &stateReader{}, copies(tt.objs)...)
			o, err := a.Owner(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, o)
		})
	}
}

// clusterState returns a cluster state.State fixture with exports and
// failureDomains as the raw exports and failure_domains outputs (omitted
// when "") and a fixed healthy health output.
func clusterState(exports, failureDomains string) *state.State {
	st := &state.State{InputsHash: "h1:c", Outputs: map[string]state.Output{
		"exports": {Value: json.RawMessage(exports)},
		"health":  {Value: json.RawMessage(healthy)},
	}}
	if failureDomains != "" {
		st.Outputs["failure_domains"] = state.Output{Value: json.RawMessage(failureDomains)}
	}
	return st
}

// TestBuildInputs proves adapter.BuildInputs builds pool inputs carrying
// the cluster's exports and sorted failure-domain names from one state
// read (or {} and the status' names for an externally managed
// TerraformCluster) once every gate is met, and gates on a cluster state
// not ready or an invalid output, the bootstrap data, and the owners.
func TestBuildInputs(t *testing.T) {
	t.Parallel()
	owner := func() shared.OwnerInfo {
		return shared.OwnerInfo{HasOwnerRef: true, MachinePool: testMP(), Cluster: testCluster(), InfraCluster: testTC()}
	}
	ready := bootstrapSecret(map[string][]byte{"value": []byte("#cloud-config\n")})
	fds := `[{"name":"az-b","control_plane":true,"attributes":{}},{"name":"az-a","control_plane":false,"attributes":{}}]`
	tests := []struct {
		name   string
		owner  shared.OwnerInfo
		st     *state.State
		err    error
		objs   []client.Object
		reason string
		check  func(*testing.T, contract.MachinePoolInputs)
	}{
		{"cluster state failure domains, sorted", owner(), clusterState(`{"vpc":"v-1"}`, fds), nil, []client.Object{ready}, "", func(t *testing.T, in contract.MachinePoolInputs) {
			if string(in.ClusterOutputs) != `{"vpc":"v-1"}` || !slices.Equal(in.ClusterFailureDomains, []string{"az-a", "az-b"}) {
				t.Errorf("outputs %s, failure domains %v", in.ClusterOutputs, in.ClusterFailureDomains)
			}
		}},
		{"externally managed TerraformCluster uses its status", func() shared.OwnerInfo {
			o := owner()
			o.InfraCluster.Annotations = map[string]string{clusterv1.ManagedByAnnotation: "x"}
			o.InfraCluster.Status.FailureDomains = []clusterv1.FailureDomain{{Name: "z"}, {Name: "y"}}
			return o
		}(), nil, nil, []client.Object{ready}, "", func(t *testing.T, in contract.MachinePoolInputs) {
			if string(in.ClusterOutputs) != `{}` || !slices.Equal(in.ClusterFailureDomains, []string{"y", "z"}) {
				t.Errorf("outputs %s, failure domains %v", in.ClusterOutputs, in.ClusterFailureDomains)
			}
		}},
		{"cluster state not written yet", owner(), nil, nil, []client.Object{ready}, infrav1.WaitingForClusterExportsReason, nil},
		{"cluster state mid-write", owner(), nil, state.ErrStateInconsistent, []client.Object{ready}, infrav1.WaitingForClusterExportsReason, nil},
		{"invalid failure_domains waits", owner(), clusterState(`{}`, `[{"name":""}]`), nil, []client.Object{ready}, infrav1.WaitingForClusterExportsReason, nil},
		{"exports not hashable", owner(), clusterState(`{"ratio":0.5}`, ""), nil, []client.Object{ready}, infrav1.WaitingForClusterExportsReason, nil},
		{"bootstrap Secret missing", owner(), clusterState(`{}`, ""), nil, nil, infrav1.WaitingForBootstrapDataReason, nil},
		{"dataSecretName empty", func() shared.OwnerInfo {
			o := owner()
			o.MachinePool.Spec.Template.Spec.Bootstrap.DataSecretName = new("")
			return o
		}(), clusterState(`{}`, ""), nil, []client.Object{ready}, infrav1.WaitingForBootstrapDataReason, nil},
		{"infrastructure not provisioned", func() shared.OwnerInfo {
			o := owner()
			o.Cluster.Status.Initialization.InfrastructureProvisioned = nil
			return o
		}(), clusterState(`{}`, ""), nil, []client.Object{ready}, infrav1.WaitingForClusterInfrastructureReason, nil},
		{"MachinePool gone", shared.OwnerInfo{HasOwnerRef: true, OwnerGone: true, Cluster: testCluster()}, nil, nil, nil, infrav1.OwnerNotFoundReason, nil},
		{"no Cluster", shared.OwnerInfo{HasOwnerRef: true, MachinePool: testMP()}, nil, nil, nil, infrav1.WaitingForOwnerReason, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sr := &stateReader{err: tt.err}
			if tt.st != nil {
				sr.set(t, state.KindTerraformCluster, "c1", tt.st)
			}
			a := newTestAdapter(t, testTMP(), sr, copies(tt.objs)...)
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
			mi, ok := in.(contract.MachinePoolInputs)
			if gate != nil || !ok {
				t.Fatalf("gate %+v, inputs %T", gate, in)
			}
			tt.check(t, mi)
		})
	}
	// A genuine cluster state read failure is an error, not a gate.
	a := newTestAdapter(t, testTMP(), &stateReader{err: errors.New("etcd unavailable")}, ready.DeepCopy())
	if _, _, err := a.BuildInputs(t.Context(), owner(), nil); err == nil || !strings.Contains(err.Error(), "etcd unavailable") {
		t.Errorf("BuildInputs = %v, want the read error", err)
	}
}

// TestBuildInputsSetsAutoscalingActive proves BuildInputs sets the
// AutoscalingActive condition from the MachinePool's autoscaler
// annotations, once the MachinePool is known, whatever else gates the
// build (here: no bootstrap Secret, so every case gates on
// WaitingForBootstrapData without ever reaching the inputs).
func TestBuildInputsSetsAutoscalingActive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		annotations map[string]string
		status      metav1.ConditionStatus
		reason      string
	}{
		{"disabled: no annotations", nil, metav1.ConditionFalse, infrav1.AutoscalingDisabledReason},
		{"enabled: both valid", map[string]string{
			clusterv1.AutoscalerMinSizeAnnotation: "1", clusterv1.AutoscalerMaxSizeAnnotation: "5",
		}, metav1.ConditionTrue, infrav1.ReplicasManagedByModuleReason},
		{"invalid: min > max", map[string]string{
			clusterv1.AutoscalerMinSizeAnnotation: "6", clusterv1.AutoscalerMaxSizeAnnotation: "5",
		}, metav1.ConditionFalse, infrav1.AutoscalingAnnotationsInvalidReason},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mp := testMP(func(mp *clusterv1.MachinePool) { mp.Annotations = tt.annotations })
			owner := shared.OwnerInfo{HasOwnerRef: true, MachinePool: mp, Cluster: testCluster(), InfraCluster: testTC()}
			tmp := testTMP()
			a := newTestAdapter(t, tmp, &stateReader{}, mp, testCluster(), testTC())
			if _, _, err := a.BuildInputs(t.Context(), owner, nil); err != nil {
				t.Fatal(err)
			}
			c := conditions.Get(a.obj, infrav1.AutoscalingActiveCondition)
			if c == nil || c.Status != tt.status || c.Reason != tt.reason {
				t.Errorf("AutoscalingActive = %+v, want %s/%s", c, tt.status, tt.reason)
			}
		})
	}
}

// healthy is a raw health output JSON value describing a running, healthy
// group.
const healthy = `{"state":"running","healthy":true,"message":null,"reasons":[]}`

// poolState returns a pool state.State with outs as raw output values and
// a fixed InputsHash marking it as applied.
func poolState(outs map[string]string) *state.State {
	st := &state.State{InputsHash: "h1:p", Outputs: map[string]state.Output{}}
	for k, v := range outs {
		st.Outputs[k] = state.Output{Value: json.RawMessage(v)}
	}
	return st
}

// poolOutputs returns a valid set of pool outputs with providerIDs as the
// raw provider_id_list and replicas as the raw replicas output.
func poolOutputs(providerIDs, replicas string) map[string]string {
	return map[string]string{
		"provider_id":      `"aws:///us-east-1/asg-1"`,
		"provider_id_list": providerIDs,
		"replicas":         replicas,
		"instances":        `[{"provider_id":"aws:///us-east-1a/i-2","instance_id":"i-2","addresses":[{"type":"ExternalIP","address":"1.2.3.4"},{"type":"InternalIP","address":"10.0.0.2"}],"failure_domain":"az-1","state":"running"}]`,
		"health":           healthy,
	}
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

// count returns how many recorded events have reason.
func (r *recorder) count(reason string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, got := range r.reasons {
		if got == reason {
			n++
		}
	}
	return n
}

// TestApplyOutputs proves adapter.ApplyOutputs writes the sorted,
// deduplicated provider ID list, 0 replicas explicitly and the instances,
// keeps the previous values of an output with a violation, reports a
// truncated list as valid and still latches status.ready, and never
// resets status.ready.
func TestApplyOutputs(t *testing.T) {
	t.Parallel()
	ids := `["aws:///us-east-1b/i-3","aws:///us-east-1a/i-2","aws:///us-east-1a/i-2"]`
	tests := []struct {
		name  string
		prep  func(*infrav1.TerraformMachinePool)
		outs  map[string]string
		check func(*testing.T, *infrav1.TerraformMachinePool, bool)
	}{
		{"sorted list, replicas and instances written", nil, poolOutputs(ids, `2`), func(t *testing.T, p *infrav1.TerraformMachinePool, valid bool) {
			if !valid || !slices.Equal(p.Spec.ProviderIDList, []string{"aws:///us-east-1a/i-2", "aws:///us-east-1b/i-3"}) ||
				p.Spec.ProviderID != "aws:///us-east-1/asg-1" || p.Status.Replicas == nil || *p.Status.Replicas != 2 {
				t.Errorf("spec %+v status replicas %v", p.Spec, p.Status.Replicas)
			}
			if len(p.Status.Instances) != 1 || p.Status.Instances[0].InstanceID != "i-2" || p.Status.Instances[0].FailureDomain != "az-1" ||
				p.Status.Instances[0].State != infrav1.HealthStateRunning || p.Status.Instances[0].Addresses[0].Type != clusterv1.MachineInternalIP {
				t.Errorf("instances = %+v", p.Status.Instances)
			}
			if p.Status.Ready == nil || !*p.Status.Ready {
				t.Error("ready not latched with provisioning")
			}
		}},
		{"0 replicas explicit", nil, poolOutputs(`[]`, `0`), func(t *testing.T, p *infrav1.TerraformMachinePool, _ bool) {
			if p.Status.Replicas == nil || *p.Status.Replicas != 0 || p.Spec.ProviderIDList != nil {
				t.Errorf("replicas %v, list %v", p.Status.Replicas, p.Spec.ProviderIDList)
			}
		}},
		{"truncated instances still valid and ready", nil, func() map[string]string {
			o := poolOutputs(`[]`, `1001`)
			var b strings.Builder
			b.WriteString("[")
			for i := range 1001 {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"provider_id":"aws:///us-east-1a/i-%d"}`, i)
			}
			b.WriteString("]")
			o["instances"] = b.String()
			return o
		}(), func(t *testing.T, p *infrav1.TerraformMachinePool, valid bool) {
			if !valid || len(p.Status.Instances) != 1000 || p.Status.Ready == nil || !*p.Status.Ready {
				t.Errorf("valid %v, instances %d, ready %v", valid, len(p.Status.Instances), p.Status.Ready)
			}
		}},
		{"a violation keeps the previous values", func(p *infrav1.TerraformMachinePool) {
			p.Spec.ProviderIDList = []string{"aws:///us-east-1a/i-old"}
			p.Status.Replicas = new(int32(1))
			p.Status.Instances = []infrav1.MachinePoolInstance{{ProviderID: "aws:///us-east-1a/i-old"}}
		}, func() map[string]string {
			o := poolOutputs(`[""]`, `-1`)
			o["instances"] = `"nope"`
			return o
		}(), func(t *testing.T, p *infrav1.TerraformMachinePool, valid bool) {
			if valid || !slices.Equal(p.Spec.ProviderIDList, []string{"aws:///us-east-1a/i-old"}) || *p.Status.Replicas != 1 ||
				len(p.Status.Instances) != 1 || p.Status.Instances[0].ProviderID != "aws:///us-east-1a/i-old" || p.Status.Ready != nil {
				t.Errorf("valid %v, spec %+v, status %+v", valid, p.Spec, p.Status)
			}
		}},
		{"ready latched, never reset", func(p *infrav1.TerraformMachinePool) {
			p.Status.Initialization.Provisioned = new(true)
			p.Status.Ready = new(true)
		}, map[string]string{"health": healthy}, func(t *testing.T, p *infrav1.TerraformMachinePool, valid bool) {
			if valid || p.Status.Ready == nil || !*p.Status.Ready {
				t.Errorf("valid %v, ready %v", valid, p.Status.Ready)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tmp := testTMP()
			if tt.prep != nil {
				tt.prep(tmp)
			}
			a := newTestAdapter(t, tmp, &stateReader{})
			res, _, err := a.ApplyOutputs(t.Context(), shared.OwnerInfo{}, poolState(tt.outs), nil)
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, a.obj, res.Valid())
		})
	}

	// provider_id: set with an event, a change replaces it (the pool is
	// mutable), a null one keeps it.
	a := newTestAdapter(t, testTMP(), &stateReader{})
	rec := &recorder{}
	a.d.Recorder = rec
	outs := poolOutputs(`[]`, `0`)
	for _, id := range []string{`"aws:///us-east-1/asg-1"`, `"aws:///us-east-1/asg-1"`, `"aws:///us-east-1/asg-2"`, `null`} {
		outs["provider_id"] = id
		if _, _, err := a.ApplyOutputs(t.Context(), shared.OwnerInfo{}, poolState(outs), nil); err != nil {
			t.Fatal(err)
		}
	}
	if a.obj.Spec.ProviderID != "aws:///us-east-1/asg-2" || rec.count(shared.EventProviderIDSet) != 2 {
		t.Errorf("providerID %q, events %v", a.obj.Spec.ProviderID, rec.reasons)
	}
	if blocked, err := a.DeletionBlocked(t.Context(), shared.OwnerInfo{}); blocked || err != nil {
		t.Error("a pool blocked its own deletion")
	}
}
