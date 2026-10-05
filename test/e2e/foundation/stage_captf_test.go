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
	// managerSelector selects the manager pod.
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
// Available with one Ready pod with 0 restarts running the suite's
// manager image; the CRDs are Established; the serving Certificate is
// Ready and its CA is what the webhook configuration carries; the webhook
// Service has endpoints and the webhook rejects an invalid object; the
// health endpoints answer through the pod proxy; the leader Lease is held
// by that pod; and the manager log has no error, fatal or panic line.
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
	if err := eventually(ctx, t, "leader Lease held by "+s.managerPod, componentWait, func(ctx context.Context) error {
		return health.LeaseHeld(ctx, s.c, ns, leaderLease, s.managerPod, leaseMaxAge)
	}); err != nil {
		t.Errorf("%v; inspect: %s", err, s.kubectl("-n "+ns+" get lease "+leaderLease+" -o yaml"))
	}
	if err := health.ScanLogs(ctx, s.c, ns, managerSelector, captfLogRules()); err != nil {
		t.Errorf("expected no error, fatal or panic line in the CAPTF manager log: %v; inspect: %s", err, s.kubectl("-n "+ns+" logs -l "+managerSelector+" --all-containers"))
	}
}

// checkManagerPod records the manager pod and fails t unless there is
// exactly one, Ready, whose manager container (and the Deployment's
// template) runs the suite's manager image; hint is the inspect command.
// It reads under ctx.
func (s *suite) checkManagerPod(ctx context.Context, t *testing.T, hint string) {
	t.Helper()
	ns := env.ManagerNamespace
	pods, err := podsBySelector(ctx, s.c, ns, managerSelector)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 {
		t.Fatalf("expected exactly 1 manager pod (%s), observed %d; inspect: %s", managerSelector, len(pods), hint)
	}
	p := &pods[0]
	s.managerPod = p.Name
	if !podReady(p) {
		t.Errorf("manager pod %s: expected Running and Ready, observed %s; inspect: %s", p.Name, p.Status.Phase, hint)
	}
	if got := containerImage(p.Spec.Containers, managerContainer); got != s.managerRef {
		t.Errorf("manager pod %s: expected image %s (built from this tree), observed %q", p.Name, s.managerRef, got)
	}
	d, err := s.c.Kube.AppsV1().Deployments(ns).Get(ctx, env.ManagerDeployment, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Deployment %s/%s: %v", ns, env.ManagerDeployment, err)
	}
	if got := containerImage(d.Spec.Template.Spec.Containers, managerContainer); got != s.managerRef {
		t.Errorf("Deployment %s: expected template image %s, observed %q", env.ManagerDeployment, s.managerRef, got)
	}
	t.Logf("manager pod %s runs %s", p.Name, s.managerRef)
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

// checkHealthEndpoints fails t unless the manager pod answers /healthz
// and /readyz on the health port with "ok", through the API server's pod
// proxy under ctx.
func (s *suite) checkHealthEndpoints(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, path := range []string{"/healthz", "/readyz"} {
		err := eventually(ctx, t, "manager "+path, componentWait, func(ctx context.Context) error {
			body, err := health.PodProxyGet(ctx, s.c, env.ManagerNamespace, s.managerPod, healthPort, path)
			if err != nil {
				return err
			}
			if strings.TrimSpace(string(body)) != "ok" {
				return fmt.Errorf("expected body \"ok\", observed %q", body)
			}
			return nil
		})
		if err != nil {
			t.Errorf("%v; inspect: %s", err, s.kubectl(fmt.Sprintf("get --raw /api/v1/namespaces/%s/pods/%s:%d/proxy%s", env.ManagerNamespace, s.managerPod, healthPort, path)))
		}
	}
}
