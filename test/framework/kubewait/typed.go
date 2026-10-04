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

package kubewait

import (
	"context"
	"fmt"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// The labels the product puts on every Job (internal/jobs/name.go and
// internal/state/labels.go).
const (
	// OwnerKindLabel carries the owning Terraform* kind.
	OwnerKindLabel = "captf.infrastructure.cluster.x-k8s.io/owner-kind"
	// OwnerNameLabel carries the owner's name (hashed past 63 characters,
	// which short test names never reach).
	OwnerNameLabel = "captf.infrastructure.cluster.x-k8s.io/owner-name"
	// OpLabel carries the operation: apply, destroy, refresh, drift,
	// restore or plan.
	OpLabel = "captf.infrastructure.cluster.x-k8s.io/op"
)

// EventSeen waits, listing through kube, until an Event in ns regarding the object named
// involvedName has reason reason. It looks at both core/v1 Events and
// events.k8s.io/v1 Events, since the manager's recorder writes the latter
// and other components the former. o and ctx are as for Until; list errors
// are the observation and are retried. It returns nil once an Event matches.
func EventSeen(ctx context.Context, kube kubernetes.Interface, ns, involvedName, reason string, o Options) error {
	desc := fmt.Sprintf("event %s/%s reason %s", ns, involvedName, reason)
	return poll(ctx, desc, o, func(ctx context.Context) (bool, string) {
		var seen []string
		var errs []string
		list, err := kube.EventsV1().Events(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			errs = append(errs, "events.k8s.io: "+err.Error())
		} else {
			for i := range list.Items {
				e := &list.Items[i]
				if e.Regarding.Name != involvedName {
					continue
				}
				if e.Reason == reason {
					return true, "seen"
				}
				seen = append(seen, e.Reason)
			}
		}
		core, err := kube.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			errs = append(errs, "core: "+err.Error())
		} else {
			for i := range core.Items {
				e := &core.Items[i]
				if e.InvolvedObject.Name != involvedName {
					continue
				}
				if e.Reason == reason {
					return true, "seen"
				}
				seen = append(seen, e.Reason)
			}
		}
		slices.Sort(seen)
		observed := fmt.Sprintf("reasons seen for %s: %v", involvedName, slices.Compact(seen))
		if len(errs) > 0 {
			observed += "; list errors: " + strings.Join(errs, "; ")
		}
		return false, observed
	})
}

// JobsFor returns the selector of the Jobs CAPTF created for the object of
// kind ownerKind named ownerName, from its owner-kind and owner-name labels,
// narrowed to operation op (apply, destroy, ...) when op is not empty.
func JobsFor(ownerKind, ownerName, op string) labels.Selector {
	set := labels.Set{OwnerKindLabel: ownerKind, OwnerNameLabel: ownerName}
	if op != "" {
		set[OpLabel] = op
	}
	return labels.SelectorFromSet(set)
}

// finished reports whether job has a true Complete or Failed condition, and
// which one.
func finished(job *batchv1.Job) (done bool, cond batchv1.JobConditionType) {
	for _, c := range job.Status.Conditions {
		if c.Status == corev1.ConditionTrue && (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) {
			return true, c.Type
		}
	}
	return false, ""
}

// JobFinished waits, listing through kube, until the newest Job in ns matching selector (by
// creation time, then name) is Complete or Failed, and returns it; the
// caller tells the outcome from its conditions. An older finished Job does
// not count while a newer one runs. o and ctx are as for Until; list errors
// are the observation and are retried.
func JobFinished(ctx context.Context, kube kubernetes.Interface, ns string, selector labels.Selector, o Options) (*batchv1.Job, error) {
	var result *batchv1.Job
	desc := fmt.Sprintf("job in %s matching %s finished", ns, selector)
	err := poll(ctx, desc, o, func(ctx context.Context) (bool, string) {
		list, err := kube.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return false, "list failed: " + err.Error()
		}
		if len(list.Items) == 0 {
			return false, "no matching Job"
		}
		newest := &list.Items[0]
		for i := range list.Items {
			j := &list.Items[i]
			if j.CreationTimestamp.After(newest.CreationTimestamp.Time) ||
				(j.CreationTimestamp.Equal(&newest.CreationTimestamp) && j.Name > newest.Name) {
				newest = j
			}
		}
		if done, cond := finished(newest); done {
			result = newest.DeepCopy()
			return true, string(cond)
		}
		return false, fmt.Sprintf("newest Job %s running (active %d, %d matching)", newest.Name, newest.Status.Active, len(list.Items))
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
