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

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// stateLockVersion returns the resourceVersion of the state lock Lease of
// suffix in namespace, read through c using ctx, or jobs.StateLockAbsent
// when there is none: what an apply Job records at its creation
// (jobs.StateLockVersionAnnotation). It returns any other read error.
func stateLockVersion(ctx context.Context, c client.Reader, namespace, suffix string) (string, error) {
	lease := &coordinationv1.Lease{}
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: state.LeaseName(suffix)}, lease)
	switch {
	case apierrors.IsNotFound(err):
		return jobs.StateLockAbsent, nil
	case err != nil:
		return "", fmt.Errorf("get state lock %s: %w", state.LeaseName(suffix), err)
	}
	return lease.ResourceVersion, nil
}

// lockUntouched reports, using ctx and c, whether job, an apply Job, left
// the state lock Lease of suffix in namespace as it was when the Job was
// created (jobs.StateLockVersionAnnotation): absent then and now, or at
// the same resourceVersion. Terraform and OpenTofu lock the state before
// they change anything, and the lock and unlock both write the Lease, so
// an untouched Lease means the Job's runtime never got that far. A Job
// that recorded nothing, a Lease that changed and a read error are all
// false: only the evidence clears a Job.
func lockUntouched(ctx context.Context, c client.Reader, job *batchv1.Job, namespace, suffix string) bool {
	recorded := job.Annotations[jobs.StateLockVersionAnnotation]
	if recorded == "" {
		return false
	}
	now, err := stateLockVersion(ctx, c, namespace, suffix)
	return err == nil && now == recorded
}
