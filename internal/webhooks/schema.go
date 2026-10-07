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

package webhooks

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/captf-io/cluster-api-provider-terraform/internal/imageinspect"
	"github.com/captf-io/cluster-api-provider-terraform/internal/varschema"
)

// SchemaFetchTimeout bounds the registry read an admission makes on a
// cache miss. It is far below the webhooks' 10s timeoutSeconds, and a
// failed read is remembered by the schema cache, so a registry that is
// slow or down costs one such wait a minute for an image, not one per
// admission.
const SchemaFetchTimeout = 2 * time.Second

// SchemaFetcher is the SchemaLookup of the manager: the shared schema cache
// in front of a bounded registry read. Every manager replica serves
// admission, but only the leader's controllers fill the cache as they
// inspect images, so a replica reads the registry itself on a miss
// instead of skipping the check.
type SchemaFetcher struct {
	// Cache is the shared schema cache.
	Cache *imageinspect.SchemaCache
	// Inspector reads an image's config on a miss.
	Inspector imageinspect.Inspector
	// Reader reads the namespace's pull Secrets; it should be the
	// uncached API reader.
	Reader client.Reader
}

var _ SchemaLookup = SchemaFetcher{}

// Cached returns the schema f's cache holds for ref as namespace read it,
// without contacting a registry.
func (f SchemaFetcher) Cached(namespace, ref string) (*varschema.Schema, bool) {
	return f.Cache.Cached(namespace, ref)
}

// Fetch reads ref's variables schema for namespace, authenticating with
// the pull Secrets named by pullSecrets in namespace, within
// SchemaFetchTimeout of ctx, and caches the outcome (a failure, briefly,
// too). It returns the schema (nil when the image declares none) or the
// error that made the image unreadable or its label invalid; the caller
// treats an error as "not checkable".
func (f SchemaFetcher) Fetch(ctx context.Context, namespace, ref string, pullSecrets []string) (*varschema.Schema, error) {
	ctx, cancel := context.WithTimeout(ctx, SchemaFetchTimeout)
	defer cancel()
	keychain, _, err := imageinspect.PullSecretsKeychain(ctx, f.Reader, namespace, pullSecrets)
	if err != nil {
		return nil, err
	}
	return f.Cache.Schema(ctx, f.Inspector, namespace, ref, keychain)
}
