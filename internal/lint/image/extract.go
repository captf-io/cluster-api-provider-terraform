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
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// Paths of the image contract.
const (
	moduleDir    = "/captf/module"
	runtimePath  = "/captf/runtime"
	providersDir = "/captf/providers"
)

// DefaultMaxBytes caps what one extraction keeps: the module's files, the
// mirror's JSON indexes and, through entryCost, every tar entry's header.
const DefaultMaxBytes = 512 << 20

// scanFactor sets the cap on bytes merely skipped (the runtime binary,
// provider archives, the rest of the rootfs) to scanFactor·maxBytes: 16 GiB
// by default. Skipped bytes cost time, not memory, so they get their own,
// larger cap; a multi-provider image legitimately carries several hundred
// MB of them.
const scanFactor = 32

// entryCost is what every tar entry charges against the budget, win or
// lose: one header block, regardless of the entry's type or size. Without
// it, an image of millions of empty entries costs nothing against
// DefaultMaxBytes yet holds a *tar.Header in memory for each.
const entryCost = 512

// ErrTooLarge is an image whose entries, materialized or merely scanned,
// exceed the size cap; tfcapi-lint exits 2 on it.
var ErrTooLarge = errors.New("image: extraction exceeds the size cap")

// tree is what the linter keeps of an image's filesystem.
type tree struct {
	// root is the temporary directory holding /captf/module.
	root string
	// headers are every entry of the flattened filesystem by clean
	// absolute path.
	headers map[string]*tar.Header
	// providerJSON are the mirror's *.json files, by path.
	providerJSON map[string][]byte
	// badLinks are links under /captf/module that do not end at a module
	// file or directory, in path order.
	badLinks []badLink
}

// badLink is a module link Terraform could not load from the module.
type badLink struct {
	name, target string
}

// extract streams img's flattened filesystem (whiteouts applied) into a
// tree under root, keeping module and provider-index bytes within maxBytes
// and merely scanning the rest within scanFactor*maxBytes. It returns the
// populated tree, or ErrTooLarge when either cap is exceeded.
func extract(img v1.Image, root string, maxBytes int64) (*tree, error) {
	rc := mutate.Extract(img)
	defer func() { _ = rc.Close() }()
	t := &tree{root: root, headers: map[string]*tar.Header{}, providerJSON: map[string][]byte{}}
	budget, scan := maxBytes, scanFactor*maxBytes
	if err := t.readEntries(tar.NewReader(rc), &budget, &scan); err != nil {
		return nil, err
	}
	// Links are resolved only once the whole flattened filesystem is known:
	// a link's target may sit in a later layer than the link.
	if err := t.materializeLinks(&budget); err != nil {
		return nil, err
	}
	return t, nil
}

// readEntries records every entry of tr, writes module files and keeps
// provider indexes, charging *budget for what it keeps and *scan for what it
// merely skips. It returns ErrTooLarge when either cap is exceeded.
func (t *tree) readEntries(tr *tar.Reader, budget, scan *int64) error {
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("image: read layers: %w", err)
		}
		// Every entry costs entryCost against the kept budget: its header
		// is held in memory. Without it, millions of empty entries would
		// cost nothing.
		if entryCost > *budget {
			return ErrTooLarge
		}
		*budget -= entryCost
		// Clean against the root: "../" cannot climb above it. The
		// relative form must then be local (no "..", not absolute); an
		// entry that still is not cannot be placed under the root and is
		// skipped. go-containerregistry v0.22+ already drops such entries.
		rel := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
		if rel != "" && !filepath.IsLocal(rel) {
			continue
		}
		name := "/" + rel
		t.headers[name] = hdr
		if hdr.Typeflag != tar.TypeReg {
			continue // links and devices are never materialized here
		}
		isJSON := within(name, providersDir) && strings.HasSuffix(name, ".json")
		// A kept body counts against the budget; a skipped one against
		// the scan cap, so a large file elsewhere cannot bypass both.
		if within(name, moduleDir) || isJSON {
			if hdr.Size > *budget {
				return ErrTooLarge
			}
			*budget -= hdr.Size
		} else {
			if hdr.Size > *scan {
				return ErrTooLarge
			}
			*scan -= hdr.Size
		}
		switch {
		case within(name, moduleDir):
			if err := t.write(name, tr, hdr.Size); err != nil {
				return err
			}
		case isJSON:
			b, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
			if err != nil {
				return fmt.Errorf("image: read %s: %w", name, err)
			}
			t.providerJSON[name] = b
		}
	}
}

// materializeLinks resolves every symlink and hard link under /captf/module.
// One that ends at a regular file inside the module gets that file's bytes
// written at its own path and its header replaced by a regular-file one, so
// the module lint sees what Terraform loads at run time; the copy costs the
// file's size against *budget. A link that ends anywhere else (outside the
// module, dangling, a device) is recorded in t.badLinks. A link to a
// directory inside the module needs no materialization. It returns
// ErrTooLarge when the copies exceed the budget, or the error of a failed
// copy.
func (t *tree) materializeLinks(budget *int64) error {
	var names []string
	for name, h := range t.headers {
		if (h.Typeflag == tar.TypeSymlink || h.Typeflag == tar.TypeLink) && name != moduleDir && within(name, moduleDir) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		h := t.headers[name]
		target, resolved := t.follow(name)
		switch {
		case target != nil && target.Typeflag == tar.TypeDir && within(resolved, moduleDir):
		case target != nil && target.Typeflag == tar.TypeReg && within(resolved, moduleDir):
			if target.Size > *budget {
				return ErrTooLarge
			}
			*budget -= target.Size
			if err := t.copyFile(resolved, name); err != nil {
				return err
			}
			clone := *target
			clone.Name = name
			t.headers[name] = &clone
		default:
			t.badLinks = append(t.badLinks, badLink{name: name, target: h.Linkname})
		}
	}
	return nil
}

// copyFile copies the extracted module file src to dst, both clean image
// paths under /captf/module. It returns an error when src cannot be read or
// dst cannot be written.
func (t *tree) copyFile(src, dst string) error {
	f, err := os.Open(filepath.Join(t.root, filepath.FromSlash(src)))
	if err != nil {
		return fmt.Errorf("image: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("image: %w", err)
	}
	return t.write(dst, f, info.Size())
}

// write copies size bytes of r into t.root at name, one module file,
// refusing a path that would land outside t.root. It returns an error
// when name escapes the root or the copy fails.
func (t *tree) write(name string, r io.Reader, size int64) error {
	dst := filepath.Join(t.root, filepath.FromSlash(name))
	if rel, err := filepath.Rel(t.root, dst); err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("image: %s escapes the extraction root", name)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("image: %w", err)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- dst is checked above to stay under t.root
	if err != nil {
		return fmt.Errorf("image: %w", err)
	}
	_, err = io.CopyN(f, r, size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("image: write %s: %w", name, err)
	}
	return nil
}

// within reports whether name is dir or below it.
func within(name, dir string) bool {
	return name == dir || strings.HasPrefix(name, dir+"/")
}

// under returns the entries strictly below dir, of the given types (all
// when none are given).
func (t *tree) under(dir string, types ...byte) []*tar.Header {
	var out []*tar.Header
	for name, h := range t.headers {
		if name == dir || !within(name, dir) {
			continue
		}
		if len(types) == 0 || slices.Contains(types, h.Typeflag) {
			out = append(out, h)
		}
	}
	return out
}

// hasBelow reports whether any entry lies below dir, which then exists as
// an implicit directory even without a header of its own.
func (t *tree) hasBelow(dir string) bool {
	for name := range t.headers {
		if name != dir && within(name, dir) {
			return true
		}
	}
	return false
}

// maxHops caps the links one resolution follows, as the kernel's limit of
// 40 does, so a cycle ends.
const maxHops = 40

// resolve follows name through links: it returns the final entry, or nil
// when the chain leaves the image, loops or ends nowhere.
func (t *tree) resolve(name string) *tar.Header {
	h, _ := t.follow(name)
	return h
}

// follow resolves name inside the image the way a filesystem would: a
// symlink at any component (absolute targets as they are, relative ones from
// the link's directory) and a hard link at the last (its Linkname is the
// target's tar name), at most maxHops in all. It returns the final entry and
// its clean path, or nil and "" when the chain leaves the image or loops.
func (t *tree) follow(name string) (*tar.Header, string) {
	name = path.Clean("/" + name)
	for range maxHops + 1 {
		parts := strings.Split(strings.TrimPrefix(name, "/"), "/")
		cur, next := "/", ""
		redirected := false
		for i, p := range parts {
			cur = path.Join(cur, p)
			h, ok := t.headers[cur]
			last := i == len(parts)-1
			if !ok {
				if last {
					if t.hasBelow(cur) {
						return &tar.Header{Name: cur, Typeflag: tar.TypeDir}, cur
					}
					return nil, ""
				}
				continue // an implicit parent directory
			}
			switch {
			case h.Typeflag == tar.TypeSymlink:
				target := h.Linkname
				if !path.IsAbs(target) {
					target = path.Join(path.Dir(cur), target) // #nosec G305 -- resolves in an in-memory tar index, nothing is written
				}
				next = path.Join(append([]string{target}, parts[i+1:]...)...)
				redirected = true
			case h.Typeflag == tar.TypeLink && last:
				next = path.Clean("/" + h.Linkname)
				redirected = true
			case last:
				return h, cur
			}
			if redirected {
				break
			}
		}
		if !redirected {
			return nil, ""
		}
		name = next
	}
	return nil, ""
}
