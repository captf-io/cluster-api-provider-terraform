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

package wait

import (
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Clients bundles the typed and dynamic clients of one cluster. Unit tests
// fill it with the client-go fakes.
type Clients struct {
	// Kube is the typed client, used for Deployments, Pods and Events.
	Kube kubernetes.Interface
	// Dynamic is the dynamic client, used for CRDs and for CAPI and CAPTF
	// objects, which stay unstructured.
	Dynamic dynamic.Interface
}

// ClientsFromKubeconfig builds Clients from the kubeconfig file at path.
// Only that file is read: the default loading rules, KUBECONFIG and
// ~/.kube/config are never consulted, so an empty path is an error. It
// returns a wrapped error when path is empty, unreadable or invalid.
func ClientsFromKubeconfig(path string) (Clients, error) {
	if path == "" {
		return Clients{}, fmt.Errorf("wait: load kubeconfig: path is empty (default loading rules are never used)")
	}
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: path}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return Clients{}, fmt.Errorf("wait: load kubeconfig %s: %w", path, err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return Clients{}, fmt.Errorf("wait: build typed client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return Clients{}, fmt.Errorf("wait: build dynamic client: %w", err)
	}
	return Clients{Kube: kube, Dynamic: dyn}, nil
}
