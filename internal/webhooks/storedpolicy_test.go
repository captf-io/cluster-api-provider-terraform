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

package webhooks

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// weakPolicy returns a jobs policy a stored object may carry from before
// the security context and deadline rules existed.
func weakPolicy() *infrav1.JobPolicy {
	return &infrav1.JobPolicy{
		SecurityContext:       &corev1.SecurityContext{ReadOnlyRootFilesystem: new(false)},
		ActiveDeadlineSeconds: 200,
	}
}

// otherWeakPolicy returns a second, different weak jobs policy.
func otherWeakPolicy() *infrav1.JobPolicy {
	return &infrav1.JobPolicy{SecurityContext: &corev1.SecurityContext{Privileged: new(true)}}
}

// storedWeak is one kind's update scenario: update builds an object pair
// carrying old and cur, optionally marks the new one deleting, and returns
// ValidateUpdate's error. templated kinds also reject any spec change.
type storedWeak struct {
	name      string
	templated bool
	update    func(old, cur *infrav1.JobPolicy, deleting bool) error
}

// TestStoredWeakJobPolicy proves an update never re-rejects a jobs policy
// that did not change (a metadata-only update, or any update of a deleting
// object), and still rejects a change to another weak policy, for every
// kind that carries a policy.
func TestStoredWeakJobPolicy(t *testing.T) {
	t.Parallel()
	ctx := dryRunContext(false)
	kinds := []storedWeak{
		{"TerraformCluster", false, func(old, cur *infrav1.JobPolicy, deleting bool) error {
			o, n := endpointCluster(nil), endpointCluster(nil)
			o.Spec.Jobs, n.Spec.Jobs = old, cur
			touch(&n.ObjectMeta, deleting)
			_, err := (&TerraformCluster{}).ValidateUpdate(ctx, o, n)
			return err
		}},
		{"TerraformCluster defaults", false, func(old, cur *infrav1.JobPolicy, deleting bool) error {
			o, n := endpointCluster(nil), endpointCluster(nil)
			o.Spec.Defaults = &infrav1.TerraformClusterDefaults{Jobs: old}
			n.Spec.Defaults = &infrav1.TerraformClusterDefaults{Jobs: cur}
			touch(&n.ObjectMeta, deleting)
			_, err := (&TerraformCluster{}).ValidateUpdate(ctx, o, n)
			return err
		}},
		{"TerraformMachine", false, func(old, cur *infrav1.JobPolicy, deleting bool) error {
			o, n := machine(""), machine("")
			o.Spec.Jobs, n.Spec.Jobs = old, cur
			touch(&n.ObjectMeta, deleting)
			_, err := (&TerraformMachine{ManagerUser: testManagerUser}).ValidateUpdate(userContext("someone"), o, n)
			return err
		}},
		{"TerraformMachinePool", false, func(old, cur *infrav1.JobPolicy, deleting bool) error {
			o, n := pool(""), pool("")
			o.Spec.Jobs, n.Spec.Jobs = old, cur
			touch(&n.ObjectMeta, deleting)
			_, err := (&TerraformMachinePool{}).ValidateUpdate(context.Background(), o, n)
			return err
		}},
		{"TerraformMachineTemplate", true, func(old, cur *infrav1.JobPolicy, deleting bool) error {
			o := &infrav1.TerraformMachineTemplate{}
			n := &infrav1.TerraformMachineTemplate{}
			o.Spec.Template.Spec = machine("").Spec
			n.Spec.Template.Spec = machine("").Spec
			o.Spec.Template.Spec.Jobs, n.Spec.Template.Spec.Jobs = old, cur
			touch(&n.ObjectMeta, deleting)
			_, err := (&TerraformMachineTemplate{}).ValidateUpdate(ctx, o, n)
			return err
		}},
		{"TerraformMachinePoolTemplate", true, func(old, cur *infrav1.JobPolicy, deleting bool) error {
			o := &infrav1.TerraformMachinePoolTemplate{}
			n := &infrav1.TerraformMachinePoolTemplate{}
			o.Spec.Template.Spec = pool("").Spec
			n.Spec.Template.Spec = pool("").Spec
			o.Spec.Template.Spec.Jobs, n.Spec.Template.Spec.Jobs = old, cur
			touch(&n.ObjectMeta, deleting)
			_, err := (&TerraformMachinePoolTemplate{}).ValidateUpdate(ctx, o, n)
			return err
		}},
		{"TerraformClusterTemplate", true, func(old, cur *infrav1.JobPolicy, deleting bool) error {
			o := &infrav1.TerraformClusterTemplate{}
			n := &infrav1.TerraformClusterTemplate{}
			o.Spec.Template.Spec = endpointCluster(nil).Spec
			n.Spec.Template.Spec = endpointCluster(nil).Spec
			o.Spec.Template.Spec.Jobs, n.Spec.Template.Spec.Jobs = old, cur
			touch(&n.ObjectMeta, deleting)
			_, err := (&TerraformClusterTemplate{}).ValidateUpdate(ctx, o, n)
			return err
		}},
	}
	for _, k := range kinds {
		t.Run(k.name+"/metadata-only update of a stored weak policy", func(t *testing.T) {
			t.Parallel()
			wantInvalid(t, k.update(weakPolicy(), weakPolicy(), false), false)
		})
		t.Run(k.name+"/changing to another weak policy", func(t *testing.T) {
			t.Parallel()
			err := k.update(weakPolicy(), otherWeakPolicy(), false)
			wantInvalid(t, err, true, "privileged")
		})
		if k.templated {
			continue // a changed template spec is rejected whether or not it deletes
		}
		t.Run(k.name+"/deleting object stays updatable", func(t *testing.T) {
			t.Parallel()
			wantInvalid(t, k.update(weakPolicy(), otherWeakPolicy(), true), false)
		})
	}
}

// touch marks obj as changed in metadata only (a finalizer removal) and,
// when deleting, as being deleted.
func touch(obj *metav1.ObjectMeta, deleting bool) {
	obj.Annotations = map[string]string{"touched": "true"}
	if deleting {
		now := metav1.Now()
		obj.DeletionTimestamp = &now
	}
}
