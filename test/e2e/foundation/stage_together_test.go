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
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/env"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/health"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// Stage 5's waits.
const (
	// identityWait bounds the identity reaching Ready and being deleted.
	identityWait = 2 * time.Minute
	// liveCheckEvery is the period of the Lease and API server checks
	// during the stability window.
	liveCheckEvery = 15 * time.Second
	// leaseAdvanceWithin is how long the Lease's renewTime may take to
	// advance; the manager renews about every 2 seconds.
	leaseAdvanceWithin = 10 * time.Second
	// stabilitySlack is the stability window's grace on top of its length.
	stabilitySlack = 2 * time.Minute
)

// logNamespaces are the components whose logs fail the suite only on
// panic or fatal lines.
var logNamespaces = []string{
	"cert-manager",
	"capi-system",
	"capi-kubeadm-bootstrap-system",
	"capi-kubeadm-control-plane-system",
	"kube-system",
}

// together is stage 5: a real reconcile across the CRD, the webhook, the
// manager, RBAC and the status subresource; a stability window over the
// whole cluster; and a log scan of every component. The first failing
// part stops the rest: a log scan of a cluster that just failed its
// stability window only repeats the failure.
// It runs under ctx and fails t on any problem.
func (s *suite) together(ctx context.Context, t *testing.T) {
	if !t.Run("identity-reconcile", func(t *testing.T) { s.identityReconciles(ctx, t) }) {
		return
	}
	if !t.Run("stability", func(t *testing.T) { s.stability(ctx, t) }) {
		return
	}
	t.Run("logs", func(t *testing.T) { s.logsClean(ctx, t) })
}

// identityReconciles creates a throwaway credentials Secret and a
// TerraformClusterIdentity that references it, waits for the identity's
// Ready=True with reason SecretFound, then deletes both and waits until
// they are gone. The Secret comes first: the identity controller does not
// watch Secrets and only re-reads them every 5 minutes. t.Cleanup deletes
// whatever is left.
// It runs under ctx and fails t on any problem.
func (s *suite) identityReconciles(ctx context.Context, t *testing.T) {
	ns, name := env.ManagerNamespace, randomName()
	t.Cleanup(func() { s.deleteIdentity(ctx, t, name) })

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		StringData: map[string]string{"token": "e2e-placeholder"},
	}
	if _, err := s.c.Kube.CoreV1().Secrets(ns).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create throwaway Secret %s/%s: %v", ns, name, err)
	}
	identity := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
		"kind":       "TerraformClusterIdentity",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"secretRef": map[string]any{"name": name, "namespace": ns}},
	}}
	if _, err := s.c.Dynamic.Resource(identityGVR).Create(ctx, identity, metav1.CreateOptions{}); err != nil {
		t.Fatalf("expected the CAPTF webhook (and its SubjectAccessReview) to admit TerraformClusterIdentity %s: %v; inspect: %s", name, err, s.kubectl("-n "+ns+" logs -l "+managerSelector))
	}
	if err := eventually(ctx, t, "TerraformClusterIdentity "+name+" Ready", identityWait, func(ctx context.Context) error {
		return conditionTrue(ctx, s.c.Dynamic, identityGVR, "", name, "Ready", "SecretFound")
	}); err != nil {
		t.Fatalf("expected the manager to reconcile the identity: %v; inspect: %s", err, s.kubectl("describe terraformclusteridentity "+name+"; kubectl -n "+ns+" logs -l "+managerSelector))
	}

	if err := s.c.Dynamic.Resource(identityGVR).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("expected the webhook to allow deleting the unused identity %s: %v", name, err)
	}
	if err := eventually(ctx, t, "TerraformClusterIdentity "+name+" gone", identityWait, func(ctx context.Context) error {
		return gone(ctx, s.c.Dynamic, identityGVR, "", name)
	}); err != nil {
		t.Fatalf("%v; inspect: %s", err, s.kubectl("get terraformclusteridentity "+name+" -o yaml"))
	}
	if err := s.c.Kube.CoreV1().Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete throwaway Secret %s/%s: %v", ns, name, err)
	}
	if err := eventually(ctx, t, "Secret "+ns+"/"+name+" gone", identityWait, func(ctx context.Context) error {
		_, err := s.c.Kube.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return fmt.Errorf("secret %s/%s still exists", ns, name)
		}
		return ignoreNotFound(err)
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("identity %s reached Ready=True (SecretFound) and was deleted with its Secret", name)
}

// deleteIdentity removes the throwaway identity and Secret name if they
// are still there, on a context detached from ctx, reporting failures
// through t.
func (s *suite) deleteIdentity(ctx context.Context, t *testing.T, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWait)
	defer cancel()
	if err := ignoreNotFound(s.c.Dynamic.Resource(identityGVR).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
		t.Errorf("cleanup: delete TerraformClusterIdentity %s: %v", name, err)
	}
	if err := ignoreNotFound(s.c.Kube.CoreV1().Secrets(env.ManagerNamespace).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
		t.Errorf("cleanup: delete Secret %s/%s: %v", env.ManagerNamespace, name, err)
	}
}

// stability watches the whole cluster for the configured window: no pod
// restarts, is recreated, deleted, added or goes not-ready
// (health.Stable over every namespace), and every 15 seconds the CAPTF
// leader Lease is still held by the manager pod and renewing and the API
// server is ready. Afterwards no Warning event may have appeared in the
// provider namespaces beyond warningEventAllow.
// It runs under ctx and fails t on any problem.
func (s *suite) stability(ctx context.Context, t *testing.T) {
	window := s.opts.stability
	since := time.Now()
	ctx, cancel := context.WithTimeout(ctx, window+stabilitySlack)
	defer cancel()
	t.Logf("stability: watching every namespace for %s; the CAPTF leader Lease and the API server are checked every %s", window, liveCheckEvery)

	done := make(chan error, 1)
	go func() { done <- health.Stable(ctx, s.c, nil, window, stableInterval, logWriter{t: t}) }()
	var problems []string
	tick := time.NewTicker(liveCheckEvery)
	defer tick.Stop()
	var stableErr error
watch:
	for {
		select {
		case stableErr = <-done:
			break watch
		case <-tick.C:
			if err := s.liveChecks(ctx); err != nil {
				problems = append(problems, err.Error())
				cancel()
				stableErr = <-done
				if errors.Is(stableErr, context.Canceled) {
					stableErr = nil
				}
				break watch
			}
		}
	}
	if stableErr != nil {
		problems = append(problems, stableErr.Error())
	}
	if len(problems) > 0 {
		t.Fatalf("expected the cluster to hold still for %s (no pod restarted, recreated, deleted, added or not ready; the leader Lease held by %s and renewing; the API server ready), observed after %s:\n  %s\ninspect: %s",
			window, s.managerPod, time.Since(since).Round(time.Second), strings.Join(problems, "\n  "),
			s.kubectl("get pods -A -o wide; kubectl get events -A --field-selector type=Warning; kubectl -n "+env.ManagerNamespace+" get lease "+leaderLease+" -o yaml"))
	}
	if err := health.WarningEvents(ctx, s.c, wait.DefaultProviderNamespaces, since, compile(warningEventAllow)); err != nil {
		t.Errorf("expected no new Warning events in %v during the window: %v; inspect: %s", wait.DefaultProviderNamespaces, err, s.kubectl("get events -A --field-selector type=Warning"))
	}
}

// liveChecks returns nil when, read under ctx, the leader Lease is held
// by the manager pod with a recent renewTime that advances, and the API
// server is ready and live; else the first failure.
func (s *suite) liveChecks(ctx context.Context) error {
	ns := env.ManagerNamespace
	if err := health.LeaseHeld(ctx, s.c, ns, leaderLease, s.managerPod, leaseMaxAge); err != nil {
		return err
	}
	if err := health.LeaseRenewing(ctx, s.c, ns, leaderLease, leaseAdvanceWithin); err != nil {
		return err
	}
	return health.APIServer(ctx, s.c)
}

// logsClean scans the logs: CAPTF's manager strictly (captfLogRules);
// cert-manager, CAPI and kube-system for panic and fatal lines only
// (componentLogRules), with their other E lines, minus benignErrorLines,
// reported without failing.
// It runs under ctx and fails t on any problem.
func (s *suite) logsClean(ctx context.Context, t *testing.T) {
	if err := health.ScanLogs(ctx, s.c, env.ManagerNamespace, managerSelector, captfLogRules()); err != nil {
		t.Errorf("expected no error, fatal or panic line in the CAPTF manager log: %v; inspect: %s", err, s.kubectl("-n "+env.ManagerNamespace+" logs -l "+managerSelector))
	}
	for _, ns := range logNamespaces {
		if err := health.ScanLogs(ctx, s.c, ns, "", componentLogRules()); err != nil {
			t.Errorf("expected no panic or fatal line in the logs of %s: %v; inspect: %s", ns, err, s.kubectl("-n "+ns+" logs <pod> --all-containers"))
		}
		if err := health.ScanLogs(ctx, s.c, ns, "", errorLineRules()); err != nil {
			t.Logf("report only, not failing: E-level lines in %s outside the allowlist: %v", ns, err)
		}
	}
}
