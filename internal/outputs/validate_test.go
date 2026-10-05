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

package outputs

import (
	"fmt"
	"strings"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// distinctAddrs returns n distinct InternalIP addresses.
func distinctAddrs(n int) []contract.Address {
	out := make([]contract.Address, n)
	for i := range out {
		out[i] = contract.Address{Type: "InternalIP", Address: fmt.Sprintf("10.0.%d.%d", i/250, i%250)}
	}
	return out
}

// TestValidateInstancesCaps checks the per-instance address count and
// instance_id length caps and the violation message format.
func TestValidateInstancesCaps(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 257)
	ok := strings.Repeat("x", 256)
	empty := ""
	dup := append(distinctAddrs(256), distinctAddrs(256)...)
	cases := []struct {
		name string
		inst []contract.PoolInstance
		want string // substring of the single violation; empty for none
	}{
		{name: "256 distinct ok", inst: []contract.PoolInstance{{ProviderID: "p", Addresses: distinctAddrs(256)}}},
		{name: "duplicates dedupe under cap", inst: []contract.PoolInstance{{ProviderID: "p", Addresses: dup}}},
		{
			name: "257 distinct rejected",
			inst: []contract.PoolInstance{{ProviderID: "p"}, {ProviderID: "p", Addresses: distinctAddrs(257)}},
			want: "instances: entry 1 addresses: at most 256 distinct entries, got 257",
		},
		{name: "256-char id ok", inst: []contract.PoolInstance{{ProviderID: "p", InstanceID: &ok}}},
		{
			name: "257-char id rejected",
			inst: []contract.PoolInstance{{ProviderID: "p", InstanceID: &long}},
			want: "instances: entry 0 instance_id must be 1-256 characters, got 257",
		},
		{
			name: "empty id rejected",
			inst: []contract.PoolInstance{{ProviderID: "p", InstanceID: &empty}},
			want: "instances: entry 0 instance_id must be 1-256 characters, got 0",
		},
		{
			name: "address message labels both indices",
			inst: []contract.PoolInstance{
				{ProviderID: "p"}, {ProviderID: "p"}, {ProviderID: "p"},
				{ProviderID: "p", Addresses: append(distinctAddrs(7), contract.Address{Type: "Bogus", Address: "a"})},
			},
			want: "instances: entry 3 address 7 has type",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			vs := ValidateInstances(c.inst)
			if c.want == "" {
				if len(vs) != 0 {
					t.Fatalf("unexpected violations: %v", vs)
				}
				return
			}
			if len(vs) != 1 || !strings.Contains(vs[0].Message, c.want) || vs[0].Output != "instances" {
				t.Fatalf("violations = %+v, want one containing %q", vs, c.want)
			}
		})
	}
}
