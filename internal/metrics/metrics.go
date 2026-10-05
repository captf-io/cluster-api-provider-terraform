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

package metrics

import (
	"errors"
	"fmt"
	"slices"
	"time"

	apimachineryversion "k8s.io/apimachinery/pkg/version"
	cbmetrics "k8s.io/component-base/metrics"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Metric names.
const (
	BuildInfoName           = "captf_build_info"
	JobsTotalName           = "captf_jobs_total"
	JobDurationName         = "captf_job_duration_seconds"
	JobStepDurationName     = "captf_job_step_duration_seconds"
	JobQueueName            = "captf_job_queue_seconds"
	JobErrorsName           = "captf_job_errors_total"
	JobsActiveName          = "captf_jobs_active"
	JobAttemptsName         = "captf_job_attempts"
	ResourcesChangedName    = "captf_resources_changed_total"
	DriftResourcesName      = "captf_drift_resources_total"
	DecisionsName           = "captf_reconcile_op_decisions_total"
	StateReadErrorsName     = "captf_state_read_errors_total"
	OutputsInvalidName      = "captf_outputs_invalid_total"
	DriftDetectedName       = "captf_drift_detected"
	ReadyName               = "captf_ready"
	InfraHealthyName        = "captf_infrastructure_healthy"
	StateResourcesName      = "captf_state_resources"
	StateBytesName          = "captf_state_bytes"
	InputsBytesName         = "captf_inputs_bytes"
	LastSuccessName         = "captf_last_success_timestamp_seconds"
	UnhealthySamplesName    = "captf_unhealthy_samples"
	ForceUnlocksName        = "captf_lock_force_unlocks_total"
	IdentityDeniedName      = "captf_identity_denied_total"
	ImageInspectName        = "captf_image_inspect_errors_total"
	HashChangesName         = "captf_inputs_hash_changes_total"
	RemediationRequestsName = "captf_remediation_requests_total"
	ApprovalsConsumedName   = "captf_destructive_plan_approvals_consumed_total"
	LeaseWaitsName          = "captf_lease_waits_total"
	StateBackupsName        = "captf_state_backups_total"
	StateRestoresName       = "captf_state_restores_total"
	PlanApprovalsName       = "captf_plan_approvals_total"
)

// Plan approval results, the result label of captf_plan_approvals_total.
const (
	// PlanApproved: the apply of an approved plan succeeded.
	PlanApproved = "approved"
	// PlanChanged: an approved apply planned other changes and stopped.
	PlanChanged = "changed"
)

// State backup results, the result label of captf_state_backups_total.
const (
	// BackupTaken: a new state serial was copied into a backup.
	BackupTaken = "taken"
	// BackupPruned: a backup beyond --state-backups was deleted.
	BackupPruned = "pruned"
	// BackupSkipped: a new serial was not backed up (encrypted, corrupt,
	// inconsistent or oversized state, or a failed copy).
	BackupSkipped = "skipped"
)

// RestoreNotFound is the captf_state_restores_total result of a restore
// request naming no backup; a restore Job's own result is ResultSucceeded
// or ResultFailed.
const RestoreNotFound = "not_found"

// Lease wait reasons, the reason label of captf_lease_waits_total.
const (
	// LeaseWaitRunLease: the object's run lease is held by another live Job.
	LeaseWaitRunLease = "run_lease"
	// LeaseWaitClusterOperation: a machine's apply or destroy waits for its
	// TerraformCluster's.
	LeaseWaitClusterOperation = "cluster_operation"
	// LeaseWaitMachineOperations: a TerraformCluster's apply or destroy
	// waits for its machines' in flight.
	LeaseWaitMachineOperations = "machine_operations"
)

// Job results, the result label of captf_jobs_total and
// captf_job_duration_seconds.
const (
	ResultSucceeded = "succeeded"
	ResultFailed    = "failed"
	ResultDeadline  = "deadline"
	// ResultBlocked is a guarded apply (a TerraformCluster's, or a
	// TerraformMachinePool's of a change of the cluster's exports) stopped
	// before a plan that deletes or replaces resources, for want of an
	// approval.
	ResultBlocked = "blocked"
	// ResultInterrupted is a Job the runner reported stopped from outside
	// (SIGTERM: a drain, eviction or deletion) before its deadline.
	ResultInterrupted = "interrupted"
	// ResultPlanChanged is a TerraformCluster apply approved for one plan
	// that planned other changes and stopped before applying them.
	ResultPlanChanged = "plan_changed"
)

// Error kinds of captf_job_errors_total besides the runner's own (step,
// image-layout, interrupted, blocked): a failed Job without a result.
const (
	ErrorKindDeadline = "deadline"
	ErrorKindUnknown  = "unknown"
	// ErrorStepNone is the step label of an error no step reported.
	ErrorStepNone = "none"
)

// Actions, the action label of the resource counters.
const (
	ActionAdd     = "add"
	ActionChange  = "change"
	ActionDestroy = "destroy"
	ActionImport  = "import"
)

// Remediation request actions, the action label of
// captf_remediation_requests_total.
const (
	RemediationRequested = "requested"
	RemediationWithdrawn = "withdrawn"
)

// Spec describes one series; Specs is the table every series is built from.
type Spec struct {
	Name   string
	Type   string
	Labels []string
	Help   string
}

// PerObject are the per-object gauges: the only series with namespace and
// name labels.
var PerObject = []string{
	DriftDetectedName, ReadyName, InfraHealthyName, StateResourcesName, StateBytesName,
	InputsBytesName, LastSuccessName, UnhealthySamplesName,
}

// Specs returns every captf_* series in the order they are registered.
func Specs() []Spec {
	return []Spec{
		{JobsTotalName, "counter", []string{"kind", "op", "result"}, "Jobs completed, by result: succeeded, failed, deadline, interrupted (stopped from outside: a drain, eviction or deletion), blocked (a guarded apply, of a TerraformCluster or of a TerraformMachinePool's change of the cluster's exports, stopped before a plan that deletes or replaces resources, awaiting approval) or plan_changed (an apply approved for one plan planned other changes and stopped, applyPolicy Manual). Op plan is a plan Job that applies nothing."},
		{JobDurationName, "histogram", []string{"kind", "op", "result"}, "Wall time of a completed Job, from start to finish, by the result of captf_jobs_total."},
		{JobStepDurationName, "histogram", []string{"kind", "op", "step"}, "Wall time of each runner step of a completed Job (init, force-unlock, validate, plan, show-json, apply, apply-refresh-only, destroy, state-push, state-list, prepare; anything else is other)."},
		{JobQueueName, "histogram", []string{"kind", "op"}, "Time from a Job's creation to its source container's start: scheduling, image pulls and the runner copy. Not observed when the pod reports no start."},
		{JobErrorsName, "counter", []string{"kind", "op", "error_kind", "step"}, "Jobs that did not succeed, by the runner's error kind (step, image-layout, interrupted, blocked, plan-changed; deadline or unknown without a result) and failing step (none when no step failed)."},
		{JobsActiveName, "gauge", []string{"kind", "op"}, "Jobs currently running, counted from the Job cache at scrape time."},
		{JobAttemptsName, "histogram", []string{"kind", "op"}, "Retry number of a Job that succeeded: 1 plus the failed Jobs of its op since that op last succeeded (interrupted Jobs do not count)."},
		{ResourcesChangedName, "counter", []string{"kind", "op", "action"}, "Resources an apply or destroy Job changed, by action (add, change, destroy, import), from the runtime's final summary line."},
		{DriftResourcesName, "counter", []string{"kind", "action"}, "Resources a drift Job that detected drift found to add, change or destroy."},
		{DecisionsName, "counter", []string{"kind", "op", "reason"}, "What the reconcile decided to run (op none: nothing) and why."},
		{StateReadErrorsName, "counter", []string{"kind", "reason"}, "State reads that turned unreadable: inconsistent, encrypted or corrupt (an unsupported state version counts as corrupt), lost (a provisioned object's state is gone) or locked (held by a holder that is not this object's runner)."},
		{OutputsInvalidName, "counter", []string{"kind", "reason"}, "Outputs that turned invalid against the module contract."},
		{DriftDetectedName, "gauge", []string{"kind", "namespace", "name"}, "1 while DriftDetected is True, else 0."},
		{ReadyName, "gauge", []string{"kind", "namespace", "name"}, "1, 0 or -1 for a True, False or Unknown Ready condition."},
		{InfraHealthyName, "gauge", []string{"kind", "namespace", "name"}, "1, 0 or -1 for a True, False or Unknown InfrastructureHealthy condition."},
		{StateResourcesName, "gauge", []string{"kind", "namespace", "name"}, "Managed resources (not data sources) in the object's state, as last read."},
		{StateBytesName, "gauge", []string{"kind", "namespace", "name"}, "Compressed size of the object's state summed over its Secrets, as last read; the kubernetes backend holds at most 1 MiB per Secret."},
		{InputsBytesName, "gauge", []string{"kind", "namespace", "name"}, "Size of the object's rendered main.tf.json and terraform.tfvars.json (the durable inputs, or the last render); no Job starts above 1000000."},
		{LastSuccessName, "gauge", []string{"kind", "namespace", "name", "op"}, "Unix time the newest successful Job of an op finished. Drift and refresh are exported only while that op is scheduled (not deleting or paused, with a drift interval or health checks)."},
		{UnhealthySamplesName, "gauge", []string{"namespace", "name"}, "A TerraformMachine's consecutive unhealthy health samples (status.unhealthySamples)."},
		{ForceUnlocksName, "counter", []string{"kind"}, "Stale state locks force-unlocked."},
		{IdentityDeniedName, "counter", []string{"reason"}, "Identity refusals: notfound or namespace."},
		{ImageInspectName, "counter", []string{"reason"}, "Registry or image-label failures while resolving template capacity."},
		{HashChangesName, "counter", []string{"kind"}, "Applies started because the inputs of a mutable kind changed."},
		{RemediationRequestsName, "counter", []string{"action"}, "cluster.x-k8s.io/remediate-machine annotations set on (requested) or removed from (withdrawn) a Machine."},
		{ApprovalsConsumedName, "counter", []string{"kind"}, "Destructive-plan approvals removed after the approved apply succeeded."},
		{LeaseWaitsName, "counter", []string{"kind", "reason"}, "Operations that started waiting for a run lease, once per wait: run_lease (another live Job of the object holds it), cluster_operation (a machine's apply or destroy waits for its TerraformCluster's) or machine_operations (a TerraformCluster's apply or destroy waits for its machines')."},
		{StateBackupsName, "counter", []string{"kind", "result"}, "State backups: taken (a new state serial copied into captf-state-backup-* Secrets), pruned (a backup beyond --state-backups deleted) or skipped (a new serial not backed up: encrypted, unreadable or oversized state, or a failed copy)."},
		{StateRestoresName, "counter", []string{"kind", "result"}, "State restores requested with captf.io/restore-state: succeeded or failed (a restore Job finished), or not_found (the annotation names no backup)."},
		{PlanApprovalsName, "counter", []string{"kind", "result"}, "Plans approved with captf.io/approve-plan (applyPolicy Manual): approved (the apply of the approved plan succeeded and the annotation was removed) or changed (the approved apply planned other changes and stopped; the new plan waits for approval)."},
		{BuildInfoName, "gauge", []string{"version", "commit", "contract"}, "A metric with a constant '1' value labeled by the version, commit and module contract of the manager."},
	}
}

// spec returns the Spec named name from Specs, and panics if none matches:
// every collector built from a name is built from a name Specs lists.
func spec(name string) Spec {
	for _, s := range Specs() {
		if s.Name == name {
			return s
		}
	}
	panic("metrics: no spec " + name)
}

// counter returns a CounterVec for the series named name, with its Specs
// help text and labels, ALPHA stability.
func counter(name string) *cbmetrics.CounterVec {
	s := spec(name)
	return cbmetrics.NewCounterVec(&cbmetrics.CounterOpts{Name: s.Name, Help: s.Help, StabilityLevel: cbmetrics.ALPHA}, s.Labels)
}

// gauge returns a GaugeVec for the series named name, with its Specs help
// text and labels, ALPHA stability.
func gauge(name string) *cbmetrics.GaugeVec {
	s := spec(name)
	return cbmetrics.NewGaugeVec(&cbmetrics.GaugeOpts{Name: s.Name, Help: s.Help, StabilityLevel: cbmetrics.ALPHA}, s.Labels)
}

// histogram returns a HistogramVec for the series named name, with buckets
// and its Specs help text and labels, ALPHA stability.
func histogram(name string, buckets []float64) *cbmetrics.HistogramVec {
	s := spec(name)
	return cbmetrics.NewHistogramVec(&cbmetrics.HistogramOpts{Name: s.Name, Help: s.Help, Buckets: buckets, StabilityLevel: cbmetrics.ALPHA}, s.Labels)
}

// Register registers the build-information gauge on reg, set to 1 with the
// GitVersion and GitCommit of info (k8s.io/component-base/version.Get())
// and the module contract, contract.Version, and returns any registration
// error.
func Register(reg cbmetrics.KubeRegistry, info apimachineryversion.Info) error {
	buildInfo := gauge(BuildInfoName)
	// Register (which Creates the lazily instantiated GaugeVec) must run
	// before WithLabelValues: called on an uncreated series, it returns a
	// no-op that silently drops the Set.
	if err := reg.Register(buildInfo); err != nil {
		return fmt.Errorf("metrics: register %s: %w", BuildInfoName, err)
	}
	buildInfo.WithLabelValues(info.GitVersion, info.GitCommit, contract.Version).Set(1)
	return nil
}

// ErrAlreadyRegistered is the error (*Recorder).Register returns when
// called a second time on the same Recorder: its active-jobs collector, a
// component-base StableCollector, cannot be Create'd twice.
var ErrAlreadyRegistered = errors.New("metrics: recorder already registered")

// Recorder holds the series the reconcilers update.
type Recorder struct {
	jobsTotal         *cbmetrics.CounterVec
	jobDuration       *cbmetrics.HistogramVec
	stepDuration      *cbmetrics.HistogramVec
	queue             *cbmetrics.HistogramVec
	jobErrors         *cbmetrics.CounterVec
	jobAttempts       *cbmetrics.HistogramVec
	resourcesChanged  *cbmetrics.CounterVec
	driftResources    *cbmetrics.CounterVec
	decisions         *cbmetrics.CounterVec
	stateReadErrors   *cbmetrics.CounterVec
	outputsInvalid    *cbmetrics.CounterVec
	driftDetected     *cbmetrics.GaugeVec
	ready             *cbmetrics.GaugeVec
	infraHealthy      *cbmetrics.GaugeVec
	stateResources    *cbmetrics.GaugeVec
	stateBytes        *cbmetrics.GaugeVec
	inputsBytes       *cbmetrics.GaugeVec
	lastSuccess       *cbmetrics.GaugeVec
	unhealthySamples  *cbmetrics.GaugeVec
	forceUnlocks      *cbmetrics.CounterVec
	identityDenied    *cbmetrics.CounterVec
	imageInspect      *cbmetrics.CounterVec
	hashChanges       *cbmetrics.CounterVec
	remediations      *cbmetrics.CounterVec
	approvalsConsumed *cbmetrics.CounterVec
	leaseWaits        *cbmetrics.CounterVec
	stateBackups      *cbmetrics.CounterVec
	stateRestores     *cbmetrics.CounterVec
	planApprovals     *cbmetrics.CounterVec
	active            *ActiveJobs
	// registered guards against a second Register: r.active, a component-
	// base StableCollector, panics if Create runs on it twice.
	registered bool
}

// New builds and returns a Recorder; Register it before use.
func New() *Recorder {
	return &Recorder{
		jobsTotal: counter(JobsTotalName),
		// 15s … 2h: a Job is at least an init.
		jobDuration: histogram(JobDurationName, cbmetrics.ExponentialBucketsRange(15, 7200, 10)),
		// 1s … 2h: a validate takes a second, an apply up to the deadline.
		stepDuration: histogram(JobStepDurationName, cbmetrics.ExponentialBucketsRange(1, 7200, 12)),
		// 300 s, the CAPTFJobQueueSlow threshold, is a boundary.
		queue:             histogram(JobQueueName, []float64{5, 10, 20, 30, 60, 120, 180, 300, 600, 900, 1800}),
		jobErrors:         counter(JobErrorsName),
		jobAttempts:       histogram(JobAttemptsName, cbmetrics.LinearBuckets(1, 1, 10)),
		resourcesChanged:  counter(ResourcesChangedName),
		driftResources:    counter(DriftResourcesName),
		decisions:         counter(DecisionsName),
		stateReadErrors:   counter(StateReadErrorsName),
		outputsInvalid:    counter(OutputsInvalidName),
		driftDetected:     gauge(DriftDetectedName),
		ready:             gauge(ReadyName),
		infraHealthy:      gauge(InfraHealthyName),
		stateResources:    gauge(StateResourcesName),
		stateBytes:        gauge(StateBytesName),
		inputsBytes:       gauge(InputsBytesName),
		lastSuccess:       gauge(LastSuccessName),
		unhealthySamples:  gauge(UnhealthySamplesName),
		forceUnlocks:      counter(ForceUnlocksName),
		identityDenied:    counter(IdentityDeniedName),
		imageInspect:      counter(ImageInspectName),
		hashChanges:       counter(HashChangesName),
		remediations:      counter(RemediationRequestsName),
		approvalsConsumed: counter(ApprovalsConsumedName),
		leaseWaits:        counter(LeaseWaitsName),
		stateBackups:      counter(StateBackupsName),
		stateRestores:     counter(StateRestoresName),
		planApprovals:     counter(PlanApprovalsName),
		active:            &ActiveJobs{desc: activeDesc()},
	}
}

// Register registers every series of r except captf_build_info on reg
// (r.active, the active-jobs collector, through reg.CustomRegister), and
// returns any registration error.
func (r *Recorder) Register(reg cbmetrics.KubeRegistry) error {
	// r.active is a component-base StableCollector: Create (run by
	// CustomRegister) panics if it runs a second time on the same
	// instance, so a second Register must error out before reaching it.
	if r.registered {
		return ErrAlreadyRegistered
	}
	for _, c := range []cbmetrics.Registerable{
		r.jobsTotal, r.jobDuration, r.stepDuration, r.queue, r.jobErrors, r.jobAttempts,
		r.resourcesChanged, r.driftResources, r.decisions, r.stateReadErrors, r.outputsInvalid,
		r.driftDetected, r.ready, r.infraHealthy, r.stateResources, r.stateBytes, r.inputsBytes,
		r.lastSuccess, r.unhealthySamples, r.forceUnlocks, r.identityDenied, r.imageInspect,
		r.hashChanges, r.remediations, r.approvalsConsumed, r.leaseWaits, r.stateBackups, r.stateRestores, r.planApprovals,
	} {
		if err := reg.Register(c); err != nil {
			return fmt.Errorf("metrics: register: %w", err)
		}
	}
	if err := reg.CustomRegister(r.active); err != nil {
		return fmt.Errorf("metrics: register: %w", err)
	}
	r.registered = true
	return nil
}

// ActiveJobs returns r's captf_jobs_active collector.
func (r *Recorder) ActiveJobs() *ActiveJobs {
	if r == nil {
		return nil
	}
	return r.active
}

// Job is one finished Job, as JobFinished records it. Every string is a
// bounded label value; the caller maps anything else (StepLabel).
type Job struct {
	Kind, Op, Result string
	// Duration is the Job's wall time; 0 when unknown.
	Duration time.Duration
	// Queue is creation to the source container's start; 0 when unknown.
	Queue time.Duration
	// Attempt is the retry number of a succeeded Job; 0 when unknown.
	Attempt int
	// Steps are the runner's steps, from its result.
	Steps []Step
	// ErrorKind and ErrorStep label captf_job_errors_total for a Job that
	// did not succeed; "" becomes unknown and none.
	ErrorKind, ErrorStep string
	// Changes is what an apply or destroy changed; nil when not reported.
	Changes *Changes
	// Drift is what a drift Job that detected drift found; nil otherwise.
	Drift *Changes
}

// Step is one runner step.
type Step struct {
	Name    string
	Seconds float64
}

// Changes counts resources by action.
type Changes struct {
	Add, Change, Destroy, Import int
}

// byAction returns c's counts keyed by their action label.
func (c Changes) byAction() map[string]int {
	return map[string]int{ActionAdd: c.Add, ActionChange: c.Change, ActionDestroy: c.Destroy, ActionImport: c.Import}
}

// JobFinished records j, a Job seen finished for the first time: its
// result, wall time, queue time, steps, error, the resources it changed
// and, on success, its attempt number. The caller calls it once per Job.
func (r *Recorder) JobFinished(j Job) {
	if r == nil {
		return
	}
	r.jobsTotal.WithLabelValues(j.Kind, j.Op, j.Result).Inc()
	if j.Duration > 0 {
		r.jobDuration.WithLabelValues(j.Kind, j.Op, j.Result).Observe(j.Duration.Seconds())
	}
	if j.Queue > 0 {
		r.queue.WithLabelValues(j.Kind, j.Op).Observe(j.Queue.Seconds())
	}
	for _, s := range j.Steps {
		if s.Seconds >= 0 {
			r.stepDuration.WithLabelValues(j.Kind, j.Op, s.Name).Observe(s.Seconds)
		}
	}
	if j.Result == ResultSucceeded {
		if j.Attempt > 0 {
			r.jobAttempts.WithLabelValues(j.Kind, j.Op).Observe(float64(j.Attempt))
		}
	} else {
		kind, step := j.ErrorKind, j.ErrorStep
		if kind == "" {
			kind = ErrorKindUnknown
		}
		if step == "" {
			step = ErrorStepNone
		}
		r.jobErrors.WithLabelValues(j.Kind, j.Op, kind, step).Inc()
	}
	if j.Changes != nil {
		for action, n := range j.Changes.byAction() {
			if n > 0 {
				r.resourcesChanged.WithLabelValues(j.Kind, j.Op, action).Add(float64(n))
			}
		}
	}
	if j.Drift != nil {
		for action, n := range j.Drift.byAction() {
			if n > 0 && action != ActionImport {
				r.driftResources.WithLabelValues(j.Kind, action).Add(float64(n))
			}
		}
	}
}

// Decision records one reconcile decision of kind, running op for reason;
// op is "none" when nothing runs.
func (r *Recorder) Decision(kind, op, reason string) {
	if r != nil {
		r.decisions.WithLabelValues(kind, op, reason).Inc()
	}
}

// StateReadError records a state of kind that turned unreadable, for
// reason.
func (r *Recorder) StateReadError(kind, reason string) {
	if r != nil {
		r.stateReadErrors.WithLabelValues(kind, reason).Inc()
	}
}

// OutputsInvalid records outputs of kind that turned invalid, for reason.
func (r *Recorder) OutputsInvalid(kind, reason string) {
	if r != nil {
		r.outputsInvalid.WithLabelValues(kind, reason).Inc()
	}
}

// Object is the condition-derived per-object gauges: Ready and Healthy are
// 1, 0 or -1 for True, False or Unknown.
type Object struct {
	Ready, Healthy float64
	Drift          bool
}

// SetObject sets the condition-derived per-object gauges of o, for the
// object of kind named name in namespace.
func (r *Recorder) SetObject(kind, namespace, name string, o Object) {
	if r == nil {
		return
	}
	r.ready.WithLabelValues(kind, namespace, name).Set(o.Ready)
	r.infraHealthy.WithLabelValues(kind, namespace, name).Set(o.Healthy)
	d := 0.0
	if o.Drift {
		d = 1
	}
	r.driftDetected.WithLabelValues(kind, namespace, name).Set(d)
}

// SetState sets the state gauges, resources and bytes, of a state read for
// the object of kind named name in namespace.
func (r *Recorder) SetState(kind, namespace, name string, resources, bytes int) {
	if r == nil {
		return
	}
	r.stateResources.WithLabelValues(kind, namespace, name).Set(float64(resources))
	r.stateBytes.WithLabelValues(kind, namespace, name).Set(float64(bytes))
}

// SetInputsBytes sets the rendered inputs' size to n bytes, for the object
// of kind named name in namespace.
func (r *Recorder) SetInputsBytes(kind, namespace, name string, n int) {
	if r != nil {
		r.inputsBytes.WithLabelValues(kind, namespace, name).Set(float64(n))
	}
}

// SetLastSuccess sets to t when op's newest successful Job finished, for
// the object of kind named name in namespace.
func (r *Recorder) SetLastSuccess(kind, namespace, name, op string, t time.Time) {
	if r != nil {
		r.lastSuccess.WithLabelValues(kind, namespace, name, op).Set(float64(t.Unix()))
	}
}

// DeleteLastSuccess removes op's last-success series of the object of kind
// named name in namespace: no success of op is expected now.
func (r *Recorder) DeleteLastSuccess(kind, namespace, name, op string) {
	if r != nil {
		r.lastSuccess.DeleteLabelValues(kind, namespace, name, op)
	}
}

// SetUnhealthySamples sets to n the unhealthy sample count of the
// TerraformMachine named name in namespace.
func (r *Recorder) SetUnhealthySamples(namespace, name string, n int32) {
	if r != nil {
		r.unhealthySamples.WithLabelValues(namespace, name).Set(float64(n))
	}
}

// DeleteObject removes every per-object gauge of the deleted object of
// kind named name in namespace.
func (r *Recorder) DeleteObject(kind, namespace, name string) {
	if r == nil {
		return
	}
	match := map[string]string{"kind": kind, "namespace": namespace, "name": name}
	for _, g := range []*cbmetrics.GaugeVec{
		r.ready, r.driftDetected, r.infraHealthy, r.stateResources, r.stateBytes, r.inputsBytes, r.lastSuccess,
	} {
		// DeletePartialMatch is promoted from the embedded raw
		// *prometheus.GaugeVec, which before Create (Register) is a bare
		// noopGaugeVec{} whose internal MetricVec is nil: guard it, like
		// every other write here, against an unregistered Recorder.
		if g.IsCreated() {
			g.DeletePartialMatch(match)
		}
	}
	// It has no kind label: a TerraformCluster of the same name must not
	// take a machine's series with it.
	if kind == state.KindTerraformMachine {
		r.unhealthySamples.DeleteLabelValues(namespace, name)
	}
}

// ForceUnlock records a stale lock force-unlock of an object of kind.
func (r *Recorder) ForceUnlock(kind string) {
	if r != nil {
		r.forceUnlocks.WithLabelValues(kind).Inc()
	}
}

// IdentityDenied records an identity refusal, for reason: notfound or
// namespace.
func (r *Recorder) IdentityDenied(reason string) {
	if r != nil {
		r.identityDenied.WithLabelValues(reason).Inc()
	}
}

// ImageInspectError records a failed capacity resolution, for reason.
func (r *Recorder) ImageInspectError(reason string) {
	if r != nil {
		r.imageInspect.WithLabelValues(reason).Inc()
	}
}

// InputsHashChanged records an apply of an object of kind started for
// changed inputs.
func (r *Recorder) InputsHashChanged(kind string) {
	if r != nil {
		r.hashChanges.WithLabelValues(kind).Inc()
	}
}

// RemediationRequest records a remediate-machine annotation set
// (RemediationRequested) or removed (RemediationWithdrawn); action is the
// label value, anything else is dropped.
func (r *Recorder) RemediationRequest(action string) {
	if r != nil && slices.Contains([]string{RemediationRequested, RemediationWithdrawn}, action) {
		r.remediations.WithLabelValues(action).Inc()
	}
}

// ApprovalConsumed records a destructive-plan approval of an object of kind
// removed after its apply succeeded.
func (r *Recorder) ApprovalConsumed(kind string) {
	if r != nil {
		r.approvalsConsumed.WithLabelValues(kind).Inc()
	}
}

// LeaseWait records an operation on an object of kind that started waiting
// for a run lease; reason is LeaseWaitRunLease, LeaseWaitClusterOperation
// or LeaseWaitMachineOperations, anything else is dropped.
func (r *Recorder) LeaseWait(kind, reason string) {
	if r != nil && slices.Contains([]string{LeaseWaitRunLease, LeaseWaitClusterOperation, LeaseWaitMachineOperations}, reason) {
		r.leaseWaits.WithLabelValues(kind, reason).Inc()
	}
}

// StateBackup records n backups of an object of kind, of result
// BackupTaken, BackupPruned or BackupSkipped; anything else is dropped.
func (r *Recorder) StateBackup(kind, result string, n int) {
	if r != nil && n > 0 && slices.Contains([]string{BackupTaken, BackupPruned, BackupSkipped}, result) {
		r.stateBackups.WithLabelValues(kind, result).Add(float64(n))
	}
}

// PlanApproval records an approved plan of an object of kind that was
// applied (PlanApproved) or found changed (PlanChanged); anything else in
// result is dropped.
func (r *Recorder) PlanApproval(kind, result string) {
	if r != nil && slices.Contains([]string{PlanApproved, PlanChanged}, result) {
		r.planApprovals.WithLabelValues(kind, result).Inc()
	}
}

// StateRestore records a restore of an object of kind that finished
// (ResultSucceeded, ResultFailed) or named no backup (RestoreNotFound);
// anything else in result is dropped.
func (r *Recorder) StateRestore(kind, result string) {
	if r != nil && slices.Contains([]string{ResultSucceeded, ResultFailed, RestoreNotFound}, result) {
		r.stateRestores.WithLabelValues(kind, result).Inc()
	}
}
