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
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/env"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/providers"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// Stage 4's waits.
const (
	// componentWait bounds each readiness wait of stage 4.
	componentWait = 2 * time.Minute
	// certificateWait bounds the throwaway Certificate's issuance.
	certificateWait = 2 * time.Minute
	// cleanupWait bounds the removal of a throwaway object.
	cleanupWait = 2 * time.Minute
)

// certManagerNamespace is cert-manager's namespace.
const certManagerNamespace = "cert-manager"

// certManagerDeployments are cert-manager's three Deployments.
var certManagerDeployments = []string{"cert-manager", "cert-manager-cainjector", "cert-manager-webhook"}

// capiProvider is one CAPI provider as clusterctl installs it.
type capiProvider struct {
	// namespace is the provider's namespace.
	namespace string
	// deployment is its controller Deployment.
	deployment string
	// label is the clusterctl provider label, cluster.x-k8s.io/provider,
	// on its CRDs; it is also its inventory Provider object's name.
	label string
	// keyCRD is one CRD the provider must own.
	keyCRD string
}

// capiProviders are the CAPI core and kubeadm providers.
var capiProviders = []capiProvider{
	{"capi-system", "capi-controller-manager", "cluster-api", "clusters.cluster.x-k8s.io"},
	{"capi-kubeadm-bootstrap-system", "capi-kubeadm-bootstrap-controller-manager", "bootstrap-kubeadm", "kubeadmconfigs.bootstrap.cluster.x-k8s.io"},
	{"capi-kubeadm-control-plane-system", "capi-kubeadm-control-plane-controller-manager", "control-plane-kubeadm", "kubeadmcontrolplanes.controlplane.cluster.x-k8s.io"},
}

// providerLabel is the label clusterctl puts on every provider object.
const providerLabel = "cluster.x-k8s.io/provider"

// captfComponents is stage 4. Each component is a subtest, so every
// failing one is reported: cert-manager (and a throwaway self-signed
// Certificate), the CAPI providers (Deployments, pods, CRDs, webhooks),
// the clusterctl inventory, and CAPTF (see captfHealthy).
// It runs under ctx and fails t on any problem.
func (s *suite) captfComponents(ctx context.Context, t *testing.T) {
	t.Run("cert-manager", func(t *testing.T) { s.certManagerHealthy(ctx, t) })
	t.Run("cluster-api", func(t *testing.T) { s.capiHealthy(ctx, t) })
	t.Run("clusterctl-inventory", func(t *testing.T) { s.inventoryMatches(ctx, t) })
	t.Run("captf", func(t *testing.T) { s.captfHealthy(ctx, t) })
}

// certManagerHealthy checks cert-manager's Deployments and pods, then
// that it works: a self-signed Issuer and a Certificate in a throwaway
// namespace become Ready, and are deleted afterwards.
// It runs under ctx and fails t on any problem.
func (s *suite) certManagerHealthy(ctx context.Context, t *testing.T) {
	hint := s.kubectl("-n cert-manager get deploy,pods")
	if err := eventually(ctx, t, "cert-manager Deployments ready", componentWait, func(ctx context.Context) error {
		return requireDeployments(ctx, s.c, certManagerNamespace, certManagerDeployments, hint)
	}); err != nil {
		t.Fatal(err)
	}
	if err := eventually(ctx, t, "cert-manager pods healthy", componentWait, func(ctx context.Context) error {
		return podsHealthy(ctx, s.c, []string{certManagerNamespace}, hint)
	}); err != nil {
		t.Fatal(err)
	}
	s.selfSignedCertificate(ctx, t)
}

// selfSignedCertificate creates, in a throwaway namespace, a self-signed
// Issuer and a Certificate from it, and fails t unless both become Ready
// within certificateWait. The namespace (and so both objects) is deleted
// in t.Cleanup, which waits until it is gone.
// It runs under ctx.
func (s *suite) selfSignedCertificate(ctx context.Context, t *testing.T) {
	ns := randomName()
	if _, err := s.c.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create throwaway namespace %s: %v", ns, err)
	}
	t.Cleanup(func() { s.deleteNamespace(ctx, t, ns) })

	issuer := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Issuer",
		"metadata":   map[string]any{"name": ns, "namespace": ns},
		"spec":       map[string]any{"selfSigned": map[string]any{}},
	}}
	cert := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata":   map[string]any{"name": ns, "namespace": ns},
		"spec": map[string]any{
			"secretName": ns + "-tls",
			"commonName": ns + ".e2e.invalid",
			"dnsNames":   []any{ns + ".e2e.invalid"},
			"issuerRef":  map[string]any{"name": ns, "kind": "Issuer"},
		},
	}}
	// Creates go through cert-manager's webhook; retry briefly in case it
	// is still warming up its serving certificate.
	for _, obj := range []*unstructured.Unstructured{issuer, cert} {
		gvr := issuerGVR
		if obj.GetKind() == "Certificate" {
			gvr = certificateGVR
		}
		if err := eventually(ctx, t, "create "+obj.GetKind()+" "+ns+"/"+ns, componentWait, func(ctx context.Context) error {
			_, err := s.c.Dynamic.Resource(gvr).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
			return err
		}); err != nil {
			t.Fatalf("%v; inspect: %s", err, s.kubectl("-n cert-manager logs deploy/cert-manager-webhook"))
		}
	}
	if err := eventually(ctx, t, "self-signed Certificate "+ns+"/"+ns+" Ready", certificateWait, func(ctx context.Context) error {
		if err := conditionTrue(ctx, s.c.Dynamic, issuerGVR, ns, ns, "Ready", ""); err != nil {
			return err
		}
		return conditionTrue(ctx, s.c.Dynamic, certificateGVR, ns, ns, "Ready", "")
	}); err != nil {
		t.Fatalf("expected cert-manager to issue a self-signed Certificate: %v; inspect: %s", err, s.kubectl("-n "+ns+" describe issuer,certificate,certificaterequest; kubectl -n cert-manager logs deploy/cert-manager"))
	}
	if _, err := s.c.Kube.CoreV1().Secrets(ns).Get(ctx, ns+"-tls", metav1.GetOptions{}); err != nil {
		t.Errorf("expected the issued Secret %s/%s-tls: %v", ns, ns, err)
	}
}

// deleteNamespace deletes the throwaway namespace ns and waits until it
// is gone, reporting a failure through t. It runs on a context detached
// from ctx, so it also cleans up after a timeout.
func (s *suite) deleteNamespace(ctx context.Context, t *testing.T, ns string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWait)
	defer cancel()
	if err := ignoreNotFound(s.c.Kube.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})); err != nil {
		t.Errorf("cleanup: delete namespace %s: %v", ns, err)
		return
	}
	if err := eventually(ctx, t, "throwaway namespace "+ns+" deleted", cleanupWait, func(ctx context.Context) error {
		_, err := s.c.Kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if err == nil {
			return fmt.Errorf("namespace %s still exists", ns)
		}
		return ignoreNotFound(err)
	}); err != nil {
		t.Errorf("cleanup: %v; inspect: %s", err, s.kubectl("get ns "+ns+" -o yaml"))
	}
}

// capiHealthy checks each CAPI provider: its Deployment and pods, its
// CRDs Established (found by clusterctl's provider label, including its
// key CRD), and its admission webhooks (caBundle injected, Service
// endpoints ready).
// It runs under ctx and fails t on any problem.
func (s *suite) capiHealthy(ctx context.Context, t *testing.T) {
	for _, p := range capiProviders {
		hint := s.kubectl("-n " + p.namespace + " get deploy,pods")
		if err := eventually(ctx, t, p.deployment+" ready", componentWait, func(ctx context.Context) error {
			if err := requireDeployments(ctx, s.c, p.namespace, []string{p.deployment}, hint); err != nil {
				return err
			}
			return podsHealthy(ctx, s.c, []string{p.namespace}, hint)
		}); err != nil {
			t.Error(err)
		}
		crds, err := s.providerCRDs(ctx, p.label)
		switch {
		case err != nil:
			t.Error(err)
		case !slices.Contains(crds, p.keyCRD):
			t.Errorf("expected CRD %s among the %s=%s CRDs, observed %v; inspect: %s", p.keyCRD, providerLabel, p.label, crds, s.kubectl("get crd -l "+providerLabel+"="+p.label))
		default:
			if err := wait.CRDsEstablished(ctx, s.c, crds, waitOpts(t, componentWait)); err != nil {
				t.Errorf("%v; inspect: %s", err, s.kubectl("get crd -l "+providerLabel+"="+p.label))
			}
		}
		if err := eventually(ctx, t, p.namespace+" webhooks wired", componentWait, func(ctx context.Context) error {
			configs, err := webhooksWired(ctx, s.c, p.namespace)
			if err == nil {
				t.Logf("%s: webhooks %v carry a caBundle and their Services have ready endpoints", p.namespace, configs)
			}
			return err
		}); err != nil {
			t.Error(err)
		}
	}
}

// providerCRDs returns the names of the CRDs labeled
// cluster.x-k8s.io/provider=label, sorted, read under ctx; an error when
// listing fails or there are none.
func (s *suite) providerCRDs(ctx context.Context, label string) ([]string, error) {
	list, err := s.c.Dynamic.Resource(crdGVR).List(ctx, metav1.ListOptions{LabelSelector: providerLabel + "=" + label})
	if err != nil {
		return nil, fmt.Errorf("list CRDs labeled %s=%s: %w", providerLabel, label, err)
	}
	var names []string
	for _, u := range list.Items {
		names = append(names, u.GetName())
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("expected CRDs labeled %s=%s, observed none; inspect: %s", providerLabel, label, s.kubectl("get crd --show-labels"))
	}
	return names, nil
}

// inventoryProvider is one expected clusterctl inventory entry.
type inventoryProvider struct {
	// key is "namespace/name" of the Provider object.
	key string
	// version is the expected spec version.
	version string
}

// inventoryMatches checks clusterctl's inventory: exactly the Provider
// objects of the CAPI core, kubeadm bootstrap, kubeadm control plane and
// CAPTF, each at its pinned version.
// It runs under ctx and fails t on any problem.
func (s *suite) inventoryMatches(ctx context.Context, t *testing.T) {
	want := []inventoryProvider{
		{"capi-system/cluster-api", framework.CAPIVersion},
		{"capi-kubeadm-bootstrap-system/bootstrap-kubeadm", framework.CAPIVersion},
		{"capi-kubeadm-control-plane-system/control-plane-kubeadm", framework.CAPIVersion},
		{env.ManagerNamespace + "/infrastructure-terraform", providers.CAPTFVersion},
	}
	list, err := s.c.Dynamic.Resource(providerGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list clusterctl inventory (%s): %v", providerGVR, err)
	}
	have := map[string]string{}
	for _, u := range list.Items {
		v, _, _ := unstructured.NestedString(u.Object, "version")
		have[u.GetNamespace()+"/"+u.GetName()] = v
	}
	var problems []string
	for _, w := range want {
		got, ok := have[w.key]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("Provider %s: expected at version %s, observed missing", w.key, w.version))
		case got != w.version:
			problems = append(problems, fmt.Sprintf("Provider %s: expected version %s, observed %s", w.key, w.version, got))
		}
		delete(have, w.key)
	}
	for k, v := range have {
		problems = append(problems, fmt.Sprintf("Provider %s (version %s): not expected", k, v))
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("clusterctl inventory does not match the pins:\n  %s\ninspect: %s", strings.Join(problems, "\n  "), s.kubectl("get providers.clusterctl.cluster.x-k8s.io -A"))
	}
}
