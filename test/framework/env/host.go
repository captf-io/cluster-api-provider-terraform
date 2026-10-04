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
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/images"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/providers"
)

// downloadTimeout bounds each pinned download.
const downloadTimeout = 5 * time.Minute

// osHost is the real Host: it runs every command in the repository root
// with the environment childEnv builds.
type osHost struct {
	// runner runs every command; an engine.OSRunner outside tests.
	runner engine.Runner
	// clusterctl is the clusterctl binary.
	clusterctl string
	// http downloads the pinned artifacts.
	http *http.Client
	// out receives progress lines.
	out io.Writer
}

// NewHost returns the Host that runs commands with os/exec in c.RepoRoot,
// with the current environment minus make's job-control variables and with
// KUBECONFIG forced to c.Kubeconfig(), so no child ever reads
// ~/.kube/config. Progress goes to out.
func NewHost(c Config, out io.Writer) Host {
	return &osHost{
		runner:     engine.OSRunner{Dir: c.RepoRoot, Env: childEnv(os.Environ(), c.Kubeconfig())},
		clusterctl: c.Clusterctl,
		http:       &http.Client{Timeout: downloadTimeout},
		out:        out,
	}
}

// childEnv returns base without KUBECONFIG and without the variables a
// parent make uses to pass flags and its jobserver to sub-makes (so the
// make the host runs behaves like a top-level one), plus
// KUBECONFIG=kubeconfig.
func childEnv(base []string, kubeconfig string) []string {
	drop := map[string]bool{"KUBECONFIG": true, "MAKEFLAGS": true, "MFLAGS": true, "MAKELEVEL": true, "MAKEOVERRIDES": true, "GNUMAKEFLAGS": true}
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			out = append(out, kv)
		}
	}
	return append(out, "KUBECONFIG="+kubeconfig)
}

// DetectEngine returns engine.Detect's choice for override, probing under
// ctx, or its error.
func (h *osHost) DetectEngine(ctx context.Context, override string) (*engine.Engine, error) {
	return engine.Detect(ctx, override, h.runner)
}

// TreeID returns images.TreeID of the repository root under ctx, or its
// error.
func (h *osHost) TreeID(ctx context.Context) (string, error) {
	return images.TreeID(ctx, h.runner)
}

// BuildManager runs images.BuildManager for ref with engine e under ctx,
// with the build date make passes to the build pinned through the
// environment (buildStamp). It returns an error if git or the build fails.
func (h *osHost) BuildManager(ctx context.Context, e *engine.Engine, ref string) error {
	stamp, err := h.buildStamp(ctx)
	if err != nil {
		return err
	}
	r := h.runner
	if o, ok := r.(engine.OSRunner); ok {
		o.Env = append(append([]string(nil), o.Env...), stamp...)
		r = o
	}
	return images.BuildManager(ctx, r, e, ref, images.WithLog(h.out))
}

// buildStamp returns the DATE override for a build under ctx, as a
// "KEY=value" entry: HEAD's commit time instead of make's default (the
// current time), so the image of an unchanged tree is a full layer-cache
// hit with the same image ID and a rebuild costs seconds. VERSION keeps
// make's default (hack/version.sh), always a valid semantic version. It
// returns an error if git fails.
func (h *osHost) buildStamp(ctx context.Context) ([]string, error) {
	out, err := h.runner.Run(ctx, "git", "show", "-s", "--format=%ct", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("env: build manager: commit time: %w", err)
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("env: build manager: commit time %q: %w", strings.TrimSpace(string(out)), err)
	}
	return []string{"DATE=" + time.Unix(secs, 0).UTC().Format("2006-01-02T15:04:05Z")}, nil
}

// SmokeManager runs images.SmokeManager for ref with engine e under ctx,
// and returns its error.
func (h *osHost) SmokeManager(ctx context.Context, e *engine.Engine, ref string) error {
	return images.SmokeManager(ctx, e, ref, images.WithLog(h.out))
}

// EnsureCache runs providers.EnsureCache for every pinned artifact into
// cacheDir under ctx, and returns its error.
func (h *osHost) EnsureCache(ctx context.Context, cacheDir string) error {
	return providers.EnsureCache(ctx, h.http, cacheDir, framework.Artifacts())
}

// RenderCAPTF runs providers.RenderCAPTF for image into outDir under ctx,
// and returns its error.
func (h *osHost) RenderCAPTF(ctx context.Context, image, outDir string) error {
	return providers.RenderCAPTF(ctx, h.runner, image, outDir)
}

// WriteRepository runs providers.WriteRepository from cacheDir and
// captfDir into repoDir at providers.CAPTFVersion, and returns the
// clusterctl.yaml path or its error.
func (h *osHost) WriteRepository(cacheDir, captfDir, repoDir string) (string, error) {
	return providers.WriteRepository(cacheDir, captfDir, repoDir, providers.CAPTFVersion)
}

// InitProviders runs providers.Init with the pinned clusterctl against
// kubeconfig, configPath and homeDir under ctx, and returns its error.
func (h *osHost) InitProviders(ctx context.Context, kubeconfig, configPath, homeDir string) error {
	return providers.Init(ctx, h.runner, h.clusterctl, kubeconfig, configPath, homeDir, providers.CAPTFVersion)
}

// RemoveNetwork removes the network name through engine e under ctx when
// `network ls` lists it by that exact name, and returns an error if
// listing or removal fails.
func (h *osHost) RemoveNetwork(ctx context.Context, e *engine.Engine, name string) error {
	out, err := e.Run(ctx, "network", "ls", "--format", "{{.Name}}")
	if err != nil {
		return fmt.Errorf("env: remove network: %w", err)
	}
	for line := range strings.Lines(string(out)) {
		if strings.TrimSpace(line) != name {
			continue
		}
		if _, err := e.Run(ctx, "network", "rm", name); err != nil {
			return fmt.Errorf("env: remove network: %w", err)
		}
		fmt.Fprintf(h.out, "testenv: removed network %s\n", name)
		return nil
	}
	return nil
}
