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

package shared

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/strutil"
)

// ContainerConfigGrace is how long a Job's pod may wait on a container
// the kubelet cannot create (jobs.ContainerConfigFailed) before
// configStuck deletes the Job: a Secret created a moment late clears
// within it.
const ContainerConfigGrace = 2 * time.Minute

// configStuck deletes job, the active Job, when one of its pods (listed
// using ctx) has waited ContainerConfigGrace on a container the kubelet
// cannot create: a Secret it needs is gone, its credential mirror revoked
// or its per-run Secret deleted early. Such a pod never runs; left alone
// the Job waits for activeDeadlineSeconds, up to a day, with nothing on
// the object saying why, and holds its leases meanwhile. It releases
// status.activeJob on the API server and deletes the Job, as
// DeleteStuckJob does (it never ran, so nothing vanished), and emits
// StuckJobDeleted with the kubelet's reason; the next pass prepares the
// credentials again before it starts the next Job. It returns whether it
// deleted the Job, and any error listing pods, releasing or deleting.
func (r *reconciler) configStuck(ctx context.Context, job *batchv1.Job) (bool, error) {
	if r.d.Clock.Now().Sub(job.CreationTimestamp.Time) < ContainerConfigGrace {
		return false, nil
	}
	pods, err := r.d.Jobs.Pods(ctx, job)
	if err != nil {
		return false, fmt.Errorf("list pods of %s: %w", job.Name, err)
	}
	for i := range pods {
		p := &pods[i]
		if job.UID != "" && !metav1.IsControlledBy(p, job) {
			continue
		}
		reason, msg, ok := jobs.ContainerConfigFailed(p)
		if !ok || r.d.Clock.Now().Sub(p.CreationTimestamp.Time) < ContainerConfigGrace {
			continue
		}
		if err := r.deletePullStuck(ctx, job); err != nil {
			return false, err
		}
		klog.FromContext(ctx).Info("Deleted a Job whose pod cannot create its container; the next Job starts once its Secrets are prepared again",
			"Job", klog.KObj(job), "reason", reason)
		r.d.EmitRelated(r.obj, job, corev1.EventTypeWarning, EventStuckJobDeleted, "Run",
			"Deleted Job %s: its pod cannot create its container (%s: %s); the next Job starts once its credentials and inputs are prepared again",
			job.Name, reason, strutil.Truncate(msg, maxPullMessage))
		return true, nil
	}
	return false, nil
}
