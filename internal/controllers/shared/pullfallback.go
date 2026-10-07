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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
)

// PullFailureGrace is how old a destroy, refresh, drift or restore Job
// must be before pullStuck acts on its module image failing to pull: a
// registry hiccup clears within it, and the kubelet retries meanwhile.
const PullFailureGrace = 2 * time.Minute

// pullRunbook is the runbook section on image pull failures.
const pullRunbook = "https://captf.io/docs/operator-guide/runbooks/job-failures.html#image-pull-failures"

// pullStuck handles job, the active Job, when it is a destroy, refresh,
// drift or restore Job at least PullFailureGrace old whose module image
// cannot be pulled (jobs.SourcePullFailed on one of its pods, listed
// using ctx): a pinned digest the registry garbage collected would
// otherwise hold it until activeDeadlineSeconds, again on every retry.
//
// With an image left to fall back to (ImageFallbacksAnnotation, then
// spec.source.image as it is now), the Job's image is recorded on the
// durable Secret as unpullable (inputs.AddUnpullable), status.activeJob
// is released on the API server and the Job deleted, as DeleteStuckJob
// does, and the Warning ImagePullFallback says which image runs next;
// the next pass starts the operation on it (ChooseImage). With none
// left, or no durable Secret to record it on, the Job is left to its
// deadline and the operation's condition reports ImagePullFailed now:
// ApplyJobSucceeded for a destroy (returned, for finish), else
// DriftJobSucceeded or RestoreJobSucceeded (set here).
//
// Apply and plan Jobs are never touched: they run spec.source.image, and
// only the operator can fix it. It returns whether job was deleted, the
// ApplyJobSucceeded condition to report (nil for none), and any error
// listing pods, recording the image, releasing or deleting the Job.
func (r *reconciler) pullStuck(ctx context.Context, job *batchv1.Job) (bool, *metav1.Condition, error) {
	op := jobs.OpOf(job)
	// A Job with a ready pod pulled its image: its pods are not read.
	if op == jobs.OpApply || op == jobs.OpPlan || r.d.Clock.Now().Sub(job.CreationTimestamp.Time) < PullFailureGrace ||
		(job.Status.Ready != nil && *job.Status.Ready > 0) {
		return false, nil, nil
	}
	pods, err := r.d.Jobs.Pods(ctx, job)
	if err != nil {
		return false, nil, fmt.Errorf("list pods of %s: %w", job.Name, err)
	}
	reason := ""
	for i := range pods {
		// A pod of an earlier Job of this name (deleted here, its pods
		// still going) is not this Job's.
		if job.UID != "" && !metav1.IsControlledBy(&pods[i], job) {
			continue
		}
		if why, ok := jobs.SourcePullFailed(&pods[i]); ok {
			reason = why
			break
		}
	}
	if reason == "" {
		return false, nil, nil
	}
	image := jobs.SourceImage(job)
	var unpullable []string
	if r.durable != nil {
		unpullable = r.durable.Unpullable
	}
	next := r.pullFallbacks(job, image, unpullable)
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "Job", klog.KObj(job), "image", image, "reason", reason)
	if len(next) == 0 {
		logger.Info("The Job's module image cannot be pulled, and no other image is left to try; it fails at activeDeadlineSeconds")
		return false, r.pullFailed(job, image, reason, ""), nil
	}
	recorded, added, err := inputs.AddUnpullable(ctx, r.d.Client, r.obj, unpullable, image)
	switch {
	case errors.Is(err, inputs.ErrNotFound):
		logger.Info("The Job's module image cannot be pulled, but the durable inputs Secret that records it is missing; it fails at activeDeadlineSeconds")
		return false, r.pullFailed(job, image, reason, "the durable inputs Secret that records unpullable images is missing, so no other image can be tried"), nil
	case err != nil:
		return false, nil, err
	}
	if r.durable != nil {
		r.durable.Unpullable = recorded
	}
	if err := releaseActiveJob(ctx, r.d, r.obj, job.Name); err != nil {
		return false, nil, fmt.Errorf("release status.activeJob before deleting %s: %w", job.Name, err)
	}
	if err := r.d.Jobs.Delete(ctx, job); err != nil {
		return false, nil, err
	}
	logger.Info("Deleted a Job whose module image cannot be pulled; the operation starts again on the next image", "next", next[0])
	if added {
		r.d.EmitRelated(r.obj, job, corev1.EventTypeWarning, EventImagePullFallback, "Run",
			"Could not pull %s (%s): deleted %s Job %s; retrying %s with %s", image, reason, op, job.Name, op, next[0])
	}
	return true, nil, nil
}

// pullFallbacks returns the images job, which runs image, falls back to
// in order: those it recorded when it started (ImageFallbacksAnnotation),
// then spec.source.image as it is now (an operator may have fixed it
// since), less image itself and unpullable, the images already known not
// to pull.
func (r *reconciler) pullFallbacks(job *batchv1.Job, image string, unpullable []string) []string {
	var recorded []string
	if raw := job.Annotations[ImageFallbacksAnnotation]; raw != "" {
		// Unparsable, it leaves only the spec image to fall back to.
		_ = json.Unmarshal([]byte(raw), &recorded)
	}
	var out []string
	for _, ref := range append(recorded, r.k.Spec().Source.Image) {
		if ref != "" && ref != image && !slices.Contains(unpullable, ref) && !slices.Contains(out, ref) {
			out = append(out, ref)
		}
	}
	return out
}

// pullFailed reports that job cannot pull image (the kubelet's reason)
// and will fail at its deadline: why says why no other image is tried
// ("" for none left). It sets DriftJobSucceeded for a refresh or drift
// Job and RestoreJobSucceeded for a restore, False/ImagePullFailed, and
// returns ApplyJobSucceeded so for a destroy (nil otherwise), whose
// message names the ways out, Retain among them.
func (r *reconciler) pullFailed(job *batchv1.Job, image, reason, why string) *metav1.Condition {
	if why == "" {
		why = "no other image is left to try"
	}
	var fix strings.Builder
	fmt.Fprintf(&fix, "Make %s pullable again (push it back, or let the Job's pull secrets reach it)", image)
	if r.k.Mutable() {
		fix.WriteString(", or set spec.source.image to an image that can be pulled")
	}
	msg := fmt.Sprintf("Job %s: cannot pull its module image %s (%s), and %s; it fails at activeDeadlineSeconds. ",
		job.Name, image, reason, why)
	op := jobs.OpOf(job)
	c := metav1.Condition{Status: metav1.ConditionFalse, Reason: infrav1.ImagePullFailedReason}
	switch op {
	case jobs.OpDestroy:
		c.Type = infrav1.ApplyJobSucceededCondition
		c.Message = msg + fix.String() + "; or set " + retainHint + "; see " + pullRunbook
		return &c
	case jobs.OpRestore:
		c.Type = infrav1.RestoreJobSucceededCondition
	default:
		c.Type = infrav1.DriftJobSucceededCondition
	}
	c.Message = msg + fix.String() + "; see " + pullRunbook
	conditions.Set(r.obj, c)
	return nil
}
