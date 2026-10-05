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

package health

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// Workloads checks, with the clients c under ctx, every Deployment,
// DaemonSet and StatefulSet of
// namespaces (all namespaces when empty): Deployments Available and fully
// rolled out, DaemonSets with desired, ready and updated counts equal and
// the generation observed, StatefulSets with readyReplicas equal to
// replicas. It returns nil when all are ready, else an error listing every
// workload that is not.
func Workloads(ctx context.Context, c wait.Clients, namespaces []string) error {
	var problems []string
	for _, ns := range nsOrAll(namespaces) {
		deps, err := c.Kube.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("health: list deployments in %q: %w", ns, err)
		}
		for i := range deps.Items {
			d := &deps.Items[i]
			if ok, why := deploymentReady(d); !ok {
				problems = append(problems, fmt.Sprintf("deployment %s/%s: %s", d.Namespace, d.Name, why))
			}
		}
		dss, err := c.Kube.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("health: list daemonsets in %q: %w", ns, err)
		}
		for i := range dss.Items {
			d := &dss.Items[i]
			if ok, why := daemonSetReady(d); !ok {
				problems = append(problems, fmt.Sprintf("daemonset %s/%s: %s", d.Namespace, d.Name, why))
			}
		}
		sts, err := c.Kube.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("health: list statefulsets in %q: %w", ns, err)
		}
		for i := range sts.Items {
			s := &sts.Items[i]
			if ok, why := statefulSetReady(s); !ok {
				problems = append(problems, fmt.Sprintf("statefulset %s/%s: %s", s.Namespace, s.Name, why))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("health: workloads not ready:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// deploymentReady reports whether d is Available and fully rolled out. It
// mirrors the unexported evaluation in the wait package: it returns true
// when ready, else false and a short reason.
func deploymentReady(d *appsv1.Deployment) (bool, string) {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	if d.Status.ObservedGeneration < d.Generation {
		return false, "generation not yet observed"
	}
	available := false
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			available = c.Status == corev1.ConditionTrue
		}
	}
	switch {
	case !available:
		return false, "not Available"
	case d.Status.UpdatedReplicas < want:
		return false, fmt.Sprintf("%d/%d replicas updated", d.Status.UpdatedReplicas, want)
	case d.Status.Replicas > d.Status.UpdatedReplicas:
		return false, fmt.Sprintf("%d old replicas pending termination", d.Status.Replicas-d.Status.UpdatedReplicas)
	case d.Status.AvailableReplicas < want:
		return false, fmt.Sprintf("%d/%d replicas available", d.Status.AvailableReplicas, want)
	}
	return true, ""
}

// daemonSetReady reports whether d has its generation observed and
// desired, ready and updated counts equal. It returns true when ready,
// else false and a short reason.
func daemonSetReady(d *appsv1.DaemonSet) (bool, string) {
	st := d.Status
	switch {
	case st.ObservedGeneration < d.Generation:
		return false, "generation not yet observed"
	case st.NumberReady != st.DesiredNumberScheduled:
		return false, fmt.Sprintf("%d/%d pods ready", st.NumberReady, st.DesiredNumberScheduled)
	case st.UpdatedNumberScheduled != st.DesiredNumberScheduled:
		return false, fmt.Sprintf("%d/%d pods updated", st.UpdatedNumberScheduled, st.DesiredNumberScheduled)
	}
	return true, ""
}

// statefulSetReady reports whether s has its generation observed and
// readyReplicas equal to replicas. It returns true when ready, else false
// and a short reason.
func statefulSetReady(s *appsv1.StatefulSet) (bool, string) {
	want := int32(1)
	if s.Spec.Replicas != nil {
		want = *s.Spec.Replicas
	}
	switch {
	case s.Status.ObservedGeneration < s.Generation:
		return false, "generation not yet observed"
	case s.Status.ReadyReplicas != want:
		return false, fmt.Sprintf("%d/%d replicas ready", s.Status.ReadyReplicas, want)
	}
	return true, ""
}
