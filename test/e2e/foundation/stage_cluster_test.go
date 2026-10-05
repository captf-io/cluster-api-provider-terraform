//go:build e2e

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

package foundation

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/kind/pkg/cluster/nodes"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/greenlight"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/images"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/kindcluster"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// clusterTimeout bounds stage 1: the manager build, the cluster creation,
// the manager side-load and the in-node noop pulls.
const clusterTimeout = 15 * time.Minute

// clusterBuild is stage 1. It refuses an existing cluster unless reuse is
// set, invalidates any previous green light, runs env.UpCluster, then
// checks that the cluster exists with the expected nodes, that the
// explicit kubeconfig reaches an API server of the pinned version, that
// every node runs the pinned node image, that the side-loaded manager
// image is on every node with the image ID the host holds, and that every
// node holds each noop image with its exact pinned repo digest.
// It runs under ctx and fails t on any problem.
func (s *suite) clusterBuild(ctx context.Context, t *testing.T) {
	ctx, cancel := context.WithTimeout(ctx, clusterTimeout)
	defer cancel()
	eng, err := engine.Detect(ctx, s.cfg.EngineOverride, engine.ExecRunner())
	if err != nil {
		t.Fatalf("expected a usable container engine: %v", err)
	}
	s.eng = eng
	if s.kind, err = kindcluster.New(eng, kindcluster.WithLogWriter(os.Stderr)); err != nil {
		t.Fatalf("kind: %v", err)
	}
	exists, err := s.kind.Exists(s.cfg.Name)
	if err != nil {
		t.Fatalf("cannot tell whether cluster %s exists: %v", s.cfg.Name, err)
	}
	if exists && !s.opts.reuse {
		t.Fatalf("expected no cluster %s (the suite starts from a fresh cluster), observed an existing one: set %s=1 to reuse it, or delete it with make e2e-down",
			s.cfg.Name, reuseEnv)
	}
	s.invalidateGreenLight(t)

	res, err := s.env.UpCluster(ctx)
	if err != nil {
		s.envCollected = s.clusterExists()
		t.Fatalf("env.UpCluster failed: %v", err)
	}
	s.res, s.managerRef, s.treeID = res, res.State.ManagerRef, res.State.TreeID
	if !s.clusterExists() {
		t.Fatalf("expected kind cluster %s to exist after UpCluster, observed none; inspect: podman ps -a --filter name=%s", s.cfg.Name, s.cfg.Name)
	}
	if s.c, err = wait.ClientsFromKubeconfig(s.cfg.Kubeconfig()); err != nil {
		t.Fatalf("expected the explicit kubeconfig %s to load: %v", s.cfg.Kubeconfig(), err)
	}
	s.checkServerVersion(t)
	s.checkNodes(ctx, t)
	s.checkNodeImages(ctx, t)
}

// invalidateGreenLight removes the previous run's greenlight.json, so a
// failing run never leaves an older green light behind. It fails t if the
// file exists and cannot be removed.
func (s *suite) invalidateGreenLight(t *testing.T) {
	t.Helper()
	path := greenlight.Path(s.cfg.WorkDir())
	switch err := os.Remove(path); {
	case err == nil:
		t.Logf("removed the previous green light %s; only a passing run writes a new one", path)
	case !errors.Is(err, os.ErrNotExist):
		t.Fatalf("cannot remove the previous green light %s: %v", path, err)
	}
}

// checkServerVersion records the API server's version and fails t unless
// it is framework.KubernetesVersion.
func (s *suite) checkServerVersion(t *testing.T) {
	t.Helper()
	v, err := s.c.Kube.Discovery().ServerVersion()
	if err != nil {
		t.Fatalf("expected the API server behind %s to answer /version: %v; inspect: %s", s.cfg.Kubeconfig(), err, s.kubectl("version"))
	}
	s.serverVersion = v.GitVersion
	if v.GitVersion != framework.KubernetesVersion {
		t.Errorf("expected API server version %s (framework.KubernetesVersion), observed %s", framework.KubernetesVersion, v.GitVersion)
	}
}

// checkNodes fails t unless the cluster has nodeCount nodes, in the API
// and in kind, each with kubelet framework.KubernetesVersion, read under
// ctx.
func (s *suite) checkNodes(ctx context.Context, t *testing.T) {
	t.Helper()
	list, err := s.c.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(list.Items) != s.nodeCount() {
		t.Errorf("expected %d nodes (1 control plane + %d workers), observed %d; inspect: %s", s.nodeCount(), s.opts.workers, len(list.Items), s.kubectl("get nodes -o wide"))
	}
	for _, n := range list.Items {
		if got := n.Status.NodeInfo.KubeletVersion; got != framework.KubernetesVersion {
			t.Errorf("node %s: expected kubelet %s, observed %s", n.Name, framework.KubernetesVersion, got)
		}
	}
	names, err := s.kind.Nodes(s.cfg.Name)
	if err != nil {
		t.Fatalf("kind nodes of %s: %v", s.cfg.Name, err)
	}
	if len(names) != s.nodeCount() {
		t.Errorf("expected %d kind node containers, observed %d", s.nodeCount(), len(names))
	}
}

// checkNodeImages fails t unless every kind node container runs
// framework.KindNodeImage, holds the manager image with the host's image
// ID, and holds every noop image with a repo digest of exactly
// <repository>@<pinned digest>, inspected under ctx. The manager image is
// side-loaded, which drops repo digests, so none is expected of it; the
// noop images are pulled in the node by their pinned reference, so they
// carry the registry's real digest, which CAPTF's digest pinning needs.
func (s *suite) checkNodeImages(ctx context.Context, t *testing.T) {
	t.Helper()
	nodeImageID, err := s.eng.ImageID(ctx, framework.KindNodeImage)
	if err != nil {
		t.Fatalf("expected the pinned node image %s on the host: %v", framework.KindNodeImage, err)
	}
	managerID, err := s.eng.ImageID(ctx, s.managerRef)
	if err != nil {
		t.Fatalf("expected image %s on the host after UpCluster: %v", s.managerRef, err)
	}
	nodeList, err := s.kind.Nodes(s.cfg.Name)
	if err != nil {
		t.Fatalf("kind nodes of %s: %v", s.cfg.Name, err)
	}
	for _, n := range nodeList {
		out, err := s.eng.Run(ctx, "inspect", "--type", "container", "--format", "{{.Image}}", n.String())
		if err != nil {
			t.Errorf("inspect node container %s: %v", n, err)
		} else if got := normalizeID(string(out)); got != nodeImageID {
			t.Errorf("node %s: expected node image %s (%s), observed image %s; inspect: %s inspect %s", n, framework.KindNodeImage, nodeImageID, got, s.eng.Name, n)
		}
		s.checkManagerImage(ctx, t, n, managerID)
		for _, img := range framework.NoopImages() {
			s.checkNoopImage(ctx, t, n, img)
		}
	}
	t.Logf("nodes run %s, hold the side-loaded manager image with the host's ID, and hold %d noop images with their pinned repo digests",
		framework.KindNodeImage, len(framework.NoopImages()))
}

// checkManagerImage fails t unless node n holds the manager image with
// the image ID wantID (the host's), inspected under ctx.
func (s *suite) checkManagerImage(ctx context.Context, t *testing.T, n nodes.Node, wantID string) {
	t.Helper()
	info, err := images.NodeImage(ctx, n, s.managerRef)
	if err != nil {
		t.Errorf("node %s: expected image %s side-loaded: %v; inspect: %s exec %s crictl images", n, s.managerRef, err, s.eng.Name, n)
		return
	}
	if info.ID != wantID {
		t.Errorf("node %s: image %s: expected ID %s (the host's), observed %s", n, s.managerRef, wantID, info.ID)
	}
}

// checkNoopImage fails t unless node n holds img, found by its pinned
// reference, with a repo digest of exactly img.Pinned(), inspected under
// ctx.
func (s *suite) checkNoopImage(ctx context.Context, t *testing.T, n nodes.Node, img framework.NoopImage) {
	t.Helper()
	info, err := images.NodeImage(ctx, n, img.Pinned())
	if err != nil {
		t.Errorf("node %s: expected image %s pulled in the node: %v; inspect: %s exec %s crictl images", n, img.Pinned(), err, s.eng.Name, n)
		return
	}
	if !slices.Contains(info.RepoDigests, img.Pinned()) {
		t.Errorf("node %s: image %s: expected repo digest %s, observed %v (a side-load would leave only a synthetic import-<date> digest)",
			n, img.Ref, img.Pinned(), info.RepoDigests)
	}
}

// normalizeID returns the trimmed image ID s with the "sha256:" prefix
// podman leaves out.
func normalizeID(s string) string {
	s = strings.TrimSpace(s)
	if s != "" && !strings.HasPrefix(s, "sha256:") {
		s = "sha256:" + s
	}
	return s
}
