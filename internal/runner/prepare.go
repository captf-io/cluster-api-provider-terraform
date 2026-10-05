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

package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
)

// Prepared is the result of Prepare.
type Prepared struct {
	// RootDir is the working directory of every step.
	RootDir string
	// Env is the environment of every step.
	Env []string
	// ProvidersMirror is true when the image ships a provider mirror.
	ProvidersMirror bool
	// Dropped names the TF_* and KUBE_* variables of base left out of Env.
	Dropped []string
}

// Prepare copies the rendered root from configDir into <workDir>/root,
// writes the CLI config when providersDir exists, and builds the step
// environment from base: TF_DATA_DIR and HOME are forced, TMPDIR is kept
// when set (the Job's /tmp emptyDir) and otherwise <workDir>/tmp, and
// TF_CLI_CONFIG_FILE is set only with a mirror (the Job cannot know).
//
// configDir is a Secret mount: only top-level regular files (symlinks
// followed) whose names do not start with "." are copied, which skips the
// kubelet's ..data and ..<timestamp> entries. It returns the resulting
// Prepared, or a non-nil error when a filesystem operation fails.
func Prepare(configDir, workDir, providersDir string, base []string) (Prepared, error) {
	root := filepath.Join(workDir, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Prepared{}, fmt.Errorf("create %s: %w", root, err)
	}
	entries, err := os.ReadDir(configDir)
	if err != nil {
		return Prepared{}, fmt.Errorf("read %s: %w", configDir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		src := filepath.Join(configDir, e.Name())
		info, err := os.Stat(src) // follows symlinks
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(src) // #nosec G304 -- a ReadDir entry of the runner's mounted config dir
		if err != nil {
			return Prepared{}, fmt.Errorf("read %s: %w", src, err)
		}
		if err := os.WriteFile(filepath.Join(root, e.Name()), data, 0o600); err != nil { // #nosec G703 -- a ReadDir name has no separator, so it stays under root
			return Prepared{}, fmt.Errorf("write %s: %w", e.Name(), err)
		}
	}

	env, dropped := Environ(base, workDir, "")
	if !hasKey(base, "TMPDIR") {
		// The Job sets TMPDIR=/tmp on an emptyDir; elsewhere use the work
		// directory, since the image's root filesystem is read-only.
		tmp := filepath.Join(workDir, "tmp")
		if err := os.MkdirAll(tmp, 0o700); err != nil {
			return Prepared{}, fmt.Errorf("create %s: %w", tmp, err)
		}
	}

	p := Prepared{RootDir: root, Env: env, Dropped: dropped}
	if info, err := os.Stat(providersDir); err == nil && info.IsDir() {
		cfg := filepath.Join(workDir, filepath.Base(render.CLIConfigPath))
		if err := os.WriteFile(cfg, render.CLIConfig(providersDir), 0o600); err != nil {
			return Prepared{}, fmt.Errorf("write %s: %w", cfg, err)
		}
		p.Env = append(p.Env, "TF_CLI_CONFIG_FILE="+cfg)
		p.ProvidersMirror = true
	}
	return p, nil
}

// Environ returns the step environment Prepare builds from base and
// workDir, and the names it dropped: HOME and every TF_* and KUBE_*
// variable of base are removed (runnerOwned), except KUBE_NAMESPACE, then
// TF_DATA_DIR=<workDir>/.terraform and HOME=<workDir> are forced. TMPDIR
// is kept as-is when base sets it, and otherwise defaults to
// <workDir>/tmp; Prepare additionally creates that directory when it
// applies the default. TF_CLI_CONFIG_FILE is appended as cliConfigFile
// when cliConfigFile is not empty; Prepare only passes a non-empty value
// when the image ships a provider mirror, since only it can tell. Environ
// does no filesystem I/O itself.
func Environ(base []string, workDir, cliConfigFile string) (env, dropped []string) {
	env, dropped = runnerOwned(withoutKeys(base, "HOME"))
	env = append(env, "TF_DATA_DIR="+filepath.Join(workDir, ".terraform"), "HOME="+workDir)
	if !hasKey(env, "TMPDIR") {
		env = append(env, "TMPDIR="+filepath.Join(workDir, "tmp"))
	}
	if cliConfigFile != "" {
		env = append(env, "TF_CLI_CONFIG_FILE="+cliConfigFile)
	}
	return env, dropped
}

// runnerOwned drops every TF_* and KUBE_* variable except the ones the Job
// sets itself (jobs.Build: TF_IN_AUTOMATION, TF_INPUT, KUBE_NAMESPACE; the
// container's explicit env wins over envFrom, so these never carry an
// identity Secret's value) and returns the dropped names. The others reach
// the pod through the identity Secret's envFrom or the image's ENV and
// would change what the runner runs: TF_WORKSPACE moves state, TF_CLI_ARGS* rewrites every step, TF_VAR_*
// overrides the rendered inputs, TF_LOG logs provider traffic with its
// credentials, and KUBE_* redirects the kubernetes backend.
func runnerOwned(env []string) (kept, dropped []string) {
	kept = make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if (strings.HasPrefix(name, "TF_") || strings.HasPrefix(name, "KUBE_")) && !slices.Contains([]string{"TF_IN_AUTOMATION", "TF_INPUT", "KUBE_NAMESPACE"}, name) {
			dropped = append(dropped, name)
			continue
		}
		kept = append(kept, kv)
	}
	return kept, dropped
}

// withoutKeys returns env with every entry named in keys removed.
func withoutKeys(env []string, keys ...string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, k := range keys {
			if name == k {
				drop = true
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// hasKey reports whether env has an entry named key.
func hasKey(env []string, key string) bool {
	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); name == key {
			return true
		}
	}
	return false
}
