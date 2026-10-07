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
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/captf-io/cluster-api-provider-terraform/internal/varschema"
)

// countingInspector answers every Config with cfg and counts the calls.
type countingInspector struct {
	cfg   *Config
	err   error
	calls int
}

// Config counts the call and returns the fixed config or error.
func (c *countingInspector) Config(context.Context, string, authn.Keychain, *v1.Platform) (*Config, error) {
	c.calls++
	return c.cfg, c.err
}

// goodSchema is a valid variables schema label.
const goodSchema = `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}`

// TestSchemaCache: answers come from the cache by digest, tag bindings
// expire and digest references never do.
func TestSchemaCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Unix(1000, 0)
	c := NewSchemaCache()
	c.now = func() time.Time { return now }
	insp := &countingInspector{cfg: &Config{Digest: "sha256:aa", Labels: map[string]string{VariablesSchemaLabel: goodSchema}}}

	if _, ok := c.Cached("ns", "img:v1"); ok {
		t.Fatal("an empty cache knows nothing")
	}
	s, err := c.Schema(ctx, insp, "ns", "img:v1", nil)
	if err != nil || s == nil || s.Properties["a"] == nil {
		t.Fatalf("Schema = %v, %v", s, err)
	}
	if _, err := c.Schema(ctx, insp, "ns", "img:v1", nil); err != nil || insp.calls != 1 {
		t.Fatalf("second read: err %v, %d registry calls", err, insp.calls)
	}
	if s, ok := c.Cached("ns", "img:v1"); !ok || s == nil {
		t.Error("the tag reference is cached")
	}
	if _, ok := c.Cached("ns", "other/img@sha256:aa"); ok {
		t.Error("a digest reference is known only once the namespace has read it")
	}
	if _, err := c.Schema(ctx, insp, "ns", "other/img@sha256:aa", nil); err != nil || insp.calls != 2 {
		t.Fatalf("first digest read: err %v, %d calls", err, insp.calls)
	}
	if s, ok := c.Cached("ns", "other/img@sha256:aa"); !ok || s == nil {
		t.Error("a digest reference is cached after the namespace read it")
	}
	if _, err := c.Schema(ctx, insp, "ns", "other/img@sha256:aa", nil); err != nil || insp.calls != 2 {
		t.Errorf("a cached digest reference reads no registry: err %v, %d calls", err, insp.calls)
	}
	now = now.Add(SchemaTagTTL + time.Second)
	if _, ok := c.Cached("ns", "img:v1"); ok {
		t.Error("a tag binding expires")
	}
	if _, ok := c.Cached("ns", "other/img@sha256:aa"); !ok {
		t.Error("a digest never expires")
	}
	if c.String() == "" {
		t.Error("String is empty")
	}
}

// blockingInspector answers every Config with cfg once release is closed,
// counting the calls.
type blockingInspector struct {
	cfg     *Config
	release chan struct{}
	calls   atomic.Int32
}

// Config counts the call, waits for release and returns the fixed config.
func (b *blockingInspector) Config(context.Context, string, authn.Keychain, *v1.Platform) (*Config, error) {
	b.calls.Add(1)
	<-b.release
	return b.cfg, nil
}

// TestSchemaCacheCoalescesReads: concurrent misses of one reference wait
// for one registry read and all get its answer.
func TestSchemaCacheCoalescesReads(t *testing.T) {
	t.Parallel()
	c := NewSchemaCache()
	insp := &blockingInspector{cfg: &Config{Digest: "sha256:aa", Labels: map[string]string{VariablesSchemaLabel: goodSchema}}, release: make(chan struct{})}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			s, err := c.Schema(context.Background(), insp, "ns", "img:v1", nil)
			if err == nil && s == nil {
				err = errors.New("no schema")
			}
			errs <- err
		})
	}
	// Let every reader reach the cache before the read answers.
	for insp.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(insp.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if n := insp.calls.Load(); n != 1 {
		t.Errorf("registry reads = %d, want 1", n)
	}
}

// TestSchemaCacheNamespaceBound: a namespace that reads more references
// than maxNamespaceRefs loses its own bindings, never another
// namespace's.
func TestSchemaCacheNamespaceBound(t *testing.T) {
	t.Parallel()
	c := NewSchemaCache()
	insp := &countingInspector{cfg: &Config{Digest: "sha256:aa", Labels: map[string]string{VariablesSchemaLabel: goodSchema}}}
	if _, err := c.Schema(context.Background(), insp, "quiet", "img:v1", nil); err != nil {
		t.Fatal(err)
	}
	for i := range maxSchemaEntries {
		insp.cfg = &Config{Digest: "sha256:" + strconv.Itoa(i), Labels: map[string]string{VariablesSchemaLabel: goodSchema}}
		if _, err := c.Schema(context.Background(), insp, "noisy", "img:"+strconv.Itoa(i), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := c.Cached("quiet", "img:v1"); !ok {
		t.Error("another namespace's reads emptied this one's binding")
	}
}

// TestSchemaCacheTransientFailures: the caller's own deadline or
// cancellation and an authorization refusal are not remembered, so the
// next reader (the controllers after the webhook's short budget, an
// object with the right pull Secret) reads the registry again; a registry
// answer that holds for every reader, a missing image, is.
func TestSchemaCacheTransientFailures(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		ctx    func() context.Context
		err    error
		cached bool
	}{
		{"deadline", context.Background, context.DeadlineExceeded, false},
		{"caller canceled", func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}, errors.New("request canceled"), false},
		{"unauthorized", context.Background, &transport.Error{StatusCode: http.StatusUnauthorized}, false},
		{"forbidden", context.Background, &transport.Error{StatusCode: http.StatusForbidden}, false},
		{"not found", context.Background, &transport.Error{StatusCode: http.StatusNotFound}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := NewSchemaCache()
			insp := &countingInspector{err: tt.err}
			if _, err := c.Schema(tt.ctx(), insp, "ns", "img:v1", nil); err == nil {
				t.Fatal("no error")
			}
			if _, err := c.Schema(context.Background(), insp, "ns", "img:v1", nil); err == nil {
				t.Fatal("no error on the second read")
			}
			if read := insp.calls == 2; read == tt.cached {
				t.Errorf("registry calls = %d, want the failure cached %v", insp.calls, tt.cached)
			}
		})
	}
}

// TestSchemaCacheLabels: a missing label is a known absence, an invalid
// one is cached and reported, and failures are remembered briefly.
func TestSchemaCacheLabels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := NewSchemaCache()
	none := &countingInspector{cfg: &Config{Digest: "sha256:bb"}}
	if s, err := c.Schema(ctx, none, "ns", "none:1", nil); s != nil || err != nil {
		t.Errorf("no label = %v, %v", s, err)
	}
	if s, ok := c.Cached("ns", "none:1"); s != nil || !ok {
		t.Errorf("an image without a label is a known absence: %v, %v", s, ok)
	}
	bad := &countingInspector{cfg: &Config{Digest: "sha256:cc", Labels: map[string]string{VariablesSchemaLabel: `[]`}}}
	for range 2 {
		if _, err := c.Schema(ctx, bad, "ns", "bad:1", nil); !errors.Is(err, varschema.ErrInvalid) {
			t.Errorf("invalid label err = %v", err)
		}
	}
	if bad.calls != 1 {
		t.Errorf("an invalid label is cached: %d calls", bad.calls)
	}
	if s, ok := c.Cached("ns", "bad:1"); s != nil || !ok {
		t.Errorf("Cached(bad) = %v, %v", s, ok)
	}
	boom := &countingInspector{err: errors.New("boom")}
	if _, err := c.Schema(ctx, boom, "ns", "x:1", nil); err == nil {
		t.Error("an inspection failure is an error")
	}
	if _, ok := c.Cached("ns", "x:1"); ok {
		t.Error("failures are not cached as schemas")
	}
	if _, err := c.Schema(ctx, boom, "ns", "x:1", nil); err == nil || boom.calls != 1 {
		t.Errorf("a failure is remembered for %s: err %v, %d calls", SchemaFailureTTL, err, boom.calls)
	}
	c.now = func() time.Time { return time.Now().Add(2 * SchemaFailureTTL) }
	if _, err := c.Schema(ctx, boom, "ns", "x:1", nil); err == nil || boom.calls != 2 {
		t.Errorf("a remembered failure expires: err %v, %d calls", err, boom.calls)
	}
	// A cache that fills up empties itself rather than growing.
	for i := range maxSchemaEntries + 2 {
		c.put("ns", string(rune('a'+i%26))+"-"+time.Duration(i).String(), "sha256:"+time.Duration(i).String(), schemaEntry{})
	}
	if len(c.byDigest) > maxSchemaEntries {
		t.Errorf("cache grew to %d", len(c.byDigest))
	}
}

// TestSchemaCacheNamespaces: a pull Secret is namespace-local, so one
// namespace's authenticated read of a private image must not answer
// another namespace, by tag or by digest; the schema data itself is shared
// by digest, and each namespace reads the registry itself once.
func TestSchemaCacheNamespaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := NewSchemaCache()
	insp := &countingInspector{cfg: &Config{Digest: "sha256:aa", Labels: map[string]string{VariablesSchemaLabel: goodSchema}}}
	for _, ref := range []string{"priv/img:v1", "priv/img@sha256:aa"} {
		if _, err := c.Schema(ctx, insp, "tenant-a", ref, nil); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.Cached("tenant-a", ref); !ok {
			t.Errorf("%s: the reading namespace has it cached", ref)
		}
		if s, ok := c.Cached("tenant-b", ref); ok || s != nil {
			t.Errorf("%s: tenant-b is answered from tenant-a's read: %v", ref, s)
		}
	}
	calls := insp.calls
	insp.err = errors.New("401")
	if _, err := c.Schema(ctx, insp, "tenant-b", "priv/img:v1", nil); err == nil || insp.calls != calls+1 {
		t.Errorf("tenant-b must reach the registry with its own credentials: err %v, %d calls", err, insp.calls)
	}
	if _, ok := c.Cached("tenant-a", "priv/img:v1"); !ok {
		t.Error("tenant-b's failure must not evict tenant-a")
	}
}
