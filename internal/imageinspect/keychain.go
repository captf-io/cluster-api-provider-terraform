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
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	pathpkg "path"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// dockerHub are the names Docker Hub goes by in config files; go-
// containerregistry reports it as index.docker.io, the first entry.
var dockerHub = []string{"index.docker.io", "docker.io", "registry-1.docker.io"}

// Keychain is the registry credentials of a namespace's pull Secrets
// (kubernetes.io/dockerconfigjson and kubernetes.io/dockercfg). Each
// credential keeps the full scope of its config key (host[:port] and an
// optional path) and is matched against an image the way the kubelet does,
// so two keys under one host stay distinct. It never falls back to the
// manager's own credentials: an unmatched registry is anonymous.
type Keychain struct {
	creds []credential
}

var _ authn.Keychain = &Keychain{}

// credential is one config key's credentials together with its scope.
type credential struct {
	cfg authn.AuthConfig
	// host is the lowercased host[:port], possibly with '*' globs.
	host string
	// path are the key's path segments; empty matches every repository.
	path []string
	// wildcards counts the '*' characters in host and path.
	wildcards int
	// seq is the order the credential was read in: Secret order, then
	// sorted key order within a Secret.
	seq int
}

// Resolve returns the most specific credentials matching res, or anonymous
// access when none match. Use Candidates to try the rest on a refusal.
func (k *Keychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	if c := k.Candidates(res); len(c) > 0 {
		return c[0], nil
	}
	return authn.Anonymous, nil
}

// Candidates returns the authenticators of every credential matching res,
// most specific first: the longer path prefix, then fewer wildcards, then
// the earlier Secret, then the earlier key. The order is deterministic.
// It returns nil when no credential matches.
func (k *Keychain) Candidates(res authn.Resource) []authn.Authenticator {
	host, path := imageScope(res)
	var matched []credential
	for _, c := range k.creds {
		if c.matches(host, path) {
			matched = append(matched, c)
		}
	}
	slices.SortStableFunc(matched, func(a, b credential) int {
		return cmp.Or(
			cmp.Compare(len(b.path), len(a.path)),
			cmp.Compare(a.wildcards, b.wildcards),
			cmp.Compare(a.seq, b.seq),
		)
	})
	out := make([]authn.Authenticator, len(matched))
	for i, c := range matched {
		out[i] = authn.FromConfig(c.cfg)
	}
	return out
}

// matches reports whether c's scope covers an image at host (host[:port])
// and path segments: the host matches label by label with '*' globs and an
// equal port, and c's path is a prefix of path on segment boundaries.
func (c credential) matches(host string, path []string) bool {
	if !globHost(c.host, host) || len(c.path) > len(path) {
		return false
	}
	for i, seg := range c.path {
		if ok, err := pathpkg.Match(seg, path[i]); err != nil || !ok {
			return false
		}
	}
	return true
}

// globHost reports whether host matches glob: the same number of
// dot-separated labels, each matching with '*' globs, the port (part of
// the last label) included.
func globHost(glob, host string) bool {
	gl, hl := strings.Split(glob, "."), strings.Split(host, ".")
	if len(gl) != len(hl) {
		return false
	}
	for i := range gl {
		if ok, err := pathpkg.Match(gl[i], hl[i]); err != nil || !ok {
			return false
		}
	}
	return true
}

// imageScope returns the normalized registry host and repository path
// segments of res, with any tag or digest removed.
func imageScope(res authn.Resource) (string, []string) {
	host := normalizeHost(res.RegistryStr())
	rest := strings.TrimPrefix(res.String(), res.RegistryStr())
	rest, _, _ = strings.Cut(rest, "@")
	segs := splitPath(rest)
	if n := len(segs); n > 0 {
		segs[n-1], _, _ = strings.Cut(segs[n-1], ":")
	}
	return host, segs
}

// normalizeHost lowercases host and maps every Docker Hub alias to the
// name go-containerregistry reports.
func normalizeHost(host string) string {
	host = strings.ToLower(host)
	if slices.Contains(dockerHub, host) {
		return dockerHub[0]
	}
	return host
}

// splitPath splits the slash-separated path p and returns its non-empty
// segments.
func splitPath(p string) []string {
	var segs []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	return segs
}

// scopeOf parses a config key (a host, a URL, or either with a path) into
// its normalized host and path segments, which it returns. A lone /v1 or /v2 path, which
// docker login writes for the registry API, is not part of the scope.
func scopeOf(key string) (string, []string) {
	h := key
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	h, p, _ := strings.Cut(h, "/")
	segs := splitPath(p)
	if len(segs) == 1 && (segs[0] == "v1" || segs[0] == "v2") {
		segs = nil
	}
	return normalizeHost(h), segs
}

// PullSecretsKeychain reads, bounded by ctx, the pull Secrets names from
// namespace through reader, which callers wire to the uncached API reader
// (pull Secrets are not captf.io/managed). Missing Secrets are returned by
// name rather than failing: a public image must not fail because a Secret
// is misnamed, and the caller reports them if the registry then refuses.
// It returns the populated Keychain and the missing names, or an error
// when a present Secret cannot be read or is not a Docker config Secret.
func PullSecretsKeychain(ctx context.Context, reader client.Reader, namespace string, names []string) (*Keychain, []string, error) {
	k := &Keychain{}
	var missing []string
	for _, n := range names {
		s := &corev1.Secret{}
		err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: n}, s)
		switch {
		case apierrors.IsNotFound(err):
			missing = append(missing, n)
			continue
		case err != nil:
			return nil, nil, fmt.Errorf("imageinspect: get pull secret %s/%s: %w", namespace, n, err)
		}
		if err := k.add(s); err != nil {
			return nil, nil, err
		}
	}
	return k, missing, nil
}

// entry is one registry's credentials in a Docker config file.
type entry struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	Auth          string `json:"auth"`
	IdentityToken string `json:"identitytoken"`
	RegistryToken string `json:"registrytoken"`
}

// add decodes s, a dockerconfigjson or dockercfg pull Secret, and appends
// one scoped credential per config key, in sorted key order. It returns an
// error when s is not a recognized Docker config Secret or its data cannot
// be decoded.
func (k *Keychain) add(s *corev1.Secret) error {
	var auths map[string]entry
	switch {
	case s.Type == corev1.SecretTypeDockerConfigJson || len(s.Data[corev1.DockerConfigJsonKey]) > 0:
		var cfg struct {
			Auths map[string]entry `json:"auths"`
		}
		if err := json.Unmarshal(s.Data[corev1.DockerConfigJsonKey], &cfg); err != nil {
			return fmt.Errorf("imageinspect: pull secret %s: invalid %s", s.Name, corev1.DockerConfigJsonKey)
		}
		auths = cfg.Auths
	case s.Type == corev1.SecretTypeDockercfg || len(s.Data[corev1.DockerConfigKey]) > 0:
		if err := json.Unmarshal(s.Data[corev1.DockerConfigKey], &auths); err != nil {
			return fmt.Errorf("imageinspect: pull secret %s: invalid %s", s.Name, corev1.DockerConfigKey)
		}
	default:
		return fmt.Errorf("imageinspect: pull secret %s is not a Docker config Secret", s.Name)
	}
	for _, key := range slices.Sorted(maps.Keys(auths)) {
		e := auths[key]
		cfg := authn.AuthConfig{Username: e.Username, Password: e.Password, IdentityToken: e.IdentityToken, RegistryToken: e.RegistryToken}
		if e.Auth != "" && cfg.Username == "" {
			raw, err := base64.StdEncoding.DecodeString(e.Auth)
			if err != nil {
				return fmt.Errorf("imageinspect: pull secret %s: invalid auth for a registry", s.Name)
			}
			cfg.Username, cfg.Password, _ = strings.Cut(string(raw), ":")
		}
		host, path := scopeOf(key)
		k.creds = append(k.creds, credential{
			cfg: cfg, host: host, path: path, seq: len(k.creds),
			wildcards: strings.Count(host, "*") + strings.Count(strings.Join(path, "/"), "*"),
		})
	}
	return nil
}
