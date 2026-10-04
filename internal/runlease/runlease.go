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

package runlease

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Labels and annotations of a run or cluster lease.
const (
	// KindLabel is run or cluster. It is what tells these Leases from the
	// backend's lock-tfstate-* Lease, which carries the same owner labels.
	KindLabel = "captf.io/lease"
	// KindRun marks an object's run lease.
	KindRun = "run"
	// KindCluster marks a Cluster's write lease.
	KindCluster = "cluster"
	// OpAnnotation records the operation of the holder Job.
	OpAnnotation = "captf.io/lease-op"
	// AcquiredAnnotation records when the holder took the lease (RFC 3339).
	AcquiredAnnotation = "captf.io/lease-acquired-at"
)

const (
	// Grace is how long a lease whose holder Job does not exist stays held:
	// it covers a manager that crashed between taking the lease and
	// creating the Job, and a reader racing that create.
	Grace = time.Minute
	// backstopExtra is added to the holder's activeDeadlineSeconds for the
	// backstop, past which a lease is free whatever its holder: a 600 s
	// grace for a Job still stopping at its deadline, plus five minutes.
	backstopExtra = 600*time.Second + 5*time.Minute
)

// RunName returns the run lease of the object with the state suffix suffix:
// captf-run-<suffix>, 29 characters whatever the object's name.
func RunName(suffix string) string {
	return "captf-run-" + suffix
}

// ClusterName returns the write lease of the Cluster namespace/cluster:
// captf-cluster-<hex(sha256(namespace/cluster))[:16]>, 30 characters.
func ClusterName(namespace, cluster string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + cluster))
	return "captf-cluster-" + hex.EncodeToString(sum[:])[:16]
}

// Mutating reports whether op changes infrastructure or what the state
// records of it (apply, destroy, restore): only those take part in the
// cluster operation gate. A TerraformCluster's restore takes the cluster
// write lease like its apply; a machine's restore waits for the cluster's
// operation, and a cluster's apply waits for it.
func Mutating(op jobs.Op) bool {
	return op == jobs.OpApply || op == jobs.OpDestroy || op == jobs.OpRestore
}

// Spec is a lease to hold.
type Spec struct {
	Namespace string
	Name      string
	// Kind is KindRun or KindCluster.
	Kind string
	// Holder is the name of the Job about to be created.
	Holder string
	Op     jobs.Op
	// OwnerKind and OwnerName are the Terraform* object that takes it.
	OwnerKind string
	OwnerName string
	// ClusterName is the object's Cluster; "" when unknown.
	ClusterName string
	// Deadline is the holder Job's activeDeadlineSeconds; 0 means the
	// default.
	Deadline time.Duration
}

// write sets everything s says onto l, taken at now.
func (s Spec) write(l *coordinationv1.Lease, now time.Time) {
	l.Namespace, l.Name = s.Namespace, s.Name
	l.Labels = map[string]string{
		state.OwnerKindLabel:       s.OwnerKind,
		state.OwnerNameLabel:       state.LabelValue(s.OwnerName),
		clusterv1.ClusterNameLabel: state.LabelValue(s.ClusterName),
		state.ManagedLabel:         "true",
		KindLabel:                  s.Kind,
	}
	metav1.SetMetaDataAnnotation(&l.ObjectMeta, OpAnnotation, string(s.Op))
	metav1.SetMetaDataAnnotation(&l.ObjectMeta, AcquiredAnnotation, now.UTC().Format(time.RFC3339Nano))
	holder := s.Holder
	at := metav1.NewMicroTime(now)
	deadline := cmp.Or(s.Deadline, time.Duration(jobs.DefaultActiveDeadlineSeconds)*time.Second)
	backstop := int32((deadline + backstopExtra) / time.Second) // #nosec G115 -- the CRD caps the deadline at 86400s
	l.Spec = coordinationv1.LeaseSpec{HolderIdentity: &holder, AcquireTime: &at, LeaseDurationSeconds: &backstop}
}

// Result is the outcome of Acquire.
type Result struct {
	// Acquired is true when the caller holds the lease now.
	Acquired bool
	// Holder is the Job that holds it when it was not acquired; "" when a
	// concurrent writer won the race.
	Holder string
	// Previous is the holder Acquire took the lease over from; "" when the
	// lease was new or already the caller's.
	Previous string
}

// Acquire takes the lease s for s.Holder using ctx: a Create through c,
// else, when it exists, an Update with the resourceVersion read through r
// (the API server, never a cache) if it is free or already s.Holder's, with
// now as the acquire time. It always writes, so the acquire time is fresh
// and the write precedes whatever the caller reads next. An AlreadyExists
// or Conflict means another writer won: Acquired is false and the caller
// retries on a later reconcile, never in this one. It returns the resulting
// Result and a non-nil error only when a Create or Update fails for another
// reason.
func Acquire(ctx context.Context, c client.Client, r client.Reader, now time.Time, s Spec) (Result, error) {
	l := &coordinationv1.Lease{}
	s.write(l, now)
	err := c.Create(ctx, l)
	switch {
	case err == nil:
		return Result{Acquired: true}, nil
	case !apierrors.IsAlreadyExists(err):
		return Result{}, fmt.Errorf("runlease: create %s/%s: %w", s.Namespace, s.Name, err)
	}
	existing := &coordinationv1.Lease{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Name}, existing); err != nil {
		if apierrors.IsNotFound(err) {
			// Released between the create and the read: someone else's turn.
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("runlease: get %s/%s: %w", s.Namespace, s.Name, err)
	}
	holder := HolderOf(existing)
	var previous string
	if holder != s.Holder {
		free, err := Free(ctx, r, existing, now)
		if err != nil {
			return Result{}, err
		}
		if !free {
			return Result{Holder: holder}, nil
		}
		previous = holder
	}
	s.write(existing, now)
	if err := c.Update(ctx, existing); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("runlease: update %s/%s: %w", s.Namespace, s.Name, err)
	}
	return Result{Acquired: true, Previous: previous}, nil
}

// HolderOf returns l's holder Job name; "" when none.
func HolderOf(l *coordinationv1.Lease) string {
	if l.Spec.HolderIdentity == nil {
		return ""
	}
	return *l.Spec.HolderIdentity
}

// AcquiredAt returns when l's holder took the lease: the annotation, else
// spec.acquireTime, else the creation time.
func AcquiredAt(l *coordinationv1.Lease) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, l.Annotations[AcquiredAnnotation]); err == nil {
		return t
	}
	if l.Spec.AcquireTime != nil {
		return l.Spec.AcquireTime.Time
	}
	return l.CreationTimestamp.Time
}

// backstop returns the age past which l is free whatever its holder.
func backstop(l *coordinationv1.Lease) time.Duration {
	if d := l.Spec.LeaseDurationSeconds; d != nil && *d > 0 {
		return time.Duration(*d) * time.Second
	}
	return time.Duration(jobs.DefaultActiveDeadlineSeconds)*time.Second + backstopExtra
}

// podsRunning reports whether any pod of the Job namespace/job is neither
// Succeeded nor Failed, listing the pods using ctx through r. A pod being
// deleted (deletionTimestamp set) still counts until its phase is terminal:
// its Terraform keeps running through the termination grace period.
func podsRunning(ctx context.Context, r client.Reader, namespace, job string) (bool, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{batchv1.JobNameLabel: job}); err != nil {
		return false, fmt.Errorf("runlease: list pods of holder Job %s/%s: %w", namespace, job, err)
	}
	for i := range pods.Items {
		if p := pods.Items[i].Status.Phase; p != corev1.PodSucceeded && p != corev1.PodFailed {
			return true, nil
		}
	}
	return false, nil
}

// Free reports whether l may be taken over at now: it has no holder; it is
// older than its backstop; or its holder is gone. The holder is gone when
// no pod of the holder Job is still running and either the Job has
// finished or it does not exist and the lease was taken more than Grace
// ago. The pod check matters because a Job's pod outlives both cases: a
// deleted Job's pod runs on through its termination grace period, and a
// Job past its activeDeadlineSeconds can be marked Failed while its pod is
// still stopping. The backstop is the absolute bound, checked first, so a
// stuck pod cannot hold a lease forever. The Job and pods are read using
// ctx through r, which must be the API server: a cache that has not seen a
// just-created Job would free a live lease.
func Free(ctx context.Context, r client.Reader, l *coordinationv1.Lease, now time.Time) (bool, error) {
	holder := HolderOf(l)
	if holder == "" {
		return true, nil
	}
	age := now.Sub(AcquiredAt(l))
	if age > backstop(l) {
		return true, nil
	}
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: l.Namespace, Name: holder}, job)
	switch {
	case apierrors.IsNotFound(err):
		if age <= Grace {
			return false, nil
		}
	case err != nil:
		return false, fmt.Errorf("runlease: get holder Job %s/%s: %w", l.Namespace, holder, err)
	default:
		if jobs.OutcomeOf(job) == jobs.Running {
			return false, nil
		}
	}
	running, err := podsRunning(ctx, r, l.Namespace, holder)
	if err != nil {
		return false, err
	}
	return !running, nil
}

// Live reads the lease namespace/name using ctx through r and reports its
// holder and whether that holder is live (the lease exists and is not Free
// as of now).
func Live(ctx context.Context, r client.Reader, namespace, name string, now time.Time) (holder string, live bool, err error) {
	l := &coordinationv1.Lease{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, l); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("runlease: get %s/%s: %w", namespace, name, err)
	}
	free, err := Free(ctx, r, l, now)
	if err != nil {
		return "", false, err
	}
	return HolderOf(l), !free, nil
}

// LiveMachineOps returns, sorted, the holder Jobs of the run leases in
// namespace of cluster's machines (every kind but TerraformCluster) that
// apply or destroy and are live as of now. It reads the leases using ctx
// through r.
func LiveMachineOps(ctx context.Context, r client.Reader, namespace, cluster string, now time.Time) ([]string, error) {
	list := &coordinationv1.LeaseList{}
	if err := r.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{
		KindLabel:                  KindRun,
		clusterv1.ClusterNameLabel: state.LabelValue(cluster),
		state.ManagedLabel:         "true",
	}); err != nil {
		return nil, fmt.Errorf("runlease: list run leases of cluster %s/%s: %w", namespace, cluster, err)
	}
	var live []string
	for i := range list.Items {
		l := &list.Items[i]
		if l.Labels[state.OwnerKindLabel] == state.KindTerraformCluster || !Mutating(jobs.Op(l.Annotations[OpAnnotation])) {
			continue
		}
		free, err := Free(ctx, r, l, now)
		if err != nil {
			return nil, err
		}
		if !free {
			live = append(live, HolderOf(l))
		}
	}
	slices.Sort(live)
	return live, nil
}

// Release deletes the lease namespace/name when holder holds it, reading it
// using ctx through r and deleting it through c, with the read UID and
// resourceVersion as preconditions: a lease another holder took in the
// meantime stays. It reports whether it deleted the lease.
func Release(ctx context.Context, c client.Client, r client.Reader, namespace, name, holder string) (bool, error) {
	l := &coordinationv1.Lease{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, l); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("runlease: get %s/%s: %w", namespace, name, err)
	}
	if HolderOf(l) != holder {
		return false, nil
	}
	err := c.Delete(ctx, l, client.Preconditions{UID: &l.UID, ResourceVersion: &l.ResourceVersion})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		return false, nil
	}
	return false, fmt.Errorf("runlease: delete %s/%s: %w", namespace, name, err)
}

// DeleteOwned deletes every run and cluster lease the object
// ownerKind/ownerName in namespace took, whoever holds it now, listing them
// using ctx through r and deleting them through c. Only Leases with
// KindLabel match: the backend's state lock carries the same owner labels
// and is left to state.Cleanup. It returns a non-nil error when the list or
// a delete fails.
func DeleteOwned(ctx context.Context, c client.Client, r client.Reader, namespace, ownerKind, ownerName string) error {
	list := &coordinationv1.LeaseList{}
	if err := r.List(ctx, list, client.InNamespace(namespace), client.HasLabels{KindLabel}, client.MatchingLabels{
		state.OwnerKindLabel: ownerKind,
		state.OwnerNameLabel: state.LabelValue(ownerName),
		state.ManagedLabel:   "true",
	}); err != nil {
		return fmt.Errorf("runlease: list leases of %s %s/%s: %w", ownerKind, namespace, ownerName, err)
	}
	for i := range list.Items {
		if err := client.IgnoreNotFound(c.Delete(ctx, &list.Items[i])); err != nil {
			return fmt.Errorf("runlease: delete %s/%s: %w", namespace, list.Items[i].Name, err)
		}
	}
	return nil
}
