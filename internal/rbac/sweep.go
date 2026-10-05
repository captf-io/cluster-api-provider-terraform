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

package rbac

import (
	"context"
	"fmt"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// managed is the list/delete-object filter for every CAPTF-owned RBAC and
// lock object this file sweeps: captf.io/managed=true.
var managed = client.MatchingLabels{state.ManagedLabel: "true"}

// Sweep deletes the captf.io/managed=true ServiceAccounts, RoleBindings and
// Leases of every namespace that holds no Terraform* object: what clusterctl
// move leaves behind on the source. It checks the given namespaces, or with
// none every namespace holding such an object. Every read goes through
// uncached, which callers wire to mgr.GetAPIReader(); nothing is filtered by
// --watch-filter, so another manager instance's objects keep a namespace.
// ctx bounds every call this makes; uncached is the reader used for lists, c
// the client used for deletes. A failure in one namespace does not stop the
// others; the errors are joined and returned.
func Sweep(ctx context.Context, uncached client.Reader, c client.Client, namespaces ...string) error {
	if len(namespaces) == 0 {
		found, err := managedNamespaces(ctx, uncached)
		if err != nil {
			return err
		}
		namespaces = found
	}
	var errs []error
	for _, ns := range namespaces {
		if _, err := SweepNamespace(ctx, uncached, c, ns); err != nil {
			errs = append(errs, err)
		}
	}
	return kerrors.NewAggregate(errs)
}

// SweepNamespace sweeps one namespace and reports whether it was empty of
// Terraform* objects when it started and still was just before it deleted
// (a namespace that gained an object in between is reported as not empty and
// keeps everything). Each delete is conditional on the listed UID and
// resourceVersion; a Conflict or NotFound skips that object until the next
// sweep. A namespace that still holds objects keeps its runner
// objects, but its RoleBinding loses the override ServiceAccounts no object
// runs as any more (pruneSubjects). ctx bounds every call this makes;
// uncached is the reader used for lists, c the client used for deletes and
// updates. It also returns an error from a failed list, delete or update.
func SweepNamespace(ctx context.Context, uncached client.Reader, c client.Client, namespace string) (bool, error) {
	empty, err := noTerraformObjects(ctx, uncached, namespace)
	if err != nil {
		return false, err
	}
	if !empty {
		return false, pruneSubjects(ctx, uncached, c, namespace)
	}
	var objs []client.Object
	sas := &corev1.ServiceAccountList{}
	rbs := &rbacv1.RoleBindingList{}
	leases := &coordinationv1.LeaseList{}
	for _, l := range []client.ObjectList{sas, rbs, leases} {
		if err := uncached.List(ctx, l, client.InNamespace(namespace), managed); err != nil {
			return true, fmt.Errorf("rbac: sweep %s: %w", namespace, err)
		}
	}
	for i := range sas.Items {
		objs = append(objs, &sas.Items[i])
	}
	for i := range rbs.Items {
		objs = append(objs, &rbs.Items[i])
	}
	for i := range leases.Items {
		objs = append(objs, &leases.Items[i])
	}
	// An object created since the first check owns this RBAC and these
	// leases: leave them for its reconcile. The deletes below carry the
	// listed UID and resourceVersion, so anything re-created or changed after
	// the list survives until the next sweep.
	if empty, err := noTerraformObjects(ctx, uncached, namespace); err != nil || !empty {
		return false, err
	}
	// No Kubernetes event: the sweep acts for no single object (the
	// Terraform* objects are gone, and what it deletes goes with the event's
	// regarding object), so the log line is the record.
	var errs []error
	var deleted []string
	for _, o := range objs {
		uid, rv := o.GetUID(), o.GetResourceVersion()
		err := c.Delete(ctx, o, client.Preconditions{UID: &uid, ResourceVersion: &rv})
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			// Gone, or changed since the list: the next sweep decides.
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("rbac: sweep %s/%s: %w", namespace, o.GetName(), err))
			continue
		}
		// %T is *v1.ServiceAccount, *v1.RoleBinding or *v1.Lease.
		deleted = append(deleted, strings.TrimPrefix(fmt.Sprintf("%T", o), "*v1.")+"/"+o.GetName())
	}
	if len(deleted) > 0 {
		klog.FromContext(ctx).Info("Swept the runner objects of a namespace without Terraform objects", "namespace", namespace, "deleted", deleted)
	}
	return true, kerrors.NewAggregate(errs)
}

// noTerraformObjects reports whether namespace holds no TerraformCluster, no
// TerraformMachine and no TerraformMachinePool, listed through uncached,
// bounded by ctx. Templates run no Jobs. It also returns an error from a
// failed list.
func noTerraformObjects(ctx context.Context, uncached client.Reader, namespace string) (bool, error) {
	for _, l := range []client.ObjectList{&infrav1.TerraformClusterList{}, &infrav1.TerraformMachineList{}, &infrav1.TerraformMachinePoolList{}} {
		if err := uncached.List(ctx, l, client.InNamespace(namespace), client.Limit(1)); err != nil {
			return false, fmt.Errorf("rbac: list terraform objects in %s: %w", namespace, err)
		}
		if meta.LenList(l) > 0 {
			return false, nil
		}
	}
	return true, nil
}

// pruneSubjects drops from the managed RoleBinding captf-runner every subject
// that is not a ServiceAccount some TerraformCluster, TerraformMachine or
// TerraformMachinePool of namespace runs as, or a captf Job or pod still
// runs as (usedServiceAccounts). Switching jobs.serviceAccountName from A to
// B would otherwise leave A with the runner's Secret permissions until the
// namespace is empty, but not until the Job running as A has finished. The binding is read before the objects are
// listed, and written with its resourceVersion: a subject a reconcile added
// after the read makes the update conflict, and one added before it belongs
// to an object the list already sees. ctx bounds every call this makes;
// uncached is the reader used for the reads, c the client used for the
// delete or update. It returns nil when the binding is missing or not
// managed, or an error from a failed read, delete or update.
func pruneSubjects(ctx context.Context, uncached client.Reader, c client.Client, namespace string) error {
	rb := &rbacv1.RoleBinding{}
	if err := uncached.Get(ctx, client.ObjectKey{Namespace: namespace, Name: RoleBinding}, rb); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("rbac: get rolebinding %s/%s: %w", namespace, RoleBinding, err)
	}
	if rb.Labels[state.ManagedLabel] != "true" {
		return nil
	}
	used, err := usedServiceAccounts(ctx, uncached, namespace)
	if err != nil {
		return err
	}
	subjects := slices.DeleteFunc(slices.Clone(rb.Subjects), func(s rbacv1.Subject) bool {
		return s.Kind != rbacv1.ServiceAccountKind || s.Namespace != namespace || !used.Has(s.Name)
	})
	switch {
	case len(subjects) == len(rb.Subjects):
		return nil
	case len(subjects) == 0:
		// The next reconcile re-creates it with the subject it needs.
		if err := client.IgnoreNotFound(c.Delete(ctx, rb, client.Preconditions{ResourceVersion: &rb.ResourceVersion})); err != nil {
			return fmt.Errorf("rbac: delete rolebinding %s/%s: %w", namespace, RoleBinding, err)
		}
		klog.FromContext(ctx).Info("Deleted the runner RoleBinding: no object runs as any of its subjects", "namespace", namespace)
		return nil
	}
	pruned := len(rb.Subjects) - len(subjects)
	rb.Subjects = subjects
	if err := c.Update(ctx, rb); err != nil {
		return fmt.Errorf("rbac: update rolebinding %s/%s: %w", namespace, RoleBinding, err)
	}
	// A log line, not an event: the binding is shared by the namespace, and
	// no single Terraform* object caused the prune.
	klog.FromContext(ctx).Info("Pruned runner RoleBinding subjects no object runs as", "namespace", namespace, "pruned", pruned)
	return nil
}

// usedServiceAccounts returns the ServiceAccounts the Terraform* objects of
// namespace run as. captf-runner is always in it: the caller only asks while
// objects exist. A cluster runs as its jobs.serviceAccountName, else
// captf-runner. A machine or pool runs as its own, else its
// TerraformCluster's spec.defaults.jobs.serviceAccountName
// (identity.ClusterIndex: the cluster with its cluster-name label), else
// captf-runner. When a machine or pool without its own has no
// TerraformCluster found, every cluster default of the namespace is kept:
// pruning one it does use would flap its binding on every sweep.
//
// The specs alone are not enough: jobs.serviceAccountName is mutable, and a
// Job already running keeps the ServiceAccount it was created with, which
// needs its state Secret and Lease permissions until it is done. So the
// ServiceAccount of every captf-managed Job whose outcome is still Running
// is kept, and of every captf-managed pod that is not Succeeded or Failed (a
// Job marked Failed can still have a terminating pod writing state). ctx
// bounds the list calls this makes through uncached. It also returns an
// error from a failed list.
func usedServiceAccounts(ctx context.Context, uncached client.Reader, namespace string) (sets.Set[string], error) {
	clusters := &infrav1.TerraformClusterList{}
	machines := &infrav1.TerraformMachineList{}
	pools := &infrav1.TerraformMachinePoolList{}
	for _, l := range []client.ObjectList{clusters, machines, pools} {
		if err := uncached.List(ctx, l, client.InNamespace(namespace)); err != nil {
			return nil, fmt.Errorf("rbac: list terraform objects in %s: %w", namespace, err)
		}
	}
	used := sets.New(ServiceAccount)
	add := func(name string) {
		if name != "" {
			used.Insert(name)
		}
	}
	defaultsSA := func(tc *infrav1.TerraformCluster) string {
		if d := tc.Spec.Defaults; d != nil && d.Jobs != nil {
			return d.Jobs.ServiceAccountName
		}
		return ""
	}
	for i := range clusters.Items {
		if j := clusters.Items[i].Spec.Jobs; j != nil {
			add(j.ServiceAccountName)
		}
	}
	ix := identity.IndexClusters(clusters.Items)
	unresolved := false
	inherit := func(o client.Object, jobs *infrav1.JobPolicy) {
		switch tc := ix.For(o); {
		case jobs != nil && jobs.ServiceAccountName != "":
			add(jobs.ServiceAccountName)
		case tc != nil:
			add(defaultsSA(tc))
		default:
			unresolved = true
		}
	}
	for i := range machines.Items {
		inherit(&machines.Items[i], machines.Items[i].Spec.Jobs)
	}
	for i := range pools.Items {
		inherit(&pools.Items[i], pools.Items[i].Spec.Jobs)
	}
	if unresolved {
		for i := range clusters.Items {
			add(defaultsSA(&clusters.Items[i]))
		}
	}
	jobList := &batchv1.JobList{}
	podList := &corev1.PodList{}
	for _, l := range []client.ObjectList{jobList, podList} {
		if err := uncached.List(ctx, l, client.InNamespace(namespace), managed); err != nil {
			return nil, fmt.Errorf("rbac: list jobs and pods in %s: %w", namespace, err)
		}
	}
	for i := range jobList.Items {
		if jobs.OutcomeOf(&jobList.Items[i]) == jobs.Running {
			add(jobList.Items[i].Spec.Template.Spec.ServiceAccountName)
		}
	}
	for i := range podList.Items {
		if p := &podList.Items[i]; p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			add(p.Spec.ServiceAccountName)
		}
	}
	return used, nil
}

// managedNamespaces lists the namespaces holding a captf.io/managed=true
// ServiceAccount, RoleBinding or Lease, sorted, listed through uncached and
// bounded by ctx. It returns that sorted list, or an error from a failed
// list.
func managedNamespaces(ctx context.Context, uncached client.Reader) ([]string, error) {
	found := sets.New[string]()
	sas := &corev1.ServiceAccountList{}
	rbs := &rbacv1.RoleBindingList{}
	leases := &coordinationv1.LeaseList{}
	for _, l := range []client.ObjectList{sas, rbs, leases} {
		if err := uncached.List(ctx, l, managed); err != nil {
			return nil, fmt.Errorf("rbac: list managed objects: %w", err)
		}
	}
	for _, o := range sas.Items {
		found.Insert(o.Namespace)
	}
	for _, o := range rbs.Items {
		found.Insert(o.Namespace)
	}
	for _, o := range leases.Items {
		found.Insert(o.Namespace)
	}
	return sets.List(found), nil
}
