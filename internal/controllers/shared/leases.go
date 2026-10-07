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
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runlease"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// JobName returns the deterministic name StartJob gives the Job of req for
// k: the name jobs.Build derives from the same fields.
func JobName(k Kind, req JobRequest) string {
	return jobs.Name(kindShort(k), k.Object().GetName(), req.Op, req.Attempt,
		jobs.Hash6(req.InputsHash, req.Op, req.Attempt, req.DriftTick))
}

// leaseWait is why an operation waits for a lease; the zero value means it
// may start.
type leaseWait struct {
	reason  string
	message string
	// requeue is when to look again; zero means GateRequeue.
	requeue time.Duration
}

// after returns when to look again after w: its own requeue, else
// GateRequeue.
func (w leaseWait) after() time.Duration {
	if w.requeue > 0 {
		return w.requeue
	}
	return GateRequeue
}

// maxNamedHolders caps the Job names a WaitingForMachineOperations message
// lists.
const maxNamedHolders = 5

// takeLeases takes, using ctx, what name, the Job about to be created for
// req, needs: the object's run lease always, and for an apply or destroy
// under the cluster operation gate the Cluster's write lease (a
// TerraformCluster) or a check of it (a machine or machine pool). A zero
// leaseWait means the Job may be created now. It returns the leaseWait
// (zero when nothing blocks) and any error acquiring a lease.
func (r *reconciler) takeLeases(ctx context.Context, req JobRequest, name string) (leaseWait, error) {
	return acquireLeases(ctx, r.d, r.k, req, name)
}

// acquireLeases is takeLeases for k's object, using ctx and the shared
// dependencies d, with req.Suffix as the object's state suffix; taking
// them again for the same name refreshes their acquire time and repeats
// the gate check. It returns the leaseWait (zero when nothing blocks) and
// any error acquiring a lease.
func acquireLeases(ctx context.Context, d Deps, k Kind, req JobRequest, name string) (leaseWait, error) {
	obj := k.Object()
	now := d.Clock.Now()
	run := runlease.Spec{
		Namespace: obj.GetNamespace(), Name: runlease.RunName(req.Suffix), Kind: runlease.KindRun,
		Holder: name, Op: req.Op, OwnerKind: k.Kind(), OwnerName: obj.GetName(), ClusterName: req.ClusterName,
		Deadline: time.Duration(req.Policy.ActiveDeadlineSeconds) * time.Second,
	}
	res, err := runlease.Acquire(ctx, d.Client, d.APIReader, now, run)
	if err != nil {
		return leaseWait{}, err
	}
	if !res.Acquired {
		return leaseWait{reason: infrav1.WaitingForRunLeaseReason, message: heldBy("the run lease "+run.Name, res.Holder, req.Op)}, nil
	}
	logger := klog.FromContext(ctx)
	if res.Previous != "" {
		logger.Info("Took over the run lease from a Job that finished or never started", "lease", run.Name, "previous", res.Previous)
	}
	if !d.ClusterOperationGate || !runlease.Mutating(req.Op) || req.ClusterName == "" {
		return leaseWait{}, nil
	}
	var w leaseWait
	if k.Kind() == state.KindTerraformCluster {
		w, err = clusterGate(ctx, d, run, now)
	} else {
		w, err = machineGate(ctx, d, run, now)
	}
	if err != nil || (w.reason != "" && w.reason != infrav1.WaitingForMachineOperationsReason) {
		// No Job named for the run lease will be created this pass: give
		// the run lease back so the next pass, which may pick another Job
		// name, does not wait out the grace on it. A TerraformCluster that
		// waits for machine operations keeps both its leases on purpose, so
		// no new machine operation starts meanwhile. A failed gate also
		// gives back the cluster write lease the pass may have taken.
		names := []string{run.Name}
		if err != nil && k.Kind() == state.KindTerraformCluster {
			names = append(names, runlease.ClusterName(run.Namespace, run.ClusterName))
		}
		releaseUnstarted(ctx, d, run.Namespace, run.Holder, names...)
	}
	return w, err
}

// leaseFresh is how recently the run lease must have been taken for its
// Job to be created without taking the leases again: half of
// runlease.Grace, past which a lease whose holder Job does not exist is
// free to others, so the create lands well inside it.
const leaseFresh = runlease.Grace / 2

// ensureFreshLeases makes sure, just before the Job name for req is
// created, using ctx and the shared dependencies d for k's object, that
// its leases are not about to lapse. Between takeLeases and the create,
// startJob writes block-move and the plan key; under a
// throttled client that can pass runlease.Grace, after which the run lease
// of a Job that does not exist yet reads as free, and a cluster's and a
// machine's apply could both start. The run lease is read live; the
// cluster write lease is taken after it, so its age bounds both. A run
// lease taken less than leaseFresh ago is kept. An older or missing one
// is taken again with the cluster write lease, which refreshes their
// acquire time and repeats the gate check, since a lapsed lease may have
// let another operation start. A gate that now waits, or a retake that
// itself took leaseFresh, defers the start, as does a run lease another
// Job took meanwhile, which is left to it. It returns an error wrapping
// ErrStartDeferred when the start is deferred, or any error reading or
// taking a lease.
func ensureFreshLeases(ctx context.Context, d Deps, k Kind, req JobRequest, name string) error {
	l := &coordinationv1.Lease{}
	key := client.ObjectKey{Namespace: k.Object().GetNamespace(), Name: runlease.RunName(req.Suffix)}
	err := d.APIReader.Get(ctx, key, l)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("read run lease %s: %w", key.Name, err)
	case runlease.HolderOf(l) != name:
		// This pass took the run lease for name: another holder took it
		// since, after it lapsed, and may run on the same state. Creating
		// the Job could run two at once; the caller gives back only the
		// leases name holds, so the other holder keeps its own.
		return fmt.Errorf("%w: the run lease %s is now held by %s", ErrStartDeferred, key.Name, runlease.HolderOf(l))
	case d.Clock.Since(runlease.AcquiredAt(l)) < leaseFresh:
		return nil
	}
	taken := d.Clock.Now()
	w, err := acquireLeases(ctx, d, k, req, name)
	if err != nil {
		return err
	}
	if w.reason != "" {
		return fmt.Errorf("%w: %s", ErrStartDeferred, w.message)
	}
	if d.Clock.Since(taken) >= leaseFresh {
		return fmt.Errorf("%w: taking the leases again took %s", ErrStartDeferred, d.Clock.Since(taken))
	}
	klog.FromContext(ctx).V(LogFlow).Info("Took the leases again: they were close to lapsing before the Job existed", "lease", key.Name, "Job", name)
	return nil
}

// releaseUnstarted gives back, using ctx and d, the leases names in
// namespace that holder, the name of a Job this pass did not create, took.
// Job names are deterministic, so a pass with a lagging Job cache can
// rebuild the name of a Job an earlier pass created, and a rejected create
// (a quota answers Forbidden before AlreadyExists) proves nothing about it:
// holder is read live and the leases go only when the API server has no such
// Job. Any other answer keeps them. Release checks the holder with the UID
// and resourceVersion it read, so a lease another Job took stays. A failure
// is only logged: the grace frees the lease anyway.
func releaseUnstarted(ctx context.Context, d Deps, namespace, holder string, names ...string) {
	logger := klog.FromContext(ctx)
	err := d.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: holder}, &batchv1.Job{})
	switch {
	case err == nil:
		logger.V(LogDebug).Info("Kept the leases: the Job exists", "Job", holder)
		return
	case !apierrors.IsNotFound(err):
		logger.Error(err, "Reading the Job of leases to release failed; they are kept and free after the grace", "Job", holder)
		return
	}
	for _, name := range names {
		released, err := runlease.Release(ctx, d.Client, d.APIReader, namespace, name, holder)
		if err != nil {
			logger.Error(err, "Releasing the lease of a Job that was not created failed; it is free after the grace anyway", "lease", name, "Job", holder)
			continue
		}
		if released {
			logger.V(LogDebug).Info("Released the lease of a Job that was not created", "lease", name, "Job", holder)
		}
	}
}

// clusterGate takes, using ctx and d, the Cluster's write lease, derived from
// run, for a TerraformCluster's apply or destroy at now, then waits while
// a machine or machine pool of the Cluster applies or destroys. It keeps
// both leases while it waits, so no new machine or pool operation starts
// meanwhile (writer preference: the cluster is not starved by a stream of
// machine or pool creates). It returns the leaseWait (zero when nothing
// blocks) and any error acquiring a lease or listing machine and pool
// operations.
func clusterGate(ctx context.Context, d Deps, run runlease.Spec, now time.Time) (leaseWait, error) {
	cl := run
	cl.Name, cl.Kind = runlease.ClusterName(run.Namespace, run.ClusterName), runlease.KindCluster
	res, err := runlease.Acquire(ctx, d.Client, d.APIReader, now, cl)
	if err != nil {
		return leaseWait{}, err
	}
	if !res.Acquired {
		return leaseWait{reason: infrav1.WaitingForRunLeaseReason, message: heldBy("the cluster write lease "+cl.Name, res.Holder, run.Op)}, nil
	}
	live, err := runlease.LiveMachineOps(ctx, d.APIReader, run.Namespace, run.ClusterName, now)
	if err != nil {
		return leaseWait{}, err
	}
	if len(live) == 0 {
		// The leases are writable by every module of the namespace: the
		// Jobs, which no module can touch, are checked too.
		if live, err = runningMutatingJobs(ctx, d, run.Namespace, run.ClusterName, state.KindTerraformMachine, state.KindTerraformMachinePool); err != nil || len(live) == 0 {
			return leaseWait{}, err
		}
	}
	named := live[:min(len(live), maxNamedHolders)]
	msg := fmt.Sprintf("Waiting for %d machine and machine pool apply or destroy Jobs of cluster %s to finish before the %s starts: %s",
		len(live), run.ClusterName, run.Op, strings.Join(named, ", "))
	if len(live) > len(named) {
		msg += ", …"
	}
	return leaseWait{reason: infrav1.WaitingForMachineOperationsReason, message: msg + ". New machine and machine pool applies and destroys wait for it meanwhile"}, nil
}

// machineGate checks, using ctx and d and at now, after run, the machine's or
// pool's run lease, is written, whether its TerraformCluster applies or
// destroys. The write precedes the read, and the cluster writes its lease
// before it reads the machines' and pools': on a linearizable store at
// least one of the two sees the other. takeLeases gives the run lease of a
// machine or pool that sees the cluster back, so the cluster is not held up
// by an operation that did not start.
// It returns the leaseWait (zero when nothing blocks) and any error
// reading a lease.
func machineGate(ctx context.Context, d Deps, run runlease.Spec, now time.Time) (leaseWait, error) {
	name := runlease.ClusterName(run.Namespace, run.ClusterName)
	holder, live, err := runlease.Live(ctx, d.APIReader, run.Namespace, name, now)
	if err != nil {
		return leaseWait{}, err
	}
	if !live {
		// The lease is writable by every module of the namespace, which
		// could free it under a running cluster Job: the Jobs, which no
		// module can touch, are checked too.
		running, err := runningMutatingJobs(ctx, d, run.Namespace, run.ClusterName, state.KindTerraformCluster)
		if err != nil || len(running) == 0 {
			return leaseWait{}, err
		}
		holder = running[0]
	}
	return leaseWait{reason: infrav1.WaitingForClusterOperationReason, message: fmt.Sprintf(
		"Waiting for the TerraformCluster's Job %s of cluster %s to finish before the %s starts", holder, run.ClusterName, run.Op)}, nil
}

// runningMutatingJobs returns, sorted, the names of the running apply,
// destroy and restore Jobs (runlease.Mutating) of objects of kinds in
// cluster clusterName of namespace, listed using ctx through d.Client
// (the Job cache). The run and cluster write leases say the same when
// nobody tampered with them; these Jobs only the manager creates. It
// returns any list error.
func runningMutatingJobs(ctx context.Context, d Deps, namespace, clusterName string, kinds ...string) ([]string, error) {
	var out []string
	for _, kind := range kinds {
		var list batchv1.JobList
		if err := d.Client.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{
			state.OwnerKindLabel: kind, clusterv1.ClusterNameLabel: state.LabelValue(clusterName),
		}); err != nil {
			return nil, fmt.Errorf("list %s Jobs of cluster %s: %w", kind, clusterName, err)
		}
		for i := range list.Items {
			j := &list.Items[i]
			if runlease.Mutating(jobs.OpOf(j)) && jobs.OutcomeOf(j) == jobs.Running {
				out = append(out, j.Name)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// heldBy returns the message of a wait for lease, held by holder ("" when
// a concurrent writer won the race), for the operation op.
func heldBy(lease, holder string, op jobs.Op) string {
	if holder == "" {
		return fmt.Sprintf("Waiting for %s: another manager took it first; the %s starts once it is free", lease, op)
	}
	return fmt.Sprintf("Waiting for %s, held by Job %s: the %s starts once that Job finishes", lease, holder, op)
}

// waitForLease reports, logging with ctx, op, an operation that waits for
// the lease described by w: ApplyJobSucceeded (apply, destroy, plan),
// DriftJobSucceeded (refresh, drift) or RestoreJobSucceeded (restore)
// Unknown with the wait's reason, and a requeue at GateRequeue. No Job was
// created, so the wait counts toward no retry backoff or remediation cap;
// the event and metric follow the condition's transition, once per wait.
// bk is the pass's bookkeeping, passed to finish. It returns the result
// and error from finish.
func (r *reconciler) waitForLease(ctx context.Context, bk *Bookkeeping, op jobs.Op, w leaseWait) (ctrl.Result, error) {
	klog.FromContext(ctx).V(LogFlow).Info("Waiting for a lease", "op", op, "reason", w.reason, "message", w.message)
	c := metav1.Condition{Type: infrav1.ApplyJobSucceededCondition, Status: metav1.ConditionUnknown, Reason: w.reason, Message: w.message}
	if op == jobs.OpRestore {
		c.Type = infrav1.RestoreJobSucceededCondition
		conditions.Set(r.obj, c)
		return r.finish(bk, nil, ctrl.Result{RequeueAfter: w.after()})
	}
	if !runlease.Mutating(op) && op != jobs.OpPlan {
		// A plan Job stands for the apply it plans: ApplyJobSucceeded.
		c.Type = infrav1.DriftJobSucceededCondition
		conditions.Set(r.obj, c)
		return r.finish(bk, nil, ctrl.Result{RequeueAfter: w.after()})
	}
	return r.finish(bk, &c, ctrl.Result{RequeueAfter: w.after()})
}

// releaseLeases gives back, using ctx, the run lease, and a
// TerraformCluster's write lease, held by each Job bk, this pass's
// bookkeeping, counted: the once-only point of a finished Job. A failure
// is only logged: a lease whose holder finished is free anyway, so the
// next Job takes it over.
func (r *reconciler) releaseLeases(ctx context.Context, bk *Bookkeeping) {
	ns := r.obj.GetNamespace()
	clusterName := ClusterName(r.obj, r.owner)
	logger := klog.FromContext(ctx)
	for _, job := range bk.counted() {
		names := []string{runlease.RunName(r.suffix)}
		if r.k.Kind() == state.KindTerraformCluster && clusterName != "" {
			names = append(names, runlease.ClusterName(ns, clusterName))
		}
		for _, name := range names {
			released, err := runlease.Release(ctx, r.d.Client, r.d.APIReader, ns, name, job)
			if err != nil {
				logger.Error(err, "Releasing a lease failed; it is free anyway once its Job finished", "lease", name, "Job", job)
				continue
			}
			if released {
				logger.V(LogDebug).Info("Released a lease", "lease", name, "Job", job)
			}
		}
	}
}
