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

package jobs

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Runner creates, lists and deletes an object's Jobs and lists their pods.
// It is the seam the controller's unit tests fake.
type Runner interface {
	// Create sets owner as job's controller and creates it using ctx. It
	// returns a non-nil error when either step fails.
	Create(ctx context.Context, owner client.Object, job *batchv1.Job) error
	// List returns the Jobs of owner whose owner-kind label is ownerKind
	// and whose controller is owner (by UID), read using ctx.
	List(ctx context.Context, owner client.Object, ownerKind string) ([]batchv1.Job, error)
	// Delete deletes job using ctx, and its pods in the background. It
	// returns a non-nil error only when deleting job itself fails.
	Delete(ctx context.Context, job *batchv1.Job) error
	// Pods returns the pods of job, read using ctx.
	Pods(ctx context.Context, job *batchv1.Job) ([]corev1.Pod, error)
}

// NewRunner returns a Runner. c creates, lists and deletes Jobs (they are
// cached by the manager); pods reads pods and must be uncached
// (manager.GetAPIReader), because pods are never cached.
func NewRunner(c client.Client, pods client.Reader) Runner {
	return &clientRunner{c: c, pods: pods}
}

// clientRunner is the Runner backed by a live controller-runtime client.
type clientRunner struct {
	c    client.Client
	pods client.Reader
}

// Create sets owner as job's controller and creates it through r.c using
// ctx. It returns a non-nil error when either step fails.
func (r *clientRunner) Create(ctx context.Context, owner client.Object, job *batchv1.Job) error {
	if err := controllerutil.SetControllerReference(owner, job, r.c.Scheme()); err != nil {
		return fmt.Errorf("jobs: controller reference: %w", err)
	}
	if err := r.c.Create(ctx, job); err != nil {
		return fmt.Errorf("jobs: create %s: %w", job.Name, err)
	}
	return nil
}

// List returns the Jobs of owner whose owner-kind label is ownerKind and
// whose controller is owner, read through r.c using ctx. The labels carry
// only the owner's name, so a Job of an earlier object of that name, its
// garbage collection still pending, matches them too: counting it would
// give the new object that object's attempts, backoff and outcomes.
func (r *clientRunner) List(ctx context.Context, owner client.Object, ownerKind string) ([]batchv1.Job, error) {
	var list batchv1.JobList
	if err := r.c.List(ctx, &list, client.InNamespace(owner.GetNamespace()), client.MatchingLabels{
		state.OwnerKindLabel: ownerKind,
		state.OwnerNameLabel: state.LabelValue(owner.GetName()),
	}); err != nil {
		return nil, fmt.Errorf("jobs: list: %w", err)
	}
	return slices.DeleteFunc(list.Items, func(j batchv1.Job) bool { return !metav1.IsControlledBy(&j, owner) }), nil
}

// Delete deletes job through r.c using ctx, with background propagation for
// its pods. It returns a non-nil error only when deleting job itself fails.
func (r *clientRunner) Delete(ctx context.Context, job *batchv1.Job) error {
	if err := client.IgnoreNotFound(r.c.Delete(ctx, job, client.PropagationPolicy("Background"))); err != nil {
		return fmt.Errorf("jobs: delete %s: %w", job.Name, err)
	}
	return nil
}

// Pods returns the pods of job, read through r.pods using ctx.
func (r *clientRunner) Pods(ctx context.Context, job *batchv1.Job) ([]corev1.Pod, error) {
	var list corev1.PodList
	if err := r.pods.List(ctx, &list, client.InNamespace(job.Namespace), client.MatchingLabels{batchv1.JobNameLabel: job.Name}); err != nil {
		return nil, fmt.Errorf("jobs: pods of %s: %w", job.Name, err)
	}
	return list.Items, nil
}

// Outcome of a Job.
type Outcome int

// Job outcomes.
const (
	Running Outcome = iota
	Succeeded
	Failed
)

// OutcomeOf reads job's terminal conditions and returns its Outcome.
func OutcomeOf(job *batchv1.Job) Outcome {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return Succeeded
		case batchv1.JobFailed:
			return Failed
		}
	}
	return Running
}

// FinishedAt returns when job reached its terminal condition. A failed Job
// has no status.completionTime, so the Complete or Failed condition's
// lastTransitionTime is used; the creation time is the last resort. It
// returns the zero time for a running Job.
func FinishedAt(job *batchv1.Job) time.Time {
	for _, c := range job.Status.Conditions {
		if c.Status == corev1.ConditionTrue && (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) {
			if !c.LastTransitionTime.IsZero() {
				return c.LastTransitionTime.Time
			}
			if job.Status.CompletionTime != nil {
				return job.Status.CompletionTime.Time
			}
			return job.CreationTimestamp.Time
		}
	}
	return time.Time{}
}

// DeadlineExceeded reports whether job failed on activeDeadlineSeconds.
func DeadlineExceeded(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue && c.Reason == batchv1.JobReasonDeadlineExceeded {
			return true
		}
	}
	return false
}

// OpOf returns job's operation, from its op label.
func OpOf(job *batchv1.Job) Op {
	return Op(job.Labels[OpLabel])
}

// Active returns the newest Job in jobs that has not finished.
func Active(jobs []batchv1.Job) (*batchv1.Job, bool) {
	var active *batchv1.Job
	for i := range jobs {
		if OutcomeOf(&jobs[i]) != Running {
			continue
		}
		if active == nil || jobs[i].CreationTimestamp.After(active.CreationTimestamp.Time) {
			active = &jobs[i]
		}
	}
	return active, active != nil
}

// Attempt returns the attempt number of the next Job for op, among jobs:
// the highest attempt among all retained Jobs of op, whatever their
// outcome, plus one, so a new name never equals a retained one. Counting
// only failed Jobs plus one would collide twice over: once pruning removed
// older failures (failed a1-a5, newest 3 kept: count 3 reuses a4), and with
// a retained successful Job when an apply repeats the same inputs (a drift
// remediation of an unchanged spec, or recovery from a crash between the
// Job's success and Adopt).
func Attempt(jobs []batchv1.Job, op Op) int32 {
	var highest int32
	for i := range jobs {
		j := &jobs[i]
		if j.Labels[OpLabel] != string(op) {
			continue
		}
		if a, err := strconv.ParseInt(j.Labels[AttemptLabel], 10, 32); err == nil && int32(a) > highest {
			highest = int32(a)
		}
	}
	return highest + 1
}

// Prune deletes, through r using ctx, the Jobs among jobs finished beyond
// the history limits successful and failed, per op and oldest first:
// successful Jobs beyond successfulJobsHistoryLimit and failed Jobs beyond
// failedJobsHistoryLimit (default 3 each, when the pointer is nil). The
// limits count per op, so refresh and drift history never pushes out an
// apply's. Whatever the limits, two Jobs of an op stay: its newest success,
// without which an older failure would pose as the op's latest outcome
// (ApplyJobSucceeded), and its newest failure while no success of that op
// is newer, which retry backoff and the remediation cap count. Running Jobs
// are never touched. It returns a non-nil error when a delete fails.
func Prune(ctx context.Context, r Runner, jobs []batchv1.Job, successful, failed *int32) error {
	limit := func(p *int32) int {
		if p == nil {
			return 3
		}
		return int(max(*p, 0))
	}
	limits := map[Outcome]int{Succeeded: limit(successful), Failed: limit(failed)}
	byOp := map[string][]*batchv1.Job{}
	for i := range jobs {
		if _, finished := limits[OutcomeOf(&jobs[i])]; finished {
			op := jobs[i].Labels[OpLabel]
			byOp[op] = append(byOp[op], &jobs[i])
		}
	}
	for _, op := range slices.Sorted(maps.Keys(byOp)) {
		list := byOp[op]
		// Newest first; ties by name for a stable order.
		slices.SortFunc(list, func(a, b *batchv1.Job) int {
			return cmp.Or(b.CreationTimestamp.Compare(a.CreationTimestamp.Time), cmp.Compare(a.Name, b.Name))
		})
		seen := map[Outcome]int{}
		for _, j := range list {
			o := OutcomeOf(j)
			newest := seen[o] == 0 && (o == Succeeded || seen[Succeeded] == 0)
			keep := seen[o] < limits[o] || newest
			seen[o]++
			if keep {
				continue
			}
			if err := r.Delete(ctx, j); err != nil {
				return err
			}
		}
	}
	return nil
}
