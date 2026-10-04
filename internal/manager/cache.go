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

package manager

import (
	"errors"
	"fmt"
	"net/http"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

const (
	// ManagedSecretLabel selects the Secrets CAPTF owns (state, state
	// backups, inputs, credential mirrors); only these are cached.
	ManagedSecretLabel = state.ManagedLabel

	// JobOwnerKindLabel is present on every Job CAPTF creates
	// (internal/jobs.Labels); only these Jobs, which also carry
	// captf.io/managed=true, are cached.
	JobOwnerKindLabel = state.OwnerKindLabel
)

// CacheOptions scopes the manager's informers (CAPD's DefaultNamespaces):
//
//   - ns restricts every informer to one namespace; empty watches all;
//   - Secrets are cached only with captf.io/managed=true (state, inputs,
//     credential mirrors), so not every Secret of the cluster is held in
//     memory;
//   - Jobs are cached only with the owner-kind label and captf.io/managed=true
//     that every CAPTF Job carries, so a Job anyone else creates in a watched
//     namespace is never held or counted.
//
// The sync period is set by ManagerOptions. It returns the resulting
// ctrlcache.Options, scoped to ns.
func CacheOptions(ns string) ctrlcache.Options {
	var namespaces map[string]ctrlcache.Config
	if ns != "" {
		namespaces = map[string]ctrlcache.Config{ns: {}}
	}
	return ctrlcache.Options{
		DefaultNamespaces: namespaces,
		ByObject: map[client.Object]ctrlcache.ByObject{
			&corev1.Secret{}: {Label: labels.SelectorFromSet(labels.Set{ManagedSecretLabel: "true"})},
			&batchv1.Job{}:   {Label: jobSelector()},
		},
	}
}

// VariablesCacheOptions scopes the second cache that backs the
// spec.variablesFrom watches: only ConfigMaps and Secrets labeled
// captf.io/variables=true, in ns (all namespaces when empty), with their
// data stripped before they are stored. The main cache cannot hold them:
// its Secret informer selects captf.io/managed=true, and a cache has one
// label selector per GVK. The data is read through the API reader when
// the variables are resolved, so no value is ever held in memory here. ns
// scopes the cache as CacheOptions does; scheme, mapper and httpClient are
// passed through unchanged to the underlying ctrlcache.Options, from the
// manager's own Scheme, RESTMapper and HTTP client. It returns the resulting
// ctrlcache.Options.
func VariablesCacheOptions(ns string, scheme *runtime.Scheme, mapper meta.RESTMapper, httpClient *http.Client) ctrlcache.Options {
	var namespaces map[string]ctrlcache.Config
	if ns != "" {
		namespaces = map[string]ctrlcache.Config{ns: {}}
	}
	sel := labels.SelectorFromSet(labels.Set{infrav1.VariablesSourceLabel: "true"})
	return ctrlcache.Options{
		HTTPClient:        httpClient,
		Scheme:            scheme,
		Mapper:            mapper,
		DefaultNamespaces: namespaces,
		ByObject: map[client.Object]ctrlcache.ByObject{
			&corev1.Secret{}:    {Label: sel, Transform: StripData},
			&corev1.ConfigMap{}: {Label: sel, Transform: StripData},
		},
	}
}

// StripData drops the data of obj before the variables cache stores it, when
// obj is a *corev1.Secret or *corev1.ConfigMap; the watches need only the
// name and namespace. It returns obj unchanged, mutated in place, and a nil
// error: it implements the ctrlcache.TransformFunc signature, which never
// fails for this transform.
func StripData(obj any) (any, error) {
	switch o := obj.(type) {
	case *corev1.Secret:
		o.Data, o.StringData = nil, nil
	case *corev1.ConfigMap:
		o.Data, o.BinaryData = nil, nil
	}
	return obj, nil
}

// VariablesCache wraps the variables cache for mgr.Add: GetCache puts it in
// the manager's cache group, which starts and syncs before leader election
// and the controllers, as the main cache does.
type VariablesCache struct {
	ctrlcache.Cache
}

// GetCache returns the wrapped cache.
func (v VariablesCache) GetCache() ctrlcache.Cache { return v.Cache }

// UncachedObjects are the types the default client reads straight from the
// API server instead of starting an informer for them. Code that must see
// objects outside the cache scope (bootstrap, identity and user Secrets,
// override ServiceAccounts) takes mgr.GetAPIReader() explicitly; this list
// keeps the default client from starting cluster-wide informers by accident:
//
//   - Secret, ConfigMap: reads outside the captf.io/managed selector;
//   - Pod: Job pod status is read on demand, no watch needs it;
//   - Lease: internal/locks reads lock Leases on demand; a cached Get would
//     start a cluster-wide Lease informer;
//   - ServiceAccount, RoleBinding: internal/rbac reads the runner's RBAC
//     objects on demand, including admin-created, unlabeled override
//     ServiceAccounts; no watch needs them.
//
// Namespace stays cached: the allowedNamespaces selector needs a Namespace
// watch. It returns the fixed list of uncached object types.
func UncachedObjects() []client.Object {
	return []client.Object{
		&corev1.Secret{},
		&corev1.ConfigMap{},
		&corev1.Pod{},
		&coordinationv1.Lease{},
		&corev1.ServiceAccount{},
		&rbacv1.RoleBinding{},
	}
}

// NewScheme registers every API group the manager reads or writes. The
// client-go scheme already covers batch (Jobs), coordination (Leases) and
// rbac (RoleBindings). It returns the built *runtime.Scheme, or an error if
// any AddToScheme call fails.
func NewScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		apiextensionsv1.AddToScheme,
		clusterv1.AddToScheme,
		infrav1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			return nil, fmt.Errorf("manager: build scheme: %w", err)
		}
	}
	return scheme, nil
}

// jobSelector returns the Job informer's selector: the owner-kind label
// present and captf.io/managed=true.
func jobSelector() labels.Selector {
	req, err := labels.NewRequirement(state.ManagedLabel, selection.Equals, []string{"true"})
	if err != nil {
		// Only reachable with an invalid constant key.
		panic(errors.Join(fmt.Errorf("manager: label requirement %q", state.ManagedLabel), err))
	}
	return existsSelector(JobOwnerKindLabel).Add(*req)
}

// existsSelector returns a label selector matching any object that carries
// the label key, regardless of its value.
func existsSelector(key string) labels.Selector {
	req, err := labels.NewRequirement(key, selection.Exists, nil)
	if err != nil {
		// Only reachable with an invalid constant key.
		panic(errors.Join(fmt.Errorf("manager: label requirement %q", key), err))
	}
	return labels.NewSelector().Add(*req)
}
