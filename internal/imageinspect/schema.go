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
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/captf-io/cluster-api-provider-terraform/internal/varschema"
)

// VariablesSchemaLabel is the image label that carries the JSON Schema of
// the module's user variables.
const VariablesSchemaLabel = varschema.Label

const (
	// SchemaTagTTL is how long a schema read through a mutable tag stays
	// cached; a digest reference never expires.
	SchemaTagTTL = 10 * time.Minute
	// SchemaFailureTTL is how long a failed read of a reference is
	// remembered, so a registry that is down costs one attempt a minute
	// instead of one per reconcile.
	SchemaFailureTTL = time.Minute
	// maxSchemaEntries bounds the cache. When it is full the cache is
	// emptied: schemas are cheap to read again.
	maxSchemaEntries = 512
)

// schemaEntry is what one image digest declares.
type schemaEntry struct {
	// schema is nil when the image carries no usable label.
	schema *varschema.Schema
	// invalid is the problem with a label that is present but unusable.
	invalid error
}

// refEntry binds an image reference to the digest it resolved to.
type refEntry struct {
	digest  string
	expires time.Time
}

// SchemaCache holds the variables schemas of the images the manager has
// inspected, by digest, and the digest each reference last resolved to.
// The admission webhook reads it without contacting a registry. The zero
// value is not usable; call NewSchemaCache. It is safe for concurrent use.
type SchemaCache struct {
	// now is the time source; tests replace it.
	now func() time.Time

	mu       sync.Mutex
	byDigest map[string]schemaEntry
	byRef    map[string]refEntry
	failed   map[string]failure
}

// failure is a remembered failed read.
type failure struct {
	err     error
	expires time.Time
}

// NewSchemaCache returns an empty SchemaCache.
func NewSchemaCache() *SchemaCache {
	return &SchemaCache{now: time.Now, byDigest: map[string]schemaEntry{}, byRef: map[string]refEntry{}, failed: map[string]failure{}}
}

// digestOf returns the digest of ref, a reference that pins one
// ("repo@sha256:…", possibly with a tag before the @), or "".
func digestOf(ref string) string {
	if _, d, ok := strings.Cut(ref, "@"); ok && strings.Contains(d, ":") {
		return d
	}
	return ""
}

// Cached returns what ref's image declares when the cache already knows
// it: the schema (nil when the image declares none or an unusable one) and
// true. It never contacts a registry; for an unknown reference it returns
// nil and false.
func (c *SchemaCache) Cached(ref string) (*varschema.Schema, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	digest := digestOf(ref)
	if digest == "" {
		r, ok := c.byRef[ref]
		if !ok || !c.now().Before(r.expires) {
			return nil, false
		}
		digest = r.digest
	}
	e, ok := c.byDigest[digest]
	return e.schema, ok
}

// Schema returns the variables schema ref's image declares, from the cache
// or else by reading the image config with insp, bounded by ctx and
// authenticated with keychain, and caches the answer by digest. A nil
// schema with a nil error means the image declares none. It returns an
// error when the image cannot be read or its label is present but invalid
// (wrapping varschema.ErrInvalid or varschema.ErrTooLarge); an invalid
// label is cached too, so it is reported once.
func (c *SchemaCache) Schema(ctx context.Context, insp Inspector, ref string, keychain authn.Keychain) (*varschema.Schema, error) {
	if digest := digestOf(ref); digest != "" {
		c.mu.Lock()
		e, ok := c.byDigest[digest]
		c.mu.Unlock()
		if ok {
			return e.schema, e.invalid
		}
	} else if s, ok := c.Cached(ref); ok {
		return s, c.invalidOf(ref)
	}
	if err := c.recentFailure(ref); err != nil {
		return nil, err
	}
	cfg, err := insp.Config(ctx, ref, keychain, DefaultPlatform())
	if err != nil {
		c.mu.Lock()
		if len(c.failed) >= maxSchemaEntries {
			c.failed = map[string]failure{}
		}
		c.failed[ref] = failure{err: err, expires: c.now().Add(SchemaFailureTTL)}
		c.mu.Unlock()
		return nil, err
	}
	e := schemaEntry{}
	if label, ok := cfg.Labels[VariablesSchemaLabel]; ok {
		if e.schema, e.invalid = varschema.Parse(label); e.invalid != nil {
			e.schema = nil
		}
	}
	c.put(ref, cfg.Digest, e)
	return e.schema, e.invalid
}

// recentFailure returns the error of ref's last failed read when it is
// still remembered, else nil.
func (c *SchemaCache) recentFailure(ref string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.failed[ref]
	if !ok || !c.now().Before(f.expires) {
		delete(c.failed, ref)
		return nil
	}
	return f.err
}

// invalidOf returns the cached label problem of ref's image, or nil.
func (c *SchemaCache) invalidOf(ref string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byDigest[c.byRef[ref].digest].invalid
}

// put records e for digest and, for a reference that does not pin one,
// binds ref to it for SchemaTagTTL.
func (c *SchemaCache) put(ref, digest string, e schemaEntry) {
	if digest == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.byDigest) >= maxSchemaEntries {
		c.byDigest, c.byRef, c.failed = map[string]schemaEntry{}, map[string]refEntry{}, map[string]failure{}
	}
	c.byDigest[digest] = e
	delete(c.failed, ref)
	if digestOf(ref) == "" {
		c.byRef[ref] = refEntry{digest: digest, expires: c.now().Add(SchemaTagTTL)}
	}
}

// String describes the cache's size, for logs. It returns the counts.
func (c *SchemaCache) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fmt.Sprintf("%d schemas, %d references", len(c.byDigest), len(c.byRef))
}
