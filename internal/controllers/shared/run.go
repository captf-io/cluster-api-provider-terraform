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
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/plankey"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runlease"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// ChooseImage picks the image reference a Job for op runs ("Digest
// pinning"): Apply, and the plan Job that plans it, run specRef, the spec
// reference, as written (a new image is an input change); every other
// operation runs pinned, the repo@digest, when one is recorded.
// digestUnknown is true when a non-apply operation falls back to the spec
// reference (DigestUnknown). It returns the image reference to run and
// digestUnknown.
func ChooseImage(op jobs.Op, specRef, pinned string) (ref string, digestUnknown bool) {
	if op == jobs.OpApply || op == jobs.OpPlan {
		return specRef, false
	}
	if pinned != "" {
		return pinned, false
	}
	return specRef, true
}

// JobRequest is one Job to start.
type JobRequest struct {
	Op    jobs.Op
	Files render.Files
	// InputsHash is the hash of the rendered inputs (apply), or the hash
	// recorded in state for other operations.
	InputsHash string
	// Source is the image to run: the spec's for Apply and for mutable
	// kinds, the durable Secret's for immutable kinds.
	Source infrav1.Source
	// PinnedDigest is the durable Secret's repo@digest, if any.
	PinnedDigest string
	// Identity names the identity whose mirror the Job mounts, or the
	// namespace-local Secret it mounts when IdentityKind is Secret.
	Identity string
	// IdentityKind is the kind Identity names; "" for a
	// TerraformClusterIdentity.
	IdentityKind   infrav1.IdentityKind
	ServiceAccount string
	Suffix         string
	ClusterName    string
	Attempt        int32
	ForceUnlockID  string
	DriftTick      string
	Policy         infrav1.JobPolicy
	// Remediation marks an apply that remediates drift
	// (RemediationAnnotation).
	Remediation bool
	// AfterFailedApply marks an apply started while an apply was due
	// because one failed (AfterFailedApplyAnnotation).
	AfterFailedApply bool
	// AfterInterruptedApply names the apply Job that disappeared while an
	// apply is due for it (AfterInterruptedApplyAnnotation); "" otherwise.
	AfterInterruptedApply string
	// AllowDeletesHash approves a destructive plan of a guarded pool apply:
	// the approval hash of its approved ExportsChange TerraformPlan
	// (Decision.AllowDeletes).
	AllowDeletesHash string
	// ApprovalHash guards a pool apply that renders a change of the
	// cluster's exports (jobs.Spec.ApprovalHash), and is recorded on the
	// Job as ApprovalHashAnnotation; "" otherwise.
	ApprovalHash string
	// ExportsHash is hash.Exports of the cluster exports a pool apply
	// renders (ClusterOutputsHashAnnotation); "" for other kinds.
	ExportsHash string
	// HeldExports marks a pool apply that renders the exports of its last
	// successful apply while a change of them waits for approval
	// (HeldClusterOutputsAnnotation).
	HeldExports bool
	// ExpectPlan is the plan hash an approved apply must plan again
	// (Decision.ExpectPlan); recorded on the Job as ApprovedPlanAnnotation.
	ExpectPlan string
	// Plan names the approved TerraformPlan an apply applies
	// (Decision.Plan); recorded on the Job as PlanAnnotation.
	Plan string
	// Why is the decision reason (DecideOp), for the JobCreated event.
	Why string
	// Restore is the backup a restore pushes; nil for other ops.
	Restore *jobs.Restore
}

// whyNotes explain a decision reason in JobCreated.
var whyNotes = map[string]string{
	"Deleting":               "the object is being deleted; this destroys its infrastructure",
	"NoState":                "no state yet: first apply",
	"StateWithoutInputsHash": "the state carries no inputs hash",
	"InputsChanged":          "the inputs changed",
	"LastApplyFailed":        "retrying the failed apply",
	"DriftRemediation":       "remediating drift",
	"RefreshAfterApply":      "reading the outputs after the apply",
	"HealthPending":          "the instance is pending",
	"HealthCheckDue":         "health check due",
	"MembershipConverging":   "the group's membership is converging",
	"MembershipRefreshDue":   "membership refresh due",
	"DriftDue":               "drift check due",
	ReasonRestoreRequested:   "restoring a state backup (" + infrav1.RestoreStateAnnotation + ")",
}

// planWhy is the JobCreated note of a plan Job.
const planWhy = "planning for approval (applyPolicy Manual): "

// StuckJobAge is how old an active Job must be before DeleteStuckJob looks
// at it: a Job created moments ago legitimately has no per-run Secret yet.
const StuckJobAge = time.Minute

// DeleteStuckJob deletes job when its per-run Secret is missing and none of
// its pods has started: a crash or error between creating the Job and
// the Secret, after which the pod would wait for the volume until
// activeDeadlineSeconds. The next reconcile starts the operation again, with
// its Secret. A pod that started keeps running whatever happened to the
// Secret since. Both reads, done through ctx and the shared dependencies
// d, are uncached: a cache that has not seen a just-created Secret must
// not delete a healthy Job. Jobs younger than StuckJobAge are not
// checked, which also spares the reads on the reconciles right after a
// create. obj is the owning object, for the deletion event, and the one
// whose status.activeJob is cleared on the API server first
// (releaseActiveJob), so a deleted Job that never started is not named
// there if the status write of the pass is lost; that write failing aborts
// the delete. It returns whether job was deleted, and any error reading,
// listing pods for, releasing, or deleting it.
func DeleteStuckJob(ctx context.Context, d Deps, obj Object, job *batchv1.Job) (bool, error) {
	if d.Clock.Now().Sub(job.CreationTimestamp.Time) < StuckJobAge {
		return false, nil
	}
	err := d.APIReader.Get(ctx, client.ObjectKey{Namespace: job.Namespace, Name: inputs.RunName(job.Name)}, &corev1.Secret{})
	switch {
	case err == nil:
		return false, nil
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("get per-run Secret of %s: %w", job.Name, err)
	}
	pods, err := d.Jobs.Pods(ctx, job)
	if err != nil {
		return false, fmt.Errorf("list pods of %s: %w", job.Name, err)
	}
	for i := range pods {
		if pods[i].Status.Phase != corev1.PodPending {
			return false, nil
		}
	}
	// Before the delete: status.activeJob is otherwise cleared only by the
	// deferred status patch, and a crash, lost leadership or failed patch
	// between the two leaves the API server's status naming a Job that is
	// gone and never started, which the next pass would record as a vanished
	// apply.
	if err := releaseActiveJob(ctx, d, obj, job.Name); err != nil {
		return false, fmt.Errorf("release status.activeJob before deleting %s: %w", job.Name, err)
	}
	if err := d.Jobs.Delete(ctx, job); err != nil {
		return false, err
	}
	klog.FromContext(ctx).Info("Deleted a Job that cannot start: its per-run inputs Secret is missing; the next reconcile starts it again", "Job", klog.KObj(job))
	d.EmitRelated(obj, job, corev1.EventTypeWarning, EventStuckJobDeleted, "Run",
		"Deleted %s Job %s: its per-run inputs Secret is missing and no pod started; the operation starts again", jobs.OpOf(job), job.Name)
	return true, nil
}

// ErrStartDeferred is returned, wrapped, by StartJob when it created no
// Job and gave back the leases for a reason a later pass resolves: the
// object or its Cluster was paused live after block-move was written (a
// clusterctl move started), or the leases grew too old before the create
// and could not be taken again, or another Job took the run lease
// meanwhile (ensureFreshLeases). The caller requeues
// soon instead of counting it as a failure.
var ErrStartDeferred = errors.New("job start deferred")

// StartJob starts the Job for req, using ctx, the shared dependencies d
// and k's object: block-move persisted before the Job exists and pause
// read live after it (pauseHandshake), the durable inputs Secret for
// Apply, the image choice, leases fresh enough to outlast the create
// (ensureFreshLeases), the Job, its per-run Secret (the Job first: the
// Secret is owned by it, and the pod waits for the volume) and
// status.activeJob. A Job that already exists (a retry after a crash) is
// taken as the active one. It returns the started (or existing) Job, or
// any error creating it or its per-run Secret; an error wrapping
// ErrStartDeferred when no Job was created for a reason a later pass
// resolves.
func StartJob(ctx context.Context, d Deps, k Kind, req JobRequest) (*batchv1.Job, error) {
	job, created, err := startJob(ctx, d, k, req)
	if err != nil && !created {
		// This pass did not create the Job that holds the leases. If no Job
		// of that name exists, give them back so the next pass takes them
		// at once, whatever name it picks; a Job that exists, created by an
		// earlier pass the cache has not shown, keeps them.
		names := []string{runlease.RunName(req.Suffix)}
		if k.Kind() == state.KindTerraformCluster && req.ClusterName != "" {
			names = append(names, runlease.ClusterName(k.Object().GetNamespace(), req.ClusterName))
		}
		releaseUnstarted(ctx, d, k.Object().GetNamespace(), JobName(k, req), names...)
	}
	return job, err
}

// createRejected reports whether err, from a Job create, proves the API
// server refused the Job, so it does not exist: the request was invalid,
// forbidden, unauthorized, too large, or its namespace is gone.
func createRejected(err error) bool {
	return apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) ||
		apierrors.IsBadRequest(err) || apierrors.IsRequestEntityTooLargeError(err) || apierrors.IsNotFound(err)
}

// startJob starts the Job for req, using ctx, d and k, as StartJob does. It
// returns the Job, whether it exists (created now or adopted: true on every
// failure after the create, so the leases of a Job that does not exist can
// go and no others), and any error.
func startJob(ctx context.Context, d Deps, k Kind, req JobRequest) (job *batchv1.Job, created bool, err error) {
	obj := k.Object()
	// Block-move and the pause check come first: a start that a move
	// defers writes nothing else.
	if err := persistBlockMove(ctx, d.Client, obj); err != nil {
		return nil, false, err
	}
	if err := pauseHandshake(ctx, d, obj, req.ClusterName); err != nil {
		return nil, false, err
	}
	if req.Op == jobs.OpApply {
		meta := inputs.Meta{Image: req.Source.Image, Identity: req.Identity, IdentityKind: string(req.IdentityKind)}
		if err := inputs.Write(ctx, d.Client, obj, req.Files, meta); err != nil {
			return nil, false, err
		}
	}
	ref, digestUnknown := ChooseImage(req.Op, req.Source.Image, req.PinnedDigest)
	if digestUnknown {
		d.Emit(obj, corev1.EventTypeWarning, EventDigestUnknown, "Run", "No image digest is pinned; %s runs %s", req.Op, ref)
	}

	// A plan, a guarded apply (which reports its plan when it blocks) and an
	// approved apply fingerprint their plan; all must key it with the same
	// per-object key, so the Secret exists before any of those Jobs does.
	var planKey string
	if (jobs.Spec{Op: req.Op, OwnerKind: k.Kind(), ApprovalHash: req.ApprovalHash, ExpectPlan: req.ExpectPlan}).Fingerprints() {
		name, err := plankey.Ensure(ctx, d.Client, obj)
		if err != nil {
			return nil, false, err
		}
		planKey = name
	}

	spec := jobs.Spec{
		OwnerKind:      k.Kind(),
		OwnerName:      obj.GetName(),
		Namespace:      obj.GetNamespace(),
		ClusterName:    req.ClusterName,
		KindShort:      kindShort(k),
		Op:             req.Op,
		Attempt:        req.Attempt,
		InputsHash:     req.InputsHash,
		DriftTick:      req.DriftTick,
		ImageRef:       ref,
		PullPolicy:     req.Source.ImagePullPolicy,
		ServiceAccount: req.ServiceAccount,
		CredsSecret:    identity.CredentialsSecretName(infrav1.IdentityReference{Name: req.Identity, Kind: req.IdentityKind}),
		PlanKeySecret:  planKey,
		Policy:         req.Policy,
		Suffix:         req.Suffix,
		BackendLabels:  state.BackendLabels(k.Kind(), obj.GetName(), req.ClusterName),
		ForceUnlockID:  req.ForceUnlockID,

		AllowDeletesHash: req.AllowDeletesHash,
		ApprovalHash:     req.ApprovalHash,
		ExpectPlan:       req.ExpectPlan,
		Restore:          req.Restore,

		Events:   d.RunnerEvents,
		OwnerUID: obj.GetUID(),
	}
	job, _ = jobs.Build(spec, d.RunnerImage)
	if req.Remediation {
		metav1.SetMetaDataAnnotation(&job.ObjectMeta, RemediationAnnotation, "true")
	}
	if req.AfterFailedApply && req.Op == jobs.OpApply {
		metav1.SetMetaDataAnnotation(&job.ObjectMeta, AfterFailedApplyAnnotation, "true")
	}
	if req.AfterInterruptedApply != "" && req.Op == jobs.OpApply {
		metav1.SetMetaDataAnnotation(&job.ObjectMeta, AfterInterruptedApplyAnnotation, req.AfterInterruptedApply)
	}
	if req.ExpectPlan != "" && req.Op == jobs.OpApply {
		metav1.SetMetaDataAnnotation(&job.ObjectMeta, ApprovedPlanAnnotation, req.ExpectPlan)
	}
	if req.Plan != "" && req.Op == jobs.OpApply {
		metav1.SetMetaDataAnnotation(&job.ObjectMeta, PlanAnnotation, req.Plan)
	}
	annotateExports(job, req)
	if err := ensureFreshLeases(ctx, d, k, req, job.Name); err != nil {
		return nil, false, err
	}
	if err := d.Jobs.Create(ctx, obj, job); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			// A timeout or a server error may hide a create that went
			// through: only a rejection proves no Job holds the leases.
			return nil, !createRejected(err), fmt.Errorf("create job %s: %w", job.Name, err)
		}
		existing := &batchv1.Job{}
		if err := d.Client.Get(ctx, client.ObjectKeyFromObject(job), existing); err != nil {
			return nil, true, fmt.Errorf("get existing job %s: %w", job.Name, err)
		}
		job = existing
	}
	if err := inputs.CreateRun(ctx, d.Client, job, req.Files); err != nil {
		return nil, true, err
	}
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "Job", klog.KObj(job))
	logger.Info("Started Job", "op", req.Op, "attempt", req.Attempt, "image", ref)
	if req.AllowDeletesHash != "" {
		logger.Info("The Job may apply a plan that deletes or replaces resources: its hash is approved", "approvedHash", req.AllowDeletesHash)
	}
	if req.HeldExports {
		logger.Info("The Job renders the cluster exports of the last successful apply: a change of them waits for approval")
	}
	if req.ForceUnlockID != "" {
		logger.Info("The Job force-unlocks a stale state lock")
		d.Metrics.ForceUnlock(k.Kind())
		d.EmitRelated(obj, job, corev1.EventTypeWarning, EventForceUnlocked, "Run",
			"Job %s force-unlocks a stale state lock whose holder pod no longer exists", job.Name)
	}

	now := metav1.NewTime(d.Clock.Now())
	k.Status().ActiveJob = infrav1.ActiveJob{
		Name:      job.Name,
		Operation: infrav1.Operation(req.Op),
		Attempt:   req.Attempt,
		StartTime: &now,
	}
	note := fmt.Sprintf("Started %s Job %s (attempt %d, image %s)", req.Op, job.Name, req.Attempt, ref)
	if req.Why != "" {
		note += ": "
		if req.Op == jobs.OpPlan {
			note += planWhy
		}
		note += cmp.Or(whyNotes[req.Why], req.Why)
	}
	if req.Plan != "" && req.Op == jobs.OpApply {
		note += "; it applies the approved TerraformPlan " + req.Plan
	}
	d.EmitRelated(obj, job, corev1.EventTypeNormal, EventJobCreated, "Run", "%s", note)
	return job, true, nil
}

// persistBlockMove writes block-move on obj now, using ctx and the client
// c, not in the deferred status patch, so a crash between here and the
// Job create cannot leave a Job without it. It patches a copy: patching
// obj itself would bump the resourceVersion the deferred patch helper
// diffs against, and that patch would then conflict. obj gets the
// annotation in memory, so the deferred patch agrees. It returns any
// patch error.
func persistBlockMove(ctx context.Context, c client.Client, obj Object) error {
	before, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return nil
	}
	after, ok := before.DeepCopyObject().(client.Object)
	if !ok || !SetBlockMove(after) {
		return nil
	}
	if err := c.Patch(ctx, after, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("set block-move: %w", err)
	}
	SetBlockMove(obj)
	return nil
}

// pauseHandshake reads, using ctx through d's uncached reader, whether
// obj or its Cluster clusterName is paused now that block-move is written,
// and if so clears block-move again and returns an error wrapping
// ErrStartDeferred, so no Job starts.
//
// Pause was decided earlier in the pass from the cached Cluster, which
// may predate a clusterctl move. The two sides form a write-then-read
// handshake: this provider writes block-move, then reads pause;
// clusterctl writes pause (spec.paused), then reads block-move
// (waitReadyForMove). On a linearizable store one of them sees the
// other's write: either clusterctl waits for block-move, or this pass
// sees the pause and creates no Job. A Cluster that does not exist (no
// owner, being deleted) keeps the cached decision. It returns that
// deferral, or any error reading or clearing.
func pauseHandshake(ctx context.Context, d Deps, obj Object, clusterName string) error {
	paused, err := pausedLive(ctx, d, obj, clusterName)
	if err != nil || !paused {
		return err
	}
	if err := clearBlockMoveLive(ctx, d.Client, obj); err != nil {
		return err
	}
	klog.FromContext(ctx).Info("Not starting a Job: the object or its Cluster was paused after this reconcile read it; block-move is cleared for clusterctl move")
	return fmt.Errorf("%w: paused", ErrStartDeferred)
}

// pausedLive reports, using ctx through d's uncached reader, whether obj
// carries the paused annotation or its Cluster clusterName (in obj's
// namespace) sets spec.paused on the API server. A Cluster that is not
// found, or no clusterName, counts as unpaused. It returns paused and any
// read error.
func pausedLive(ctx context.Context, d Deps, obj Object, clusterName string) (bool, error) {
	gvk, err := apiutil.GVKForObject(obj, d.Client.Scheme())
	if err != nil {
		return false, fmt.Errorf("pause check: %w", err)
	}
	meta := &metav1.PartialObjectMetadata{}
	meta.SetGroupVersionKind(gvk)
	if err := d.APIReader.Get(ctx, client.ObjectKeyFromObject(obj), meta); err != nil {
		return false, fmt.Errorf("pause check: read %s: %w", obj.GetName(), err)
	}
	if annotations.HasPaused(meta) {
		return true, nil
	}
	if clusterName == "" {
		return false, nil
	}
	cl := &clusterv1.Cluster{}
	err = d.APIReader.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: clusterName}, cl)
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("pause check: read Cluster %s: %w", clusterName, err)
	}
	return cl.Spec.Paused != nil && *cl.Spec.Paused, nil
}

// clearBlockMoveLive removes block-move from obj on the API server now,
// using ctx and the client c, and then from obj in memory, as
// persistBlockMove sets it: it patches a copy, so the deferred patch
// helper's resourceVersion stays the one it diffs against. The deferred
// patch would not clear it: its snapshot predates persistBlockMove. It
// returns any patch error.
func clearBlockMoveLive(ctx context.Context, c client.Client, obj Object) error {
	before, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return nil
	}
	after, ok := before.DeepCopyObject().(client.Object)
	if !ok || !ClearBlockMove(after) {
		return nil
	}
	if err := c.Patch(ctx, after, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("clear block-move: %w", err)
	}
	ClearBlockMove(obj)
	return nil
}

// ClusterName returns obj's cluster.x-k8s.io/cluster-name label, else
// owner's Cluster name.
func ClusterName(obj client.Object, owner OwnerInfo) string {
	if n := obj.GetLabels()[clusterv1.ClusterNameLabel]; n != "" {
		return n
	}
	if owner.Cluster != nil {
		return owner.Cluster.Name
	}
	return ""
}
