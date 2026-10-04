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

package rbac

import (
	"context"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Names of the runner's RBAC objects. The ClusterRole ships in
// config/rbac/runner_clusterrole.yaml (named runner there, captf-runner after
// the kustomize namePrefix).
const (
	ClusterRole    = "captf-runner"
	ServiceAccount = "captf-runner"
	RoleBinding    = "captf-runner"
	// RunnerLabel, captf.io/runner=true, opts an override ServiceAccount in
	// to being bound to the runner ClusterRole. It is consent, not
	// authorization: any principal who can label the ServiceAccount opts it
	// in, whether or not they administer the namespace. It grants little on
	// its own, because whoever can create pods as that ServiceAccount can
	// already read the namespace's Secrets through volume mounts; it keeps a
	// ServiceAccount from gaining Secret write by merely being named in
	// jobs.serviceAccountName.
	RunnerLabel = "captf.io/runner"
)

// ErrBindingConflict reports that a RoleBinding named captf-runner exists
// without captf.io/managed=true; it is never modified.
var ErrBindingConflict = errors.New("rbac: rolebinding captf-runner is not managed by captf")

// EnsureRunner makes the runner ServiceAccount usable in namespace and
// returns its name and the RunnerRBACReady reason:
//
//   - override set: the ServiceAccount must exist and carry
//     captf.io/runner=true, else ServiceAccountNotOptedIn (no Job). A
//     ServiceAccount that lost the label is also removed from the
//     RoleBinding, so withdrawing consent withdraws the permissions;
//   - otherwise, including an override naming captf-runner itself, the
//     ServiceAccount captf-runner is created with captf.io/managed=true when
//     missing.
//
// Either way the RoleBinding captf-runner binds ClusterRole captf-runner to
// it. Its subjects are the union of every ServiceAccount the namespace's
// objects run as. ctx bounds every call this makes through the client c.
// Errors return RBACFailed.
func EnsureRunner(ctx context.Context, c client.Client, namespace string, override *string) (saName, reason string, err error) {
	// Naming captf-runner explicitly is the default, not an override: it
	// needs no opt-in, and treating it as one would unbind the default runner.
	if override != nil && *override != "" && *override != ServiceAccount {
		name := *override
		sa := &corev1.ServiceAccount{}
		err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sa)
		if client.IgnoreNotFound(err) != nil {
			return "", infrav1.RBACFailedReason, fmt.Errorf("rbac: get serviceaccount %s/%s: %w", namespace, name, err)
		}
		if err != nil || sa.Labels[RunnerLabel] != "true" {
			if err := removeSubject(ctx, c, namespace, name); err != nil {
				return "", infrav1.RBACFailedReason, err
			}
			return "", infrav1.ServiceAccountNotOptedInReason, nil
		}
		if err := ensureBinding(ctx, c, namespace, name); err != nil {
			return "", infrav1.RBACFailedReason, err
		}
		return name, infrav1.RBACReadyReason, nil
	}
	if err := ensureServiceAccount(ctx, c, namespace); err != nil {
		return "", infrav1.RBACFailedReason, err
	}
	if err := ensureBinding(ctx, c, namespace, ServiceAccount); err != nil {
		return "", infrav1.RBACFailedReason, err
	}
	return ServiceAccount, infrav1.RBACReadyReason, nil
}

// ensureServiceAccount creates captf-runner in namespace when missing,
// through client c bounded by ctx. One the namespace admin created is used
// as it is. It returns nil on success, or an error from a failed create
// other than already-exists.
func ensureServiceAccount(ctx context.Context, c client.Client, namespace string) error {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace,
		Name:      ServiceAccount,
		Labels:    map[string]string{state.ManagedLabel: "true"},
	}}
	err := c.Create(ctx, sa)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("rbac: create serviceaccount %s/%s: %w", namespace, ServiceAccount, err)
	}
	klog.FromContext(ctx).Info("Created the runner ServiceAccount", "namespace", namespace, "name", ServiceAccount)
	return nil
}

// roleRef returns the fixed RoleRef every runner RoleBinding points at: the
// ClusterRole captf-runner.
func roleRef() rbacv1.RoleRef {
	return rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: ClusterRole}
}

// subject returns the RoleBinding subject for the ServiceAccount sa in
// namespace.
func subject(namespace, sa string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Namespace: namespace, Name: sa}
}

// newBinding builds the runner RoleBinding for namespace, bound to
// ClusterRole captf-runner with subjects, not yet created on the API
// server. It returns the built RoleBinding.
func newBinding(namespace string, subjects []rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      RoleBinding,
			Labels:    map[string]string{state.ManagedLabel: "true"},
		},
		RoleRef:  roleRef(),
		Subjects: subjects,
	}
}

// ensureBinding makes RoleBinding captf-runner bind the ClusterRole to sa.
// Its subjects are recomputed on every call: sa plus those existing
// subjects that are still legitimate runners (allowedSubjects). The
// captf.io/managed label only says who may modify the binding, not who may
// be in it: anyone who can create a RoleBinding can set that label, so a
// subject the manager did not put there must never be carried into a
// binding of the runner ClusterRole. roleRef is immutable, so a managed
// binding with another roleRef is re-created. ctx bounds every call this
// makes through client c, against the RoleBinding of namespace. It returns
// nil on success, or an error from a failed read, create, delete or update,
// including ErrBindingConflict when the existing binding is not managed.
func ensureBinding(ctx context.Context, c client.Client, namespace, sa string) error {
	want := subject(namespace, sa)
	rb := &rbacv1.RoleBinding{}
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: RoleBinding}, rb)
	if apierrors.IsNotFound(err) {
		if err := c.Create(ctx, newBinding(namespace, []rbacv1.Subject{want})); err != nil {
			return fmt.Errorf("rbac: create rolebinding %s/%s: %w", namespace, RoleBinding, err)
		}
		klog.FromContext(ctx).Info("Created the runner RoleBinding", "namespace", namespace, "name", RoleBinding, "subjectAdded", sa)
		return nil
	}
	if err != nil {
		return fmt.Errorf("rbac: get rolebinding %s/%s: %w", namespace, RoleBinding, err)
	}
	if rb.Labels[state.ManagedLabel] != "true" {
		return fmt.Errorf("%w: %s", ErrBindingConflict, namespace)
	}
	subjects, err := allowedSubjects(ctx, c, namespace, rb.Subjects)
	if err != nil {
		return err
	}
	if !slices.Contains(subjects, want) {
		subjects = append(subjects, want)
	}
	if rb.RoleRef != roleRef() {
		if err := client.IgnoreNotFound(c.Delete(ctx, rb)); err != nil {
			return fmt.Errorf("rbac: delete rolebinding %s/%s: %w", namespace, RoleBinding, err)
		}
		if err := c.Create(ctx, newBinding(namespace, subjects)); err != nil {
			return fmt.Errorf("rbac: create rolebinding %s/%s: %w", namespace, RoleBinding, err)
		}
		klog.FromContext(ctx).Info("Re-created the runner RoleBinding: its roleRef was not the runner ClusterRole", "namespace", namespace, "name", RoleBinding, "subjects", logSubjects(subjects))
		return nil
	}
	if slices.Equal(subjects, rb.Subjects) {
		return nil
	}
	before := rb.Subjects
	rb.Subjects = subjects
	if err := c.Update(ctx, rb); err != nil {
		return fmt.Errorf("rbac: update rolebinding %s/%s: %w", namespace, RoleBinding, err)
	}
	klog.FromContext(ctx).Info("Updated the runner RoleBinding subjects", "namespace", namespace, "name", RoleBinding,
		"added", logSubjects(subjectsMissing(subjects, before)), "removed", logSubjects(subjectsMissing(before, subjects)))
	return nil
}

// allowedSubjects keeps, in order, the subjects of the runner binding that
// the manager could have put there: ServiceAccounts of namespace that are
// captf-runner or carry captf.io/runner=true. Users, groups, other
// namespaces' ServiceAccounts and unlabeled or missing ServiceAccounts are
// dropped. ctx bounds every ServiceAccount read this makes through client c.
// It returns the filtered subjects, or an error from a failed read other
// than not-found.
func allowedSubjects(ctx context.Context, c client.Client, namespace string, subjects []rbacv1.Subject) ([]rbacv1.Subject, error) {
	var out []rbacv1.Subject
	for _, s := range subjects {
		if s.Kind != rbacv1.ServiceAccountKind || s.Namespace != namespace || slices.Contains(out, s) {
			continue
		}
		if s.Name == ServiceAccount {
			out = append(out, s)
			continue
		}
		sa := &corev1.ServiceAccount{}
		err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: s.Name}, sa)
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return nil, fmt.Errorf("rbac: get serviceaccount %s/%s: %w", namespace, s.Name, err)
		}
		if sa.Labels[RunnerLabel] == "true" {
			out = append(out, s)
		}
	}
	return out, nil
}

// removeSubject drops sa from namespace's managed RoleBinding, deleting the
// binding when no subject remains, through client c bounded by ctx. It
// returns nil when the binding is missing or not managed, or an error from
// a failed read, delete or update.
func removeSubject(ctx context.Context, c client.Client, namespace, sa string) error {
	rb := &rbacv1.RoleBinding{}
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: RoleBinding}, rb)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("rbac: get rolebinding %s/%s: %w", namespace, RoleBinding, err)
	}
	if rb.Labels[state.ManagedLabel] != "true" {
		return nil
	}
	drop := subject(namespace, sa)
	subjects := slices.DeleteFunc(slices.Clone(rb.Subjects), func(s rbacv1.Subject) bool { return s == drop })
	switch {
	case len(subjects) == len(rb.Subjects):
		return nil
	case len(subjects) == 0:
		if err := client.IgnoreNotFound(c.Delete(ctx, rb)); err != nil {
			return fmt.Errorf("rbac: delete rolebinding %s/%s: %w", namespace, RoleBinding, err)
		}
		klog.FromContext(ctx).Info("Deleted the runner RoleBinding: its last subject was withdrawn", "namespace", namespace, "name", RoleBinding, "removed", sa)
		return nil
	}
	rb.Subjects = subjects
	if err := c.Update(ctx, rb); err != nil {
		return fmt.Errorf("rbac: update rolebinding %s/%s: %w", namespace, RoleBinding, err)
	}
	klog.FromContext(ctx).Info("Removed a subject from the runner RoleBinding", "namespace", namespace, "name", RoleBinding, "removed", sa)
	return nil
}

// subjectsMissing returns the subjects of from that are not in other, in
// order.
func subjectsMissing(from, other []rbacv1.Subject) []rbacv1.Subject {
	return slices.DeleteFunc(slices.Clone(from), func(s rbacv1.Subject) bool { return slices.Contains(other, s) })
}

// logSubjects returns the names of subjects, in order, for logging.
func logSubjects(subjects []rbacv1.Subject) []string {
	names := make([]string, 0, len(subjects))
	for _, s := range subjects {
		names = append(names, s.Name)
	}
	return names
}
