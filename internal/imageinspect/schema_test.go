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
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"

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

	if _, ok := c.Cached("img:v1"); ok {
		t.Fatal("an empty cache knows nothing")
	}
	s, err := c.Schema(ctx, insp, "img:v1", nil)
	if err != nil || s == nil || s.Properties["a"] == nil {
		t.Fatalf("Schema = %v, %v", s, err)
	}
	if _, err := c.Schema(ctx, insp, "img:v1", nil); err != nil || insp.calls != 1 {
		t.Fatalf("second read: err %v, %d registry calls", err, insp.calls)
	}
	if s, ok := c.Cached("img:v1"); !ok || s == nil {
		t.Error("the tag reference is cached")
	}
	if s, ok := c.Cached("other/img@sha256:aa"); !ok || s == nil {
		t.Error("a digest reference finds the schema by digest")
	}
	if _, err := c.Schema(ctx, insp, "other/img@sha256:aa", nil); err != nil || insp.calls != 1 {
		t.Errorf("a cached digest reference reads no registry: err %v, %d calls", err, insp.calls)
	}
	now = now.Add(SchemaTagTTL + time.Second)
	if _, ok := c.Cached("img:v1"); ok {
		t.Error("a tag binding expires")
	}
	if _, ok := c.Cached("other/img@sha256:aa"); !ok {
		t.Error("a digest never expires")
	}
	if c.String() == "" {
		t.Error("String is empty")
	}
}

// TestSchemaCacheLabels: a missing label is a known absence, an invalid
// one is cached and reported, and failures are remembered briefly.
func TestSchemaCacheLabels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := NewSchemaCache()
	none := &countingInspector{cfg: &Config{Digest: "sha256:bb"}}
	if s, err := c.Schema(ctx, none, "none:1", nil); s != nil || err != nil {
		t.Errorf("no label = %v, %v", s, err)
	}
	if s, ok := c.Cached("none:1"); s != nil || !ok {
		t.Errorf("an image without a label is a known absence: %v, %v", s, ok)
	}
	bad := &countingInspector{cfg: &Config{Digest: "sha256:cc", Labels: map[string]string{VariablesSchemaLabel: `[]`}}}
	for range 2 {
		if _, err := c.Schema(ctx, bad, "bad:1", nil); !errors.Is(err, varschema.ErrInvalid) {
			t.Errorf("invalid label err = %v", err)
		}
	}
	if bad.calls != 1 {
		t.Errorf("an invalid label is cached: %d calls", bad.calls)
	}
	if s, ok := c.Cached("bad:1"); s != nil || !ok {
		t.Errorf("Cached(bad) = %v, %v", s, ok)
	}
	boom := &countingInspector{err: errors.New("boom")}
	if _, err := c.Schema(ctx, boom, "x:1", nil); err == nil {
		t.Error("an inspection failure is an error")
	}
	if _, ok := c.Cached("x:1"); ok {
		t.Error("failures are not cached as schemas")
	}
	if _, err := c.Schema(ctx, boom, "x:1", nil); err == nil || boom.calls != 1 {
		t.Errorf("a failure is remembered for %s: err %v, %d calls", SchemaFailureTTL, err, boom.calls)
	}
	c.now = func() time.Time { return time.Now().Add(2 * SchemaFailureTTL) }
	if _, err := c.Schema(ctx, boom, "x:1", nil); err == nil || boom.calls != 2 {
		t.Errorf("a remembered failure expires: err %v, %d calls", err, boom.calls)
	}
	// A cache that fills up empties itself rather than growing.
	for i := range maxSchemaEntries + 2 {
		c.put("r", string(rune('a'+i%26))+"-"+time.Duration(i).String(), schemaEntry{})
	}
	if len(c.byDigest) > maxSchemaEntries {
		t.Errorf("cache grew to %d", len(c.byDigest))
	}
}
