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

package env

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
)

// repoTree returns a temporary repository root of t holding go.work, and
// a directory two levels below it.
func repoTree(t *testing.T) (root, sub string) {
	t.Helper()
	root = t.TempDir()
	sub = filepath.Join(root, "test", "env")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.work"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, sub
}

// envOf returns a getenv over the map m.
func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestLoadConfigDefaults checks the defaults of an empty environment.
func TestLoadConfigDefaults(t *testing.T) {
	t.Parallel()
	root, sub := repoTree(t)
	c, err := LoadConfig(envOf(nil), sub, "/cache")
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Name:       framework.DefaultClusterName,
		RepoRoot:   root,
		CacheDir:   "/cache",
		Clusterctl: filepath.Join(root, "hack", "tools", "bin", "clusterctl"),
		Kustomize:  filepath.Join(root, "hack", "tools", "bin", "kustomize"),
	}
	if c != want {
		t.Errorf("config:\n got %+v\nwant %+v", c, want)
	}
	if got := c.WorkDir(); got != filepath.Join(root, "bin", "testenv", "captf-test-dev") {
		t.Errorf("WorkDir = %s", got)
	}
	if got := c.Kubeconfig(); got != filepath.Join(root, "bin", "testenv", "captf-test-dev", "kubeconfig") {
		t.Errorf("Kubeconfig = %s", got)
	}
}

// TestLoadConfigValues checks that every variable is read.
func TestLoadConfigValues(t *testing.T) {
	t.Parallel()
	root, sub := repoTree(t)
	c, err := LoadConfig(envOf(map[string]string{
		NameEnv:             " captf-test-x ",
		framework.EngineEnv: "docker",
		WorkersEnv:          "2",
		ReuseEnv:            "1",
		AllEnv:              "true",
		ClusterctlEnv:       "/abs/clusterctl-v1",
		KustomizeEnv:        "rel/kustomize",
	}), sub, "/cache")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "captf-test-x" || c.EngineOverride != "docker" || c.Workers != 2 || !c.Reuse || !c.All ||
		c.Clusterctl != "/abs/clusterctl-v1" || c.Kustomize != filepath.Join(root, "rel", "kustomize") {
		t.Errorf("config: %+v", c)
	}
}

// TestLoadConfigErrors checks that every invalid value is refused with
// its variable named.
func TestLoadConfigErrors(t *testing.T) {
	t.Parallel()
	_, sub := repoTree(t)
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"protected": {map[string]string{NameEnv: framework.ProtectedClusterName}, NameEnv},
		"prefix":    {map[string]string{NameEnv: "kind"}, NameEnv},
		"engine":    {map[string]string{framework.EngineEnv: "lxc"}, framework.EngineEnv},
		"workers":   {map[string]string{WorkersEnv: "many"}, WorkersEnv},
		"negative":  {map[string]string{WorkersEnv: "-1"}, WorkersEnv},
		"too many":  {map[string]string{WorkersEnv: "6"}, WorkersEnv},
		"reuse":     {map[string]string{ReuseEnv: "maybe"}, ReuseEnv},
		"all":       {map[string]string{AllEnv: "yes please"}, AllEnv},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadConfig(envOf(tc.env), sub, "/cache")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error naming %s", err, tc.want)
			}
		})
	}
	if _, err := LoadConfig(envOf(nil), t.TempDir(), "/cache"); err == nil || !strings.Contains(err.Error(), "no go.work") {
		t.Errorf("outside a repository: got %v", err)
	}
}

// TestFindRepoRoot checks the upward search, starting at the root itself
// and below it.
func TestFindRepoRoot(t *testing.T) {
	t.Parallel()
	root, sub := repoTree(t)
	for _, dir := range []string{root, sub} {
		got, err := FindRepoRoot(dir)
		if err != nil || got != root {
			t.Errorf("FindRepoRoot(%s) = %s, %v; want %s", dir, got, err, root)
		}
	}
}

// TestConfigFromEnv checks that the process configuration resolves to
// this repository (when the environment holds no invalid value).
func TestConfigFromEnv(t *testing.T) {
	t.Parallel()
	c, err := ConfigFromEnv()
	if err != nil {
		t.Skipf("environment holds an invalid testenv value: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.RepoRoot, "go.work")); err != nil {
		t.Errorf("RepoRoot %s has no go.work: %v", c.RepoRoot, err)
	}
	if c.CacheDir == "" {
		t.Error("CacheDir is empty")
	}
}
