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

package kindcluster

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
)

// DefaultWaitForReady is the control-plane readiness wait used when
// Options.WaitForReady is zero.
const DefaultWaitForReady = 3 * time.Minute

// maxNameLen caps a cluster name so the longest node container name
// ("<name>-control-plane", 14 extra bytes) stays a valid 63-byte hostname.
const maxNameLen = 63 - len("-control-plane")

// dns1123Label matches an RFC 1123 DNS label.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Options describes one cluster to create.
type Options struct {
	// Name is the cluster name; it must pass ValidateName.
	Name string
	// Workers is the number of worker nodes; 0 means a single
	// control-plane node.
	Workers int
	// NodeImage is the kind node image; empty means framework.KindNodeImage.
	NodeImage string
	// KubeconfigPath is where Create writes the external kubeconfig. It is
	// required and explicit: ~/.kube/config is never used.
	KubeconfigPath string
	// Network is the container network the nodes join; empty means
	// framework.KindNetwork.
	Network string
	// WaitForReady is how long Create waits for the control plane; zero
	// means DefaultWaitForReady.
	WaitForReady time.Duration
}

// withDefaults returns a copy of o with the empty NodeImage, Network and
// WaitForReady fields replaced by their defaults.
func (o Options) withDefaults() Options {
	if o.NodeImage == "" {
		o.NodeImage = framework.KindNodeImage
	}
	if o.Network == "" {
		o.Network = framework.KindNetwork
	}
	if o.WaitForReady == 0 {
		o.WaitForReady = DefaultWaitForReady
	}
	return o
}

// ValidateName returns nil if name is a cluster name this package may
// touch: not framework.ProtectedClusterName, prefixed with
// framework.ClusterNamePrefix plus at least one more character, an RFC 1123
// DNS label, and short enough for its node container names. Otherwise it
// returns an error saying why.
func ValidateName(name string) error {
	if name == framework.ProtectedClusterName {
		return fmt.Errorf("kindcluster: validate name: %q is the operator's own cluster and is never touched", name)
	}
	if !strings.HasPrefix(name, framework.ClusterNamePrefix) || name == framework.ClusterNamePrefix {
		return fmt.Errorf("kindcluster: validate name: %q must start with %q and have a suffix", name, framework.ClusterNamePrefix)
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("kindcluster: validate name: %q is longer than %d characters", name, maxNameLen)
	}
	if !dns1123Label.MatchString(name) {
		return fmt.Errorf("kindcluster: validate name: %q is not a DNS-1123 label", name)
	}
	return nil
}

// RenderConfig returns the kind cluster configuration for o, with defaults
// applied: one control-plane node plus o.Workers workers, all on
// o.NodeImage, with an IPv4 cluster whose API server listens on 127.0.0.1
// at a kind-chosen port. It is pure. It returns an error if o.Name fails
// ValidateName or o.Workers is negative.
func RenderConfig(o Options) (*v1alpha4.Cluster, error) {
	if err := ValidateName(o.Name); err != nil {
		return nil, err
	}
	if o.Workers < 0 {
		return nil, fmt.Errorf("kindcluster: render config: workers is %d, want >= 0", o.Workers)
	}
	o = o.withDefaults()
	nodes := []v1alpha4.Node{{Role: v1alpha4.ControlPlaneRole, Image: o.NodeImage}}
	for range o.Workers {
		nodes = append(nodes, v1alpha4.Node{Role: v1alpha4.WorkerRole, Image: o.NodeImage})
	}
	return &v1alpha4.Cluster{
		TypeMeta: v1alpha4.TypeMeta{Kind: "Cluster", APIVersion: "kind.x-k8s.io/v1alpha4"},
		Name:     o.Name,
		Nodes:    nodes,
		Networking: v1alpha4.Networking{
			IPFamily:         v1alpha4.IPv4Family,
			APIServerAddress: "127.0.0.1",
		},
	}, nil
}
