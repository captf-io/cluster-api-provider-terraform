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
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/captf-io/cluster-api-provider-terraform/internal/imageinspect"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Deps are the reconcilers' dependencies, built once in cmd/manager.
//
// Only jobs.Runner, state.Reader and Clock are seams; identity, rbac,
// inputs and locks are called as package functions over Client and
// APIReader, against the controller-runtime fake client in tests.
type Deps struct {
	// Client is the manager's default client. Secret, ConfigMap, Pod,
	// Lease, ServiceAccount and RoleBinding reads through it go to the API
	// server (manager.UncachedObjects), so a Secret written in this
	// reconcile is read back live.
	Client client.Client
	// APIReader is mgr.GetAPIReader(): identity, bootstrap and user
	// Secrets, Namespaces, lock Leases and the unfiltered Lists that decide
	// cleanup.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
	// Metrics records the captf_* series; nil records nothing.
	Metrics *metrics.Recorder

	// Jobs is jobs.NewRunner(Client, APIReader): pods are listed through
	// the API reader, never an informer.
	Jobs jobs.Runner
	// State is state.NewReader(Client). Reads are live (Secrets are
	// uncached), which the adopt-then-read sequence relies on.
	State state.Reader
	// Clock is the time source; unit tests fake it.
	Clock clock.PassiveClock
	// Inspector reads image configs for TerraformMachineTemplate capacity
	// (imageinspect.Remote{} in the manager).
	Inspector imageinspect.Inspector
	// Schemas caches the variables schemas the Inspector reads
	// (io.captf.variables-schema), by image digest. The admission webhook
	// reads the same cache. nil validates no variables.
	Schemas *imageinspect.SchemaCache
	// VariablesCache holds the metadata of the ConfigMaps and Secrets
	// labeled captf.io/variables=true (manager.VariablesCacheOptions), for
	// the variablesFrom watches. The
	// main cache cannot: its Secret informer is scoped to captf.io/managed,
	// and one GVK has one label selector. nil registers no such watch.
	VariablesCache ctrlcache.Cache

	// RunnerImage is the init container image that injects the runner.
	RunnerImage string
	// RunnerEvents passes the owner reference to each Job's runner, which
	// then reports its progress as events on the object (--runner-events).
	RunnerEvents bool
	// DriftDefault applies when an object sets no drift interval.
	DriftDefault time.Duration
	// WatchFilter is the cluster.x-k8s.io/watch-filter value, if any.
	WatchFilter string
	// ClusterOperationGate keeps a TerraformCluster's apply or destroy and
	// its machines' applies and destroys from running at once through the
	// cluster write lease (--cluster-operation-gate). The per-object run
	// lease is always on.
	ClusterOperationGate bool
	// StateBackups is how many state backups to keep per object
	// (--state-backups); 0 takes none. Backups are Secrets read and written
	// through Client, which reads Secrets live.
	StateBackups int
	// MaxActiveJobs caps the running Jobs across the manager
	// (--max-active-jobs); 0 is no cap.
	MaxActiveJobs int
	// ClusterMaxActiveJobs caps the running Jobs of one cluster
	// (--cluster-max-active-jobs) unless the TerraformCluster sets
	// spec.maxActiveJobs; 0 is no cap.
	ClusterMaxActiveJobs int
}
