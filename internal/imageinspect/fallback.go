/*
Copyright 2026 The cluster-api-provider-terraform Authors.

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

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// FallbackInspector wraps an Inspector so that, given a *Keychain, a
// registry refusing a credential (HTTP 401 or 403) is retried with the
// next matching credential, as the kubelet tries every matching one. Any
// other keychain is passed through unchanged.
type FallbackInspector struct {
	// Inner performs each attempt.
	Inner Inspector
}

var _ Inspector = FallbackInspector{}

// fixedKeychain resolves every resource to one authenticator.
type fixedKeychain struct{ auth authn.Authenticator }

// Resolve returns the fixed authenticator.
func (f fixedKeychain) Resolve(authn.Resource) (authn.Authenticator, error) { return f.auth, nil }

// Config reads ref's image config through f.Inner, bounded by ctx and
// selecting platform as Inspector.Config does, trying each of keychain's
// matching credentials in order until one is not refused. It returns the
// first success, the first refusal when every credential is refused, or
// the first non-authentication error as soon as it occurs.
func (f FallbackInspector) Config(ctx context.Context, ref string, keychain authn.Keychain, platform *v1.Platform) (*Config, error) {
	k, ok := keychain.(*Keychain)
	if !ok {
		return f.Inner.Config(ctx, ref, keychain, platform)
	}
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		return f.Inner.Config(ctx, ref, keychain, platform)
	}
	candidates := k.Candidates(parsed.Context())
	if len(candidates) < 2 {
		return f.Inner.Config(ctx, ref, keychain, platform)
	}
	var first error
	for _, a := range candidates {
		cfg, err := f.Inner.Config(ctx, ref, fixedKeychain{auth: a}, platform)
		if err == nil {
			return cfg, nil
		}
		if !authRefused(err) {
			return nil, err
		}
		if first == nil {
			first = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, first
}

// authRefused reports whether err is a registry's 401 or 403 response.
func authRefused(err error) bool {
	var te *transport.Error
	return errors.As(err, &te) && (te.StatusCode == http.StatusUnauthorized || te.StatusCode == http.StatusForbidden)
}
