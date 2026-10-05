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

package inputs

import (
	"context"
	"fmt"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// RunName returns the per-run Secret name of jobName, captf-run-<job>. Job
// names are at most 57 characters (jobs.MaxNameLength), so it always fits a
// Secret name.
func RunName(jobName string) string {
	return runPrefix + jobName
}

// CreateRun creates, through c using ctx, the per-run Secret job mounts,
// owned by job (blockOwnerDeletion unset) with exactly files' two entries.
// The Job is created first, because the ownerReference needs its UID; its
// pod waits for the Secret volume. It returns any error from those calls.
//
// An existing Secret owned by job is left as is: the Job name embeds the
// inputs hash, so a retry after a crash between the two creates renders the
// same content. One owned by anything else is left over from an earlier
// Job of the same name (pruned, its garbage collection pending): adopting
// it would let that GC delete it under the new pod, so it is replaced.
func CreateRun(ctx context.Context, c client.Client, job *batchv1.Job, files render.Files) error {
	refs, err := soleOwnerRef(c, job)
	if err != nil {
		return err
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       job.Namespace,
			Name:            RunName(job.Name),
			OwnerReferences: refs,
			Labels:          map[string]string{state.ManagedLabel: "true"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: fileData(files),
	}
	err = c.Create(ctx, s)
	if !apierrors.IsAlreadyExists(err) {
		if err != nil {
			return fmt.Errorf("inputs: create %s: %w", s.Name, err)
		}
		return nil
	}
	existing := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(s), existing); err != nil {
		return fmt.Errorf("inputs: get %s: %w", s.Name, err)
	}
	if slices.ContainsFunc(existing.OwnerReferences, func(r metav1.OwnerReference) bool { return r.UID == job.UID }) {
		return nil
	}
	err = client.IgnoreNotFound(c.Delete(ctx, existing, client.Preconditions{UID: &existing.UID}))
	if err != nil {
		return fmt.Errorf("inputs: delete stale %s: %w", s.Name, err)
	}
	if err := c.Create(ctx, s); err != nil {
		return fmt.Errorf("inputs: recreate %s: %w", s.Name, err)
	}
	return nil
}

// DeleteRun removes job's per-run Secret through c using ctx, once job has
// finished; a missing one is fine. It returns whether this call deleted it,
// which happens exactly once per Job: the caller counts the Job's
// completion then (captf_jobs_total).
func DeleteRun(ctx context.Context, c client.Client, job *batchv1.Job) (bool, error) {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: RunName(job.Name)}}
	err := c.Delete(ctx, s)
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("inputs: delete %s: %w", s.Name, err)
	}
	return true, nil
}
