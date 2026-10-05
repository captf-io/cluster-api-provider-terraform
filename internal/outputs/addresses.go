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
	"cmp"
	"slices"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// SortAddresses returns addrs in canonical order: by type precedence
// InternalIP, InternalDNS, ExternalIP, ExternalDNS, Hostname (unknown types
// last), then by address. CAPI copies the list verbatim, so a stable order
// keeps status from flapping and puts the InternalIP first.
func SortAddresses(addrs []contract.Address) []contract.Address {
	out := slices.Clone(addrs)
	rank := func(t string) int {
		if i := slices.Index(addressTypes, t); i >= 0 {
			return i
		}
		return len(addressTypes)
	}
	slices.SortStableFunc(out, func(a, b contract.Address) int {
		return cmp.Or(cmp.Compare(rank(a.Type), rank(b.Type)), cmp.Compare(a.Type, b.Type), cmp.Compare(a.Address, b.Address))
	})
	return out
}

// Dedupe drops repeated (type, address) pairs from sorted list addrs; it
// returns the deduplicated list.
func Dedupe(addrs []contract.Address) []contract.Address {
	return slices.Compact(slices.Clone(addrs))
}
