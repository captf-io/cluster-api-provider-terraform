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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
)

// TestBuildInputsVariables: the cluster's variables reach its inputs and
// its hash; a reserved key in a source gates with VariablesInvalid.
func TestBuildInputsVariables(t *testing.T) {
	t.Parallel()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "net", Labels: map[string]string{infrav1.VariablesSourceLabel: "true"}},
		Data:       map[string]string{"cidr": "10.0.0.0/16"},
	}
	tc := testTC(func(tc *infrav1.TerraformCluster) {
		tc.Spec.Variables = runtime.RawExtension{Raw: []byte(`{"nat_gateways":2}`)}
		tc.Spec.VariablesFrom = []infrav1.VariablesSource{{ConfigMapRef: infrav1.VariablesSourceReference{Name: "net"}}}
	})
	a, _ := newTestAdapter(t, tc, cm)
	owner := shared.OwnerInfo{HasOwnerRef: true, Cluster: testCluster()}
	in, gate, err := a.BuildInputs(t.Context(), owner, nil)
	if err != nil || gate != nil {
		t.Fatalf("BuildInputs: %+v, %v", gate, err)
	}
	ci, ok := in.(contract.ClusterInputs)
	if !ok || string(ci.Variables["cidr"].Value) != `"10.0.0.0/16"` || string(ci.Variables["nat_gateways"].Value) != `2` {
		t.Fatalf("variables = %+v", ci.Variables)
	}
	withVars, err := hash.Inputs(contract.RoleCluster, tc.Spec.Source.Image, ci)
	if err != nil {
		t.Fatal(err)
	}
	ci.Variables = nil
	without, err := hash.Inputs(contract.RoleCluster, tc.Spec.Source.Image, ci)
	if err != nil || withVars == without {
		t.Errorf("variables do not reach the hash: %s vs %s (%v)", withVars, without, err)
	}

	bad := cm.DeepCopy()
	bad.Data = map[string]string{"control_plane_initialized": "true"}
	b, _ := newTestAdapter(t, tc.DeepCopy(), bad)
	if _, gate, err := b.BuildInputs(t.Context(), owner, nil); err != nil || gate == nil || gate.Reason != infrav1.VariablesInvalidReason {
		t.Errorf("reserved key: gate %+v, err %v", gate, err)
	}
}
