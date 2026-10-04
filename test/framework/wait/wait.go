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

package wait

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// DefaultProviderNamespaces are the namespaces clusterctl init fills:
// cert-manager, the CAPI core, kubeadm bootstrap and control-plane
// providers, and CAPTF.
var DefaultProviderNamespaces = []string{
	"cert-manager",
	"capi-system",
	"capi-kubeadm-bootstrap-system",
	"capi-kubeadm-control-plane-system",
	"captf-system",
}

// crdGVR is the apiextensions v1 CustomResourceDefinition resource.
var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// terraformClusterGVR is the TerraformCluster resource the webhook probe
// creates.
var terraformClusterGVR = schema.GroupVersionResource{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Resource: "terraformclusters"}

// Options tunes a wait. The zero value is usable: zero durations take the
// defaults and a nil Out discards the progress output.
type Options struct {
	// Interval is the time between checks. Default 5s.
	Interval time.Duration
	// ReportEvery is the time between progress reports. Default 30s.
	ReportEvery time.Duration
	// Timeout bounds the whole wait. Default 5m.
	Timeout time.Duration
	// Out receives the progress reports. Nil discards them.
	Out io.Writer
}

// withDefaults returns o with zero fields replaced by the defaults. It
// returns the filled copy.
func (o Options) withDefaults() Options {
	if o.Interval <= 0 {
		o.Interval = 5 * time.Second
	}
	if o.ReportEvery <= 0 {
		o.ReportEvery = 30 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Minute
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	return o
}

// checkFunc is one readiness check. It returns true when ready, else false
// and a one-line status.
type checkFunc func(ctx context.Context) (bool, string)

// poll runs check at once and then every o.Interval until it is ready, the
// timeout passes or ctx is done. what names the wait in the output and the
// errors, and c supplies the clients for the pod report. While waiting it prints the status and, when namespaces is
// non-empty, the non-ready pods every o.ReportEvery. It returns nil when
// ready and a wrapped error carrying the last status otherwise.
func poll(ctx context.Context, c Clients, what string, namespaces []string, o Options, check checkFunc) error {
	o = o.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	start := time.Now()
	lastReport := start
	ticker := time.NewTicker(o.Interval)
	defer ticker.Stop()
	for {
		ok, status := check(ctx)
		if ok {
			fmt.Fprintf(o.Out, "wait: %s: ready after %s\n", what, time.Since(start).Round(time.Millisecond))
			return nil
		}
		if ctx.Err() == nil && time.Since(lastReport) >= o.ReportEvery {
			lastReport = time.Now()
			printProgress(ctx, c, what, status, namespaces, start, o.Out)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait: %s: not ready after %s: %s: %w", what, time.Since(start).Round(time.Millisecond), status, ctx.Err())
		case <-ticker.C:
		}
	}
}

// printProgress writes the status line and the pod report to out. A pod
// listing failure is printed instead of the report; it never fails the
// wait. ctx is the wait's context, c supplies the clients, what names the
// wait, namespaces are the ones reported on and start is when the wait
// began.
func printProgress(ctx context.Context, c Clients, what, status string, namespaces []string, start time.Time, out io.Writer) {
	fmt.Fprintf(out, "wait: %s: %s (waiting %s)\n", what, status, time.Since(start).Round(time.Second))
	if len(namespaces) == 0 {
		return
	}
	rep, err := Report(ctx, c, namespaces)
	if err != nil {
		fmt.Fprintf(out, "wait: pod report unavailable: %v\n", err)
		return
	}
	fmt.Fprintln(out, indent(rep))
}

// indent prefixes every line of s with two spaces. It returns the result.
func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

// DeploymentsAvailable waits until every Deployment in each of namespaces
// is Available and fully rolled out. A namespace with no Deployments is
// not ready, so a provider that has not been applied yet is not mistaken
// for a ready one. ctx cancels the wait, c supplies the clients and o tunes
// the polling. It returns nil when all are ready, and a wrapped error
// naming the Deployments still waiting on timeout or cancellation.
func DeploymentsAvailable(ctx context.Context, c Clients, namespaces []string, o Options) error {
	return poll(ctx, c, "deployments available", namespaces, o, func(ctx context.Context) (bool, string) {
		var waiting []string
		for _, ns := range namespaces {
			list, err := c.Kube.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, fmt.Sprintf("list deployments in %s: %v", ns, err)
			}
			if len(list.Items) == 0 {
				waiting = append(waiting, ns+": no deployments yet")
				continue
			}
			for i := range list.Items {
				if ok, why := deploymentReady(&list.Items[i]); !ok {
					waiting = append(waiting, fmt.Sprintf("%s/%s: %s", ns, list.Items[i].Name, why))
				}
			}
		}
		if len(waiting) > 0 {
			return false, strings.Join(waiting, "; ")
		}
		return true, ""
	})
}

// CRDsEstablished waits until each CustomResourceDefinition in names (for
// example "terraformclusters.infrastructure.cluster.x-k8s.io") exists and
// has the condition Established=True. ctx cancels the wait, c supplies the
// clients and o tunes the polling. It returns nil when all are
// established, and a wrapped error naming the ones still waiting on
// timeout or cancellation.
func CRDsEstablished(ctx context.Context, c Clients, names []string, o Options) error {
	return poll(ctx, c, "CRDs established", nil, o, func(ctx context.Context) (bool, string) {
		var waiting []string
		for _, name := range names {
			u, err := c.Dynamic.Resource(crdGVR).Get(ctx, name, metav1.GetOptions{})
			switch {
			case apierrors.IsNotFound(err):
				waiting = append(waiting, name+": not found")
			case err != nil:
				waiting = append(waiting, fmt.Sprintf("%s: %v", name, err))
			default:
				if ok, why := crdEstablished(u); !ok {
					waiting = append(waiting, name+": "+why)
				}
			}
		}
		if len(waiting) > 0 {
			return false, strings.Join(waiting, "; ")
		}
		return true, ""
	})
}

// probeCluster returns the intentionally invalid TerraformCluster the
// webhook probe submits in namespace. The identity is set and the image
// reference is syntactically invalid: the CRD schema accepts that (it
// checks only the length), so only the CAPTF webhook can reject it.
func probeCluster(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
		"kind":       "TerraformCluster",
		"metadata": map[string]any{
			"name":      "captf-testenv-webhook-probe",
			"namespace": namespace,
		},
		"spec": map[string]any{
			"identityRef": map[string]any{"name": "probe"},
			"source":      map[string]any{"image": "NOT A VALID IMAGE"},
		},
	}}
}

// WebhookServing waits until the CAPTF admission webhook is serving. It
// submits a server-side dry-run Create of an intentionally invalid
// TerraformCluster in namespace (nothing is stored) and is ready only when
// the webhook itself rejects it. A connection error, a "failed calling
// webhook" error, a schema rejection or an accepted object all mean not
// ready yet. ctx cancels the wait, c supplies the clients and o tunes the
// polling. It returns nil when ready, and a wrapped error carrying the
// last probe result on timeout or cancellation.
func WebhookServing(ctx context.Context, c Clients, namespace string, o Options) error {
	return poll(ctx, c, "webhook serving", nil, o, func(ctx context.Context) (bool, string) {
		_, err := c.Dynamic.Resource(terraformClusterGVR).Namespace(namespace).
			Create(ctx, probeCluster(namespace), metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		return classifyProbe(err)
	})
}
