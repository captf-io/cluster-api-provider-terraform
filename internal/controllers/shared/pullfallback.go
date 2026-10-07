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
// the next pass that may start a Job starts the operation on it
// (ChooseImage). With none left, or no durable Secret to record it on,
// the operation's condition reports ImagePullFailed now:
// ApplyJobSucceeded for a destroy (returned, for finish), else
// DriftJobSucceeded or RestoreJobSucceeded (set here). The Job is then
// left to its deadline, unless paused: a paused object's Job is deleted
// all the same, as it can never succeed and would hold block-move, and
// with it clusterctl move, until its deadline (keepPausedPullFailure
// keeps that condition while the object stays paused).
//
// Apply and plan Jobs are never touched: they run spec.source.image, and
// only the operator can fix it. It returns whether job was deleted, the
// ApplyJobSucceeded condition to report (nil for none), and any error
// listing pods, recording the image, releasing or deleting the Job.
func (r *reconciler) pullStuck(ctx context.Context, job *batchv1.Job, paused bool) (bool, *metav1.Condition, error) {
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
		return r.pullFailedJob(ctx, job, image, reason, "", paused)
	}
	recorded, added, err := inputs.AddUnpullable(ctx, r.d.Client, r.obj, unpullable, image)
	switch {
	case errors.Is(err, inputs.ErrNotFound):
		return r.pullFailedJob(ctx, job, image, reason, "the durable inputs Secret that records unpullable images is missing, so no other image can be tried", paused)
	case err != nil:
		return false, nil, err
	}
	if r.durable != nil {
		r.durable.Unpullable = recorded
	}
	if err := r.deletePullStuck(ctx, job); err != nil {
		return false, nil, err
	}
	logger.Info("Deleted a Job whose module image cannot be pulled; the operation starts again on the next image", "next", next[0], "paused", paused)
	if added {
		retry := fmt.Sprintf("retrying %s with %s", op, next[0])
		if paused {
			retry = fmt.Sprintf("%s runs with %s once the object is unpaused", op, next[0])
		}
		r.d.EmitRelated(r.obj, job, corev1.EventTypeWarning, EventImagePullFallback, "Run",
			"Could not pull %s (%s): deleted %s Job %s; %s", image, reason, op, job.Name, retry)
	}
	return true, nil, nil
}

// pullFailedJob reports, using ctx, that job cannot pull image (the
// kubelet's reason) and that no other image is tried, why saying why (""
// for none left): pullFailed sets or returns its condition. A Job of a
// paused object is deleted (deletePullStuck), as it would hold
// clusterctl move until its deadline; otherwise it is left to fail
// there. It returns whether job was deleted, the ApplyJobSucceeded
// condition to report (nil for none), and any error releasing or
// deleting the Job.
func (r *reconciler) pullFailedJob(ctx context.Context, job *batchv1.Job, image, reason, why string, paused bool) (bool, *metav1.Condition, error) {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "Job", klog.KObj(job), "image", image, "reason", reason)
	if !paused {
		logger.Info("The Job's module image cannot be pulled, and no other image is tried; it fails at activeDeadlineSeconds")
		return false, r.pullFailed(job, image, reason, why, false), nil
	}
	if err := r.deletePullStuck(ctx, job); err != nil {
		return false, nil, err
	}
	logger.Info("Deleted a paused object's Job whose module image cannot be pulled, and no other image is tried: it would hold clusterctl move until its deadline")
	return true, r.pullFailed(job, image, reason, why, true), nil
}

// deletePullStuck releases status.activeJob on the API server, then
// deletes job, using ctx, as DeleteStuckJob does: the Job never started,
// so it must not be named there, nor recorded as a vanished apply, if the
// pass's status write is lost. It returns any error releasing or
// deleting.
func (r *reconciler) deletePullStuck(ctx context.Context, job *batchv1.Job) error {
	if err := releaseActiveJob(ctx, r.d, r.obj, job.Name); err != nil {
		return fmt.Errorf("release status.activeJob before deleting %s: %w", job.Name, err)
	}
	return r.d.Jobs.Delete(ctx, job)
}

// pausedPullNote marks the ImagePullFailed message of a Job deleted while
// its object is paused (keepPausedPullFailure).
const pausedPullNote = "it was deleted, as the object is paused and the Job would hold clusterctl move until its deadline; the operation starts again once unpaused"

// keepPausedPullFailure keeps, on a pass of a paused object, the
// ImagePullFailed conditions pullStuck set when it deleted a Job on its
// last image (pausedPullNote): with that Job gone, bookkeeping would
// otherwise report an older Job's outcome again. DriftJobSucceeded and
// RestoreJobSucceeded are set here; ApplyJobSucceeded is returned, for
// finish (nil when none). Once unpaused, the operation starts again and
// reports its own outcome.
func (r *reconciler) keepPausedPullFailure() *metav1.Condition {
	var applyCond *metav1.Condition
	for _, t := range []string{infrav1.ApplyJobSucceededCondition, infrav1.DriftJobSucceededCondition, infrav1.RestoreJobSucceededCondition} {
		c, ok := r.before[t]
		if !ok || c.Reason != infrav1.ImagePullFailedReason || !strings.Contains(c.Message, pausedPullNote) {
			continue
		}
		if t == infrav1.ApplyJobSucceededCondition {
			applyCond = &c
			continue
		}
		conditions.Set(r.obj, c)
	}
	return applyCond
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
// and will fail at its deadline, or, when deleted, that it was deleted
// while paused (pausedPullNote): why says why no other image is tried
// ("" for none left). It sets DriftJobSucceeded for a refresh or drift
// Job and RestoreJobSucceeded for a restore, False/ImagePullFailed, and
// returns ApplyJobSucceeded so for a destroy (nil otherwise), whose
// message names the ways out, Retain among them.
func (r *reconciler) pullFailed(job *batchv1.Job, image, reason, why string, deleted bool) *metav1.Condition {
	if why == "" {
		why = "no other image is left to try"
	}
	outcome := "it fails at activeDeadlineSeconds"
	if deleted {
		outcome = pausedPullNote
	}
	var fix strings.Builder
	fmt.Fprintf(&fix, "Make %s pullable again (push it back, or let the Job's pull secrets reach it)", image)
	if r.k.Mutable() {
		fix.WriteString(", or set spec.source.image to an image that can be pulled")
	}
	msg := fmt.Sprintf("Job %s: cannot pull its module image %s (%s), and %s; %s. ", job.Name, image, reason, why, outcome)
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
