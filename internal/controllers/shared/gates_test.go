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

package shared

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestCheckGates proves CheckGates returns nil when every gate is met,
// otherwise the first unmet gate in order, with the caller's wording for
// the kind-specific messages.
func TestCheckGates(t *testing.T) {
	t.Parallel()
	text := GateText{OwnerGone: "gone", OutputsWait: "outputs", BootstrapWait: "bootstrap"}
	all := GateInput{ClusterFound: true, InfrastructureProvisioned: true, InfraClusterFound: true, OutputsReady: true, BootstrapReady: true}
	if g := CheckGates(all, text); g != nil {
		t.Fatalf("all met: gate = %+v, want nil", g)
	}
	with := func(f func(*GateInput)) GateInput { in := all; f(&in); return in }
	for _, tt := range []struct {
		name   string
		in     GateInput
		status metav1.ConditionStatus
		reason string
		msg    string
	}{
		{"owner gone", with(func(in *GateInput) { in.OwnerGone = true }), metav1.ConditionFalse, infrav1.OwnerNotFoundReason, "gone"},
		{"no Cluster", with(func(in *GateInput) { in.ClusterFound = false }), metav1.ConditionUnknown, infrav1.WaitingForOwnerReason, ""},
		{"not provisioned", with(func(in *GateInput) { in.InfrastructureProvisioned = false }), metav1.ConditionUnknown, infrav1.WaitingForClusterInfrastructureReason, ""},
		{"no TerraformCluster", with(func(in *GateInput) { in.InfraClusterFound = false }), metav1.ConditionUnknown, infrav1.WaitingForClusterInfrastructureReason, ""},
		{"outputs", with(func(in *GateInput) { in.OutputsReady = false }), metav1.ConditionUnknown, infrav1.WaitingForClusterExportsReason, "outputs"},
		{"bootstrap", with(func(in *GateInput) { in.BootstrapReady = false }), metav1.ConditionUnknown, infrav1.WaitingForBootstrapDataReason, "bootstrap"},
		{"order: owner first", GateInput{OwnerGone: true}, metav1.ConditionFalse, infrav1.OwnerNotFoundReason, "gone"},
		{"order: outputs before bootstrap", with(func(in *GateInput) { in.OutputsReady, in.BootstrapReady = false, false }), metav1.ConditionUnknown, infrav1.WaitingForClusterExportsReason, "outputs"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := CheckGates(tt.in, text)
			if g == nil || g.Status != tt.status || g.Reason != tt.reason || (tt.msg != "" && g.Message != tt.msg) || g.Message == "" {
				t.Errorf("gate = %+v, want %s/%s %q", g, tt.status, tt.reason, tt.msg)
			}
		})
	}
}
