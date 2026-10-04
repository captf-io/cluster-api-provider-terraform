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

package shared

import (
	"context"
	"fmt"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/captf-io/cluster-api-provider-terraform/internal/runlease"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Jobs are listed through the manager's cache, which can lag behind a Job
// this controller just created. "No Job runs" read from it would clear
// block-move (letting clusterctl move start mid-run), clear
// status.activeJob, or drop the finalizer while a Job still runs. Before
// any of those, the API server is asked directly.

// activeJobLagging reports, using ctx and the shared dependencies d,
// whether active, the Job status.activeJob of obj names, is missing from
// list (the Jobs as the cache listed them) although the API server, read
// through d.APIReader, still has it: the cache has not caught up. A Job
// the API server does not have either is gone, and clearing it is right.
// It returns any error other than NotFound from that read.
func activeJobLagging(ctx context.Context, d Deps, obj Object, active string, list []batchv1.Job) (bool, error) {
	if active == "" || slices.ContainsFunc(list, func(j batchv1.Job) bool { return j.Name == active }) {
		return false, nil
	}
	job := &batchv1.Job{}
	err := d.APIReader.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: active}, job)
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("get active job %s: %w", active, err)
	}
	return true, nil
}

// runLive reports, using ctx and the shared dependencies d, the holder of
// the run lease of the object with state suffix suffix in namespace, and
// whether that holder is a live Job (runlease.Live, read through
// d.APIReader). It returns any error from that read.
func runLive(ctx context.Context, d Deps, namespace, suffix string) (string, bool, error) {
	holder, live, err := runlease.Live(ctx, d.APIReader, namespace, runlease.RunName(suffix), d.Clock.Now())
	if err != nil {
		return "", false, fmt.Errorf("check run lease: %w", err)
	}
	return holder, live, nil
}

// leaseMatters reports whether cacheLag consults the run lease: the
// cached object carries block-move (which "no lag" clears, and which is
// written before a Job exists, so it may stand for one the caches do not
// show), or status.activeJob names a Job (which "no lag" clears), or the
// object is deleting (a pass may drop the finalizer). Otherwise nothing
// the caller does on "no lag" depends on a Job the caches have not shown,
// and starting one is the lease gate's to refuse, so the two reads per
// reconcile of every idle object are skipped.
func (r *reconciler) leaseMatters() bool {
	return HasBlockMove(r.obj) || r.st.ActiveJob.Name != "" || r.deleting
}

// cacheLag reports, using ctx, whether bk, this pass's bookkeeping, missed
// the Job status.activeJob names because the Job cache lags. status.activeJob
// comes from the same lagging object cache: block-move is written before the
// Job exists and status.activeJob in a later patch, so a pass can see the
// first and not the second. The run lease is written before the Job is
// created and read live, so the live holder of the run lease is checked too:
// a holder the Job cache does not list but the API server has, as a Job of
// this object, is a running Job the caches have not shown. A holder the API
// server does not have, or one of another object, is no lag: that wait is
// the lease gate's. The lease and its holder are read only when what
// follows a "no lag" answer can do harm (leaseMatters); an idle object
// reads neither. It returns any error reading the Job or the lease.
func (r *reconciler) cacheLag(ctx context.Context, bk *Bookkeeping) (bool, error) {
	lag, err := activeJobLagging(ctx, r.d, r.obj, r.st.ActiveJob.Name, bk.Jobs)
	if err != nil || lag || !r.leaseMatters() {
		return lag, err
	}
	holder, live, err := runLive(ctx, r.d, r.obj.GetNamespace(), r.suffix)
	if err != nil || !live || slices.ContainsFunc(bk.Jobs, func(j batchv1.Job) bool { return j.Name == holder }) {
		return false, err
	}
	job := &batchv1.Job{}
	switch err := r.d.APIReader.Get(ctx, client.ObjectKey{Namespace: r.obj.GetNamespace(), Name: holder}, job); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("get run lease holder %s: %w", holder, err)
	}
	return job.Labels[state.OwnerKindLabel] == r.k.Kind() && job.Labels[state.OwnerNameLabel] == state.LabelValue(r.obj.GetName()), nil
}
