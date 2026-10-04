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
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
)

// minimalPath is the PATH clusterctl runs with: it needs no tool beyond
// the system's.
const minimalPath = "/usr/local/bin:/usr/bin:/bin"

// InitArgs returns the clusterctl argv (without the program) that
// installs core cluster-api, the kubeadm bootstrap and control-plane
// providers at framework.CAPIVersion and the terraform infrastructure
// provider at captfVersion, from the repository configPath describes, into
// the cluster kubeconfig names. It waits for the providers to be
// installed. Every provider carries an explicit version, so clusterctl
// never resolves "latest" against a remote.
func InitArgs(kubeconfig, configPath, captfVersion string) []string {
	return []string{
		"init",
		"--kubeconfig", kubeconfig,
		"--config", configPath,
		"--core", "cluster-api:" + framework.CAPIVersion,
		"--bootstrap", "kubeadm:" + framework.CAPIVersion,
		"--control-plane", "kubeadm:" + framework.CAPIVersion,
		"--infrastructure", captfName + ":" + captfVersion,
		"--wait-providers",
	}
}

// InitEnv returns the complete, isolated environment clusterctl init runs
// in, for the cluster kubeconfig names: a minimal PATH, HOME=homeDir and XDG_CONFIG_HOME=homeDir/.config (so
// ~/.cluster-api and ~/.kube are never read), an explicit KUBECONFIG,
// the cluster-topology and machine-pool feature gates the providers'
// components default on, GOPROXY=off, and the version check disabled.
func InitEnv(kubeconfig, homeDir string) []string {
	return []string{
		"PATH=" + minimalPath,
		"HOME=" + homeDir,
		"XDG_CONFIG_HOME=" + filepath.Join(homeDir, ".config"),
		"KUBECONFIG=" + kubeconfig,
		"CLUSTER_TOPOLOGY=true",
		"EXP_MACHINE_POOL=true",
		"GOPROXY=off",
		"CLUSTERCTL_DISABLE_VERSIONCHECK=true",
	}
}

// Init runs `clusterctl init` (InitArgs for kubeconfig, configPath and
// captfVersion) with the binary clusterctlBin through r under ctx,
// creating homeDir first. When r is an engine.OSRunner (or a
// pointer to one) its Env is replaced by InitEnv, so the isolation holds
// whatever the caller built; any other Runner, a fake in tests, is used
// as is. It returns an error wrapping the clusterctl failure.
func Init(ctx context.Context, r engine.Runner, clusterctlBin, kubeconfig, configPath, homeDir, captfVersion string) error {
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		return fmt.Errorf("providers: init: %w", err)
	}
	switch o := r.(type) {
	case engine.OSRunner:
		o.Env = InitEnv(kubeconfig, homeDir)
		r = o
	case *engine.OSRunner:
		c := *o
		c.Env = InitEnv(kubeconfig, homeDir)
		r = c
	}
	if _, err := r.Run(ctx, clusterctlBin, InitArgs(kubeconfig, configPath, captfVersion)...); err != nil {
		return fmt.Errorf("providers: init: %w", err)
	}
	return nil
}
