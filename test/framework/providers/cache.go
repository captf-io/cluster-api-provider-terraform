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

package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
)

// DefaultCacheDir returns the default artifact cache directory,
// <user cache dir>/captf-testenv (~/.cache/captf-testenv on Linux), or an
// error when the user cache directory cannot be determined.
func DefaultCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("providers: cache dir: %w", err)
	}
	return filepath.Join(base, "captf-testenv"), nil
}

// CachedPath returns where artifact a lives under cacheDir:
// <cacheDir>/<sha256>/<a.Name>. The pin is part of the path, so a bumped
// artifact never reuses a stale file.
func CachedPath(cacheDir string, a framework.Artifact) string {
	return filepath.Join(cacheDir, a.SHA256, a.Name)
}

// EnsureCache makes every artifact in artifacts present in cacheDir at its
// CachedPath. A file already there with the right sha256 is kept; a
// missing or corrupt one is downloaded with client, hashed while streaming
// and renamed into place only when the hash matches a.SHA256, so a failed
// or mismatched download leaves nothing behind. ctx bounds every request.
// It returns the first error.
func EnsureCache(ctx context.Context, client *http.Client, cacheDir string, artifacts []framework.Artifact) error {
	for _, a := range artifacts {
		if err := ensureOne(ctx, client, cacheDir, a); err != nil {
			return err
		}
	}
	return nil
}

// ensureOne caches artifact a under cacheDir as EnsureCache describes,
// fetching with client under ctx, and returns an error if it cannot be
// fetched or fails verification.
func ensureOne(ctx context.Context, client *http.Client, cacheDir string, a framework.Artifact) error {
	dest := CachedPath(cacheDir, a)
	if sum, err := fileSHA256(dest); err == nil && strings.EqualFold(sum, a.SHA256) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("providers: cache %s: %w", a.Name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, http.NoBody)
	if err != nil {
		return fmt.Errorf("providers: cache %s: %w", a.Name, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("providers: cache %s: %w", a.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("providers: cache %s: GET %s: %s", a.Name, a.URL, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+a.Name+".tmp-*")
	if err != nil {
		return fmt.Errorf("providers: cache %s: %w", a.Name, err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return fmt.Errorf("providers: cache %s: download: %w", a.Name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, a.SHA256) {
		return fmt.Errorf("providers: cache %s: sha256 mismatch for %s: got %s, want %s", a.Name, a.URL, got, a.SHA256)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("providers: cache %s: %w", a.Name, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("providers: cache %s: %w", a.Name, err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("providers: cache %s: %w", a.Name, err)
	}
	committed = true
	return nil
}

// fileSHA256 returns the lowercase hex sha256 of the file at path, or the
// error from opening or reading it.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
