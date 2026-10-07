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
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
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
// redirects, bearer-token realms and DNS rebinding. Behind an egress
// proxy the destination is checked before the hand-off instead; see
// guarded.

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

// guarded is the registry transport. A direct request is checked at dial
// time, on the address its name resolved to, which also defeats DNS
// rebinding. A request through an egress proxy ($HTTPS_PROXY, honouring
// $NO_PROXY) dials only the proxy, which is operator-configured and often
// private, so the dial check exempts it; instead the destination is
// resolved and checked here before the request is handed to the proxy.
// That covers redirects and token-realm fetches, which all pass through
// the transport. For a proxied request the proxy resolves the name again,
// so a DNS answer that changes between the check and the proxy's own
// lookup (rebinding) is not caught: only a proxy-side policy closes it.
type guarded struct {
	base         *http.Transport
	allowPrivate bool
	// proxy decides, per request, the egress proxy; nil means direct.
	proxy func(*http.Request) (*url.URL, error)
	// lookup resolves a destination host for the proxied check.
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// proxies holds the host:port of every proxy proxy has returned, which
	// the dialer may reach whatever address it has.
	proxies sync.Map
}

// RoundTrip checks req's destination when it goes through a proxy, then
// sends it. It returns the response, or an error wrapping ErrEgressDenied
// when the destination is not allowed.
func (g *guarded) RoundTrip(req *http.Request) (*http.Response, error) {
	if !g.allowPrivate {
		if u, err := g.proxy(req); err != nil {
			return nil, err
		} else if u != nil {
			if err := g.checkDestination(req.Context(), req.URL.Hostname()); err != nil {
				return nil, err
			}
		}
	}
	return g.base.RoundTrip(req)
}

// checkDestination resolves host, bounded by ctx, and refuses it when any
// address is not public. It returns an error wrapping ErrEgressDenied, or the lookup's.
func (g *guarded) checkDestination(ctx context.Context, host string) error {
	addrs, err := g.lookup(ctx, host)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if denied(a) {
			return fmt.Errorf("%w: %s resolves to %s, not a public address (see --image-inspect-allow-private-registries)", ErrEgressDenied, host, a)
		}
	}
	return nil
}

// proxyFunc wraps g.proxy to remember the proxies it names. It returns the
// proxy g.proxy chose for req, or nil for a direct request, and its error.
func (g *guarded) proxyFunc(req *http.Request) (*url.URL, error) {
	u, err := g.proxy(req)
	if u != nil {
		port := u.Port()
		if port == "" {
			port = map[string]string{"https": "443", "socks5": "1080", "socks5h": "1080"}[u.Scheme]
			if port == "" {
				port = "80"
			}
		}
		g.proxies.Store(net.JoinHostPort(u.Hostname(), port), struct{}{})
	}
	return u, err
}

// NewTransport returns an http.RoundTripper for registry traffic that
// refuses non-public addresses (see denied) unless allowPrivate is set, and
// honours the proxy environment (http.ProxyFromEnvironment). It returns a
// transport with its own connection pool and HTTP/2.
func NewTransport(allowPrivate bool) http.RoundTripper {
	return newTransport(allowPrivate, http.ProxyFromEnvironment, lookupAddrs)
}

// lookupAddrs resolves host with the default resolver, bounded by ctx; an
// IP literal resolves to itself. It returns the addresses or the lookup error.
func lookupAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}, nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	return ips, nil
}

// newTransport builds the guarded transport for allowPrivate with proxy and
// lookup injected, and returns it.
func newTransport(allowPrivate bool, proxy func(*http.Request) (*url.URL, error), lookup func(context.Context, string) ([]netip.Addr, error)) *guarded {
	g := &guarded{allowPrivate: allowPrivate, proxy: proxy, lookup: lookup}
	checked := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	open := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		checked.Control = controlPublic
	}
	g.base = &http.Transport{
		Proxy: g.proxyFunc,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if _, isProxy := g.proxies.Load(addr); isProxy {
				return open.DialContext(ctx, network, addr)
			}
			return checked.DialContext(ctx, network, addr)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return g
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
