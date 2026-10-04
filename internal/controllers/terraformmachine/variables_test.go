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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
)

// TestBuildInputsVariables: the machine's variables reach its inputs once
// every other gate passes; a missing source is a gate of its own.
func TestBuildInputsVariables(t *testing.T) {
	t.Parallel()
	owner := shared.OwnerInfo{HasOwnerRef: true, Machine: testMachine(), Cluster: testCluster(), InfraCluster: testTC()}
	ready := bootstrapSecret(map[string][]byte{"value": []byte("#cloud-config\n")})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "db", Labels: map[string]string{infrav1.VariablesSourceLabel: "true"}},
		Data:       map[string][]byte{"db_password": []byte("pw")},
	}
	tm := testTM(func(m *infrav1.TerraformMachine) {
		m.Spec.Variables = runtime.RawExtension{Raw: []byte(`{"instance_type":"t3.large"}`)}
		m.Spec.VariablesFrom = []infrav1.VariablesSource{{SecretRef: infrav1.VariablesSourceReference{Name: "db"}}}
	})
	a := newTestAdapter(t, tm, stateReader{st: clusterState(`{}`)}, ready, secret)
	in, gate, err := a.BuildInputs(t.Context(), owner, nil)
	if err != nil || gate != nil {
		t.Fatalf("BuildInputs: %+v, %v", gate, err)
	}
	mi, ok := in.(contract.MachineInputs)
	if !ok || string(mi.Variables["instance_type"].Value) != `"t3.large"` || !mi.Variables["db_password"].Sensitive {
		t.Errorf("variables = %+v", mi.Variables)
	}

	missing := newTestAdapter(t, tm.DeepCopy(), stateReader{st: clusterState(`{}`)}, ready.DeepCopy())
	if _, gate, err := missing.BuildInputs(t.Context(), owner, nil); err != nil || gate == nil || gate.Reason != infrav1.VariablesSourceNotFoundReason {
		t.Errorf("missing source: gate %+v, err %v", gate, err)
	}
}
