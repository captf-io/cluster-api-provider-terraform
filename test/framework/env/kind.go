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
	"os"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/images"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/kindcluster"
)

// kindCluster is the real Cluster: kindcluster's Manager plus the images
// package's node operations.
type kindCluster struct {
	// *kindcluster.Manager provides Create, Exists, List, Delete,
	// Kubeconfig and CollectLogs, each guarded by ValidateName.
	*kindcluster.Manager
	// engine saves the images SideLoad copies.
	engine *engine.Engine
	// out receives progress lines.
	out io.Writer
}

// NewCluster returns the Cluster for engine e, with kind's progress and
// the side-load progress written to out, built on kindcluster.New with
// opts appended (tests pass kindcluster.WithProvider). It returns an
// error if kindcluster.New fails.
func NewCluster(e *engine.Engine, out io.Writer, opts ...kindcluster.Option) (Cluster, error) {
	m, err := kindcluster.New(e, append([]kindcluster.Option{kindcluster.WithLogWriter(out)}, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("env: %w", err)
	}
	return &kindCluster{Manager: m, engine: e, out: out}, nil
}

// Nodes returns the node names of the cluster name, or an error.
func (k *kindCluster) Nodes(name string) ([]string, error) {
	ns, err := k.Manager.Nodes(name)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.String())
	}
	return out, nil
}

// SideLoad runs images.SideLoad for refs into every node of the cluster
// name under ctx, with archives in workDir, and returns its error.
func (k *kindCluster) SideLoad(ctx context.Context, name string, refs []string, workDir string) error {
	ns, err := k.Manager.Nodes(name)
	if err != nil {
		return err
	}
	return images.SideLoad(ctx, k.engine, ns, refs, workDir, images.WithLog(k.out))
}

// NodePull runs images.NodePull for each of refs on every node of the
// cluster name under ctx, node by node, and returns the first error.
func (k *kindCluster) NodePull(ctx context.Context, name string, refs []string) error {
	ns, err := k.Manager.Nodes(name)
	if err != nil {
		return err
	}
	for _, n := range ns {
		for _, ref := range refs {
			if err := images.NodePull(ctx, n, ref, images.WithLog(k.out)); err != nil {
				return err
			}
		}
	}
	return nil
}

// NodeImages returns images.NodeImage for each of refs on every node of
// the cluster name, inspected under ctx, or the first error.
func (k *kindCluster) NodeImages(ctx context.Context, name string, refs []string) ([]NodeImage, error) {
	ns, err := k.Manager.Nodes(name)
	if err != nil {
		return nil, err
	}
	var out []NodeImage
	for _, n := range ns {
		for _, ref := range refs {
			info, err := images.NodeImage(ctx, n, ref)
			if err != nil {
				return nil, err
			}
			out = append(out, NodeImage{
				Node:        n.String(),
				Ref:         info.Ref,
				ID:          info.ID,
				RepoTags:    info.RepoTags,
				RepoDigests: info.RepoDigests,
			})
		}
	}
	return out, nil
}

// CollectLogs creates dir, which diag leaves to the hook, and writes the
// cluster name's node logs into it. It returns an error if either fails.
func (k *kindCluster) CollectLogs(name, dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("env: collect logs: %w", err)
	}
	return k.Manager.CollectLogs(name, dir)
}
