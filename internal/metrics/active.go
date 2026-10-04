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

package metrics

import (
	"context"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	cbmetrics "k8s.io/component-base/metrics"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// scrapeTimeout bounds the Job list of one scrape.
const scrapeTimeout = 5 * time.Second

// ActiveJobs is captf_jobs_active: it counts the running Jobs by owner
// kind and op from the manager's Job cache at scrape time. A per-object
// reconcile cannot keep an aggregate gauge right, and counting up and down
// would drift across restarts. It is a component-base custom collector
// (metrics.BaseStableCollector); register it with a metrics.KubeRegistry's
// CustomRegister or CustomMustRegister, not Register.
type ActiveJobs struct {
	cbmetrics.BaseStableCollector
	desc   *cbmetrics.Desc
	mu     sync.Mutex
	reader client.Reader
}

var _ cbmetrics.StableCollector = &ActiveJobs{}

// activeDesc returns the captf_jobs_active metric descriptor, built from
// its Spec, at StabilityLevel ALPHA.
func activeDesc() *cbmetrics.Desc {
	s := spec(JobsActiveName)
	return cbmetrics.NewDesc(s.Name, s.Help, s.Labels, nil, cbmetrics.ALPHA, "")
}

// Bind sets the reader Jobs are listed from, normally the manager's cache.
// Until then the series is empty.
func (a *ActiveJobs) Bind(reader client.Reader) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reader = reader
}

// DescribeWithStability sends a's one descriptor on ch.
func (a *ActiveJobs) DescribeWithStability(ch chan<- *cbmetrics.Desc) { ch <- a.desc }

// knownKind reports whether kind is a CAPTF owner kind.
func knownKind(kind string) bool {
	switch kind {
	case state.KindTerraformCluster, state.KindTerraformMachine, state.KindTerraformMachinePool:
		return true
	}
	return false
}

// knownOp reports whether op is a jobs.Op CAPTF runs.
func knownOp(op string) bool {
	switch jobs.Op(op) {
	case jobs.OpApply, jobs.OpDestroy, jobs.OpRefresh, jobs.OpDrift, jobs.OpRestore, jobs.OpPlan:
		return true
	}
	return false
}

// CollectWithStability lists the CAPTF Jobs and sends on ch one sample per
// kind and op that has a running Job. Jobs without captf.io/managed=true,
// or whose kind or op label is not a known value, are skipped, so a forged
// Job cannot add a series. A failed list sends nothing.
func (a *ActiveJobs) CollectWithStability(ch chan<- cbmetrics.Metric) {
	a.mu.Lock()
	reader := a.reader
	a.mu.Unlock()
	if reader == nil {
		return
	}
	// Collect has no context: this is an ingress, bounded here.
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()
	list := &batchv1.JobList{}
	if err := reader.List(ctx, list, client.MatchingLabels{state.ManagedLabel: "true"}, client.HasLabels{state.OwnerKindLabel, jobs.OpLabel}); err != nil {
		return
	}
	type key struct{ kind, op string }
	counts := map[key]int{}
	for i := range list.Items {
		j := &list.Items[i]
		kind, op := j.Labels[state.OwnerKindLabel], j.Labels[jobs.OpLabel]
		if !knownKind(kind) || !knownOp(op) {
			continue
		}
		if jobs.OutcomeOf(j) == jobs.Running {
			counts[key{kind, op}]++
		}
	}
	for k, n := range counts {
		ch <- cbmetrics.NewLazyConstMetric(a.desc, cbmetrics.GaugeValue, float64(n), k.kind, k.op)
	}
}
