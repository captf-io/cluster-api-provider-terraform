//go:build e2e

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

package noop

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/diag"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/env"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/kubewait"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/objects"
)

// nsArtifactsDir is the subdirectory of a diagnostics bundle holding the
// run namespace's pods, logs and events (env.Collect covers the provider
// namespaces only; it already writes every namespace's CAPI, CAPTF and
// Job objects).
const nsArtifactsDir = "noop-namespace"

// collectNamespace writes the run namespace's pods, container logs and
// events into dir/nsArtifactsDir under ctx, through diag.Collect, and
// returns its error.
func (s *suite) collectNamespace(ctx context.Context, dir string) error {
	return diag.Collect(ctx, s.c, filepath.Join(dir, nsArtifactsDir), diag.Options{Namespaces: []string{s.ns}})
}

// cleanupGroup is one set of kinds the cleanup deletes and waits out
// before the next: CAPI machines and pools, then Clusters, then any CAPTF
// object left behind.
type cleanupGroup []schema.GroupVersionResource

// cleanupOrder is the dependency order of the cleanup.
var cleanupOrder = []cleanupGroup{
	{objects.MachineGVR, objects.MachinePoolGVR},
	{objects.ClusterGVR},
	{objects.TerraformMachineGVR, objects.TerraformMachinePoolGVR, objects.TerraformClusterGVR},
}

// cleanup runs from t.Cleanup: it stops the pod tracker, then deletes
// whatever the run left in dependency order (cleanupOrder), waiting for
// each group to go; then the namespace, unless CAPTF objects are stuck
// in it (a namespace in termination refuses the Jobs and Secrets their
// destroy needs); then the identity and, once it is gone, its Secret,
// which the objects still using the identity need. It is best effort
// and bounded by cleanupTimeout: after a failed run problems are logged,
// after a passing run (which should have left nothing) they fail t.
func (s *suite) cleanup(t *testing.T) {
	if s.tracker != nil {
		s.tracker.stop()
	}
	if s.ns == "" {
		return
	}
	report := t.Errorf
	if t.Failed() {
		report = t.Logf
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if _, err := s.c.Kube.CoreV1().Namespaces().Get(ctx, s.ns, metav1.GetOptions{}); err == nil {
		stuck := false
		for _, group := range cleanupOrder {
			if left := s.deleteGroup(ctx, t, group); len(left) > 0 {
				report("cleanup: still in namespace %s after waiting: %v; inspect: %s", s.ns, left, s.kubectlNS("get clusters,machines,machinepools,terraformclusters,terraformmachines,terraformmachinepools,jobs -o wide"))
				stuck = true
				break
			}
		}
		if stuck {
			report("cleanup: namespace %s kept, since deleting it would strand the objects above; delete them, then the namespace: %s", s.ns, s.kubectl("delete namespace "+s.ns))
		} else if err := ignoreNotFound(s.c.Kube.CoreV1().Namespaces().Delete(ctx, s.ns, metav1.DeleteOptions{})); err != nil {
			report("cleanup: delete namespace %s: %v", s.ns, err)
		} else {
			t.Logf("cleanup: namespace %s deleted", s.ns)
		}
	}
	// The Secret goes only with the identity: objects still using the
	// identity need it to destroy.
	if err := ignoreNotFound(s.c.Dynamic.Resource(objects.TerraformClusterIdentityGVR).Delete(ctx, s.identity, metav1.DeleteOptions{})); err != nil {
		report("cleanup: delete TerraformClusterIdentity %s: %v (the webhook refuses while it is in use or mirrored); its Secret %s/%s is kept for them; delete both later: %s",
			s.identity, err, env.ManagerNamespace, s.identity, s.kubectl("delete terraformclusteridentity "+s.identity+"; kubectl -n "+env.ManagerNamespace+" delete secret "+s.identity))
		return
	}
	if err := ignoreNotFound(s.c.Kube.CoreV1().Secrets(env.ManagerNamespace).Delete(ctx, s.identity, metav1.DeleteOptions{})); err != nil {
		report("cleanup: delete Secret %s/%s: %v", env.ManagerNamespace, s.identity, err)
	}
}

// groupWait bounds the wait for one cleanup group to go. It outlasts the
// Job deadline: a TerraformMachine whose apply Job cannot pull its image
// is deleted only once that Job ends at its deadline.
const groupWait = (jobDeadline + 60) * time.Second

// deleteGroup deletes every object of the kinds in group in the run's
// namespace under ctx and waits up to groupWait for them to go, logging
// through t. It returns "resource/name" for each one still there.
func (s *suite) deleteGroup(ctx context.Context, t *testing.T, group cleanupGroup) []string {
	list := func(ctx context.Context) []string {
		var left []string
		for _, gvr := range group {
			l, err := s.c.Dynamic.Resource(gvr).Namespace(s.ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				left = append(left, fmt.Sprintf("%s (list failed: %v)", gvr.Resource, err))
				continue
			}
			for i := range l.Items {
				left = append(left, gvr.Resource+"/"+l.Items[i].GetName())
			}
		}
		return left
	}
	left := list(ctx)
	if len(left) == 0 {
		return nil
	}
	t.Logf("cleanup: deleting %v", left)
	for _, gvr := range group {
		if err := ignoreNotFound(s.c.Dynamic.Resource(gvr).Namespace(s.ns).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{})); err != nil {
			t.Logf("cleanup: delete %s in %s: %v", gvr.Resource, s.ns, err)
		}
	}
	_ = eventually(ctx, t, "cleanup: "+fmt.Sprint(left)+" gone", groupWait, func(ctx context.Context) error {
		if l := list(ctx); len(l) > 0 {
			return fmt.Errorf("left: %v", l)
		}
		return nil
	})
	return list(ctx)
}

// managerPod identifies the CAPTF manager pod and its restarts at setup.
type managerPod struct {
	// name is the pod's name.
	name string
	// uid is the pod's UID.
	uid string
	// restarts sums the restart counts of its containers.
	restarts int32
}

// managerSelector selects the CAPTF manager pod.
const managerSelector = "control-plane=controller-manager"

// currentManager returns the single CAPTF manager pod, read under ctx,
// or an error when there is not exactly one, Running.
func (s *suite) currentManager(ctx context.Context) (managerPod, error) {
	pods, err := s.c.Kube.CoreV1().Pods(env.ManagerNamespace).List(ctx, metav1.ListOptions{LabelSelector: managerSelector})
	if err != nil {
		return managerPod{}, err
	}
	if len(pods.Items) != 1 || pods.Items[0].Status.Phase != corev1.PodRunning {
		var names []string
		for i := range pods.Items {
			names = append(names, fmt.Sprintf("%s (%s)", pods.Items[i].Name, pods.Items[i].Status.Phase))
		}
		return managerPod{}, fmt.Errorf("expected one Running manager pod (%s) in %s, observed %v", managerSelector, env.ManagerNamespace, names)
	}
	p := &pods.Items[0]
	var restarts int32
	for _, cs := range p.Status.ContainerStatuses {
		restarts += cs.RestartCount
	}
	return managerPod{name: p.Name, uid: string(p.UID), restarts: restarts}, nil
}

// podRecord is the last state the tracker saw of one CAPTF Job pod.
type podRecord struct {
	// name is the pod's name.
	name string
	// job is the pod's Job.
	job string
	// kind and owner are the owning CAPTF object's kind and name.
	kind, owner string
	// op is the Job's operation.
	op string
	// image is the source container's image.
	image string
	// created is the pod's creation time.
	created time.Time
	// phase is the last phase seen.
	phase corev1.PodPhase
	// detail is a waiting or terminated reason seen last, if any.
	detail string
}

// podTracker polls the CAPTF Job pods of a namespace in the background and
// keeps the last state of each, since the pods are gone by the time the
// health stage runs (their Jobs are deleted with their objects).
type podTracker struct {
	// mu guards pods.
	mu sync.Mutex
	// pods are the records by pod name.
	pods map[string]podRecord
	// cancel stops the poller.
	cancel context.CancelFunc
	// done is closed when the poller returns.
	done chan struct{}
}

// trackEvery is the tracker's polling period. A destroy Job's pod lives
// about 8 seconds before the Job goes with its object.
const trackEvery = time.Second

// trackPods starts and returns a podTracker for the CAPTF Job pods of the
// run's namespace (the pods carrying the owner-kind label).
func (s *suite) trackPods() *podTracker {
	ctx, cancel := context.WithCancel(context.Background())
	p := &podTracker{pods: map[string]podRecord{}, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		tick := time.NewTicker(trackEvery)
		defer tick.Stop()
		for {
			p.poll(ctx, s)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return p
}

// poll lists the CAPTF Job pods of s's namespace under ctx once and
// records each; a list error is skipped.
func (p *podTracker) poll(ctx context.Context, s *suite) {
	pods, err := s.c.Kube.CoreV1().Pods(s.ns).List(ctx, metav1.ListOptions{LabelSelector: kubewait.OwnerKindLabel})
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range pods.Items {
		pod := &pods.Items[i]
		var image string
		for _, c := range pod.Spec.Containers {
			if c.Name == sourceContainer {
				image = c.Image
			}
		}
		p.pods[pod.Name] = podRecord{
			image:   image,
			name:    pod.Name,
			job:     pod.Labels["batch.kubernetes.io/job-name"],
			kind:    pod.Labels[kubewait.OwnerKindLabel],
			owner:   pod.Labels[kubewait.OwnerNameLabel],
			op:      pod.Labels[kubewait.OpLabel],
			created: pod.CreationTimestamp.Time,
			phase:   pod.Status.Phase,
			detail:  podDetail(pod),
		}
	}
}

// podDetail returns the first waiting or terminated reason of pod's
// containers, "" when none.
func podDetail(pod *corev1.Pod) string {
	for _, cs := range append(slices.Clone(pod.Status.InitContainerStatuses), pod.Status.ContainerStatuses...) {
		switch {
		case cs.State.Waiting != nil:
			return cs.Name + ": " + cs.State.Waiting.Reason
		case cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0:
			return fmt.Sprintf("%s: %s (exit %d)", cs.Name, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
		}
	}
	return ""
}

// stop stops the poller and waits for it.
func (p *podTracker) stop() {
	p.cancel()
	<-p.done
}

// records returns the records, sorted by creation time then name.
func (p *podTracker) records() []podRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]podRecord, 0, len(p.pods))
	for _, r := range p.pods {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b podRecord) int {
		if c := a.created.Compare(b.created); c != 0 {
			return c
		}
		if a.name < b.name {
			return -1
		}
		return 1
	})
	return out
}

// isNotFound reports whether err is a NotFound API error.
func isNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}
