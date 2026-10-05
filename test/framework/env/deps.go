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
	"context"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/kindcluster"
)

// Host runs the steps that spawn processes on this machine: git, make,
// the container engine and clusterctl. The real one is NewHost. In every
// method, ctx bounds the work.
type Host interface {
	// DetectEngine returns the container engine to use under ctx;
	// override is the CAPTF_TESTENV_ENGINE value (empty: auto-detect). It
	// returns an error when no engine is usable.
	DetectEngine(ctx context.Context, override string) (*engine.Engine, error)
	// TreeID returns the working tree's identity (images.TreeID) under
	// ctx, or an error.
	TreeID(ctx context.Context) (string, error)
	// BuildManager builds the manager image ref with engine e under ctx
	// and returns an error if the build fails.
	BuildManager(ctx context.Context, e *engine.Engine, ref string) error
	// SmokeManager runs the image ref's binaries with --help through
	// engine e under ctx and returns an error if either fails.
	SmokeManager(ctx context.Context, e *engine.Engine, ref string) error
	// EnsureCache fills cacheDir with every pinned download under ctx,
	// and returns an error if one fails or mismatches its sha256.
	EnsureCache(ctx context.Context, cacheDir string) error
	// RenderCAPTF renders the CAPTF provider with the manager image into
	// outDir under ctx, and returns an error if rendering fails.
	RenderCAPTF(ctx context.Context, image, outDir string) error
	// WriteRepository writes the clusterctl local repository into repoDir
	// from cacheDir and the rendered captfDir, and returns the
	// clusterctl.yaml path or an error.
	WriteRepository(cacheDir, captfDir, repoDir string) (string, error)
	// InitProviders runs clusterctl init under ctx against kubeconfig with
	// the clusterctl.yaml at configPath and the isolated homeDir, and
	// returns an error if it fails.
	InitProviders(ctx context.Context, kubeconfig, configPath, homeDir string) error
	// RemoveNetwork removes the container network name through engine e
	// under ctx if it exists, and returns an error if removal fails.
	RemoveNetwork(ctx context.Context, e *engine.Engine, name string) error
}

// Cluster is the kind side of the environment. Every name passes
// kindcluster.ValidateName in the real one (NewCluster).
type Cluster interface {
	// Create creates the cluster o describes (ctx is checked before kind
	// starts) and returns an error if kind fails.
	Create(ctx context.Context, o kindcluster.Options) error
	// Exists reports whether the cluster name exists, or returns an
	// error.
	Exists(name string) (bool, error)
	// List returns every captf-test-* cluster, or an error.
	List() ([]string, error)
	// Delete deletes the cluster name, removing its entry from the
	// kubeconfig file kubeconfigPath, and returns an error if kind fails.
	Delete(name, kubeconfigPath string) error
	// Kubeconfig returns the cluster name's external kubeconfig, or an
	// error.
	Kubeconfig(name string) (string, error)
	// Nodes returns the cluster name's node names, or an error.
	Nodes(name string) ([]string, error)
	// SideLoad copies the local images refs into every node of the
	// cluster name under ctx, using workDir for archives, and returns an
	// error at the first failure.
	SideLoad(ctx context.Context, name string, refs []string, workDir string) error
	// NodePull pulls each of refs inside every node of the cluster name
	// under ctx, so the nodes hold the registry's repo digests, and returns
	// an error at the first failure. It is idempotent.
	NodePull(ctx context.Context, name string, refs []string) error
	// NodeImages returns, for every node of the cluster name and each of
	// refs, what the node's containerd holds, inspected under ctx, or an
	// error.
	NodeImages(ctx context.Context, name string, refs []string) ([]NodeImage, error)
	// CollectLogs writes the cluster name's node logs into dir, and
	// returns an error if kind fails.
	CollectLogs(name, dir string) error
}

// Kube is the API side of one cluster, reached through one explicit
// kubeconfig. The real one is NewKube. In every method, ctx bounds the
// API calls.
type Kube interface {
	// WaitProviders waits under ctx until every provider Deployment is
	// Available, the CAPTF CRDs are Established and the webhook serves,
	// and returns an error on timeout.
	WaitProviders(ctx context.Context) error
	// Report returns the non-ready provider pods under ctx, one per line,
	// or an error.
	Report(ctx context.Context) (string, error)
	// Collect writes a diagnostics bundle into dir under ctx, calling
	// nodeLogs with the directory for the kind node logs, and returns an
	// error only when nothing could be written.
	Collect(ctx context.Context, dir string, nodeLogs func(dir string) error) error
	// ManagerImage returns the image of the CAPTF manager container, read
	// under ctx, or an error.
	ManagerImage(ctx context.Context) (string, error)
	// SetManagerImage points the CAPTF manager container, and its runner
	// image setting, at ref under ctx, and returns an error if the patch
	// fails.
	SetManagerImage(ctx context.Context, ref string) error
	// WaitManagerRollout waits under ctx until the CAPTF manager
	// Deployment has rolled out completely, and returns an error on
	// timeout.
	WaitManagerRollout(ctx context.Context) error
}

// Deps are the side effects an Env runs through.
type Deps struct {
	// Host runs processes.
	Host Host
	// Cluster returns the kind side for engine e, or an error.
	Cluster func(e *engine.Engine) (Cluster, error)
	// Kube returns the API side of the cluster behind the kubeconfig file,
	// or an error.
	Kube func(kubeconfig string) (Kube, error)
}
