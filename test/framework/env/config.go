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

package env

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/kindcluster"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/providers"
)

// The environment variables ConfigFromEnv reads, besides framework.EngineEnv.
const (
	// NameEnv is the cluster name; default framework.DefaultClusterName.
	NameEnv = "TESTENV_NAME"
	// WorkersEnv is the number of kind worker nodes; default 0.
	WorkersEnv = "TESTENV_WORKERS"
	// ReuseEnv, when true, makes Up reuse an existing cluster whose
	// state.json matches the pins.
	ReuseEnv = "CAPTF_TESTENV_REUSE"
	// AllEnv, when true, makes Down delete every captf-test-* cluster.
	AllEnv = "TESTENV_ALL"
	// ClusterctlEnv is the clusterctl binary; the Make targets pass the
	// pinned $(CLUSTERCTL). Default <repo>/hack/tools/bin/clusterctl.
	ClusterctlEnv = "CLUSTERCTL"
	// KustomizeEnv is the kustomize binary; the Make targets pass the
	// pinned $(KUSTOMIZE). Default <repo>/hack/tools/bin/kustomize.
	KustomizeEnv = "KUSTOMIZE"
)

// maxWorkers caps TESTENV_WORKERS: a test environment never needs more,
// and every node is a privileged container on the host.
const maxWorkers = 5

// toolsBin is the pinned-tools directory, relative to the repository root.
const toolsBin = "hack/tools/bin"

// Config is everything the orchestrator needs to know, resolved once.
type Config struct {
	// Name is the kind cluster name; it passes kindcluster.ValidateName.
	Name string
	// EngineOverride is the CAPTF_TESTENV_ENGINE value: "podman",
	// "docker" or empty for auto-detection (podman preferred).
	EngineOverride string
	// Workers is the number of kind worker nodes.
	Workers int
	// Reuse makes Up reuse an existing cluster whose state matches.
	Reuse bool
	// All makes Down delete every captf-test-* cluster.
	All bool
	// RepoRoot is the absolute repository root (the directory holding
	// go.work).
	RepoRoot string
	// CacheDir is the download cache (providers.DefaultCacheDir).
	CacheDir string
	// Clusterctl is the absolute path of the pinned clusterctl binary.
	Clusterctl string
	// Kustomize is the absolute path of the pinned kustomize binary.
	Kustomize string
}

// ConfigFromEnv resolves a Config from the process environment, the
// current directory (searched upward for go.work) and the user cache
// directory. It returns an error naming the variable at fault.
func ConfigFromEnv() (Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return Config{}, fmt.Errorf("env: config: %w", err)
	}
	cache, err := providers.DefaultCacheDir()
	if err != nil {
		return Config{}, fmt.Errorf("env: config: %w", err)
	}
	return LoadConfig(os.Getenv, cwd, cache)
}

// LoadConfig resolves a Config from getenv, the repository found by
// walking up from cwd to a go.work file, and cacheDir. Relative tool paths
// are taken relative to the repository root. It returns an error for an
// invalid name, engine, worker count or boolean, or when no go.work is
// found.
func LoadConfig(getenv func(string) string, cwd, cacheDir string) (Config, error) {
	c := Config{
		Name:           strings.TrimSpace(getenv(NameEnv)),
		EngineOverride: strings.TrimSpace(getenv(framework.EngineEnv)),
		CacheDir:       cacheDir,
	}
	if c.Name == "" {
		c.Name = framework.DefaultClusterName
	}
	if err := kindcluster.ValidateName(c.Name); err != nil {
		return Config{}, fmt.Errorf("env: config: %s: %w", NameEnv, err)
	}
	if c.EngineOverride != "" {
		if _, err := engine.ParseName(c.EngineOverride); err != nil {
			return Config{}, fmt.Errorf("env: config: %s: %w", framework.EngineEnv, err)
		}
	}
	if s := strings.TrimSpace(getenv(WorkersEnv)); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > maxWorkers {
			return Config{}, fmt.Errorf("env: config: %s=%q: want an integer from 0 to %d", WorkersEnv, s, maxWorkers)
		}
		c.Workers = n
	}
	var err error
	if c.Reuse, err = parseBool(getenv, ReuseEnv); err != nil {
		return Config{}, err
	}
	if c.All, err = parseBool(getenv, AllEnv); err != nil {
		return Config{}, err
	}
	if c.RepoRoot, err = FindRepoRoot(cwd); err != nil {
		return Config{}, err
	}
	c.Clusterctl = toolPath(c.RepoRoot, getenv(ClusterctlEnv), "clusterctl")
	c.Kustomize = toolPath(c.RepoRoot, getenv(KustomizeEnv), "kustomize")
	return c, nil
}

// parseBool returns the boolean value of the variable key read through
// getenv: false when unset or empty, otherwise strconv.ParseBool's result.
// It returns an error naming key for anything ParseBool rejects.
func parseBool(getenv func(string) string, key string) (bool, error) {
	s := strings.TrimSpace(getenv(key))
	if s == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("env: config: %s=%q: want a boolean (1, true, 0, false)", key, s)
	}
	return b, nil
}

// toolPath returns the tool binary path: value when set (made absolute
// against root when relative), else root's hack/tools/bin/<name> symlink.
func toolPath(root, value, name string) string {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return filepath.Join(root, toolsBin, name)
	case filepath.IsAbs(value):
		return filepath.Clean(value)
	default:
		return filepath.Join(root, value)
	}
}

// FindRepoRoot returns the absolute path of the nearest directory at or
// above dir that holds a go.work file. It returns an error when there is
// none.
func FindRepoRoot(dir string) (string, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("env: find repo root: %w", err)
	}
	for {
		if fi, err := os.Stat(filepath.Join(d, "go.work")); err == nil && fi.Mode().IsRegular() {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("env: find repo root: no go.work at or above %s", dir)
		}
		d = parent
	}
}

// TestenvRoot returns <RepoRoot>/bin/testenv, the parent of every work
// directory.
func (c Config) TestenvRoot() string {
	return filepath.Join(c.RepoRoot, "bin", "testenv")
}

// WorkDir returns the work directory of the configured cluster,
// <RepoRoot>/bin/testenv/<Name>.
func (c Config) WorkDir() string {
	return c.workDirFor(c.Name)
}

// workDirFor returns the work directory of the cluster name.
func (c Config) workDirFor(name string) string {
	return filepath.Join(c.TestenvRoot(), name)
}

// Kubeconfig returns the configured cluster's kubeconfig path,
// <WorkDir>/kubeconfig.
func (c Config) Kubeconfig() string {
	return filepath.Join(c.WorkDir(), "kubeconfig")
}

// errNoRepoRoot is returned by validate for a Config without a RepoRoot.
var errNoRepoRoot = errors.New("env: config: RepoRoot is empty")

// validate returns an error unless c's name passes ValidateName and its
// repository root is set.
func (c Config) validate() error {
	if err := kindcluster.ValidateName(c.Name); err != nil {
		return fmt.Errorf("env: config: %w", err)
	}
	if c.RepoRoot == "" {
		return errNoRepoRoot
	}
	return nil
}
