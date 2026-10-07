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

package imageinspect

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Egress policy. The manager fetches whatever image reference a tenant
// writes, from the manager's own network position, so an unrestricted
// inspector is a blind SSRF primitive: the outcome of the inspection says
// which internal hosts answer like a registry. The policy is enforced at
// dial time, on the address the name resolved to, so it also covers
// redirects, bearer-token realms and DNS rebinding. Egress proxies
// ($HTTPS_PROXY) are deliberately not honoured: the dialer could not see
// the destination behind one.

// ErrEgressDenied reports an inspection refused by the egress policy: the
// registry resolved to a non-public address, or is not in the allowed
// registries.
var ErrEgressDenied = errors.New("imageinspect: the egress policy denies this registry")

// cgnat is the shared address space of RFC 6598 (carrier-grade NAT), which
// netip.Addr.IsPrivate does not cover.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// thisNetwork is 0.0.0.0/8, the "this host" network of RFC 1122.
var thisNetwork = netip.MustParsePrefix("0.0.0.0/8")

// denied reports whether ip is not a public unicast address: loopback,
// link-local (169.254.0.0/16, fe80::/10, which includes cloud metadata
// endpoints), unspecified, multicast, private (RFC 1918, fc00::/7) or
// CGNAT (100.64.0.0/10).
func denied(ip netip.Addr) bool {
	ip = ip.Unmap()
	return !ip.IsValid() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsPrivate() ||
		cgnat.Contains(ip) || thisNetwork.Contains(ip)
}

// controlPublic is a net.Dialer.Control that refuses a connection to a
// non-public address. address is the resolved "ip:port". It returns an error
// wrapping ErrEgressDenied, or nil.
func controlPublic(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparsable address", ErrEgressDenied)
	}
	if denied(ap.Addr()) {
		return fmt.Errorf("%w: %s is not a public address (see --image-inspect-allow-private-registries)", ErrEgressDenied, ap.Addr())
	}
	return nil
}

// NewTransport returns an http.RoundTripper for registry traffic that
// refuses, after DNS resolution, every non-public address unless
// allowPrivate is set. It returns a transport with its own connection
// pool, no proxy and HTTP/2.
func NewTransport(allowPrivate bool) http.RoundTripper {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		d.Control = controlPublic
	}
	return &http.Transport{
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// defaults are the shared transports of a Remote that sets none, one per
// policy, so connections are pooled across inspections.
var defaults struct {
	once    [2]sync.Once
	transit [2]http.RoundTripper
}

// roundTripper returns r's transport: Transport when set, else the shared
// one for r's AllowPrivate.
func (r Remote) roundTripper() http.RoundTripper {
	if r.Transport != nil {
		return r.Transport
	}
	i := 0
	if r.AllowPrivate {
		i = 1
	}
	defaults.once[i].Do(func() { defaults.transit[i] = NewTransport(r.AllowPrivate) })
	return defaults.transit[i]
}

// checkRegistry returns an error wrapping ErrEgressDenied when
// AllowedRegistries is set and does not list host (host[:port]).
func (r Remote) checkRegistry(host string) error {
	if len(r.AllowedRegistries) == 0 || slices.Contains(r.AllowedRegistries, strings.ToLower(host)) {
		return nil
	}
	return fmt.Errorf("%w: registry %q is not in --image-inspect-allowed-registries", ErrEgressDenied, host)
}
