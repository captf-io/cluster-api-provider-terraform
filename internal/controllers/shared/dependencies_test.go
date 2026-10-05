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
	"encoding/json"
	"errors"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// TestReadClusterOutputs proves ReadClusterOutputs: a nil cluster waits, an
// externally managed one yields {} and its status failure domains without
// reading state, a managed one yields the decoded exports and names, a
// missing or concerning output waits, and a read failure is an error.
func TestReadClusterOutputs(t *testing.T) {
	t.Parallel()
	tc := func(external bool) *infrav1.TerraformCluster {
		c := &infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "tc"}}
		c.Status.FailureDomains = []clusterv1.FailureDomain{{Name: "b"}, {Name: "a"}}
		if external {
			c.Annotations = map[string]string{clusterv1.ManagedByAnnotation: "x"}
		}
		return c
	}
	stateWith := func(outs map[string]string) *state.State {
		st := &state.State{InputsHash: "h", Outputs: map[string]state.Output{}}
		for k, v := range outs {
			st.Outputs[k] = state.Output{Value: json.RawMessage(v)}
		}
		return st
	}
	good := map[string]string{
		"exports":         `{"vpc":"v-1"}`,
		"failure_domains": `[{"name":"z1","control_plane":true},{"name":"z2"}]`,
		"health":          `{"state":"running","healthy":true,"message":null,"reasons":[]}`,
	}
	badFDs := map[string]string{"exports": `{}`, "failure_domains": `"nope"`}
	boom := errors.New("boom")

	for _, tt := range []struct {
		name     string
		tc       *infrav1.TerraformCluster
		d        Deps
		concerns []string
		exports  string
		names    []string
		ok       bool
		wantErr  bool
	}{
		{name: "nil cluster", tc: nil, d: Deps{State: &fakeState{}}},
		{name: "external", tc: tc(true), d: Deps{State: &fakeState{err: boom}}, exports: "{}", names: []string{"b", "a"}, ok: true},
		{name: "managed", tc: tc(false), d: Deps{State: &fakeState{st: stateWith(good)}}, concerns: []string{"exports", "failure_domains"}, exports: `{"vpc":"v-1"}`, names: []string{"z1", "z2"}, ok: true},
		{name: "no state yet", tc: tc(false), d: Deps{State: &fakeState{}}, concerns: []string{"exports"}},
		{name: "concerning output", tc: tc(false), d: Deps{State: &fakeState{st: stateWith(badFDs)}}, concerns: []string{"exports", "failure_domains"}},
		{name: "unwatched concern ignored", tc: tc(false), d: Deps{State: &fakeState{st: stateWith(badFDs)}}, concerns: []string{"exports"}, exports: `{}`, ok: true},
		{name: "read failure", tc: tc(false), d: Deps{State: &fakeState{err: boom}}, concerns: []string{"exports"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			exports, names, ok, err := ReadClusterOutputs(t.Context(), tt.d, tt.tc, tt.concerns...)
			if (err != nil) != tt.wantErr || ok != tt.ok {
				t.Fatalf("ok=%v err=%v, want ok=%v wantErr=%v", ok, err, tt.ok, tt.wantErr)
			}
			if string(exports) != tt.exports {
				t.Errorf("exports = %s, want %s", exports, tt.exports)
			}
			// "unwatched concern ignored" decodes failure_domains badly: no names.
			if tt.name != "unwatched concern ignored" && !slices.Equal(names, tt.names) {
				t.Errorf("names = %v, want %v", names, tt.names)
			}
		})
	}
}
