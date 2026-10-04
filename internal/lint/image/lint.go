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

package image

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/imageinspect/labels"
	"github.com/captf-io/cluster-api-provider-terraform/internal/lint"
)

// DefaultPlatform is --platform's default.
const DefaultPlatform = "linux/amd64"

// Errors tfcapi-lint maps to exit codes: ErrReference is usage (3); ErrPull
// and a module that does not parse are exit 2.
var (
	ErrReference = errors.New("image: invalid reference")
	ErrPull      = errors.New("image: cannot read the image")
)

// Options are tfcapi-lint image's flags.
type Options struct {
	Role     contract.Role
	Contract string
	// Platform selects one manifest of an index; PlatformSet means the user
	// asked for it, so a single-platform image for another one is a finding.
	Platform     string
	PlatformSet  bool
	AllPlatforms bool
	Insecure     bool
	// Keychain resolves registry credentials; nil is authn.DefaultKeychain
	// (the docker and podman configs).
	Keychain authn.Keychain
	// MaxBytes caps an extraction; 0 is DefaultMaxBytes.
	MaxBytes int64
	// Remote are extra go-containerregistry options (tests).
	Remote []remote.Option
}

// Info identifies what was linted, for the JSON output.
type Info struct {
	Ref       string   `json:"ref"`
	Digest    string   `json:"digest"`
	Platforms []string `json:"platforms"`
}

// Result is one image's lint.
type Result struct {
	Info   Info
	Report lint.Report
}

// platformImage is one image to check and the platform it is for.
type platformImage struct {
	platform string
	img      v1.Image
}

// LayoutPrefix marks a reference to a local OCI image layout directory
// (`podman save --format oci-dir`, `skopeo copy … oci:`), read without a
// registry or a daemon.
const LayoutPrefix = "oci:"

// Lint reads ref, a registry reference or oci:<dir>, bounded by ctx for a
// registry pull, and checks every platform o selects. It returns the
// Result (what was linted plus the combined report), or an error when ref
// cannot be parsed or pulled.
func Lint(ctx context.Context, ref string, o Options) (Result, error) {
	want, err := v1.ParsePlatform(o.Platform)
	if err != nil {
		return Result{}, fmt.Errorf("%w: platform %q: %w", ErrReference, o.Platform, err)
	}
	var (
		single v1.Image
		idx    v1.ImageIndex
		digest v1.Hash
	)
	if dir, ok := strings.CutPrefix(ref, LayoutPrefix); ok {
		if idx, err = layout.ImageIndexFromPath(dir); err != nil {
			return Result{}, fmt.Errorf("%w: %w", ErrPull, err)
		}
		if digest, err = idx.Digest(); err != nil {
			return Result{}, fmt.Errorf("%w: %w", ErrPull, err)
		}
	} else {
		desc, err := get(ctx, ref, o)
		if err != nil {
			return Result{}, err
		}
		digest = desc.Digest
		if desc.MediaType.IsIndex() {
			idx, err = desc.ImageIndex()
		} else {
			single, err = desc.Image()
		}
		if err != nil {
			return Result{}, fmt.Errorf("%w: %w", ErrPull, err)
		}
	}
	res := Result{Info: Info{Ref: ref, Digest: digest.String(), Platforms: []string{}}}
	images, findings, err := selectImages(single, idx, want, o)
	if err != nil {
		return Result{}, err
	}
	capacities := map[string]string{}
	for _, pi := range images {
		fs, capLabels, fileSet, err := checkImage(pi.img, o)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", pi.platform, err)
		}
		if o.AllPlatforms {
			for i := range fs {
				fs[i].Message = "[" + pi.platform + "] " + fs[i].Message
			}
		}
		findings = append(findings, fs...)
		capacities[pi.platform] = capLabels
		res.Info.Platforms = append(res.Info.Platforms, pi.platform)
		res.Report.FileSet = fileSet
	}
	if differ(capacities) {
		findings = append(findings, finding(IDLabelCapacity, lint.SeverityWarning, "",
			"the capacity labels differ between platforms: a template sizes nodes from one of them"))
	}
	fileSet := res.Report.FileSet
	res.Report = lint.NewReport(findings)
	res.Report.FileSet = fileSet
	return res, nil
}

// differ reports whether m, a platform-to-capacity-label map, holds more
// than one distinct value.
func differ(m map[string]string) bool {
	seen := ""
	first := true
	for _, v := range m {
		if !first && v != seen {
			return true
		}
		seen, first = v, false
	}
	return false
}

// get fetches ref's descriptor from its registry, bounded by ctx and
// authenticated and configured from o (o.Keychain, o.Insecure, o.Remote).
// It returns the descriptor, or an error wrapping ErrReference when ref is
// invalid or ErrPull when the registry cannot be reached.
func get(ctx context.Context, ref string, o Options) (*remote.Descriptor, error) {
	var nameOpts []name.Option
	if o.Insecure {
		nameOpts = append(nameOpts, name.Insecure)
	}
	r, err := name.ParseReference(ref, nameOpts...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrReference, err)
	}
	kc := o.Keychain
	if kc == nil {
		kc = authn.DefaultKeychain
	}
	opts := append([]remote.Option{remote.WithAuthFromKeychain(kc), remote.WithContext(ctx)}, o.Remote...)
	desc, err := remote.Get(r, opts...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPull, err)
	}
	return desc, nil
}

// platformOf returns img's platform, read from its config file, or an
// error wrapping ErrPull when the config cannot be read.
func platformOf(img v1.Image) (v1.Platform, error) {
	cfg, err := img.ConfigFile()
	if err != nil {
		return v1.Platform{}, fmt.Errorf("%w: %w", ErrPull, err)
	}
	return v1.Platform{OS: cfg.OS, Architecture: cfg.Architecture, Variant: cfg.Variant}, nil
}

// selectImages picks the image(s) to check from single (a single image) or
// idx (an index; exactly one of the two is non-nil): every platform of an
// index under o.AllPlatforms, else the one matching want. A missing
// platform is an image/platform finding, not an error. It returns the
// selected images, any such finding, and an error only when idx cannot be
// read.
func selectImages(single v1.Image, idx v1.ImageIndex, want *v1.Platform, o Options) ([]platformImage, []lint.Finding, error) {
	if single != nil {
		have, err := platformOf(single)
		if err != nil {
			return nil, nil, err
		}
		if o.PlatformSet && !o.AllPlatforms && !have.Satisfies(*want) {
			return nil, []lint.Finding{finding(IDPlatform, lint.SeverityError, "",
				fmt.Sprintf("the image is for %s, not %s", have.String(), want.String()))}, nil
		}
		return []platformImage{{platform: have.String(), img: single}}, nil, nil
	}
	candidates, err := indexImages(idx)
	if err != nil {
		return nil, nil, err
	}
	var out []platformImage
	for _, c := range candidates {
		p, err := v1.ParsePlatform(c.platform)
		if err != nil {
			continue
		}
		if o.AllPlatforms || p.Satisfies(*want) {
			out = append(out, c)
			if !o.AllPlatforms {
				break
			}
		}
	}
	if len(out) == 0 {
		return nil, []lint.Finding{finding(IDPlatform, lint.SeverityError, "",
			"the index has no manifest for "+want.String())}, nil
	}
	return out, nil, nil
}

// indexImages lists idx's images with their platforms. A manifest without
// a platform (what an OCI layout from `podman save` holds) gets its image
// config's; a nested index is walked; attestations (unknown/unknown) are
// skipped. It returns the listed images, or an error wrapping ErrPull when
// idx or a manifest it references cannot be read.
func indexImages(idx v1.ImageIndex) ([]platformImage, error) {
	manifest, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPull, err)
	}
	var out []platformImage
	for _, m := range manifest.Manifests {
		switch {
		case m.MediaType.IsIndex():
			child, err := idx.ImageIndex(m.Digest)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrPull, err)
			}
			nested, err := indexImages(child)
			if err != nil {
				return nil, err
			}
			out = append(out, nested...)
		case m.MediaType.IsImage():
			img, err := idx.Image(m.Digest)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrPull, err)
			}
			p := m.Platform
			if p == nil {
				have, err := platformOf(img)
				if err != nil {
					return nil, err
				}
				p = &have
			}
			if p.OS == "unknown" {
				continue
			}
			out = append(out, platformImage{platform: p.String(), img: img})
		}
	}
	return out, nil
}

// checkImage checks one platform's image img against o's role and
// contract. It returns the findings, the capacity labels (to compare
// across platforms), the module's file set, and an error only when the
// image cannot be extracted or read.
func checkImage(img v1.Image, o Options) ([]lint.Finding, string, string, error) {
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, "", "", fmt.Errorf("%w: %w", ErrPull, err)
	}
	root, err := os.MkdirTemp("", "tfcapi-lint-image-")
	if err != nil {
		return nil, "", "", err
	}
	defer func() { _ = os.RemoveAll(root) }()
	maxBytes := o.MaxBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}
	t, err := extract(img, root, maxBytes)
	if err != nil {
		return nil, "", "", err
	}
	c := imageChecks{t: t, cfg: cfg, role: o.Role, contract: o.Contract, target: cfg.OS + "_" + cfg.Architecture}
	var moduleFindings []lint.Finding
	fileSet := ""
	if dir := filepath.Join(root, filepath.FromSlash(moduleDir)); hasModule(t) {
		m, err := lint.LoadModule(dir)
		if err != nil {
			return nil, "", "", err
		}
		c.module = m
		r, err := lint.Lint(m, o.Role)
		if err != nil {
			return nil, "", "", err
		}
		fileSet = r.FileSet
		for _, f := range r.Findings {
			f.File = path.Join(moduleDir, f.File)
			moduleFindings = append(moduleFindings, f)
		}
	}
	capLabels := cfg.Config.Labels[labels.CapacityLabel] + "\x00" + cfg.Config.Labels[labels.NodeInfoLabel]
	return append(c.run(), moduleFindings...), capLabels, fileSet, nil
}

// hasModule reports whether t's /captf/module has a module file at its
// top, the directory terraform-config-inspect loads. Otherwise
// image/module-present is the one finding: loading an empty directory
// would add every required-input finding on top of it.
func hasModule(t *tree) bool {
	for name, h := range t.headers {
		if h.Typeflag == tar.TypeReg && path.Dir(name) == moduleDir && lint.IsModuleFile(name) {
			return true
		}
	}
	return false
}
