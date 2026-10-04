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

package providers

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
)

// The CAPTF provider's clusterctl identity: `--infrastructure terraform`,
// whose local repository directory is infrastructure-terraform.
const (
	captfName       = "terraform"
	captfType       = "InfrastructureProvider"
	captfLabel      = "infrastructure-terraform"
	captfComponents = "infrastructure-components.yaml"
	captfMetadata   = "metadata.yaml"
	certManagerDir  = "cert-manager"
	configFileName  = "clusterctl.yaml"
)

// WriteRepository lays out a clusterctl local repository under repoDir
// from the artifacts cached in cacheDir (see EnsureCache) and the rendered
// CAPTF provider in captfDir (see RenderCAPTF):
//
//	cluster-api/<v>/{core-components.yaml,metadata.yaml}
//	bootstrap-kubeadm/<v>/{bootstrap-components.yaml,metadata.yaml}
//	control-plane-kubeadm/<v>/{control-plane-components.yaml,metadata.yaml}
//	infrastructure-terraform/<captfVersion>/<every file of captfDir>
//	cert-manager/<v>/cert-manager.yaml
//
// Files are copied, so the repository outlives cache cleanups. It also
// writes repoDir/clusterctl.yaml, whose providers entries are absolute
// file:// URLs to the components files and whose cert-manager section
// points at the copied manifest, and returns that file's path. repoDir is
// made absolute, because clusterctl rejects relative local paths.
func WriteRepository(cacheDir, captfDir, repoDir, captfVersion string) (configPath string, err error) {
	repoDir, err = filepath.Abs(repoDir)
	if err != nil {
		return "", fmt.Errorf("providers: write repository: %w", err)
	}
	var cfg strings.Builder
	cfg.WriteString("providers:\n")
	for _, p := range framework.CAPIProviders() {
		dir := filepath.Join(repoDir, p.Label, p.Version)
		for _, a := range []framework.Artifact{p.Components, p.Metadata} {
			if err := copyFile(CachedPath(cacheDir, a), filepath.Join(dir, a.Name)); err != nil {
				return "", fmt.Errorf("providers: write repository: %s: %w", p.Label, err)
			}
		}
		writeProvider(&cfg, p.Name, p.Type, filepath.Join(dir, p.Components.Name))
	}

	captfOut := filepath.Join(repoDir, captfLabel, captfVersion)
	if err := copyDir(captfDir, captfOut); err != nil {
		return "", fmt.Errorf("providers: write repository: %s: %w", captfLabel, err)
	}
	for _, name := range []string{captfComponents, captfMetadata} {
		if _, err := os.Stat(filepath.Join(captfOut, name)); err != nil {
			return "", fmt.Errorf("providers: write repository: %s: rendered provider lacks %s: %w", captfLabel, name, err)
		}
	}
	writeProvider(&cfg, captfName, captfType, filepath.Join(captfOut, captfComponents))

	cm := framework.CertManagerManifest
	cmPath := filepath.Join(repoDir, certManagerDir, framework.CertManagerVersion, cm.Name)
	if err := copyFile(CachedPath(cacheDir, cm), cmPath); err != nil {
		return "", fmt.Errorf("providers: write repository: cert-manager: %w", err)
	}
	cfg.WriteString("cert-manager:\n")
	fmt.Fprintf(&cfg, "  url: %s\n", strconv.Quote("file://"+cmPath))
	fmt.Fprintf(&cfg, "  version: %s\n", strconv.Quote(framework.CertManagerVersion))

	configPath = filepath.Join(repoDir, configFileName)
	if err := os.WriteFile(configPath, []byte(cfg.String()), 0o644); err != nil {
		return "", fmt.Errorf("providers: write repository: %w", err)
	}
	return configPath, nil
}

// writeProvider appends one providers entry to b: name, the clusterctl
// provider type typ and the file:// URL of the absolute componentsPath.
func writeProvider(b *strings.Builder, name, typ, componentsPath string) {
	fmt.Fprintf(b, "- name: %s\n  type: %s\n  url: %s\n", strconv.Quote(name), typ, strconv.Quote("file://"+componentsPath))
}

// copyDir copies every regular file directly inside src into dst, creating
// dst, and returns an error if src cannot be read or a copy fails.
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if err := copyFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return os.MkdirAll(dst, 0o755)
}

// copyFile copies the file src to dst with mode 0644, creating dst's
// directory, and returns an error if src cannot be read or dst written.
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(out, in)
	return err
}
