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
	"fmt"
	"runtime"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Timeout bounds one inspection, so a hanging registry cannot hold a
// reconcile worker.
const Timeout = 30 * time.Second

// ErrNoLinuxImage reports an index without any linux image.
var ErrNoLinuxImage = errors.New("imageinspect: the index has no linux image")

// Config is what the reconciler needs from an image's config.
type Config struct {
	// Digest is the digest the reference resolved to (index or image).
	Digest string
	// Platform is the platform read from an index, else "".
	Platform string
	// Labels are the image config labels.
	Labels map[string]string
	// User is the image config user.
	User string
}

// Inspector reads an image's config. Unit tests fake it.
type Inspector interface {
	// Config reads ref's image config from the registry, bounded by ctx,
	// authenticating with keychain and, for a multi-platform index,
	// selecting platform (nil for the implementation's default). It
	// returns the resolved Config, or an error when ref cannot be parsed
	// or pulled.
	Config(ctx context.Context, ref string, keychain authn.Keychain, platform *v1.Platform) (*Config, error)
}

// Remote is the registry Inspector.
type Remote struct {
	// Insecure allows plain-HTTP registries. Only tests set it; v1 has no
	// insecure-registry or custom-CA option.
	Insecure bool
}

var _ Inspector = Remote{}

// DefaultPlatform returns linux on the manager's own architecture.
func DefaultPlatform() *v1.Platform {
	return &v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
}

// Config resolves ref and reads one image config, bounded by ctx and
// authenticated with keychain. For an index it takes the linux image of
// platform's architecture, else the first linux image: capacity describes
// the instance type, not the image platform, so images SHOULD carry
// identical labels on every platform. Non-linux entries, such as buildx
// attestation manifests (unknown/unknown), are never chosen. It returns
// the resolved Config, or an error when ref cannot be parsed, pulled or
// has no linux image.
func (r Remote) Config(ctx context.Context, ref string, keychain authn.Keychain, platform *v1.Platform) (*Config, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	var opts []name.Option
	if r.Insecure {
		opts = append(opts, name.Insecure)
	}
	parsed, err := name.ParseReference(ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("imageinspect: parse %q: %w", ref, err)
	}
	if platform == nil {
		platform = DefaultPlatform()
	}
	desc, err := remote.Get(parsed, remote.WithContext(ctx), remote.WithAuthFromKeychain(keychain))
	if err != nil {
		return nil, fmt.Errorf("imageinspect: get %s: %w", parsed, err)
	}
	out := &Config{Digest: desc.Digest.String()}

	var img v1.Image
	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err != nil {
			return nil, fmt.Errorf("imageinspect: index %s: %w", parsed, err)
		}
		m, err := idx.IndexManifest()
		if err != nil {
			return nil, fmt.Errorf("imageinspect: index manifest %s: %w", parsed, err)
		}
		chosen, ok := choose(m.Manifests, platform.Architecture)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrNoLinuxImage, parsed)
		}
		if img, err = idx.Image(chosen.Digest); err != nil {
			return nil, fmt.Errorf("imageinspect: image %s@%s: %w", parsed, chosen.Digest, err)
		}
		out.Platform = chosen.Platform.OS + "/" + chosen.Platform.Architecture
	} else if img, err = desc.Image(); err != nil {
		return nil, fmt.Errorf("imageinspect: image %s: %w", parsed, err)
	}

	cf, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("imageinspect: config of %s: %w", parsed, err)
	}
	out.Labels, out.User = cf.Config.Labels, cf.Config.User
	return out, nil
}

// choose picks the linux manifest of arch out of manifests, else the first
// linux one. It returns the chosen descriptor and true, or a zero
// descriptor and false when manifests has no linux entry.
func choose(manifests []v1.Descriptor, arch string) (v1.Descriptor, bool) {
	var first *v1.Descriptor
	for i := range manifests {
		d := &manifests[i]
		if d.Platform == nil || d.Platform.OS != "linux" {
			continue
		}
		if d.Platform.Architecture == arch {
			return *d, true
		}
		if first == nil {
			first = d
		}
	}
	if first == nil {
		return v1.Descriptor{}, false
	}
	return *first, true
}
