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

package diag

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// clusterScoped is the directory name standing for "no namespace" under
// objects/<resource>/.
const clusterScoped = "_cluster"

// Options selects what Collect gathers.
type Options struct {
	// Namespaces are the namespaces whose pod logs and events are
	// collected: the provider namespaces plus any workload namespaces.
	Namespaces []string
	// Resources are the custom or built-in resources collected as YAML
	// from every namespace. Secrets are skipped even when listed.
	Resources []schema.GroupVersionResource
	// NodeLogs, when non-nil, writes the kind node logs into the directory
	// it is given (dir/node-logs). The orchestrator passes kindcluster's
	// CollectLogs.
	NodeLogs func(dir string) error
}

// DefaultResources returns the resources worth collecting from a CAPTF
// environment: the CAPI core, kubeadm bootstrap and control-plane objects,
// every CAPTF (v1alpha1) kind and batch Jobs. A resource a cluster does
// not serve is recorded in errors.txt, not fatal.
func DefaultResources() []schema.GroupVersionResource {
	gvr := func(group, version, resource string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
	}
	const (
		capi  = "cluster.x-k8s.io"
		infra = "infrastructure.cluster.x-k8s.io"
	)
	return []schema.GroupVersionResource{
		gvr(capi, "v1beta2", "clusters"),
		gvr(capi, "v1beta2", "machines"),
		gvr(capi, "v1beta2", "machinesets"),
		gvr(capi, "v1beta2", "machinedeployments"),
		gvr(capi, "v1beta2", "machinepools"),
		gvr("controlplane.cluster.x-k8s.io", "v1beta2", "kubeadmcontrolplanes"),
		gvr("bootstrap.cluster.x-k8s.io", "v1beta2", "kubeadmconfigs"),
		gvr(infra, "v1alpha1", "terraformclusters"),
		gvr(infra, "v1alpha1", "terraformclusteridentities"),
		gvr(infra, "v1alpha1", "terraformclustertemplates"),
		gvr(infra, "v1alpha1", "terraformmachines"),
		gvr(infra, "v1alpha1", "terraformmachinetemplates"),
		gvr(infra, "v1alpha1", "terraformmachinepools"),
		gvr(infra, "v1alpha1", "terraformmachinepooltemplates"),
		gvr(infra, "v1alpha1", "terraformplans"),
		gvr("batch", "v1", "jobs"),
	}
}

// collector writes artifacts under dir and remembers what failed.
type collector struct {
	c      wait.Clients
	dir    string
	wrote  int
	failed []string
}

// Collect writes the artifacts of the cluster behind c into dir, creating
// it. It never writes Secret data. A failure for one item is recorded in
// dir/errors.txt and collection goes on; Collect returns an error only
// when dir cannot be created or nothing at all could be written. ctx
// bounds every API call, c supplies the clients and o selects the
// namespaces, resources and node-log hook.
func Collect(ctx context.Context, c wait.Clients, dir string, o Options) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("diag: create %s: %w", dir, err)
	}
	col := &collector{c: c, dir: dir}
	for _, ns := range o.Namespaces {
		col.pods(ctx, ns)
		col.events(ctx, ns)
	}
	for _, gvr := range o.Resources {
		col.objects(ctx, gvr)
	}
	col.nodes(ctx)
	if o.NodeLogs != nil {
		nodeDir := filepath.Join(dir, "node-logs")
		if err := o.NodeLogs(nodeDir); err != nil {
			col.fail("node logs", err)
		} else {
			col.wrote++
		}
	}
	if len(col.failed) > 0 {
		// The record itself is best-effort: errors.txt failing to write
		// must not mask the collection result.
		_ = os.WriteFile(filepath.Join(dir, "errors.txt"), []byte(strings.Join(col.failed, "\n")+"\n"), 0o600)
	}
	if col.wrote == 0 {
		return fmt.Errorf("diag: collect into %s: nothing could be written (%d failures, see errors.txt)", dir, len(col.failed))
	}
	return nil
}

// fail records a failure for what. what names the item; err is the cause.
func (col *collector) fail(what string, err error) {
	col.failed = append(col.failed, fmt.Sprintf("%s: %v", what, err))
}

// writeFile writes data to the file at the slash-separated rel path under
// the artifact directory, creating parents, and counts it. It records a
// failure instead of returning one.
func (col *collector) writeFile(rel string, data []byte) {
	path := filepath.Join(col.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		col.fail(rel, err)
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		col.fail(rel, err)
		return
	}
	col.wrote++
}

// writeYAML marshals v and writes it to rel. It records a failure instead
// of returning one.
func (col *collector) writeYAML(rel string, v any) {
	data, err := yaml.Marshal(v)
	if err != nil {
		col.fail(rel, err)
		return
	}
	col.writeFile(rel, data)
}

// pods collects the logs of every container of every pod in ns, plus the
// previous logs of a restarted container. ctx bounds the API calls. Failures
// are recorded.
func (col *collector) pods(ctx context.Context, ns string) {
	pods, err := col.c.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		col.fail("list pods in "+ns, err)
		return
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		restarts := map[string]int32{}
		for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
			restarts[cs.Name] = cs.RestartCount
		}
		containers := append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...)
		for _, ctr := range containers {
			base := fmt.Sprintf("pods/%s/%s/%s", safe(ns), safe(p.Name), safe(ctr.Name))
			col.log(ctx, ns, p.Name, ctr.Name, false, base+".log")
			if restarts[ctr.Name] > 0 {
				col.log(ctx, ns, p.Name, ctr.Name, true, base+".previous.log")
			}
		}
	}
}

// log streams the log of container, in pod pod of namespace ns, to rel.
// previous selects the log of the last terminated instance and ctx bounds
// the stream. Failures are recorded.
func (col *collector) log(ctx context.Context, ns, pod, container string, previous bool, rel string) {
	stream, err := col.c.Kube.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, Previous: previous}).Stream(ctx)
	if err != nil {
		col.fail(rel, err)
		return
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		col.fail(rel, err)
		return
	}
	col.writeFile(rel, data)
}

// events writes the events of ns to events/<ns>.yaml. ctx bounds the API
// call. Failures are recorded.
func (col *collector) events(ctx context.Context, ns string) {
	list, err := col.c.Kube.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		col.fail("list events in "+ns, err)
		return
	}
	col.writeYAML("events/"+safe(ns)+".yaml", list.Items)
}

// nodes writes the cluster's nodes to nodes.yaml. ctx bounds the API call.
// A failure is recorded.
func (col *collector) nodes(ctx context.Context) {
	list, err := col.c.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		col.fail("list nodes", err)
		return
	}
	col.writeYAML("nodes.yaml", list.Items)
}

// objects writes every object of gvr, from all namespaces, to
// objects/<resource>/<namespace>/<name>.yaml. Secrets are skipped, both by
// resource and by kind. ctx bounds the API call. Failures are recorded.
func (col *collector) objects(ctx context.Context, gvr schema.GroupVersionResource) {
	if isSecret(gvr.Resource, "") {
		return
	}
	list, err := col.c.Dynamic.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		col.fail("list "+gvr.Resource+"."+gvr.Group, err)
		return
	}
	for i := range list.Items {
		u := &list.Items[i]
		if isSecret("", u.GetKind()) {
			continue
		}
		ns := u.GetNamespace()
		if ns == "" {
			ns = clusterScoped
		}
		unstructured.RemoveNestedField(u.Object, "metadata", "managedFields")
		col.writeYAML(fmt.Sprintf("objects/%s/%s/%s.yaml", safe(gvr.Resource), safe(ns), safe(u.GetName())), u.Object)
	}
}

// isSecret reports whether resource or kind names the Secret type.
// resource is a plural resource name, kind a Kind; either may be empty.
func isSecret(resource, kind string) bool {
	return strings.EqualFold(resource, "secrets") || kind == "Secret" || kind == "SecretList"
}

// safe makes s a single path element: separators and dot-only names are
// replaced so a hostile or odd name cannot escape its directory. It
// returns the sanitized element.
func safe(s string) string {
	s = strings.NewReplacer("/", "_", `\`, "_", string(os.PathSeparator), "_").Replace(s)
	if s == "" || s == "." || s == ".." {
		return "_" + s
	}
	return s
}
