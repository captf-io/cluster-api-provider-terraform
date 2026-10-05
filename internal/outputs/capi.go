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
	"maps"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// MachineAddresses converts validated, sorted addrs to the CAPI type; it
// returns the converted list, or nil for an empty addrs.
func MachineAddresses(addrs []contract.Address) []clusterv1.MachineAddress {
	if len(addrs) == 0 {
		return nil
	}
	out := make([]clusterv1.MachineAddress, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, clusterv1.MachineAddress{Type: clusterv1.MachineAddressType(a.Type), Address: a.Address})
	}
	return out
}

// FailureDomains converts validated failure domains fds to the v1beta2
// list shape. control_plane defaults to true when the module omitted it
// (KCP only uses domains with controlPlane true); attributes default to
// none. It returns the converted list, or nil for an empty fds.
func FailureDomains(fds []contract.FailureDomain) []clusterv1.FailureDomain {
	if len(fds) == 0 {
		return nil
	}
	out := make([]clusterv1.FailureDomain, 0, len(fds))
	for _, fd := range fds {
		cp := true
		if fd.ControlPlane != nil {
			cp = *fd.ControlPlane
		}
		var attrs map[string]string
		if len(fd.Attributes) > 0 {
			attrs = maps.Clone(fd.Attributes)
		}
		out = append(out, clusterv1.FailureDomain{Name: fd.Name, ControlPlane: &cp, Attributes: attrs})
	}
	return out
}

// APIEndpoint converts validated endpoint e to the CAPI type; it returns
// nil for a nil e.
func APIEndpoint(e *contract.Endpoint) *clusterv1.APIEndpoint {
	if e == nil {
		return nil
	}
	return &clusterv1.APIEndpoint{Host: e.Host, Port: e.Port}
}
