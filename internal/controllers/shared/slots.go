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
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// SlotRequeue is how long an operation that waits for a Job slot waits
// before looking again; the object's Jitter is added so waiters do not
// wake together.
const SlotRequeue = 15 * time.Second

// backgroundSharePercent is the share of a Job limit below which
// background operations (drift, refresh) start; the rest is headroom for
// apply, destroy, restore and plan.
const backgroundSharePercent = 80

// background reports whether op is a background operation, held back
// earlier than the others when Job slots run short.
func background(op jobs.Op) bool {
	return op == jobs.OpDrift || op == jobs.OpRefresh
}

// belowLimit reports whether active Jobs leave room for another of op under
// limit; 0 is no limit. Background operations need active below 80% of the
// limit, the others below all of it.
func belowLimit(active, limit int, op jobs.Op) bool {
	if limit <= 0 {
		return true
	}
	if background(op) {
		return active*100 < limit*backgroundSharePercent
	}
	return active < limit
}

// activeJobs counts, using ctx, the Jobs not yet finished that match opts,
// listed through c, normally the Job cache. It returns the count, or the
// error from the list.
func activeJobs(ctx context.Context, c client.Reader, opts ...client.ListOption) (int, error) {
	var list batchv1.JobList
	if err := c.List(ctx, &list, opts...); err != nil {
		return 0, err
	}
	n := 0
	for i := range list.Items {
		if jobs.OutcomeOf(&list.Items[i]) == jobs.Running {
			n++
		}
	}
	return n, nil
}

// clusterLimit returns the Job limit of the cluster the object belongs to:
// the TerraformCluster's spec.maxActiveJobs, else --cluster-max-active-jobs.
func (r *reconciler) clusterLimit() int {
	if r.eff.MaxActiveJobs > 0 {
		return int(r.eff.MaxActiveJobs)
	}
	return r.d.ClusterMaxActiveJobs
}

// takeJobSlot checks, using ctx, that starting a Job for op stays within
// the manager's and the cluster's Job limits. The Jobs are counted from the
// informer cache without locking, so concurrent passes may overshoot by a
// few; a Job already created is counted at once only once the cache has
// seen it. A zero leaseWait means a slot is free; otherwise the wait says
// which limit is reached, WaitingForJobSlot. The message leaves out the
// number of running Jobs, which is logged instead: it changes with every
// Job that starts or ends, and each waiter would rewrite its status for
// it. It returns that leaseWait, or the error from counting the Jobs.
func (r *reconciler) takeJobSlot(ctx context.Context, op jobs.Op) (leaseWait, error) {
	wait := func(what string, active, limit int) leaseWait {
		share := ""
		if background(op) {
			share = fmt.Sprintf(" (a %s starts below %d%% of it)", op, backgroundSharePercent)
		}
		klog.FromContext(ctx).V(LogFlow).Info("Waiting for a Job slot", "op", op, "scope", what, "active", active, "limit", limit)
		return leaseWait{
			reason:  infrav1.WaitingForJobSlotReason,
			message: fmt.Sprintf("Waiting for a Job slot: the limit of %d Jobs running %s is reached%s; the %s starts once one finishes", limit, what, share, op),
			requeue: SlotRequeue + Jitter(string(r.obj.GetUID()), SlotRequeue),
		}
	}
	if limit := r.d.MaxActiveJobs; limit > 0 {
		n, err := activeJobs(ctx, r.d.Client)
		if err != nil {
			return leaseWait{}, err
		}
		if !belowLimit(n, limit, op) {
			return wait("in all", n, limit), nil
		}
	}
	name := ClusterName(r.obj, r.owner)
	if limit := r.clusterLimit(); limit > 0 && name != "" {
		n, err := activeJobs(ctx, r.d.Client, client.InNamespace(r.obj.GetNamespace()),
			client.MatchingLabels{clusterv1.ClusterNameLabel: state.LabelValue(name)})
		if err != nil {
			return leaseWait{}, err
		}
		if !belowLimit(n, limit, op) {
			return wait("for cluster "+name, n, limit), nil
		}
	}
	return leaseWait{}, nil
}
