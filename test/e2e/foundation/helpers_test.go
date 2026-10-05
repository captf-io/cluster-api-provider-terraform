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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/health"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// The resources the suite reads and writes through the dynamic client.
// They stay unstructured so the test module never imports cert-manager,
// cluster-api, clusterctl or CAPTF types.
var (
	// issuerGVR is cert-manager's namespaced Issuer.
	issuerGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "issuers"}
	// certificateGVR is cert-manager's Certificate.
	certificateGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
	// providerGVR is clusterctl's inventory Provider.
	providerGVR = schema.GroupVersionResource{Group: "clusterctl.cluster.x-k8s.io", Version: "v1alpha3", Resource: "providers"}
	// identityGVR is CAPTF's cluster-scoped TerraformClusterIdentity.
	identityGVR = schema.GroupVersionResource{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Resource: "terraformclusteridentities"}
	// crdGVR is the apiextensions CustomResourceDefinition.
	crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
)

// Polling cadence of eventually.
const (
	// pollInterval is the time between checks.
	pollInterval = 2 * time.Second
	// progressEvery is the longest gap between progress lines.
	progressEvery = 30 * time.Second
)

// permanentError marks a check failure that waiting cannot fix (a
// restarted container, say), so eventually stops at once.
type permanentError struct {
	// err is the failure.
	err error
}

// Error returns the wrapped error's message.
func (p permanentError) Error() string { return p.err.Error() }

// Unwrap returns the wrapped error.
func (p permanentError) Unwrap() error { return p.err }

// permanent wraps err so eventually gives up on it immediately. It
// returns nil for a nil err.
func permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// eventually runs check under ctx every pollInterval until it returns
// nil, returns a permanent error, or timeout passes, logging progress
// through t at least every 30 seconds. what names the condition in the
// log and the error. It returns nil once check passes, else an error
// carrying check's last failure.
func eventually(ctx context.Context, t testing.TB, what string, timeout time.Duration, check func(context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	lastLog := start
	var last error
	for {
		err := check(ctx)
		if err == nil {
			t.Logf("%s: ok after %s", what, time.Since(start).Round(time.Millisecond))
			return nil
		}
		if pe := (permanentError{}); errors.As(err, &pe) {
			return fmt.Errorf("%s: %w", what, pe.err)
		}
		if ctx.Err() == nil || last == nil {
			last = err
		}
		if time.Since(lastLog) >= progressEvery {
			t.Logf("%s: still waiting after %s: %v", what, time.Since(start).Round(time.Second), last)
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: not reached within %s; last observed: %w", what, timeout, last)
		case <-time.After(pollInterval):
		}
	}
}

// logWriter adapts a test's log to io.Writer, one Log call per write, for
// the framework helpers that print progress.
type logWriter struct {
	// t receives the lines.
	t testing.TB
}

// Write logs p without its trailing newline and returns len(p) and nil.
func (w logWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// waitOpts returns wait.Options with timeout that log through t.
func waitOpts(t testing.TB, timeout time.Duration) wait.Options {
	return wait.Options{Interval: pollInterval, ReportEvery: progressEvery, Timeout: timeout, Out: logWriter{t: t}}
}

// randomName returns a unique throwaway object name, "e2e-" followed by
// eight hex digits.
func randomName() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "e2e-" + hex.EncodeToString(b)
}

// condition returns the status, reason and message of the condition typ
// in u's status.conditions, and reports with found whether it exists.
func condition(u *unstructured.Unstructured, typ string) (status, reason, message string, found bool) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != typ {
			continue
		}
		status, _ = m["status"].(string)
		reason, _ = m["reason"].(string)
		message, _ = m["message"].(string)
		return status, reason, message, true
	}
	return "", "", "", false
}

// conditionsSummary renders every condition of u as "Type=Status
// (Reason)", for failure messages. It returns "no conditions" when there
// are none.
func conditionsSummary(u *unstructured.Unstructured) string {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	var parts []string
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok {
			parts = append(parts, fmt.Sprintf("%v=%v (%v: %v)", m["type"], m["status"], m["reason"], m["message"]))
		}
	}
	if len(parts) == 0 {
		return "no conditions"
	}
	return strings.Join(parts, "; ")
}

// conditionTrue returns nil when the object name of resource r (in
// namespace ns, empty for cluster-scoped) has condition typ True, and
// wantReason when wantReason is not empty. It reads the object with dyn
// under ctx and returns an error describing what it saw otherwise.
func conditionTrue(ctx context.Context, dyn dynamic.Interface, r schema.GroupVersionResource, ns, name, typ, wantReason string) error {
	var ri dynamic.ResourceInterface = dyn.Resource(r)
	if ns != "" {
		ri = dyn.Resource(r).Namespace(ns)
	}
	u, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get %s %s: %w", r.Resource, qualified(ns, name), err)
	}
	status, reason, _, found := condition(u, typ)
	if !found || status != string(metav1.ConditionTrue) || (wantReason != "" && reason != wantReason) {
		want := typ + "=True"
		if wantReason != "" {
			want += " with reason " + wantReason
		}
		return fmt.Errorf("%s %s: expected %s, observed %s", r.Resource, qualified(ns, name), want, conditionsSummary(u))
	}
	return nil
}

// gone returns nil when the object name of resource r (namespace ns,
// empty for cluster-scoped) no longer exists, read with dyn under ctx,
// else an error saying it is still there.
func gone(ctx context.Context, dyn dynamic.Interface, r schema.GroupVersionResource, ns, name string) error {
	var ri dynamic.ResourceInterface = dyn.Resource(r)
	if ns != "" {
		ri = dyn.Resource(r).Namespace(ns)
	}
	u, err := ri.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	default:
		return fmt.Errorf("%s %s still exists (deletionTimestamp %v, finalizers %v)", r.Resource, qualified(ns, name), u.GetDeletionTimestamp(), u.GetFinalizers())
	}
}

// qualified returns "ns/name", or name alone when ns is empty.
func qualified(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

// ignoreNotFound returns err unless it is a NotFound API error.
func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// requireDeployments checks under ctx with c that each Deployment of
// names exists in ns and that every workload in ns is ready
// (health.Workloads). hint is the inspect command for failures. It
// returns an error listing what is missing or not ready.
func requireDeployments(ctx context.Context, c wait.Clients, ns string, names []string, hint string) error {
	list, err := c.Kube.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list deployments in %s: %w", ns, err)
	}
	var have []string
	for i := range list.Items {
		have = append(have, list.Items[i].Name)
	}
	var missing []string
	for _, n := range names {
		if !slices.Contains(have, n) {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		sort.Strings(have)
		return fmt.Errorf("expected Deployments %v in %s, missing %v (found %v); inspect: %s", names, ns, missing, have, hint)
	}
	if err := health.Workloads(ctx, c, []string{ns}); err != nil {
		return fmt.Errorf("%w; inspect: %s", err, hint)
	}
	return nil
}

// podsHealthy returns nil when health.Pods finds no problem in
// namespaces, read with c under ctx. Problems that waiting cannot fix
// (restarts, crash loops, failed pulls, bad last exits) are permanent.
// hint is the inspect command for failures.
func podsHealthy(ctx context.Context, c wait.Clients, namespaces []string, hint string) error {
	problems, err := health.Pods(ctx, c, namespaces)
	if err != nil {
		return err
	}
	if len(problems) == 0 {
		return nil
	}
	err = fmt.Errorf("%w; inspect: %s", health.ProblemsError(fmt.Sprintf("expected every pod in %v Running and Ready with 0 restarts, observed problems", namespaces), problems), hint)
	for _, p := range problems {
		switch p.Reason {
		case "NotReady", "Pending", "ContainerCreating", "PodInitializing":
		default:
			return permanent(err)
		}
	}
	return err
}

// serviceReady checks under ctx with c that the Service ns/name exists
// and that its EndpointSlices hold at least one ready endpoint (a nil
// ready condition counts as ready, as the API defines). It returns an
// error saying what it observed otherwise.
func serviceReady(ctx context.Context, c wait.Clients, ns, name string) error {
	if _, err := c.Kube.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("expected Service %s/%s: %w", ns, name, err)
	}
	list, err := c.Kube.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + name})
	if err != nil {
		return fmt.Errorf("list EndpointSlices of Service %s/%s: %w", ns, name, err)
	}
	ready, total := 0, 0
	for _, sl := range list.Items {
		for _, ep := range sl.Endpoints {
			total++
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				ready++
			}
		}
	}
	if ready == 0 {
		return fmt.Errorf("expected a ready endpoint behind Service %s/%s, observed %d EndpointSlices with %d endpoints, none ready; inspect: kubectl -n %s get endpointslices -l %s=%s",
			ns, name, len(list.Items), total, ns, discoveryv1.LabelServiceName, name)
	}
	return nil
}

// webhookRef is one admission webhook that calls a Service.
type webhookRef struct {
	// config is "<Kind>/<configuration name>".
	config string
	// webhook is the webhook's name inside the configuration.
	webhook string
	// caBundle is the webhook's clientConfig.caBundle.
	caBundle []byte
	// service is the Service it calls.
	service serviceRef
}

// serviceRef names a Service by namespace and name.
type serviceRef struct {
	// namespace is the Service's namespace.
	namespace string
	// name is the Service's name.
	name string
}

// webhooksCalling lists, with c under ctx, every validating and mutating
// webhook whose clientConfig.service lives in namespace ns. It returns
// them sorted by configuration and webhook name, or a list error.
func webhooksCalling(ctx context.Context, c wait.Clients, ns string) ([]webhookRef, error) {
	var out []webhookRef
	add := func(kind, config, name string, cc admissionv1.WebhookClientConfig) {
		if cc.Service == nil || cc.Service.Namespace != ns {
			return
		}
		out = append(out, webhookRef{
			config: kind + "/" + config, webhook: name, caBundle: cc.CABundle,
			service: serviceRef{namespace: cc.Service.Namespace, name: cc.Service.Name},
		})
	}
	vwcs, err := c.Kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list ValidatingWebhookConfigurations: %w", err)
	}
	for _, cfg := range vwcs.Items {
		for _, w := range cfg.Webhooks {
			add("ValidatingWebhookConfiguration", cfg.Name, w.Name, w.ClientConfig)
		}
	}
	mwcs, err := c.Kube.AdmissionregistrationV1().MutatingWebhookConfigurations().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list MutatingWebhookConfigurations: %w", err)
	}
	for _, cfg := range mwcs.Items {
		for _, w := range cfg.Webhooks {
			add("MutatingWebhookConfiguration", cfg.Name, w.Name, w.ClientConfig)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].config != out[j].config {
			return out[i].config < out[j].config
		}
		return out[i].webhook < out[j].webhook
	})
	return out, nil
}

// webhooksWired checks under ctx with c that namespace ns serves at least
// one admission webhook, that every webhook calling a Service in ns has a
// non-empty caBundle, and that each such Service has a ready endpoint. It
// returns the configurations it checked, or an error listing every
// problem.
func webhooksWired(ctx context.Context, c wait.Clients, ns string) ([]string, error) {
	refs, err := webhooksCalling(ctx, c, ns)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("expected admission webhooks served from namespace %s, observed none; inspect: kubectl get validatingwebhookconfigurations,mutatingwebhookconfigurations", ns)
	}
	var problems, configs []string
	services := map[serviceRef]bool{}
	for _, r := range refs {
		if !slices.Contains(configs, r.config) {
			configs = append(configs, r.config)
		}
		if len(r.caBundle) == 0 {
			problems = append(problems, fmt.Sprintf("%s webhook %s: expected a caBundle (injected by cert-manager's cainjector), observed none", r.config, r.webhook))
		}
		services[r.service] = true
	}
	var svcs []serviceRef
	for s := range services {
		svcs = append(svcs, s)
	}
	sort.Slice(svcs, func(i, j int) bool { return svcs[i].name < svcs[j].name })
	for _, s := range svcs {
		if err := serviceReady(ctx, c, s.namespace, s.name); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return configs, fmt.Errorf("webhooks of %s not wired:\n  %s", ns, strings.Join(problems, "\n  "))
	}
	return configs, nil
}

// podsBySelector lists, with c under ctx, the pods of ns matching
// selector, or returns a list error.
func podsBySelector(ctx context.Context, c wait.Clients, ns, selector string) ([]corev1.Pod, error) {
	list, err := c.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list pods in %s with %q: %w", ns, selector, err)
	}
	return list.Items, nil
}

// podReady reports whether p is Running with condition Ready=True.
func podReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
