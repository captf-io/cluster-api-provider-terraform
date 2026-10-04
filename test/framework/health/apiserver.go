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

package health

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// APIServer checks that the API server answers /readyz?verbose and /livez
// with success, using the discovery REST client of c, and ctx for the
// calls. It returns nil when
// both pass, else an error naming each failing endpoint and its cause. The
// fake clientset has no REST client, so the call is live-only (unit tests
// use an httptest server behind a real clientset).
func APIServer(ctx context.Context, c wait.Clients) error {
	rc := c.Kube.Discovery().RESTClient()
	if rc == nil {
		return fmt.Errorf("health: apiserver: client has no REST client")
	}
	var problems []string
	if _, err := rc.Get().AbsPath("/readyz").Param("verbose", "").DoRaw(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("/readyz?verbose: %v", err))
	}
	if _, err := rc.Get().AbsPath("/livez").DoRaw(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("/livez: %v", err))
	}
	if len(problems) > 0 {
		return fmt.Errorf("health: apiserver not healthy:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// Nodes checks that the cluster has exactly want nodes and that each is
// Ready with no memory, disk or PID pressure and no unavailable network.
// It lists them with c under ctx. It returns nil when all hold, else an error listing every problem.
func Nodes(ctx context.Context, c wait.Clients, want int) error {
	list, err := c.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("health: list nodes: %w", err)
	}
	problems := evalNodes(list.Items, want)
	if len(problems) > 0 {
		return fmt.Errorf("health: nodes not healthy:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// evalNodes is the pure node check: it returns one message per problem
// found in nodes, given the expected count want, and nil when healthy.
func evalNodes(nodes []corev1.Node, want int) []string {
	var out []string
	if len(nodes) != want {
		out = append(out, fmt.Sprintf("want %d nodes, found %d", want, len(nodes)))
	}
	for i := range nodes {
		out = append(out, evalNode(&nodes[i])...)
	}
	return out
}

// evalNode returns the problems of the single node n, nil when healthy.
func evalNode(n *corev1.Node) []string {
	var out []string
	seen := map[corev1.NodeConditionType]corev1.ConditionStatus{}
	for _, cond := range n.Status.Conditions {
		seen[cond.Type] = cond.Status
	}
	if seen[corev1.NodeReady] != corev1.ConditionTrue {
		st, ok := seen[corev1.NodeReady]
		if !ok {
			st = "missing"
		}
		out = append(out, fmt.Sprintf("node %s: Ready=%s", n.Name, st))
	}
	for _, t := range []corev1.NodeConditionType{corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure} {
		if st := seen[t]; st == corev1.ConditionTrue || st == corev1.ConditionUnknown {
			out = append(out, fmt.Sprintf("node %s: %s=%s", n.Name, t, st))
		}
	}
	if seen[corev1.NodeNetworkUnavailable] == corev1.ConditionTrue {
		out = append(out, fmt.Sprintf("node %s: NetworkUnavailable=True", n.Name))
	}
	return out
}

// PodProxyGet GETs path on the given port of pod in namespace through the
// API server's pod proxy, over plain HTTP (the scheme is "http", so a TLS
// port is not reachable this way). The call uses c and ctx. It returns the response body, or an
// error wrapping the failure, including any non-2xx status.
func PodProxyGet(ctx context.Context, c wait.Clients, namespace, pod string, port int, path string) ([]byte, error) {
	body, err := c.Kube.CoreV1().Pods(namespace).ProxyGet("http", pod, strconv.Itoa(port), path, nil).DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("health: proxy get %s/%s:%d%s: %w", namespace, pod, port, path, err)
	}
	return body, nil
}
