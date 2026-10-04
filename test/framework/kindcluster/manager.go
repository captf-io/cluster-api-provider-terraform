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

package kindcluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodes"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
)

// Provider is the part of kind's *cluster.Provider the Manager uses.
// *cluster.Provider satisfies it; tests substitute a fake with WithProvider.
type Provider interface {
	// Create creates the cluster called name with options, and returns an
	// error if that fails.
	Create(name string, options ...cluster.CreateOption) error
	// Delete deletes the cluster called name and its entry in the kubeconfig
	// at explicitKubeconfigPath, and returns an error if that fails.
	Delete(name, explicitKubeconfigPath string) error
	// List returns every cluster name the provider knows, or an error.
	List() ([]string, error)
	// KubeConfig returns the kubeconfig contents of the cluster called name
	// (the in-network one when internal is true), or an error.
	KubeConfig(name string, internal bool) (string, error)
	// ListNodes returns the nodes of the cluster called name, or an error.
	ListNodes(name string) ([]nodes.Node, error)
	// CollectLogs writes the node logs of the cluster called name under dir,
	// and returns an error if that fails.
	CollectLogs(name, dir string) error
}

// envMu serializes the process-environment changes Create makes.
var envMu sync.Mutex

// Manager creates and manages kind clusters through a Provider. Build it
// with New.
type Manager struct {
	// engine names the network environment variable.
	engine *engine.Engine
	// provider is kind's library, or a fake.
	provider Provider
}

// config holds what Options set before New builds the Manager.
type config struct {
	// provider overrides the kind library provider when non-nil.
	provider Provider
	// logs receives kind's progress when non-nil.
	logs io.Writer
}

// Option configures New.
type Option func(*config)

// WithProvider makes the Manager use p instead of kind's library. It is
// for tests; the engine is still used for the network variable name. It
// returns the Option to pass to New.
func WithProvider(p Provider) Option {
	return func(c *config) { c.provider = p }
}

// WithLogWriter streams kind's progress messages, one per line, to w.
// Without it they are discarded. It returns the Option to pass to New.
func WithLogWriter(w io.Writer) Option {
	return func(c *config) { c.logs = w }
}

// New returns a Manager for e's kind provider, applying opts. It returns an
// error if e is nil or names no supported engine.
func New(e *engine.Engine, opts ...Option) (*Manager, error) {
	if e == nil {
		return nil, errors.New("kindcluster: new: nil engine")
	}
	if e.KindNetworkEnv() == "" {
		return nil, fmt.Errorf("kindcluster: new: unsupported engine %q", e.Name)
	}
	var c config
	for _, o := range opts {
		o(&c)
	}
	if c.provider == nil {
		po, err := e.KindProvider()
		if err != nil {
			return nil, fmt.Errorf("kindcluster: new: %w", err)
		}
		c.provider = cluster.NewProvider(po, cluster.ProviderWithLogger(newWriterLogger(c.logs)))
	}
	return &Manager{engine: e, provider: c.provider}, nil
}

// withEnv sets the environment variable key to value while fn runs, under
// envMu, then restores the previous value (or unsets key if it was unset).
// It returns fn's error.
func withEnv(key, value string, fn func() error) error {
	envMu.Lock()
	defer envMu.Unlock()
	prev, had := os.LookupEnv(key)
	if err := os.Setenv(key, value); err != nil {
		return fmt.Errorf("set %s: %w", key, err)
	}
	defer func() {
		if had {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	}()
	return fn()
}

// Create creates the cluster o describes under ctx, writing its external
// kubeconfig to o.KubeconfigPath, and waits for the control plane. It
// returns an error if o.Name fails ValidateName, o.KubeconfigPath is
// empty, the cluster already exists, ctx is already done, or kind fails.
// While kind runs, the engine's kind network variable is set to
// o.Network (default framework.KindNetwork); see the package doc for the
// process-wide caveat. kind's library cannot be interrupted, so ctx is
// checked only before the call.
func (m *Manager) Create(ctx context.Context, o Options) error {
	cfg, err := RenderConfig(o)
	if err != nil {
		return fmt.Errorf("kindcluster: create: %w", err)
	}
	if o.KubeconfigPath == "" {
		return errors.New("kindcluster: create: KubeconfigPath is required")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("kindcluster: create: %w", err)
	}
	exists, err := m.Exists(o.Name)
	if err != nil {
		return fmt.Errorf("kindcluster: create: %w", err)
	}
	if exists {
		return fmt.Errorf("kindcluster: create: cluster %q already exists", o.Name)
	}
	o = o.withDefaults()
	err = withEnv(m.engine.KindNetworkEnv(), o.Network, func() error {
		return m.provider.Create(o.Name,
			cluster.CreateWithV1Alpha4Config(cfg),
			cluster.CreateWithNodeImage(o.NodeImage),
			cluster.CreateWithKubeconfigPath(o.KubeconfigPath),
			cluster.CreateWithWaitForReady(o.WaitForReady),
			cluster.CreateWithDisplayUsage(false),
			cluster.CreateWithDisplaySalutation(false),
		)
	})
	if err != nil {
		return fmt.Errorf("kindcluster: create %q: %w", o.Name, err)
	}
	return nil
}

// List returns the names of the existing clusters that carry
// framework.ClusterNamePrefix and pass ValidateName; other clusters are
// never reported. It returns an error if kind fails.
func (m *Manager) List() ([]string, error) {
	all, err := m.provider.List()
	if err != nil {
		return nil, fmt.Errorf("kindcluster: list: %w", err)
	}
	var out []string
	for _, n := range all {
		if strings.HasPrefix(n, framework.ClusterNamePrefix) && ValidateName(n) == nil {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Exists reports whether the cluster name exists. It returns an error if
// name fails ValidateName or kind fails.
func (m *Manager) Exists(name string) (bool, error) {
	if err := ValidateName(name); err != nil {
		return false, fmt.Errorf("kindcluster: exists: %w", err)
	}
	names, err := m.List()
	if err != nil {
		return false, fmt.Errorf("kindcluster: exists: %w", err)
	}
	return slices.Contains(names, name), nil
}

// Delete deletes the cluster name and removes its entry from the
// kubeconfig at kubeconfigPath (which may be empty to skip the kubeconfig).
// It returns an error, deleting nothing, if name fails ValidateName; kind
// treats a missing cluster as success.
func (m *Manager) Delete(name, kubeconfigPath string) error {
	if err := ValidateName(name); err != nil {
		return fmt.Errorf("kindcluster: delete: %w", err)
	}
	if err := m.provider.Delete(name, kubeconfigPath); err != nil {
		return fmt.Errorf("kindcluster: delete %q: %w", name, err)
	}
	return nil
}

// Nodes returns the cluster name's nodes. It returns an error if name
// fails ValidateName or kind fails.
func (m *Manager) Nodes(name string) ([]nodes.Node, error) {
	if err := ValidateName(name); err != nil {
		return nil, fmt.Errorf("kindcluster: nodes: %w", err)
	}
	ns, err := m.provider.ListNodes(name)
	if err != nil {
		return nil, fmt.Errorf("kindcluster: nodes %q: %w", name, err)
	}
	return ns, nil
}

// Kubeconfig returns the contents of the cluster name's external
// kubeconfig (the one that reaches the API server from the host). It
// returns an error if name fails ValidateName or kind fails.
func (m *Manager) Kubeconfig(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", fmt.Errorf("kindcluster: kubeconfig: %w", err)
	}
	kc, err := m.provider.KubeConfig(name, false)
	if err != nil {
		return "", fmt.Errorf("kindcluster: kubeconfig %q: %w", name, err)
	}
	return kc, nil
}

// CollectLogs writes the cluster name's node logs under dir. It returns an
// error if name fails ValidateName or kind fails.
func (m *Manager) CollectLogs(name, dir string) error {
	if err := ValidateName(name); err != nil {
		return fmt.Errorf("kindcluster: collect logs: %w", err)
	}
	if err := m.provider.CollectLogs(name, dir); err != nil {
		return fmt.Errorf("kindcluster: collect logs %q: %w", name, err)
	}
	return nil
}
