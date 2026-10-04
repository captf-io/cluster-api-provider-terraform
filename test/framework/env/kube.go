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

package env

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/diag"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// The CAPTF manager as config/ renders it: config/default's namespace and
// namePrefix applied to config/manager's Deployment and container.
const (
	// ManagerNamespace is the namespace CAPTF is installed into.
	ManagerNamespace = "captf-system"
	// ManagerDeployment is the manager Deployment.
	ManagerDeployment = "captf-controller-manager"
	// managerContainer is the manager container in that Deployment.
	managerContainer = "manager"
	// managerImageEnv is the manager's runner-image setting, which
	// defaults to its own image and must follow it.
	managerImageEnv = "CAPTF_MANAGER_IMAGE"
)

// webhookProbeNamespace is where the webhook probe's dry-run object goes;
// it always exists and nothing is stored.
const webhookProbeNamespace = "default"

// CAPTFCRDs are the CustomResourceDefinitions in config/crd/bases, which
// Up waits to be Established.
var CAPTFCRDs = []string{
	"terraformclusteridentities.infrastructure.cluster.x-k8s.io",
	"terraformclusters.infrastructure.cluster.x-k8s.io",
	"terraformclustertemplates.infrastructure.cluster.x-k8s.io",
	"terraformmachinepools.infrastructure.cluster.x-k8s.io",
	"terraformmachinepooltemplates.infrastructure.cluster.x-k8s.io",
	"terraformmachines.infrastructure.cluster.x-k8s.io",
	"terraformmachinetemplates.infrastructure.cluster.x-k8s.io",
}

// Timeouts of the readiness waits. clusterctl init already waits for the
// providers, so these normally pass on the first check.
const (
	// deploymentsTimeout bounds the provider Deployments wait.
	deploymentsTimeout = 10 * time.Minute
	// crdsTimeout bounds the CRD wait.
	crdsTimeout = 2 * time.Minute
	// webhookTimeout bounds the webhook wait.
	webhookTimeout = 3 * time.Minute
	// rolloutTimeout bounds the manager rollout wait.
	rolloutTimeout = 5 * time.Minute
	// pollInterval is the time between rollout checks.
	pollInterval = 2 * time.Second
	// reportEvery is the time between progress lines.
	reportEvery = 30 * time.Second
)

// diagNamespaces returns the namespaces whose pods and events Collect
// writes: the provider namespaces plus kube-system.
func diagNamespaces() []string {
	return append(append([]string{}, wait.DefaultProviderNamespaces...), "kube-system")
}

// kube is the real Kube, over client-go.
type kube struct {
	// c are the cluster's clients.
	c wait.Clients
	// out receives progress lines.
	out io.Writer
}

// NewKube returns the Kube for the cluster behind the kubeconfig file
// (only that file is read), writing progress to out. It returns an error
// if the kubeconfig cannot be loaded.
func NewKube(kubeconfig string, out io.Writer) (Kube, error) {
	c, err := wait.ClientsFromKubeconfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("env: %w", err)
	}
	return newKube(c, out), nil
}

// newKube returns the Kube over the clients c, writing progress to out.
func newKube(c wait.Clients, out io.Writer) Kube {
	return &kube{c: c, out: out}
}

// opts returns wait.Options with timeout and the package's progress
// settings.
func (k *kube) opts(timeout time.Duration) wait.Options {
	return wait.Options{Interval: pollInterval, ReportEvery: reportEvery, Timeout: timeout, Out: k.out}
}

// WaitProviders runs, under ctx, wait.DeploymentsAvailable on the
// provider namespaces, wait.CRDsEstablished on CAPTFCRDs and
// wait.WebhookServing, and returns the first error.
func (k *kube) WaitProviders(ctx context.Context) error {
	if err := wait.DeploymentsAvailable(ctx, k.c, wait.DefaultProviderNamespaces, k.opts(deploymentsTimeout)); err != nil {
		return err
	}
	if err := wait.CRDsEstablished(ctx, k.c, CAPTFCRDs, k.opts(crdsTimeout)); err != nil {
		return err
	}
	return wait.WebhookServing(ctx, k.c, webhookProbeNamespace, k.opts(webhookTimeout))
}

// Report returns wait.Report on the provider namespaces under ctx, or its
// error.
func (k *kube) Report(ctx context.Context) (string, error) {
	return wait.Report(ctx, k.c, wait.DefaultProviderNamespaces)
}

// Collect runs diag.Collect into dir under ctx, over the provider
// namespaces plus kube-system and diag.DefaultResources, with nodeLogs as
// the node-log hook, and returns its error.
func (k *kube) Collect(ctx context.Context, dir string, nodeLogs func(dir string) error) error {
	return diag.Collect(ctx, k.c, dir, diag.Options{
		Namespaces: diagNamespaces(),
		Resources:  diag.DefaultResources(),
		NodeLogs:   nodeLogs,
	})
}

// managerDeployment returns the manager Deployment, read under ctx, or an
// error.
func (k *kube) managerDeployment(ctx context.Context) (*appsv1.Deployment, error) {
	d, err := k.c.Kube.AppsV1().Deployments(ManagerNamespace).Get(ctx, ManagerDeployment, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("env: get deployment %s/%s: %w", ManagerNamespace, ManagerDeployment, err)
	}
	return d, nil
}

// ManagerImage returns the manager container's image, read under ctx, or
// an error when the Deployment or the container is missing.
func (k *kube) ManagerImage(ctx context.Context) (string, error) {
	d, err := k.managerDeployment(ctx)
	if err != nil {
		return "", err
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == managerContainer {
			return c.Image, nil
		}
	}
	return "", fmt.Errorf("env: deployment %s/%s has no %q container", ManagerNamespace, ManagerDeployment, managerContainer)
}

// managerPatch returns the strategic merge patch that sets the manager
// container's image and its CAPTF_MANAGER_IMAGE variable to ref (both
// lists merge by name, so nothing else changes).
func managerPatch(ref string) ([]byte, error) {
	patch := map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
		"containers": []any{map[string]any{
			"name":  managerContainer,
			"image": ref,
			"env":   []any{map[string]any{"name": managerImageEnv, "value": ref}},
		}},
	}}}}
	return json.Marshal(patch)
}

// SetManagerImage patches, under ctx, the manager Deployment's container
// image and CAPTF_MANAGER_IMAGE to ref, and returns an error if the patch
// fails.
func (k *kube) SetManagerImage(ctx context.Context, ref string) error {
	patch, err := managerPatch(ref)
	if err != nil {
		return fmt.Errorf("env: set manager image: %w", err)
	}
	_, err = k.c.Kube.AppsV1().Deployments(ManagerNamespace).Patch(ctx, ManagerDeployment, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("env: set manager image: %w", err)
	}
	return nil
}

// rolledOut reports whether d has rolled out completely: its controller
// has seen the latest spec, and every replica is updated and available
// with no old replica left. It also returns a one-line status.
func rolledOut(d *appsv1.Deployment) (bool, string) {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	status := fmt.Sprintf("generation %d observed %d, replicas %d updated %d available %d (want %d)",
		d.Generation, s.ObservedGeneration, s.Replicas, s.UpdatedReplicas, s.AvailableReplicas, want)
	ok := s.ObservedGeneration >= d.Generation && s.UpdatedReplicas == want &&
		s.Replicas == want && s.AvailableReplicas == want && s.UnavailableReplicas == 0
	return ok, status
}

// WaitManagerRollout polls the manager Deployment under ctx every
// pollInterval until it has rolled out, printing its status every
// reportEvery, and returns an error on timeout (rolloutTimeout) or
// cancellation.
func (k *kube) WaitManagerRollout(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, rolloutTimeout)
	defer cancel()
	start := time.Now()
	last := start
	status := "not checked"
	for {
		d, err := k.managerDeployment(ctx)
		if err == nil {
			var ok bool
			if ok, status = rolledOut(d); ok {
				fmt.Fprintf(k.out, "testenv: %s rolled out after %s\n", ManagerDeployment, time.Since(start).Round(time.Millisecond))
				return nil
			}
		} else {
			status = err.Error()
		}
		if time.Since(last) >= reportEvery {
			last = time.Now()
			fmt.Fprintf(k.out, "testenv: waiting for %s: %s\n", ManagerDeployment, status)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("env: %s rollout: %s: %w", ManagerDeployment, status, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}
