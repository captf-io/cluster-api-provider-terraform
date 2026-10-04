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

package webhooks

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// endpointCluster returns a valid TerraformCluster carrying ep.
func endpointCluster(ep *clusterv1.APIEndpoint) *infrav1.TerraformCluster {
	return &infrav1.TerraformCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec: infrav1.TerraformClusterSpec{
			WorkspaceSpec:        infrav1.WorkspaceSpec{Source: infrav1.Source{Image: testImage}, IdentityRef: infrav1.IdentityReference{Name: "id"}},
			ControlPlaneEndpoint: ep,
		},
	}
}

// TestTerraformClusterCreateHalfSetEndpoint proves ValidateCreate rejects a
// controlPlaneEndpoint with only a host or only a port, and accepts an
// unset, empty or complete one.
func TestTerraformClusterCreateHalfSetEndpoint(t *testing.T) {
	t.Parallel()
	w := &TerraformCluster{}
	tests := []struct {
		name    string
		ep      *clusterv1.APIEndpoint
		invalid bool
	}{
		{name: "unset"},
		{name: "empty", ep: &clusterv1.APIEndpoint{}},
		{name: "complete", ep: &clusterv1.APIEndpoint{Host: "api.example.com", Port: 6443}},
		{name: "host only", ep: &clusterv1.APIEndpoint{Host: "api.example.com"}, invalid: true},
		{name: "port only", ep: &clusterv1.APIEndpoint{Port: 6443}, invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := w.ValidateCreate(context.Background(), endpointCluster(tt.ep))
			wantInvalid(t, err, tt.invalid, "must set both host and port")
		})
	}
}

// TestTerraformClusterUpdateHalfSetEndpoint proves ValidateUpdate lets a
// half-set stored endpoint be completed, and keeps a valid endpoint frozen.
func TestTerraformClusterUpdateHalfSetEndpoint(t *testing.T) {
	t.Parallel()
	w := &TerraformCluster{}
	half := &clusterv1.APIEndpoint{Host: "api.example.com"}
	valid := &clusterv1.APIEndpoint{Host: "api.example.com", Port: 6443}
	tests := []struct {
		name     string
		old, cur *clusterv1.APIEndpoint
		frag     string
		invalid  bool
	}{
		{name: "half-set to valid", old: half, cur: valid},
		{name: "half-set unchanged by an unrelated update", old: half, cur: half},
		{name: "half-set to another half-set", old: half, cur: &clusterv1.APIEndpoint{Host: "other.example.com"},
			frag: "must set both host and port", invalid: true},
		{name: "unset to half-set", cur: half, frag: "must set both host and port", invalid: true},
		{name: "valid to different", old: valid, cur: &clusterv1.APIEndpoint{Host: "other.example.com", Port: 6443},
			frag: "immutable", invalid: true},
		{name: "valid to same", old: valid, cur: &clusterv1.APIEndpoint{Host: "api.example.com", Port: 6443}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := w.ValidateUpdate(context.Background(), endpointCluster(tt.old), endpointCluster(tt.cur))
			wantInvalid(t, err, tt.invalid, tt.frag)
		})
	}
}
