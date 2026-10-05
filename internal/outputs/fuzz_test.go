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
	"slices"
	"testing"
)

// FuzzDecodeMachine proves DecodeMachine survives arbitrary raw output JSON
// without panicking and never reports valid outputs that break the CAPI
// limits: whenever the Result is Valid, the provider ID is set and at most
// 512 characters, every address has a known type and 1-256 characters and
// the list is sorted, deduplicated and at most 256 long, the failure
// domain is unset or 1-256 characters, and interruptible is set.
func FuzzDecodeMachine(f *testing.F) {
	f.Add(`"libvirt:///vm-1"`, `[{"type":"InternalIP","address":"10.0.0.10"}]`, `"zone-a"`, `true`, healthy)
	f.Add(`""`, `null`, `null`, `null`, healthy)
	f.Add(`1`, `[{"type":"Bogus","address":""}]`, `""`, `"yes"`, `{"state":"nope"}`)
	f.Add(`"i-1"`, `[{"type":"Hostname","address":"a"},{"type":"Hostname","address":"a"},{"type":"ExternalIP","address":"b"}]`, `"z"`, `false`, `{"state":"running","healthy":false,"message":"x","reasons":["r"]}`)
	f.Add(`{`, `[[`, `\u0000`, ``, `[]`)
	f.Fuzz(func(t *testing.T, providerID, addresses, failureDomain, interruptible, health string) {
		out, res := DecodeMachine(stateOf(map[string]string{
			"provider_id":    providerID,
			"addresses":      addresses,
			"failure_domain": failureDomain,
			"interruptible":  interruptible,
			"health":         health,
		}))
		_ = res.Message()
		if out == nil {
			t.Fatal("DecodeMachine returned nil outputs")
		}
		if !res.Valid() {
			return
		}
		if out.ProviderID == nil || ValidateProviderID("provider_id", *out.ProviderID) != nil {
			t.Fatalf("valid result with provider_id %v", out.ProviderID)
		}
		if vs := ValidateAddresses(out.Addresses); len(vs) > 0 {
			t.Fatalf("valid result with invalid addresses: %v", vs)
		}
		if ValidateAddressCount(len(out.Addresses)) != nil {
			t.Fatalf("valid result with %d addresses", len(out.Addresses))
		}
		if !slices.Equal(out.Addresses, Dedupe(SortAddresses(out.Addresses))) {
			t.Fatalf("valid result with unsorted or duplicate addresses: %v", out.Addresses)
		}
		if out.FailureDomain != nil && ValidateFailureDomainName("failure_domain", *out.FailureDomain) != nil {
			t.Fatalf("valid result with failure_domain %q", *out.FailureDomain)
		}
		if out.Interruptible == nil {
			t.Fatal("valid result with nil interruptible")
		}
	})
}
