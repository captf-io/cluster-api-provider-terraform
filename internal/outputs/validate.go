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
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// CAPI v1beta2 CRD markers the mapped values must satisfy
// (capi/api/core/v1beta2/{cluster_types,common_types,machine_types,machinepool_types}.go).
const (
	maxProviderID     = 512
	maxAddresses      = 256
	maxInstanceID     = 256
	maxAddressLength  = 256
	maxEndpointHost   = 512
	maxPort           = 65535
	maxFailureDomains = 100
	maxFailureDomain  = 256
	maxProviderIDList = 10000
)

// addressTypes are the MachineAddressType values in canonical order: the
// first InternalIP is what RKE2's internal-first selection and most
// consumers pick.
var addressTypes = []string{"InternalIP", "InternalDNS", "ExternalIP", "ExternalDNS", "Hostname"}

// invalid returns an OutputsInvalid violation for output, its message
// formatted from format and args. Messages name the output and the rule,
// never a free-form value: sensitive outputs are plaintext in state.
func invalid(output, format string, args ...any) Violation {
	return Violation{Output: output, Reason: infrav1.OutputsInvalidReason, Message: output + ": " + fmt.Sprintf(format, args...)}
}

// NormalizeProviderID treats an id of "" like null: modules often cannot
// produce null. It returns id unchanged otherwise, or nil for a nil or
// empty id.
func NormalizeProviderID(id *string) *string {
	if id == nil || *id == "" {
		return nil
	}
	return id
}

// ValidateProviderID checks the 1..512 character marker of non-null id
// against output (the outer output name: "provider_id" for the machine
// and pool group id, "provider_id_list" or "instances" when id is one
// entry of a list); it returns a violation if id fails the check, else
// nil.
func ValidateProviderID(output, id string) *Violation {
	if id == "" || len(id) > maxProviderID {
		v := invalid(output, "must be 1-%d characters, got %d", maxProviderID, len(id))
		return &v
	}
	return nil
}

// ValidateProviderIDList checks every entry of ids, the machinepool role's
// provider_id_list, against the same 1..512 character marker
// ValidateProviderID enforces on a single provider_id; it returns a
// violation per failing entry, output "provider_id_list".
func ValidateProviderIDList(ids []string) []Violation {
	var vs []Violation
	for i, id := range ids {
		if v := ValidateProviderID("provider_id_list", id); v != nil {
			v.Message = fmt.Sprintf("provider_id_list: entry %d must be 1-%d characters, got %d", i, maxProviderID, len(id))
			vs = append(vs, *v)
		}
	}
	return vs
}

// ValidateProviderIDListCount checks the at-most-10000 marker
// (MachinePool.spec.providerIDList, capi/api/core/v1beta2/machinepool_types.go)
// against n, the sorted, deduplicated entry count; it returns a violation
// if n exceeds it, else nil.
func ValidateProviderIDListCount(n int) *Violation {
	if n > maxProviderIDList {
		v := invalid("provider_id_list", "at most %d entries, got %d", maxProviderIDList, n)
		return &v
	}
	return nil
}

// ValidateInstances checks every entry of instances, the machinepool
// role's instances output: provider_id (ValidateProviderID), addresses
// (ValidateAddresses, then the distinct count against the 256 cap), instance_id (1-256 characters when set), the failure_domain name (ValidateFailureDomainName)
// when set, and state against the health.state enum when set. It returns
// a violation per failing entry, output "instances", each message naming
// the entry index.
func ValidateInstances(instances []contract.PoolInstance) []Violation {
	var vs []Violation
	for i, inst := range instances {
		if v := ValidateProviderID("instances", inst.ProviderID); v != nil {
			v.Message = fmt.Sprintf("instances: entry %d provider_id must be 1-%d characters, got %d", i, maxProviderID, len(inst.ProviderID))
			vs = append(vs, *v)
		}
		if inst.InstanceID != nil && (*inst.InstanceID == "" || len(*inst.InstanceID) > maxInstanceID) {
			vs = append(vs, invalid("instances", "entry %d instance_id must be 1-%d characters, got %d", i, maxInstanceID, len(*inst.InstanceID)))
		}
		if avs := ValidateAddresses(inst.Addresses); len(avs) > 0 {
			for _, v := range avs {
				v.Output = "instances"
				v.Message = fmt.Sprintf("instances: entry %d address %s", i, strings.TrimPrefix(v.Message, "addresses: entry "))
				vs = append(vs, v)
			}
		} else if n := len(Dedupe(SortAddresses(inst.Addresses))); ValidateAddressCount(n) != nil {
			vs = append(vs, invalid("instances", "entry %d addresses: at most %d distinct entries, got %d", i, maxAddresses, n))
		}
		if inst.FailureDomain != nil {
			if v := ValidateFailureDomainName("instances", *inst.FailureDomain); v != nil {
				v.Message = fmt.Sprintf("instances: entry %d failure_domain must be 1-%d characters, got %d", i, maxFailureDomain, len(*inst.FailureDomain))
				vs = append(vs, *v)
			}
		}
		if inst.State != nil && !slices.Contains(contract.HealthStates(), *inst.State) {
			vs = append(vs, invalid("instances", "entry %d state %q is not one of %v", i, *inst.State, contract.HealthStates()))
		}
	}
	return vs
}

// ValidateAddresses checks every address in addrs for a valid type and
// length; it returns a violation per failing entry. The entry count is
// checked with ValidateAddressCount on the deduplicated list, which is
// what reaches status.
func ValidateAddresses(addrs []contract.Address) []Violation {
	var vs []Violation
	for i, a := range addrs {
		if !slices.Contains(addressTypes, a.Type) {
			vs = append(vs, invalid("addresses", "entry %d has type %q, not one of %v", i, a.Type, addressTypes))
		}
		if a.Address == "" || len(a.Address) > maxAddressLength {
			vs = append(vs, invalid("addresses", "entry %d address must be 1-%d characters, got %d", i, maxAddressLength, len(a.Address)))
		}
	}
	return vs
}

// ValidateAddressCount checks the at-most-256 marker against n, the
// deduplicated address count; it returns a violation if n exceeds it, else
// nil.
func ValidateAddressCount(n int) *Violation {
	if n > maxAddresses {
		v := invalid("addresses", "at most %d distinct entries, got %d", maxAddresses, n)
		return &v
	}
	return nil
}

// ValidateEndpoint checks non-null endpoint e, reporting violations under
// output: host 1..512 and port 1..65535, both set. A half-set endpoint is
// invalid, never partially written (APIEndpoint.IsValid requires both). It
// returns a violation if e fails a check, else nil.
func ValidateEndpoint(output string, e contract.Endpoint) *Violation {
	switch {
	case e.Host == "" || e.Port == 0:
		v := invalid(output, "host and port must both be set")
		return &v
	case len(e.Host) > maxEndpointHost:
		v := invalid(output, "host must be at most %d characters, got %d", maxEndpointHost, len(e.Host))
		return &v
	case e.Port < 1 || e.Port > maxPort:
		v := invalid(output, "port must be 1-%d, got %d", maxPort, e.Port)
		return &v
	}
	return nil
}

// ValidateFailureDomainName checks the 1..256 character marker against
// name, reporting a violation under output; it returns that violation if
// name fails the check, else nil.
func ValidateFailureDomainName(output, name string) *Violation {
	if name == "" || len(name) > maxFailureDomain {
		v := invalid(output, "failure domain name must be 1-%d characters, got %d", maxFailureDomain, len(name))
		return &v
	}
	return nil
}

// ValidateFailureDomains checks fds' names, uniqueness and entry count; it
// returns a violation per problem found.
func ValidateFailureDomains(fds []contract.FailureDomain) []Violation {
	var vs []Violation
	seen := sets.New[string]()
	for i, fd := range fds {
		if v := ValidateFailureDomainName("failure_domains", fd.Name); v != nil {
			v.Message = fmt.Sprintf("failure_domains: entry %d name must be 1-%d characters, got %d", i, maxFailureDomain, len(fd.Name))
			vs = append(vs, *v)
		}
		if seen.Has(fd.Name) {
			vs = append(vs, invalid("failure_domains", "entry %d repeats an earlier name", i))
		}
		seen.Insert(fd.Name)
	}
	if len(fds) > maxFailureDomains {
		vs = append(vs, invalid("failure_domains", "at most %d entries, got %d", maxFailureDomains, len(fds)))
	}
	return vs
}

// ValidateHealth checks h.State against the closed enum; it returns a
// violation if h.State is not one of contract.HealthStates, else nil.
func ValidateHealth(h contract.Health) *Violation {
	if !slices.Contains(contract.HealthStates(), h.State) {
		v := invalid("health", "state %q is not one of %v", h.State, contract.HealthStates())
		return &v
	}
	return nil
}
