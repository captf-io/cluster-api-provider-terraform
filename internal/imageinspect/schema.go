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
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

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

// refEntry binds an image reference, as one namespace read it, to the
// digest it resolved to.
type refEntry struct {
	digest string
	// expires is when a mutable tag's binding lapses; zero for a
	// reference that pins a digest.
	expires time.Time
}

// SchemaCache holds the variables schemas of the images the manager has
// inspected, by digest, and the digest each reference last resolved to.
// Schemas are shared by digest, but a reference resolves and is authorized
// per namespace: a pull Secret is namespace-local, so a namespace may
// learn a private image's schema only from a read that namespace made
// itself, never from another tenant's. The admission webhook reads it
// without contacting a registry. The zero value is not usable; call
// NewSchemaCache. It is safe for concurrent use.
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
func NewSchemaCache() *SchemaCache { return NewSchemaCacheWithClock(time.Now) }

// NewSchemaCacheWithClock returns an empty SchemaCache that reads time from
// now, so a test can make a tag binding lapse.
func NewSchemaCacheWithClock(now func() time.Time) *SchemaCache {
	return &SchemaCache{now: now, byDigest: map[string]schemaEntry{}, byRef: map[string]refEntry{}, failed: map[string]failure{}}
}

// PinsDigest reports whether ref names its image by digest, so it never
// resolves to anything else; a tag is mutable.
func PinsDigest(ref string) bool { return digestOf(ref) != "" }

// digestOf returns the digest of ref, a reference that pins one
// ("repo@sha256:…", possibly with a tag before the @), or "".
func digestOf(ref string) string {
	if _, d, ok := strings.Cut(ref, "@"); ok && strings.Contains(d, ":") {
		return d
	}
	return ""
}

// refKey returns the cache key of ref as namespace reads it.
func refKey(namespace, ref string) string { return namespace + "\x00" + ref }

// Cached returns what ref's image declares when namespace itself has
// already read it: the schema (nil when the image declares none or an
// unusable one) and true. It never contacts a registry; for a reference
// namespace has not read, or a tag binding that lapsed, it returns nil and
// false, even when another namespace has read the image.
func (c *SchemaCache) Cached(namespace, ref string) (*varschema.Schema, bool) {
	e, ok := c.lookup(namespace, ref)
	return e.schema, ok
}

// lookup returns the entry namespace's reading of ref resolved to; a nil
// cache knows nothing.
func (c *SchemaCache) lookup(namespace, ref string) (schemaEntry, bool) {
	if c == nil {
		return schemaEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byRef[refKey(namespace, ref)]
	if !ok || (!r.expires.IsZero() && !c.now().Before(r.expires)) {
		return schemaEntry{}, false
	}
	e, ok := c.byDigest[r.digest]
	return e, ok
}

// Schema returns the variables schema ref's image declares, from namespace's
// entries in the cache or else by reading the image config with insp,
// bounded by ctx and authenticated with keychain (namespace's pull
// Secrets), and caches the answer by digest and for namespace. A nil
// schema with a nil error means the image declares none. It returns an
// error when the image cannot be read or its label is present but invalid
// (wrapping varschema.ErrInvalid or varschema.ErrTooLarge); an invalid
// label is cached too, so it is reported once.
func (c *SchemaCache) Schema(ctx context.Context, insp Inspector, namespace, ref string, keychain authn.Keychain) (*varschema.Schema, error) {
	if e, ok := c.lookup(namespace, ref); ok {
		return e.schema, e.invalid
	}
	if err := c.recentFailure(namespace, ref); err != nil {
		return nil, err
	}
	cfg, err := insp.Config(ctx, ref, keychain, DefaultPlatform())
	if err != nil {
		if cacheableFailure(ctx, err) {
			c.mu.Lock()
			if len(c.failed) >= maxSchemaEntries {
				c.failed = map[string]failure{}
			}
			c.failed[refKey(namespace, ref)] = failure{err: err, expires: c.now().Add(SchemaFailureTTL)}
			c.mu.Unlock()
		}
		return nil, err
	}
	return c.Remember(namespace, ref, cfg)
}

// cacheableFailure reports whether err, from a read of an image config
// under ctx, is the registry's answer for every reader of the namespace,
// which SchemaFailureTTL may then repeat. The caller's own deadline or
// cancellation is not (the admission webhook's 2s budget would otherwise
// make every controller skip the variables check for a minute), nor is an
// authorization refusal, which depends on the pull Secrets of the object
// that read it.
func cacheableFailure(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var terr *transport.Error
	return !errors.As(err, &terr) || (terr.StatusCode != http.StatusUnauthorized && terr.StatusCode != http.StatusForbidden)
}

// SchemaOf returns the variables schema in cfg's labels: nil with a nil
// error when the image declares none, or an error wrapping
// varschema.ErrInvalid or varschema.ErrTooLarge for a label that is
// present but unusable.
func SchemaOf(cfg *Config) (*varschema.Schema, error) {
	label, ok := cfg.Labels[VariablesSchemaLabel]
	if !ok {
		return nil, nil
	}
	s, err := varschema.Parse(label)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Remember records the schema in cfg, the image config namespace's read of
// ref resolved to, by its digest, binds ref to it for namespace, and
// returns the schema as SchemaOf does. The receiver may be nil, which
// records nothing.
func (c *SchemaCache) Remember(namespace, ref string, cfg *Config) (*varschema.Schema, error) {
	s, err := SchemaOf(cfg)
	if c != nil {
		c.put(namespace, ref, cfg.Digest, schemaEntry{schema: s, invalid: err})
	}
	return s, err
}

// recentFailure returns the error of namespace's last failed read of ref
// when it is still remembered, else nil.
func (c *SchemaCache) recentFailure(namespace, ref string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := refKey(namespace, ref)
	f, ok := c.failed[k]
	if !ok || !c.now().Before(f.expires) {
		delete(c.failed, k)
		return nil
	}
	return f.err
}

// put records e for digest and binds ref to it for namespace: for
// SchemaTagTTL when ref is a mutable tag, for good when it pins a digest.
func (c *SchemaCache) put(namespace, ref, digest string, e schemaEntry) {
	if digest == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.byDigest) >= maxSchemaEntries || len(c.byRef) >= maxSchemaEntries {
		c.byDigest, c.byRef, c.failed = map[string]schemaEntry{}, map[string]refEntry{}, map[string]failure{}
	}
	c.byDigest[digest] = e
	k := refKey(namespace, ref)
	delete(c.failed, k)
	r := refEntry{digest: digest}
	if digestOf(ref) == "" {
		r.expires = c.now().Add(SchemaTagTTL)
	}
	c.byRef[k] = r
}

// String describes the cache's size, for logs. It returns the counts.
func (c *SchemaCache) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fmt.Sprintf("%d schemas, %d references", len(c.byDigest), len(c.byRef))
}
