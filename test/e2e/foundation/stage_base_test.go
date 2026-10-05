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
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/health"
)

// Stage 2's namespaces and waits.
const (
	// baseWait bounds each readiness wait of stage 2.
	baseWait = 3 * time.Minute
	// baseStableWindow is how long the base components must hold still.
	baseStableWindow = 30 * time.Second
	// stableInterval is the sampling interval of every stability check.
	stableInterval = 5 * time.Second
)

// baseNamespaces are kind's own namespaces.
var baseNamespaces = []string{"kube-system", "local-path-storage"}

// staticPods are the control-plane static pods' component labels.
var staticPods = []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler"}

// baseComponents is stage 2: the API server is ready and live, the nodes
// are Ready without pressure, the control-plane static pods run, CoreDNS,
// kindnet, kube-proxy and local-path-provisioner are ready, no pod in
// kind's namespaces has a problem, and they hold still for 30 seconds.
// It runs under ctx and fails t on any problem.
func (s *suite) baseComponents(ctx context.Context, t *testing.T) {
	if err := eventually(ctx, t, "API server ready and live", baseWait, func(ctx context.Context) error {
		return health.APIServer(ctx, s.c)
	}); err != nil {
		t.Fatalf("%v; inspect: %s", err, s.kubectl("get --raw '/readyz?verbose'"))
	}
	if err := eventually(ctx, t, "nodes Ready", baseWait, func(ctx context.Context) error {
		return health.Nodes(ctx, s.c, s.nodeCount())
	}); err != nil {
		t.Fatalf("%v; inspect: %s", err, s.kubectl("describe nodes"))
	}
	if err := eventually(ctx, t, "control-plane static pods running", baseWait, s.staticPodsRunning); err != nil {
		t.Fatalf("%v; inspect: %s", err, s.kubectl("-n kube-system get pods -l tier=control-plane -o wide"))
	}
	if err := eventually(ctx, t, "kind's workloads ready", baseWait, func(ctx context.Context) error {
		return s.baseWorkloadsReady(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	if err := eventually(ctx, t, "kind's pods healthy", baseWait, func(ctx context.Context) error {
		return podsHealthy(ctx, s.c, baseNamespaces, s.kubectl("get pods -n kube-system -o wide; kubectl get pods -n local-path-storage"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := health.Stable(ctx, s.c, baseNamespaces, baseStableWindow, stableInterval, logWriter{t: t}); err != nil {
		t.Fatalf("expected kind's pods to hold still for %s: %v; inspect: %s", baseStableWindow, err, s.kubectl("get pods -A -o wide"))
	}
}

// staticPodsRunning returns nil when, for each staticPods component, one
// pod per control-plane node (one) runs Ready in kube-system, read under
// ctx; else an error listing what it saw.
func (s *suite) staticPodsRunning(ctx context.Context) error {
	var problems []string
	for _, comp := range staticPods {
		pods, err := podsBySelector(ctx, s.c, "kube-system", "component="+comp)
		if err != nil {
			return err
		}
		if len(pods) != 1 {
			problems = append(problems, fmt.Sprintf("%s: expected 1 static pod, observed %d", comp, len(pods)))
			continue
		}
		if p := &pods[0]; !podReady(p) {
			problems = append(problems, fmt.Sprintf("%s: pod %s is %s, not Running and Ready", comp, p.Name, p.Status.Phase))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("control-plane static pods: %s", strings.Join(problems, "; "))
	}
	return nil
}

// baseWorkloadsReady returns nil when the coredns Deployment, the kindnet
// and kube-proxy DaemonSets and the local-path-provisioner Deployment
// exist and every workload in kind's namespaces is ready, read under ctx.
func (s *suite) baseWorkloadsReady(ctx context.Context) error {
	if err := requireDeployments(ctx, s.c, "kube-system", []string{"coredns"}, s.kubectl("-n kube-system get deploy,ds")); err != nil {
		return err
	}
	if err := requireDeployments(ctx, s.c, "local-path-storage", []string{"local-path-provisioner"}, s.kubectl("-n local-path-storage get deploy,pods")); err != nil {
		return err
	}
	for _, ds := range []string{"kindnet", "kube-proxy"} {
		if _, err := s.c.Kube.AppsV1().DaemonSets("kube-system").Get(ctx, ds, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("expected DaemonSet kube-system/%s: %w; inspect: %s", ds, err, s.kubectl("-n kube-system get ds"))
		}
	}
	return nil
}
