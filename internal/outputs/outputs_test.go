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

package outputs

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// healthy is a raw health output JSON value describing a running, healthy
// instance.
const healthy = `{"state":"running","healthy":true,"message":null,"reasons":[]}`

// stateOf builds a State whose outputs have the given raw JSON values; it
// returns the built state.
func stateOf(outputs map[string]string) *state.State {
	s := &state.State{Outputs: map[string]state.Output{}}
	for k, v := range outputs {
		s.Outputs[k] = state.Output{Value: json.RawMessage(v)}
	}
	return s
}

// withOutputs returns a clone of base with overrides applied: each key set
// to its override value, or deleted when the override is "<absent>".
func withOutputs(base map[string]string, overrides map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		if v == "<absent>" {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

// validMachine is a raw machine outputs fixture with every output valid.
var validMachine = map[string]string{
	"provider_id":    `"libvirt:///vm-1"`,
	"addresses":      `[{"type":"InternalIP","address":"10.0.0.10"}]`,
	"failure_domain": `"zone-a"`,
	"interruptible":  `true`,
	"health":         healthy,
}

// TestDecodeMachine proves DecodeMachine handles every machine output
// case: valid, absent or null nullable outputs, a missing or malformed
// provider_id or health, invalid addresses and failure domain, and the
// reason precedence among missing, invalid and pending.
func TestDecodeMachine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		overrides map[string]string
		reason    string
		mentions  string
		check     func(t *testing.T, o *contract.MachineOutputs)
	}{
		{name: "valid", reason: infrav1.OutputsValidReason, check: func(t *testing.T, o *contract.MachineOutputs) {
			if *o.ProviderID != "libvirt:///vm-1" || *o.FailureDomain != "zone-a" || !*o.Interruptible || len(o.Addresses) != 1 {
				t.Errorf("outputs = %+v", o)
			}
		}},
		// Absent means null: runtimes do not persist null outputs.
		{name: "nullable outputs absent", overrides: map[string]string{"failure_domain": "<absent>", "interruptible": "<absent>", "addresses": "<absent>"},
			reason: infrav1.OutputsValidReason, check: func(t *testing.T, o *contract.MachineOutputs) {
				if o.FailureDomain != nil || o.Interruptible == nil || *o.Interruptible || o.Addresses == nil || len(o.Addresses) != 0 {
					t.Errorf("absent nullable outputs = %+v", o)
				}
			}},
		{name: "nullable outputs null", overrides: map[string]string{"failure_domain": "null", "interruptible": "null", "addresses": "null"}, reason: infrav1.OutputsValidReason},
		{name: "provider_id absent", overrides: map[string]string{"provider_id": "<absent>"}, reason: infrav1.OutputsPendingReason, mentions: "provider_id"},
		{name: "provider_id null", overrides: map[string]string{"provider_id": "null"}, reason: infrav1.OutputsPendingReason, mentions: "provider_id"},
		{name: "provider_id empty", overrides: map[string]string{"provider_id": `""`}, reason: infrav1.OutputsPendingReason, mentions: "provider_id"},
		{name: "provider_id too long", overrides: map[string]string{"provider_id": `"` + strings.Repeat("p", 513) + `"`}, reason: infrav1.OutputsInvalidReason, mentions: "provider_id: must be 1-512"},
		{name: "provider_id wrong type", overrides: map[string]string{"provider_id": `42`}, reason: infrav1.OutputsInvalidReason, mentions: "provider_id: is not a string"},
		{name: "health absent", overrides: map[string]string{"health": "<absent>"}, reason: infrav1.OutputsMissingReason, mentions: "missing outputs: health"},
		{name: "health null", overrides: map[string]string{"health": "null"}, reason: infrav1.OutputsMissingReason},
		{name: "health state outside enum", overrides: map[string]string{"health": `{"state":"booting","healthy":true}`}, reason: infrav1.OutputsInvalidReason, mentions: `state "booting"`},
		{name: "health without healthy", overrides: map[string]string{"health": `{"state":"running"}`}, reason: infrav1.OutputsInvalidReason, mentions: "healthy must be a boolean"},
		{name: "health healthy wrong type", overrides: map[string]string{"health": `{"state":"running","healthy":"yes"}`}, reason: infrav1.OutputsInvalidReason},
		{name: "health reasons null", overrides: map[string]string{"health": `{"state":"degraded","healthy":false,"reasons":null}`}, reason: infrav1.OutputsValidReason,
			check: func(t *testing.T, o *contract.MachineOutputs) {
				if o.Health.Reasons == nil || o.Health.State != contract.HealthDegraded {
					t.Errorf("health = %+v, want reasons []", o.Health)
				}
			}},
		{name: "unknown address type", overrides: map[string]string{"addresses": `[{"type":"PrivateIP","address":"10.0.0.1"}]`}, reason: infrav1.OutputsInvalidReason, mentions: `type "PrivateIP"`},
		{name: "empty address", overrides: map[string]string{"addresses": `[{"type":"InternalIP","address":""}]`}, reason: infrav1.OutputsInvalidReason},
		{name: "address too long", overrides: map[string]string{"addresses": fmt.Sprintf(`[{"type":"Hostname","address":%q}]`, strings.Repeat("h", 257))}, reason: infrav1.OutputsInvalidReason},
		{name: "addresses wrong type", overrides: map[string]string{"addresses": `"10.0.0.1"`}, reason: infrav1.OutputsInvalidReason},
		// The 256 limit applies to what reaches status: duplicates collapse first.
		{name: "257 distinct addresses", overrides: map[string]string{"addresses": manyAddresses(257, false)}, reason: infrav1.OutputsInvalidReason, mentions: "at most 256 distinct"},
		{name: "300 addresses, 100 distinct", overrides: map[string]string{"addresses": manyAddresses(300, true)}, reason: infrav1.OutputsValidReason},
		{name: "failure_domain too long", overrides: map[string]string{"failure_domain": `"` + strings.Repeat("z", 257) + `"`}, reason: infrav1.OutputsInvalidReason},
		{name: "interruptible wrong type", overrides: map[string]string{"interruptible": `"no"`}, reason: infrav1.OutputsInvalidReason},
		// Missing outranks invalid, which outranks pending.
		{name: "precedence", overrides: map[string]string{"health": "<absent>", "provider_id": "null", "interruptible": `1`}, reason: infrav1.OutputsMissingReason},
		{name: "invalid over pending", overrides: map[string]string{"provider_id": "null", "interruptible": `1`}, reason: infrav1.OutputsInvalidReason},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o, res := DecodeMachine(stateOf(withOutputs(validMachine, c.overrides)))
			if got := res.Reason(); got != c.reason {
				t.Fatalf("Reason = %s, want %s (%s)", got, c.reason, res.Message())
			}
			if c.mentions != "" && !strings.Contains(res.Message(), c.mentions) {
				t.Errorf("Message %q does not mention %q", res.Message(), c.mentions)
			}
			if c.check != nil {
				c.check(t, o)
			}
		})
	}
}

// TestMessagesDoNotQuoteValues: outputs may be sensitive.
func TestMessagesDoNotQuoteValues(t *testing.T) {
	t.Parallel()
	secret := "s3cr3t-value"
	_, res := DecodeMachine(stateOf(withOutputs(validMachine, map[string]string{
		"provider_id": `"` + secret + strings.Repeat("x", 520) + `"`,
		"addresses":   fmt.Sprintf(`[{"type":"Hostname","address":"%s%s"}]`, secret, strings.Repeat("x", 260)),
	})))
	_, cres := DecodeCluster(stateOf(map[string]string{"health": healthy, "control_plane_endpoint": `{"host":"` + secret + `","port":0}`, "exports": `{"k":"` + secret + `","r":0.5}`}))
	for _, msg := range []string{res.Message(), cres.Message()} {
		if msg == "" || strings.Contains(msg, secret) {
			t.Errorf("message %q is empty or quotes an output value", msg)
		}
	}
}

// TestDecodeCluster proves DecodeCluster handles every cluster output
// case: valid, absent nullable outputs, a null-equivalent endpoint,
// invalid endpoint and failure domains, and non-integer exports.
func TestDecodeCluster(t *testing.T) {
	t.Parallel()
	valid := map[string]string{
		"control_plane_endpoint": `{"host":"api.example.com","port":6443}`,
		"failure_domains":        `[{"name":"zone-a","control_plane":false,"attributes":{"rack":"1"}},{"name":"zone-b"}]`,
		"exports":                `{"network_id":"net-1","vlan":100}`,
		"health":                 healthy,
	}
	cases := []struct {
		name      string
		overrides map[string]string
		reason    string
		mentions  string
		check     func(t *testing.T, o *contract.ClusterOutputs)
	}{
		{name: "valid", reason: infrav1.OutputsValidReason, check: func(t *testing.T, o *contract.ClusterOutputs) {
			if o.ControlPlaneEndpoint == nil || o.ControlPlaneEndpoint.Port != 6443 || len(o.FailureDomains) != 2 || string(o.Exports) != valid["exports"] {
				t.Errorf("outputs = %+v", o)
			}
		}},
		{name: "all nullable absent", overrides: map[string]string{"control_plane_endpoint": "<absent>", "failure_domains": "<absent>", "exports": "<absent>"},
			reason: infrav1.OutputsValidReason, check: func(t *testing.T, o *contract.ClusterOutputs) {
				if o.ControlPlaneEndpoint != nil || o.FailureDomains != nil || string(o.Exports) != "{}" {
					t.Errorf("outputs = %+v", o)
				}
			}},
		{name: "exports null", overrides: map[string]string{"exports": "null"}, reason: infrav1.OutputsValidReason,
			check: func(t *testing.T, o *contract.ClusterOutputs) {
				if string(o.Exports) != "{}" {
					t.Errorf("exports = %s", o.Exports)
				}
			}},
		{name: "endpoint both zero is null", overrides: map[string]string{"control_plane_endpoint": `{"host":"","port":0}`}, reason: infrav1.OutputsValidReason,
			check: func(t *testing.T, o *contract.ClusterOutputs) {
				if o.ControlPlaneEndpoint != nil {
					t.Errorf("endpoint = %+v, want nil", o.ControlPlaneEndpoint)
				}
			}},
		{name: "endpoint without port", overrides: map[string]string{"control_plane_endpoint": `{"host":"api.example.com","port":0}`}, reason: infrav1.OutputsInvalidReason, mentions: "host and port must both be set"},
		{name: "endpoint without host", overrides: map[string]string{"control_plane_endpoint": `{"host":"","port":6443}`}, reason: infrav1.OutputsInvalidReason},
		{name: "endpoint port range", overrides: map[string]string{"control_plane_endpoint": `{"host":"a","port":70000}`}, reason: infrav1.OutputsInvalidReason},
		{name: "endpoint host too long", overrides: map[string]string{"control_plane_endpoint": fmt.Sprintf(`{"host":%q,"port":6443}`, strings.Repeat("h", 513))}, reason: infrav1.OutputsInvalidReason},
		{name: "endpoint wrong type", overrides: map[string]string{"control_plane_endpoint": `"api:6443"`}, reason: infrav1.OutputsInvalidReason},
		{name: "duplicate failure domain", overrides: map[string]string{"failure_domains": `[{"name":"a"},{"name":"a"}]`}, reason: infrav1.OutputsInvalidReason, mentions: "repeats"},
		{name: "empty failure domain name", overrides: map[string]string{"failure_domains": `[{"name":""}]`}, reason: infrav1.OutputsInvalidReason},
		{name: "too many failure domains", overrides: map[string]string{"failure_domains": manyDomains(101)}, reason: infrav1.OutputsInvalidReason, mentions: "at most 100"},
		{name: "fractional exports", overrides: map[string]string{"exports": `{"ratio":0.5}`}, reason: infrav1.OutputsInvalidReason, mentions: "integers or strings"},
		{name: "health missing", overrides: map[string]string{"health": "<absent>"}, reason: infrav1.OutputsMissingReason},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o, res := DecodeCluster(stateOf(withOutputs(valid, c.overrides)))
			if got := res.Reason(); got != c.reason {
				t.Fatalf("Reason = %s, want %s (%s)", got, c.reason, res.Message())
			}
			if c.mentions != "" && !strings.Contains(res.Message(), c.mentions) {
				t.Errorf("Message %q does not mention %q", res.Message(), c.mentions)
			}
			if c.check != nil {
				c.check(t, o)
			}
		})
	}
}

// validPool is a raw machinepool outputs fixture with every output valid.
var validPool = map[string]string{
	"provider_id":      `"libvirt-group:///pool-1"`,
	"provider_id_list": `["libvirt:///vm-1","libvirt:///vm-2"]`,
	"replicas":         `2`,
	"instances":        `[{"provider_id":"libvirt:///vm-1","state":"running"},{"provider_id":"libvirt:///vm-2","state":"running"}]`,
	"health":           healthy,
}

// manyInstances returns a raw instances output JSON array of n entries with
// distinct provider ids, all state "running".
func manyInstances(n int) string {
	var parts []string
	for i := range n {
		parts = append(parts, fmt.Sprintf(`{"provider_id":"vm-%d","state":"running"}`, i))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// TestDecodeMachinePool proves DecodeMachinePool handles every machinepool
// output case: valid, a pending provider_id and replicas, missing
// provider_id_list/instances, an invalid entry in each list, negative
// replicas, provider_id_list sorted and deduplicated, and a truncated
// instances list.
func TestDecodeMachinePool(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		overrides map[string]string
		reason    string
		mentions  string
		check     func(t *testing.T, o *contract.MachinePoolOutputs)
	}{
		{name: "valid", reason: infrav1.OutputsValidReason, check: func(t *testing.T, o *contract.MachinePoolOutputs) {
			if *o.ProviderID != "libvirt-group:///pool-1" || *o.Replicas != 2 || len(o.ProviderIDList) != 2 || len(o.Instances) != 2 {
				t.Errorf("outputs = %+v", o)
			}
		}},
		{name: "provider_id absent", overrides: map[string]string{"provider_id": "<absent>"}, reason: infrav1.OutputsPendingReason, mentions: "provider_id"},
		{name: "provider_id null", overrides: map[string]string{"provider_id": "null"}, reason: infrav1.OutputsPendingReason, mentions: "provider_id"},
		{name: "provider_id too long", overrides: map[string]string{"provider_id": `"` + strings.Repeat("p", 513) + `"`}, reason: infrav1.OutputsInvalidReason, mentions: "provider_id: must be 1-512"},
		{name: "empty list, replicas 0", overrides: map[string]string{"provider_id_list": "[]", "instances": "[]", "replicas": "0"},
			reason: infrav1.OutputsValidReason, check: func(t *testing.T, o *contract.MachinePoolOutputs) {
				if len(o.ProviderIDList) != 0 || len(o.Instances) != 0 || o.Replicas == nil || *o.Replicas != 0 {
					t.Errorf("outputs = %+v", o)
				}
			}},
		{name: "provider_id_list absent", overrides: map[string]string{"provider_id_list": "<absent>"}, reason: infrav1.OutputsMissingReason, mentions: "missing outputs: provider_id_list"},
		{name: "provider_id_list null", overrides: map[string]string{"provider_id_list": "null"}, reason: infrav1.OutputsMissingReason},
		{name: "provider_id_list wrong type", overrides: map[string]string{"provider_id_list": `"vm-1"`}, reason: infrav1.OutputsInvalidReason},
		{name: "bad id in provider_id_list", overrides: map[string]string{"provider_id_list": `["vm-1",""]`}, reason: infrav1.OutputsInvalidReason, mentions: "provider_id_list: entry 1"},
		{name: "provider_id_list sorted and deduped", overrides: map[string]string{"provider_id_list": `["vm-2","vm-1","vm-2"]`}, reason: infrav1.OutputsValidReason,
			check: func(t *testing.T, o *contract.MachinePoolOutputs) {
				if !slices.Equal(o.ProviderIDList, []string{"vm-1", "vm-2"}) {
					t.Errorf("provider_id_list = %v", o.ProviderIDList)
				}
			}},
		{name: "instances absent", overrides: map[string]string{"instances": "<absent>"}, reason: infrav1.OutputsMissingReason, mentions: "missing outputs: instances"},
		{name: "instances null", overrides: map[string]string{"instances": "null"}, reason: infrav1.OutputsMissingReason},
		{name: "instances wrong type", overrides: map[string]string{"instances": `"vm-1"`}, reason: infrav1.OutputsInvalidReason},
		{name: "bad provider id in instances", overrides: map[string]string{"instances": `[{"provider_id":""}]`}, reason: infrav1.OutputsInvalidReason, mentions: "instances: entry 0 provider_id"},
		{name: "bad instance state", overrides: map[string]string{"instances": `[{"provider_id":"vm-1","state":"booting"}]`}, reason: infrav1.OutputsInvalidReason, mentions: `instances: entry 0 state "booting"`},
		{name: "1001 instances truncated", overrides: map[string]string{"instances": manyInstances(1001)}, reason: infrav1.InstancesTruncatedReason, mentions: "outputs truncated: instances",
			check: func(t *testing.T, o *contract.MachinePoolOutputs) {
				if len(o.Instances) != MaxInstances {
					t.Errorf("instances = %d, want %d", len(o.Instances), MaxInstances)
				}
			}},
		{name: "1000 instances not truncated", overrides: map[string]string{"instances": manyInstances(1000)}, reason: infrav1.OutputsValidReason},
		{name: "replicas absent", overrides: map[string]string{"replicas": "<absent>"}, reason: infrav1.OutputsPendingReason, mentions: "replicas"},
		{name: "replicas null", overrides: map[string]string{"replicas": "null"}, reason: infrav1.OutputsPendingReason},
		{name: "replicas negative", overrides: map[string]string{"replicas": "-1"}, reason: infrav1.OutputsInvalidReason, mentions: "replicas: must be >= 0"},
		{name: "replicas non-integer", overrides: map[string]string{"replicas": "1.5"}, reason: infrav1.OutputsInvalidReason},
		{name: "health absent", overrides: map[string]string{"health": "<absent>"}, reason: infrav1.OutputsMissingReason},
		// Missing outranks invalid, which outranks pending, which outranks truncated.
		{name: "precedence missing over invalid", overrides: map[string]string{"health": "<absent>", "replicas": "-1"}, reason: infrav1.OutputsMissingReason},
		{name: "precedence invalid over pending", overrides: map[string]string{"replicas": "null", "provider_id": `"` + strings.Repeat("p", 513) + `"`}, reason: infrav1.OutputsInvalidReason},
		{name: "precedence pending over truncated", overrides: map[string]string{"replicas": "null", "instances": manyInstances(1001)}, reason: infrav1.OutputsPendingReason},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o, res := DecodeMachinePool(stateOf(withOutputs(validPool, c.overrides)))
			if got := res.Reason(); got != c.reason {
				t.Fatalf("Reason = %s, want %s (%s)", got, c.reason, res.Message())
			}
			if c.mentions != "" && !strings.Contains(res.Message(), c.mentions) {
				t.Errorf("Message %q does not mention %q", res.Message(), c.mentions)
			}
			if c.reason == infrav1.OutputsValidReason || c.reason == infrav1.InstancesTruncatedReason {
				if !res.Valid() {
					t.Errorf("Valid() = false for reason %s", c.reason)
				}
				if res.Status() != metav1.ConditionTrue {
					t.Errorf("Status() = %s for reason %s, want True", res.Status(), c.reason)
				}
			} else if res.Valid() {
				t.Errorf("Valid() = true for reason %s", c.reason)
			}
			if c.check != nil {
				c.check(t, o)
			}
		})
	}
}

// manyAddresses returns a raw addresses output JSON array of n InternalIP
// entries; repeat cycles through only 100 distinct addresses, so entry
// count and distinct count can be tested separately.
func manyAddresses(n int, repeat bool) string {
	var parts []string
	for i := range n {
		if repeat {
			i %= 100
		}
		parts = append(parts, fmt.Sprintf(`{"type":"InternalIP","address":"10.0.%d.%d"}`, i/256, i%256))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// manyDomains returns a raw failure_domains output JSON array of n
// distinctly named entries.
func manyDomains(n int) string {
	var parts []string
	for i := range n {
		parts = append(parts, fmt.Sprintf(`{"name":"fd-%d"}`, i))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// TestSortAndDedupe proves SortAddresses orders addresses by type
// precedence then address without modifying its input, that unknown types
// sort last, and that Dedupe of the decode path collapses a duplicate
// entry into one status entry.
func TestSortAndDedupe(t *testing.T) {
	t.Parallel()
	shuffled := []contract.Address{
		{Type: "Hostname", Address: "vm-1"},
		{Type: "ExternalDNS", Address: "vm-1.example.com"},
		{Type: "InternalIP", Address: "10.0.0.2"},
		{Type: "ExternalIP", Address: "203.0.113.1"},
		{Type: "InternalDNS", Address: "vm-1.internal"},
		{Type: "InternalIP", Address: "10.0.0.1"},
		{Type: "InternalIP", Address: "10.0.0.1"},
	}
	got := Dedupe(SortAddresses(shuffled))
	want := []contract.Address{
		{Type: "InternalIP", Address: "10.0.0.1"},
		{Type: "InternalIP", Address: "10.0.0.2"},
		{Type: "InternalDNS", Address: "vm-1.internal"},
		{Type: "ExternalIP", Address: "203.0.113.1"},
		{Type: "ExternalDNS", Address: "vm-1.example.com"},
		{Type: "Hostname", Address: "vm-1"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("sorted = %v, want %v", got, want)
	}
	if shuffled[0].Type != "Hostname" {
		t.Error("SortAddresses modified its input")
	}
	// Unknown types sort last (they are rejected by validation anyway).
	if s := SortAddresses([]contract.Address{{Type: "Zeta"}, {Type: "Hostname"}}); s[0].Type != "Hostname" {
		t.Errorf("unknown type sorted first: %v", s)
	}
	// Dedupe of the decode path: duplicates collapse into one status entry.
	o, res := DecodeMachine(stateOf(withOutputs(validMachine, map[string]string{
		"addresses": `[{"type":"Hostname","address":"a"},{"type":"InternalIP","address":"b"},{"type":"Hostname","address":"a"}]`,
	})))
	if !res.Valid() || len(o.Addresses) != 2 || o.Addresses[0].Type != "InternalIP" {
		t.Errorf("decoded addresses = %v (%s)", o.Addresses, res.Message())
	}
}

// TestPlacementAndProviderID proves CheckPlacement flags only a
// requested-but-different (or missing) actual domain, ProviderIDChanged
// flags only a changed non-empty value, and Result.Reason/Status apply
// the declared precedence among controller-added and decode violations.
func TestPlacementAndProviderID(t *testing.T) {
	t.Parallel()
	a, b := "zone-a", "zone-b"
	cases := []struct {
		requested, actual *string
		mismatch          bool
	}{{nil, nil, false}, {nil, &a, false}, {&a, &a, false}, {&a, &b, true}, {&a, nil, true}}
	for _, c := range cases {
		v := CheckPlacement(c.requested, c.actual)
		if (v != nil) != c.mismatch || (v != nil && v.Reason != infrav1.FailureDomainMismatchReason) {
			t.Errorf("CheckPlacement(%v, %v) = %v", c.requested, c.actual, v)
		}
	}
	for _, c := range []struct {
		prev, next string
		changed    bool
	}{{"", "x", false}, {"x", "", false}, {"x", "x", false}, {"x", "y", true}} {
		if got := ProviderIDChanged(c.prev, c.next); got != c.changed {
			t.Errorf("ProviderIDChanged(%q, %q) = %v", c.prev, c.next, got)
		}
	}
	// Reason precedence among controller-added violations.
	r := Result{Violations: []Violation{ProviderIDChangedViolation(), *CheckPlacement(&a, &b)}, Pending: []string{"x"}}
	if r.Reason() != infrav1.FailureDomainMismatchReason || r.Status() != metav1.ConditionFalse {
		t.Errorf("Reason = %s, Status = %s", r.Reason(), r.Status())
	}
	if (Result{Pending: []string{"provider_id"}}).Status() != metav1.ConditionUnknown || (Result{}).Status() != metav1.ConditionTrue {
		t.Error("Status for pending/valid")
	}
	if (Result{Violations: []Violation{{Reason: "Other"}}}).Reason() != "Other" {
		t.Error("an unlisted violation reason is not reported")
	}
}

// TestReasonsAreDeclared proves every OutputsValid reason this package
// produces is declared for the matching status in api/v1alpha1's
// ConditionReasons table.
func TestReasonsAreDeclared(t *testing.T) {
	t.Parallel()
	table := infrav1.ConditionReasons()[infrav1.OutputsValidCondition]
	for status, reasons := range map[metav1.ConditionStatus][]string{
		metav1.ConditionTrue:    {infrav1.OutputsValidReason, infrav1.InstancesTruncatedReason},
		metav1.ConditionFalse:   {infrav1.OutputsMissingReason, infrav1.OutputsInvalidReason, infrav1.FailureDomainMismatchReason, infrav1.ProviderIDChangedReason},
		metav1.ConditionUnknown: {infrav1.OutputsPendingReason},
	} {
		for _, r := range reasons {
			if !slices.Contains(table[status], r) {
				t.Errorf("reason %s is not an OutputsValid=%s reason in api/v1alpha1", r, status)
			}
		}
	}
}

// TestCAPIConverters proves FailureDomains, MachineAddresses and
// APIEndpoint convert correctly (including the control_plane default) and
// all three return nil for a nil or empty input.
func TestCAPIConverters(t *testing.T) {
	t.Parallel()
	f := false
	fds := FailureDomains([]contract.FailureDomain{{Name: "a"}, {Name: "b", ControlPlane: &f, Attributes: map[string]string{"k": "v"}}})
	if len(fds) != 2 || !*fds[0].ControlPlane || *fds[1].ControlPlane || fds[0].Attributes != nil || fds[1].Attributes["k"] != "v" {
		t.Errorf("FailureDomains = %+v", fds)
	}
	if FailureDomains(nil) != nil || MachineAddresses(nil) != nil || APIEndpoint(nil) != nil {
		t.Error("nil inputs must convert to nil")
	}
	addrs := MachineAddresses([]contract.Address{{Type: "InternalIP", Address: "10.0.0.1"}})
	if len(addrs) != 1 || addrs[0].Type != clusterv1.MachineInternalIP {
		t.Errorf("MachineAddresses = %+v", addrs)
	}
	if e := APIEndpoint(&contract.Endpoint{Host: "h", Port: 6443}); e.Host != "h" || e.Port != 6443 || !e.IsValid() {
		t.Errorf("APIEndpoint = %+v", e)
	}
}
