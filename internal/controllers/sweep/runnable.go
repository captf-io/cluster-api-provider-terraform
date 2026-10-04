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

package sweep

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/captf-io/cluster-api-provider-terraform/internal/rbac"
)

// Runnable sweeps at start and every Interval while this manager leads.
type Runnable struct {
	// Reader is the uncached reader the decision goes through.
	Reader client.Reader
	// Client deletes.
	Client client.Client
	// Interval is --sync-period.
	Interval time.Duration
	// Namespace is --namespace: a namespace-scoped manager sweeps only its
	// own namespace. Empty sweeps every namespace holding a managed object.
	Namespace string
}

var (
	_ manager.Runnable               = &Runnable{}
	_ manager.LeaderElectionRunnable = &Runnable{}
)

// New builds the sweep for mgr, ticking every interval and scoped to
// namespace (empty for every namespace holding a managed object). It returns
// the Runnable ready to register with mgr.
func New(mgr manager.Manager, interval time.Duration, namespace string) *Runnable {
	return &Runnable{Reader: mgr.GetAPIReader(), Client: mgr.GetClient(), Interval: interval, Namespace: namespace}
}

// NeedLeaderElection reports whether Start should only run on the leader; it
// always returns true, since two managers sweeping would only race.
func (r *Runnable) NeedLeaderElection() bool { return true }

// Start sweeps immediately, then every Interval (with 10% jitter, sliding)
// until ctx is done. A failed sweep is logged and retried at the next tick,
// never returned: an error from a Runnable stops the manager, and a lasting
// one (a missing RBAC rule) would become a leader failover loop. It always
// returns nil.
func (r *Runnable) Start(ctx context.Context) error {
	logger := klog.LoggerWithName(klog.FromContext(ctx), "sweep")
	var namespaces []string
	if r.Namespace != "" {
		namespaces = []string{r.Namespace}
	}
	wait.JitterUntilWithContext(ctx, func(ctx context.Context) {
		if err := rbac.Sweep(ctx, r.Reader, r.Client, namespaces...); err != nil && ctx.Err() == nil {
			logger.Error(err, "Orphan sweep failed; retrying at the next resync")
		}
	}, r.Interval, 0.1, true)
	return nil
}
