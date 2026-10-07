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
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/env"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/health"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// The CAPTF objects config/default renders, besides env.ManagerNamespace
// and env.ManagerDeployment.
const (
	// managerSelector selects the manager pods.
	managerSelector = "control-plane=controller-manager"
	// managerContainer is the manager container.
	managerContainer = "manager"
	// healthPort is the manager's health-probe port (/healthz, /readyz).
	healthPort = 9440
	// leaderLease is the manager's leader-election Lease.
	leaderLease = "controller-leader-election-captf"
	// leaseMaxAge is how old the Lease's renewTime may be; the manager
	// renews about every 2 seconds and its lease lasts 15.
	leaseMaxAge = 30 * time.Second
	// servingCertificate is the webhook's cert-manager Certificate.
	servingCertificate = "captf-serving-cert"
	// servingSecret is the Secret that Certificate writes.
	servingSecret = "captf-webhook-service-cert"
	// webhookConfiguration is CAPTF's ValidatingWebhookConfiguration.
	webhookConfiguration = "captf-validating-webhook-configuration"
	// webhookService is the webhook's Service.
	webhookService = "captf-webhook-service"
	// webhookProbeNamespace is where wait.WebhookServing's dry-run goes.
	webhookProbeNamespace = "default"
)

// captfHealthy checks the CAPTF manager end to end: the Deployment is
// Available with spec.replicas Ready pods running the suite's manager
// image; the CRDs are Established; the serving Certificate is
// Ready and its CA is what the webhook configuration carries; the webhook
// Service has endpoints and the webhook rejects an invalid object; the
// health endpoints answer through the pod proxy on every pod; the leader
// Lease is held by one of the pods; and the manager log has no error, fatal or panic line.
// It runs under ctx and fails t on any problem.
func (s *suite) captfHealthy(ctx context.Context, t *testing.T) {
	ns := env.ManagerNamespace
	hint := s.kubectl("-n " + ns + " describe deploy " + env.ManagerDeployment + "; kubectl -n " + ns + " logs deploy/" + env.ManagerDeployment)
	if err := eventually(ctx, t, "CAPTF manager ready", componentWait, func(ctx context.Context) error {
		if err := requireDeployments(ctx, s.c, ns, []string{env.ManagerDeployment}, hint); err != nil {
			return err
		}
		return podsHealthy(ctx, s.c, []string{ns}, hint)
	}); err != nil {
		t.Fatal(err)
	}
	s.checkManagerPod(ctx, t, hint)

	if err := wait.CRDsEstablished(ctx, s.c, env.CAPTFCRDs, waitOpts(t, componentWait)); err != nil {
		t.Errorf("%v; inspect: %s", err, s.kubectl("get crd | grep terraform"))
	}
	if err := eventually(ctx, t, "Certificate "+servingCertificate+" Ready", componentWait, func(ctx context.Context) error {
		return conditionTrue(ctx, s.c.Dynamic, certificateGVR, ns, servingCertificate, "Ready", "")
	}); err != nil {
		t.Errorf("%v; inspect: %s", err, s.kubectl("-n "+ns+" describe certificate "+servingCertificate))
	}
	if err := eventually(ctx, t, "webhook caBundle matches the serving CA", componentWait, s.caBundleMatches); err != nil {
		t.Error(err)
	}
	if err := eventually(ctx, t, "webhook Service "+webhookService+" endpoints ready", componentWait, func(ctx context.Context) error {
		return serviceReady(ctx, s.c, ns, webhookService)
	}); err != nil {
		t.Error(err)
	}
	if err := wait.WebhookServing(ctx, s.c, webhookProbeNamespace, waitOpts(t, componentWait)); err != nil {
		t.Errorf("expected the CAPTF webhook to reject an invalid TerraformCluster: %v; inspect: %s", err, hint)
	}
	s.checkHealthEndpoints(ctx, t)
	if err := eventually(ctx, t, "leader Lease held by a manager pod", componentWait, func(ctx context.Context) error {
		leader, err := s.leaderPod(ctx)
		if err != nil {
			return err
		}
		return health.LeaseHeld(ctx, s.c, ns, leaderLease, leader+"_", leaseMaxAge)
	}); err != nil {
		t.Errorf("%v; inspect: %s", err, s.kubectl("-n "+ns+" get lease "+leaderLease+" -o yaml"))
	}
	if err := health.ScanLogs(ctx, s.c, ns, managerSelector, captfLogRules()); err != nil {
		t.Errorf("expected no error, fatal or panic line in the CAPTF manager log: %v; inspect: %s", err, s.kubectl("-n "+ns+" logs -l "+managerSelector+" --all-containers"))
	}
}

// checkManagerPod fails t unless the number of manager pods equals the
// Deployment's spec.replicas and status.readyReplicas, and every pod is
// Ready with a manager container (and the Deployment's template) running
// the suite's manager image. It records every pod's name in
// s.managerPods and the one that holds the leader Lease in s.managerPod;
// hint is the inspect command. It reads under ctx.
func (s *suite) checkManagerPod(ctx context.Context, t *testing.T, hint string) {
	t.Helper()
	ns := env.ManagerNamespace
	d, err := s.c.Kube.AppsV1().Deployments(ns).Get(ctx, env.ManagerDeployment, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Deployment %s/%s: %v", ns, env.ManagerDeployment, err)
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	if got := containerImage(d.Spec.Template.Spec.Containers, managerContainer); got != s.managerRef {
		t.Errorf("Deployment %s: expected template image %s, observed %q", env.ManagerDeployment, s.managerRef, got)
	}
	if d.Status.ReadyReplicas != want {
		t.Errorf("Deployment %s: expected %d ready replicas (spec.replicas), observed %d; inspect: %s", env.ManagerDeployment, want, d.Status.ReadyReplicas, hint)
	}
	pods, err := podsBySelector(ctx, s.c, ns, managerSelector)
	if err != nil {
		t.Fatal(err)
	}
	if int32(len(pods)) != want {
		t.Fatalf("expected %d manager pods (%s, the Deployment's spec.replicas), observed %d: %s; inspect: %s", want, managerSelector, len(pods), podNames(pods), hint)
	}
	s.managerPods = s.managerPods[:0]
	for i := range pods {
		p := &pods[i]
		s.managerPods = append(s.managerPods, p.Name)
		if !podReady(p) {
			t.Errorf("manager pod %s: expected Running and Ready, observed %s; inspect: %s", p.Name, p.Status.Phase, hint)
		}
		if got := containerImage(p.Spec.Containers, managerContainer); got != s.managerRef {
			t.Errorf("manager pod %s: expected image %s (built from this tree), observed %q", p.Name, s.managerRef, got)
		}
	}
	if err := eventually(ctx, t, "a leader among the manager pods", componentWait, func(ctx context.Context) error {
		leader, err := s.leaderPod(ctx)
		s.managerPod = leader
		return err
	}); err != nil {
		t.Errorf("%v; inspect: %s", err, s.kubectl("-n "+ns+" get lease "+leaderLease+" -o yaml"))
	}
	t.Logf("%d manager pods %v run %s; leader %s", len(pods), s.managerPods, s.managerRef, s.managerPod)
}

// podNames returns the names of pods, for messages.
func podNames(pods []corev1.Pod) []string {
	names := make([]string, 0, len(pods))
	for i := range pods {
		names = append(names, pods[i].Name)
	}
	return names
}

// leaderPod returns the name of the manager pod that the leader Lease
// names (its holderIdentity is "<pod name>_<uuid>"), read under ctx, or an
// error when the Lease is missing, unheld or names none of s.managerPods.
func (s *suite) leaderPod(ctx context.Context) (string, error) {
	l, err := s.c.Kube.CoordinationV1().Leases(env.ManagerNamespace).Get(ctx, leaderLease, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get Lease %s/%s: %w", env.ManagerNamespace, leaderLease, err)
	}
	holder := ""
	if l.Spec.HolderIdentity != nil {
		holder = *l.Spec.HolderIdentity
	}
	for _, name := range s.managerPods {
		if strings.HasPrefix(holder, name+"_") {
			return name, nil
		}
	}
	return "", fmt.Errorf("expected the leader Lease %s/%s to be held by one of the manager pods %v, observed holderIdentity %q", env.ManagerNamespace, leaderLease, s.managerPods, holder)
}

// containerImage returns the image of the container name among cs, or ""
// when there is none.
func containerImage(cs []corev1.Container, name string) string {
	for _, c := range cs {
		if c.Name == name {
			return c.Image
		}
	}
	return ""
}

// caBundleMatches returns nil when every webhook of CAPTF's
// ValidatingWebhookConfiguration carries exactly the ca.crt of the
// serving Secret, read under ctx; else an error saying what differs.
func (s *suite) caBundleMatches(ctx context.Context) error {
	ns := env.ManagerNamespace
	sec, err := s.c.Kube.CoreV1().Secrets(ns).Get(ctx, servingSecret, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("expected serving Secret %s/%s: %w", ns, servingSecret, err)
	}
	ca := sec.Data["ca.crt"]
	if len(ca) == 0 {
		return fmt.Errorf("expected ca.crt in Secret %s/%s, observed none", ns, servingSecret)
	}
	vwc, err := s.c.Kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, webhookConfiguration, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("expected ValidatingWebhookConfiguration %s: %w", webhookConfiguration, err)
	}
	if len(vwc.Webhooks) == 0 {
		return fmt.Errorf("expected webhooks in %s, observed none", webhookConfiguration)
	}
	var bad []string
	for _, w := range vwc.Webhooks {
		if !bytes.Equal(w.ClientConfig.CABundle, ca) {
			bad = append(bad, fmt.Sprintf("%s (caBundle %d bytes)", w.Name, len(w.ClientConfig.CABundle)))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("expected every webhook of %s to carry the ca.crt of %s/%s (%d bytes, injected by cainjector from Certificate %s), observed a different caBundle on: %s; inspect: %s",
			webhookConfiguration, ns, servingSecret, len(ca), servingCertificate, strings.Join(bad, ", "), s.kubectl("-n cert-manager logs deploy/cert-manager-cainjector"))
	}
	return nil
}

// checkHealthEndpoints fails t unless every manager pod, the standby as
// well as the leader (the standby serves the webhooks), answers /healthz
// and /readyz on the health port with "ok", through the API server's pod
// proxy under ctx.
func (s *suite) checkHealthEndpoints(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, pod := range s.managerPods {
		for _, path := range []string{"/healthz", "/readyz"} {
			err := eventually(ctx, t, "manager "+pod+" "+path, componentWait, func(ctx context.Context) error {
				body, err := health.PodProxyGet(ctx, s.c, env.ManagerNamespace, pod, healthPort, path)
				if err != nil {
					return err
				}
				if strings.TrimSpace(string(body)) != "ok" {
					return fmt.Errorf("expected body \"ok\", observed %q", body)
				}
				return nil
			})
			if err != nil {
				t.Errorf("%v; inspect: %s", err, s.kubectl(fmt.Sprintf("get --raw /api/v1/namespaces/%s/pods/%s:%d/proxy%s", env.ManagerNamespace, pod, healthPort, path)))
			}
		}
	}
}
