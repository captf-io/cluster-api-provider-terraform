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
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Report summarizes, through the typed client of c, the non-ready pods of
// namespaces (ctx bounds the listing), one line each:
// namespace/name, phase, ready containers, restarts and the last waiting or
// terminated reason. Succeeded pods (completed Jobs) are skipped. It
// returns "no non-ready pods" when everything is ready, and a wrapped
// error when a namespace cannot be listed.
func Report(ctx context.Context, c Clients, namespaces []string) (string, error) {
	var lines []string
	for _, ns := range namespaces {
		pods, err := c.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return "", fmt.Errorf("wait: list pods in %s: %w", ns, err)
		}
		for i := range pods.Items {
			if line, bad := podLine(&pods.Items[i]); bad {
				lines = append(lines, line)
			}
		}
	}
	if len(lines) == 0 {
		return "no non-ready pods", nil
	}
	return strings.Join(lines, "\n"), nil
}

// podLine formats p as a report line. It returns the line and true when p
// is not ready, or "" and false when p is ready or Succeeded.
func podLine(p *corev1.Pod) (string, bool) {
	if p.Status.Phase == corev1.PodSucceeded {
		return "", false
	}
	statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	ready, restarts := 0, int32(0)
	reason := ""
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
	}
	for _, cs := range statuses {
		restarts += cs.RestartCount
		switch {
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
			reason = cs.Name + " waiting: " + cs.State.Waiting.Reason
		case cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason != "" && reason == "":
			reason = cs.Name + " last terminated: " + cs.LastTerminationState.Terminated.Reason
		case cs.State.Terminated != nil && cs.State.Terminated.Reason != "" && cs.State.Terminated.ExitCode != 0:
			reason = cs.Name + " terminated: " + cs.State.Terminated.Reason
		}
	}
	total := len(p.Spec.Containers)
	if p.Status.Phase == corev1.PodRunning && ready == total && total > 0 {
		return "", false
	}
	line := fmt.Sprintf("%s/%s %s ready %d/%d restarts %d", p.Namespace, p.Name, p.Status.Phase, ready, total, restarts)
	if reason == "" && p.Status.Reason != "" {
		reason = p.Status.Reason
	}
	if reason != "" {
		line += " (" + reason + ")"
	}
	return line, true
}
