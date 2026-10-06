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

package manager

import (
	"fmt"
	"slices"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestCacheOptions proves CacheOptions scopes DefaultNamespaces to the
// given namespace (or leaves it nil for every namespace) and applies the
// Secret and Job label selectors, and strips the data of cached Secrets.
func TestCacheOptions(t *testing.T) {
	t.Parallel()
	scoped := CacheOptions("tenant-a")
	if _, ok := scoped.DefaultNamespaces["tenant-a"]; !ok || len(scoped.DefaultNamespaces) != 1 {
		t.Errorf("namespaced DefaultNamespaces = %v, want only tenant-a", scoped.DefaultNamespaces)
	}
	if all := CacheOptions(""); all.DefaultNamespaces != nil {
		t.Errorf("cluster-wide DefaultNamespaces = %v, want nil (all namespaces)", all.DefaultNamespaces)
	}

	selectors := map[string]labels.Selector{}
	for obj, by := range scoped.ByObject {
		switch obj.(type) {
		case *corev1.Secret:
			selectors["secret"] = by.Label
			// The managed Secrets hold state chunks, backups and inputs;
			// the informer must keep only their metadata.
			if by.Transform == nil {
				t.Error("managed Secret informer has no Transform: it would hold every payload")
				break
			}
			got, err := by.Transform(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "s", Labels: map[string]string{"captf.io/managed": "true"}},
				Data:       map[string][]byte{"state": []byte("payload")},
				StringData: map[string]string{"k": "v"},
			})
			if err != nil {
				t.Fatal(err)
			}
			sec := got.(*corev1.Secret)
			if sec.Data != nil || sec.StringData != nil || sec.Name != "s" || sec.Labels["captf.io/managed"] != "true" {
				t.Errorf("Secret Transform = %+v, want data stripped and metadata kept", sec)
			}
		case *batchv1.Job:
			selectors["job"] = by.Label
		default:
			t.Errorf("unexpected ByObject entry %T", obj)
		}
	}
	if s := selectors["secret"]; s == nil || !s.Matches(labels.Set{"captf.io/managed": "true"}) ||
		s.Matches(labels.Set{}) || s.Matches(labels.Set{"captf.io/managed": "false"}) {
		t.Errorf("Secret selector = %v, want captf.io/managed=true", s)
	}
	if s := selectors["job"]; s == nil || !s.Matches(labels.Set{JobOwnerKindLabel: "TerraformMachine", "captf.io/managed": "true"}) ||
		s.Matches(labels.Set{}) || s.Matches(labels.Set{JobOwnerKindLabel: "TerraformMachine"}) ||
		s.Matches(labels.Set{"captf.io/managed": "true"}) ||
		s.Matches(labels.Set{JobOwnerKindLabel: "TerraformMachine", "captf.io/managed": "false"}) {
		t.Errorf("Job selector = %v, want %s present and captf.io/managed=true", s, JobOwnerKindLabel)
	}
}

// TestUncachedObjects proves UncachedObjects returns exactly the expected
// set of types and never includes Namespace.
func TestUncachedObjects(t *testing.T) {
	t.Parallel()
	var got []string
	for _, o := range UncachedObjects() {
		got = append(got, fmt.Sprintf("%T", o))
	}
	want := []string{
		"*v1.ConfigMap", "*v1.Lease", "*v1.Pod", "*v1.RoleBinding", "*v1.Secret", "*v1.ServiceAccount",
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("UncachedObjects = %v, want %v", got, want)
	}
	for _, o := range UncachedObjects() {
		if _, ok := o.(*corev1.Namespace); ok {
			t.Error("Namespace must stay cached: the allowedNamespaces selector watches it")
		}
	}
}

// TestNewScheme proves NewScheme registers the Kubernetes, CAPI and CAPTF
// API groups the manager needs to resolve GVKs for.
func TestNewScheme(t *testing.T) {
	t.Parallel()
	s, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []client.Object{
		&batchv1.Job{}, &coordinationv1.Lease{}, &rbacv1.RoleBinding{}, &corev1.Secret{},
		&infrav1.TerraformCluster{}, &infrav1.TerraformMachine{}, &infrav1.TerraformClusterIdentity{},
	} {
		if _, err := apiutil.GVKForObject(o, s); err != nil {
			t.Errorf("scheme cannot resolve %T: %v", o, err)
		}
	}
}
