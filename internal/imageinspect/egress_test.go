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
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// TestDenied proves denied refuses loopback, link-local, unspecified,
// multicast, RFC 1918, CGNAT, ULA and the IPv4-mapped forms of those, and
// allows public addresses.
func TestDenied(t *testing.T) {
	t.Parallel()
	for _, ip := range []string{
		"127.0.0.1", "127.8.8.8", "::1", "169.254.169.254", "fe80::1", "0.0.0.0", "::", "0.1.2.3",
		"224.0.0.1", "ff02::1", "10.0.0.5", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"100.64.0.1", "100.127.255.255", "fc00::1", "fd12:3456::1", "::ffff:10.0.0.1", "::ffff:127.0.0.1",
	} {
		if !denied(netip.MustParseAddr(ip)) {
			t.Errorf("denied(%s) = false, want true", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "100.128.0.1", "172.32.0.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if denied(netip.MustParseAddr(ip)) {
			t.Errorf("denied(%s) = true, want false", ip)
		}
	}
}

// TestControlPublic proves the dial control refuses a private address and
// accepts a public one, and refuses an unparsable address.
func TestControlPublic(t *testing.T) {
	t.Parallel()
	for addr, wantDenied := range map[string]bool{"10.0.0.1:443": true, "[fe80::1]:443": true, "8.8.8.8:443": false, "nonsense": true} {
		err := controlPublic("tcp", addr, nil)
		if got := errors.Is(err, ErrEgressDenied); got != wantDenied {
			t.Errorf("controlPublic(%s) = %v, want denied=%v", addr, err, wantDenied)
		}
	}
}

// TestTransportDial proves the transport refuses a loopback server by
// default (before any request is sent) and reaches it with allowPrivate.
func TestTransportDial(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	get := func(rt http.RoundTripper) error {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
		resp, err := rt.RoundTrip(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	if err := get(NewTransport(false)); !errors.Is(err, ErrEgressDenied) || hits.Load() != 0 {
		t.Fatalf("loopback by default: err = %v, hits = %d", err, hits.Load())
	}
	if err := get(NewTransport(true)); err != nil || hits.Load() != 1 {
		t.Fatalf("loopback allowed: err = %v, hits = %d", err, hits.Load())
	}
}

// TestRemoteEgress proves Remote refuses a loopback registry by default,
// including a ref by IP literal, and enforces the allowed registries.
func TestRemoteEgress(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	ref := host + "/x:1"

	_, err := Remote{Insecure: true}.Config(t.Context(), ref, authn.DefaultKeychain, nil)
	if !errors.Is(err, ErrEgressDenied) {
		t.Errorf("default policy: %v, want ErrEgressDenied", err)
	}
	_, err = Remote{Insecure: true, AllowPrivate: true, AllowedRegistries: []string{"ghcr.io"}}.Config(t.Context(), ref, authn.DefaultKeychain, nil)
	if !errors.Is(err, ErrEgressDenied) {
		t.Errorf("not allow-listed: %v, want ErrEgressDenied", err)
	}
	// Allow-listed and private-allowed: the request is made (and answered 404).
	_, err = Remote{Insecure: true, AllowPrivate: true, AllowedRegistries: []string{strings.ToUpper(host), host}}.Config(t.Context(), ref, authn.DefaultKeychain, nil)
	if err == nil || errors.Is(err, ErrEgressDenied) {
		t.Errorf("allow-listed: %v, want a registry error", err)
	}
}

// TestRemoteConfigTooLarge proves a manifest declaring a 100 MiB config is
// rejected with ErrConfigTooLarge before the blob is fetched, so the
// manager reads far less than the declared size; a negative size, which
// would disable the library's limit, is rejected too.
func TestRemoteConfigTooLarge(t *testing.T) {
	t.Parallel()
	for _, size := range []int64{100 << 20, -1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			t.Parallel()
			var served atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v2/":
					w.WriteHeader(http.StatusOK)
				case strings.Contains(r.URL.Path, "/manifests/"):
					body := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json",`+
						`"config":{"mediaType":"application/vnd.docker.container.image.v1+json","size":%d,"digest":"sha256:%064d"},"layers":[]}`, size, 1)
					w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
					n, _ := w.Write([]byte(body))
					served.Add(int64(n))
				default: // the config blob: an endless stream
					chunk := make([]byte, 64<<10)
					for range 2000 {
						n, err := w.Write(chunk)
						served.Add(int64(n))
						if err != nil {
							return
						}
					}
				}
			}))
			t.Cleanup(srv.Close)
			_, err := Remote{Insecure: true, AllowPrivate: true}.Config(t.Context(), strings.TrimPrefix(srv.URL, "http://")+"/x:1", authn.DefaultKeychain, nil)
			if !errors.Is(err, ErrConfigTooLarge) {
				t.Fatalf("err = %v, want ErrConfigTooLarge", err)
			}
			if got := served.Load(); got > 2<<20 {
				t.Errorf("served %d bytes, want under 2 MiB", got)
			}
		})
	}
}

// deadlineInspector refuses every attempt with the registry's 401 and
// records the context deadline each attempt ran under.
type deadlineInspector struct {
	deadlines []time.Time
}

// Config records ctx's deadline and returns a 401.
func (d *deadlineInspector) Config(ctx context.Context, _ string, _ authn.Keychain, _ *v1.Platform) (*Config, error) {
	dl, _ := ctx.Deadline()
	d.deadlines = append(d.deadlines, dl)
	return nil, &transport.Error{StatusCode: http.StatusUnauthorized}
}

// TestFallbackTotalDeadline proves every credential candidate runs under
// the same deadline of one Timeout, instead of a fresh window each.
func TestFallbackTotalDeadline(t *testing.T) {
	t.Parallel()
	k := keychainOf(t, pullSecret("s", `{"r.example/team":{"username":"a","password":"x"},"r.example":{"username":"b","password":"y"}}`))
	insp := &deadlineInspector{}
	before := time.Now()
	if _, err := (FallbackInspector{Inner: insp}).Config(t.Context(), "r.example/team/app:1", k, nil); !authRefused(err) {
		t.Fatalf("err = %v, want a 401", err)
	}
	if len(insp.deadlines) != 2 || !insp.deadlines[0].Equal(insp.deadlines[1]) {
		t.Fatalf("deadlines = %v, want two equal", insp.deadlines)
	}
	if insp.deadlines[0].After(before.Add(Timeout + time.Second)) {
		t.Errorf("deadline %s is later than one Timeout from the start", insp.deadlines[0])
	}
}
