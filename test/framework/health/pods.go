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

package health

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// Problem is one thing wrong with a pod or one of its containers.
type Problem struct {
	// Namespace is the pod's namespace.
	Namespace string
	// Pod is the pod's name.
	Pod string
	// Container is the container name, empty for a pod-level problem.
	Container string
	// Reason is the short cause, such as CrashLoopBackOff or OOMKilled.
	Reason string
	// Detail is a longer explanation, possibly empty.
	Detail string
	// Restarts is the container's restart count (0 for a pod-level
	// problem).
	Restarts int32
}

// String renders the problem on one line, for example
// "pod kube-system/x: container y CrashLoopBackOff (restarts 3)". It
// returns that line.
func (p Problem) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "pod %s/%s:", p.Namespace, p.Pod)
	if p.Container != "" {
		fmt.Fprintf(&b, " container %s", p.Container)
	}
	fmt.Fprintf(&b, " %s", p.Reason)
	if p.Restarts > 0 {
		fmt.Fprintf(&b, " (restarts %d)", p.Restarts)
	}
	if p.Detail != "" {
		fmt.Fprintf(&b, ": %s", p.Detail)
	}
	return b.String()
}

// badWaiting is the set of container waiting reasons that mean the
// container is broken rather than starting.
var badWaiting = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"CreateContainerConfigError": true,
	"RunContainerError":          true,
	"CreateContainerError":       true,
}

// Pods lists, with c under ctx, the pods of namespaces (all namespaces when empty) and
// returns every Problem found, sorted by namespace, pod and container. A
// pod is healthy when it is Succeeded, or Running with every container
// ready and restartCount 0. The error is non-nil only when listing fails.
// Callers decide what to do with a non-empty result; see ProblemsError.
func Pods(ctx context.Context, c wait.Clients, namespaces []string) ([]Problem, error) {
	pods, err := listPods(ctx, c, namespaces)
	if err != nil {
		return nil, err
	}
	var out []Problem
	for i := range pods {
		out = append(out, evalPod(&pods[i])...)
	}
	sortProblems(out)
	return out, nil
}

// ProblemsError turns problems into one error listing each on its own
// line under the heading what. It returns nil when problems is empty.
func ProblemsError(what string, problems []Problem) error {
	if len(problems) == 0 {
		return nil
	}
	lines := make([]string, len(problems))
	for i, p := range problems {
		lines[i] = p.String()
	}
	return fmt.Errorf("health: %s:\n  %s", what, strings.Join(lines, "\n  "))
}

// sortProblems orders ps in place by namespace, pod, container and reason.
func sortProblems(ps []Problem) {
	sort.SliceStable(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Pod != b.Pod {
			return a.Pod < b.Pod
		}
		if a.Container != b.Container {
			return a.Container < b.Container
		}
		return a.Reason < b.Reason
	})
}

// listPods lists, with c under ctx, the pods of namespaces, or of all
// namespaces when it is empty. It returns the pods, or a wrapped list error.
func listPods(ctx context.Context, c wait.Clients, namespaces []string) ([]corev1.Pod, error) {
	var out []corev1.Pod
	for _, ns := range nsOrAll(namespaces) {
		list, err := c.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("health: list pods in %q: %w", ns, err)
		}
		out = append(out, list.Items...)
	}
	return out, nil
}

// nsOrAll returns namespaces, or the all-namespaces marker when it is
// empty.
func nsOrAll(namespaces []string) []string {
	if len(namespaces) == 0 {
		return []string{metav1.NamespaceAll}
	}
	return namespaces
}

// evalPod is the pure pod check. It returns the problems of p (nil when
// healthy), covering the init containers too.
func evalPod(p *corev1.Pod) []Problem {
	if p.Status.Phase == corev1.PodSucceeded {
		return nil
	}
	var out []Problem
	add := func(container, reason, detail string, restarts int32) {
		out = append(out, Problem{Namespace: p.Namespace, Pod: p.Name, Container: container, Reason: reason, Detail: detail, Restarts: restarts})
	}
	switch p.Status.Phase {
	case corev1.PodRunning:
	case corev1.PodFailed:
		add("", orDefault(p.Status.Reason, "Failed"), p.Status.Message, 0)
	default:
		add("", orDefault(p.Status.Reason, "Pending"), pendingDetail(p), 0)
	}
	check := func(statuses []corev1.ContainerStatus, init bool) {
		for i := range statuses {
			cs := &statuses[i]
			flagged := false
			if w := cs.State.Waiting; w != nil && badWaiting[w.Reason] {
				add(cs.Name, w.Reason, w.Message, cs.RestartCount)
				flagged = true
			}
			if t := cs.LastTerminationState.Terminated; t != nil && (t.Reason == "OOMKilled" || t.Reason == "Error" || t.ExitCode != 0) {
				add(cs.Name, orDefault(t.Reason, "Terminated"), fmt.Sprintf("last exit code %d", t.ExitCode), cs.RestartCount)
				flagged = true
			}
			if cs.RestartCount > 0 && !flagged {
				add(cs.Name, "Restarted", "", cs.RestartCount)
				flagged = true
			}
			if !init && !cs.Ready && p.Status.Phase == corev1.PodRunning && !flagged {
				add(cs.Name, "NotReady", waitingDetail(cs), cs.RestartCount)
			}
		}
	}
	check(p.Status.InitContainerStatuses, true)
	check(p.Status.ContainerStatuses, false)
	return out
}

// pendingDetail describes why the pod p is not yet running, from its
// unmet conditions. It returns the empty string when none says why.
func pendingDetail(p *corev1.Pod) string {
	for _, c := range p.Status.Conditions {
		if c.Status == corev1.ConditionFalse && c.Message != "" {
			return string(c.Type) + ": " + c.Message
		}
	}
	return p.Status.Message
}

// waitingDetail describes why the container status cs is not ready. It
// returns the waiting reason, else the empty string.
func waitingDetail(cs *corev1.ContainerStatus) string {
	if w := cs.State.Waiting; w != nil {
		return w.Reason + " " + w.Message
	}
	return ""
}

// orDefault returns s, or def when s is empty.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
