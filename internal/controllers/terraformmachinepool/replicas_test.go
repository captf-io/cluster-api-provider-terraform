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

package terraformmachinepool

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
)

// autoscaled sets both autoscaler size annotations on mp, min 1 and max 10.
func autoscaled(mp *clusterv1.MachinePool) {
	if mp.Annotations == nil {
		mp.Annotations = map[string]string{}
	}
	mp.Annotations[clusterv1.AutoscalerMinSizeAnnotation] = "1"
	mp.Annotations[clusterv1.AutoscalerMaxSizeAnnotation] = "10"
}

// managedBy returns a mutator setting the replicas-managed-by annotation
// on a MachinePool to value.
func managedBy(value string) func(*clusterv1.MachinePool) {
	return func(mp *clusterv1.MachinePool) {
		if mp.Annotations == nil {
			mp.Annotations = map[string]string{}
		}
		mp.Annotations[clusterv1.ReplicasManagedByAnnotation] = value
	}
}

// specReplicas returns a mutator setting a MachinePool's spec.replicas to n.
func specReplicas(n int32) func(*clusterv1.MachinePool) {
	return func(mp *clusterv1.MachinePool) { mp.Spec.Replicas = new(n) }
}

// observed returns a mutator setting a pool's status.replicas to n.
func observed(n int32) func(*infrav1.TerraformMachinePool) {
	return func(p *infrav1.TerraformMachinePool) { p.Status.Replicas = new(n) }
}

// TestSyncReplicas proves SyncReplicas annotates an autoscaled pool's
// MachinePool and writes the observed replicas, clamped to the autoscaler bounds, back in one patch, with
// an event only when replicas changed; removes only its own annotation
// once autoscaling is off or invalid; and skips a gone MachinePool, a
// deleting pool and a paused one.
func TestSyncReplicas(t *testing.T) {
	t.Parallel()
	notFound := interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
		return apierrors.NewNotFound(schema.GroupResource{Group: clusterv1.GroupVersion.Group, Resource: "machinepools"}, "c1-mp-0")
	}}
	tests := []struct {
		name       string
		mp         *clusterv1.MachinePool
		tmp        *infrav1.TerraformMachinePool
		funcs      interceptor.Funcs
		annotation *string // nil: absent
		replicas   *int32
		events     int
		patches    int
	}{
		{"enabled, unannotated: annotation and replicas written", testMP(autoscaled, specReplicas(2)), testTMP(observed(5)), interceptor.Funcs{},
			new(ReplicasManagedByValue), new(int32(5)), 1, 1},
		{"enabled, equal replicas: annotation only", testMP(autoscaled, specReplicas(5)), testTMP(observed(5)), interceptor.Funcs{},
			new(ReplicasManagedByValue), new(int32(5)), 0, 1},
		{"enabled, observed unknown: annotation only", testMP(autoscaled, specReplicas(2)), testTMP(), interceptor.Funcs{},
			new(ReplicasManagedByValue), new(int32(2)), 0, 1},
		{"enabled, spec unset: written", testMP(autoscaled), testTMP(observed(3)), interceptor.Funcs{},
			new(ReplicasManagedByValue), new(int32(3)), 1, 1},
		{"enabled, converged: no patch", testMP(autoscaled, managedBy(ReplicasManagedByValue), specReplicas(5)), testTMP(observed(5)), interceptor.Funcs{},
			new(ReplicasManagedByValue), new(int32(5)), 0, 0},
		{"enabled, observed 0, min 0: spec.replicas 0", testMP(autoscaled, func(mp *clusterv1.MachinePool) {
			mp.Annotations[clusterv1.AutoscalerMinSizeAnnotation] = "0"
		}, managedBy(ReplicasManagedByValue), specReplicas(3)), testTMP(observed(0)), interceptor.Funcs{},
			new(ReplicasManagedByValue), new(int32(0)), 1, 1},
		{"enabled, observed above max: clamped to max", testMP(autoscaled, managedBy(ReplicasManagedByValue), specReplicas(3)), testTMP(observed(12)), interceptor.Funcs{},
			new(ReplicasManagedByValue), new(int32(10)), 1, 1},
		{"enabled, observed below min: clamped to min", testMP(autoscaled, managedBy(ReplicasManagedByValue), specReplicas(3)), testTMP(observed(0)), interceptor.Funcs{},
			new(ReplicasManagedByValue), new(int32(1)), 1, 1},
		{"enabled, foreign truthy value: kept, replicas not written", testMP(autoscaled, managedBy("other"), specReplicas(2)), testTMP(observed(5)), interceptor.Funcs{},
			new("other"), new(int32(2)), 0, 0},
		{"enabled, \"false\": replaced with ours", testMP(autoscaled, managedBy("false"), specReplicas(5)), testTMP(observed(5)), interceptor.Funcs{},
			new(ReplicasManagedByValue), new(int32(5)), 0, 1},
		{"disabled with our annotation: removed", testMP(managedBy(ReplicasManagedByValue), specReplicas(2)), testTMP(observed(5)), interceptor.Funcs{},
			nil, new(int32(2)), 0, 1},
		{"disabled with a foreign value: untouched", testMP(managedBy("other"), specReplicas(2)), testTMP(observed(5)), interceptor.Funcs{},
			new("other"), new(int32(2)), 0, 0},
		{"disabled, unannotated: nothing", testMP(specReplicas(2)), testTMP(observed(5)), interceptor.Funcs{},
			nil, new(int32(2)), 0, 0},
		{"invalid annotations with ours: removed", testMP(managedBy(ReplicasManagedByValue), specReplicas(2), func(mp *clusterv1.MachinePool) {
			mp.Annotations[clusterv1.AutoscalerMinSizeAnnotation] = "5"
			mp.Annotations[clusterv1.AutoscalerMaxSizeAnnotation] = "1"
		}), testTMP(observed(5)), interceptor.Funcs{}, nil, new(int32(2)), 0, 1},
		{"MachinePool gone: no error", testMP(autoscaled, specReplicas(2)), testTMP(observed(5)), notFound,
			nil, new(int32(2)), 0, 1},
		{"deleting pool: no-op", testMP(autoscaled, specReplicas(2)), testTMP(observed(5), func(p *infrav1.TerraformMachinePool) {
			p.Finalizers = []string{Finalizer}
			p.DeletionTimestamp = &metav1.Time{Time: t0}
		}), interceptor.Funcs{}, nil, new(int32(2)), 0, 0},
		{"deleting MachinePool: no-op", testMP(autoscaled, specReplicas(2), func(mp *clusterv1.MachinePool) {
			mp.Finalizers = []string{"x"}
			mp.DeletionTimestamp = &metav1.Time{Time: t0}
		}), testTMP(observed(5)), interceptor.Funcs{}, nil, new(int32(2)), 0, 0},
		{"paused: no-op", testMP(autoscaled, specReplicas(2)), testTMP(observed(5), func(p *infrav1.TerraformMachinePool) {
			p.Status.Conditions = []metav1.Condition{{Type: clusterv1.PausedCondition, Status: metav1.ConditionTrue, Reason: clusterv1.PausedReason}}
		}), interceptor.Funcs{}, nil, new(int32(2)), 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			patches := 0
			funcs := tt.funcs
			inner := funcs.Patch
			funcs.Patch = func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				patches++
				if inner != nil {
					return inner(ctx, c, obj, p, opts...)
				}
				return c.Patch(ctx, obj, p, opts...)
			}
			c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(tt.mp).WithInterceptorFuncs(funcs).Build()
			rec := &recorder{}
			mp := &clusterv1.MachinePool{}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(tt.mp), mp); err != nil {
				t.Fatal(err)
			}
			if err := SyncReplicas(t.Context(), shared.Deps{Client: c, Recorder: rec}, mp, tt.tmp); err != nil {
				t.Fatal(err)
			}
			got := &clusterv1.MachinePool{}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(tt.mp), got); err != nil {
				t.Fatal(err)
			}
			value, ok := got.Annotations[clusterv1.ReplicasManagedByAnnotation]
			if (tt.annotation == nil) == ok || (ok && value != *tt.annotation) {
				t.Errorf("replicas-managed-by = %q (present %v), want %v", value, ok, tt.annotation)
			}
			if (got.Spec.Replicas == nil) != (tt.replicas == nil) || (got.Spec.Replicas != nil && *got.Spec.Replicas != *tt.replicas) {
				t.Errorf("spec.replicas = %v, want %v", got.Spec.Replicas, tt.replicas)
			}
			if n := rec.count(shared.EventReplicasWrittenBack); n != tt.events || patches != tt.patches {
				t.Errorf("events %d, patches %d; want %d, %d", n, patches, tt.events, tt.patches)
			}
			// The pass after a write-back is a no-op: it converges.
			if err := SyncReplicas(t.Context(), shared.Deps{Client: c, Recorder: rec}, got, tt.tmp); err != nil {
				t.Fatal(err)
			}
			if patches != tt.patches && tt.funcs.Patch == nil {
				t.Errorf("second pass patched again (%d patches)", patches)
			}
		})
	}
	if err := SyncReplicas(t.Context(), shared.Deps{}, nil, testTMP()); err != nil {
		t.Errorf("nil MachinePool: %v", err)
	}
}

// TestForeignOwnerWarnsOnTransition proves a foreign truthy
// replicas-managed-by value emits one Warning event when the
// AutoscalingActive condition enters ReplicasManagedExternally and none
// while it stays there, while CAPTF's own value and "false" emit none.
func TestForeignOwnerWarnsOnTransition(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		value  string
		warned int
	}{{"other", 1}, {ReplicasManagedByValue, 0}, {"false", 0}} {
		t.Run(tt.value, func(t *testing.T) {
			t.Parallel()
			tmp := testTMP()
			rec := &recorder{}
			a := newAdapter(shared.Deps{Recorder: rec}, tmp)
			mp := testMP(autoscaled, managedBy(tt.value))
			a.setAutoscalingActive(mp)
			a.setAutoscalingActive(mp)
			if n := rec.count(shared.EventReplicasManagedExternally); n != tt.warned {
				t.Errorf("warning events = %d, want %d", n, tt.warned)
			}
		})
	}
}

// TestSyncReplicasForeignOwnerWrites proves a foreign owner's pool is
// left alone: spec.replicas is not written back.
func TestSyncReplicasForeignOwnerWrites(t *testing.T) {
	t.Parallel()
	src := testMP(autoscaled, managedBy("other"), specReplicas(2))
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(src).Build()
	mp := &clusterv1.MachinePool{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(src), mp); err != nil {
		t.Fatal(err)
	}
	if err := SyncReplicas(t.Context(), shared.Deps{Client: c, Recorder: &recorder{}}, mp, testTMP(observed(5))); err != nil {
		t.Fatal(err)
	}
	if mp.Spec.Replicas == nil || *mp.Spec.Replicas != 2 || mp.Annotations[clusterv1.ReplicasManagedByAnnotation] != "other" {
		t.Errorf("MachinePool = replicas %v, annotation %q; want 2 and other", mp.Spec.Replicas, mp.Annotations[clusterv1.ReplicasManagedByAnnotation])
	}
}

// TestAutoscalingActiveForeignOwner proves a valid autoscaler pair with a
// foreign replicas-managed-by value reports AutoscalingActive False with
// ReplicasManagedExternally naming the value, and CAPTF's own value or
// "false" True.
func TestAutoscalingActiveForeignOwner(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		value  string
		status metav1.ConditionStatus
		reason string
	}{
		{"other", metav1.ConditionFalse, infrav1.ReplicasManagedExternallyReason},
		{ReplicasManagedByValue, metav1.ConditionTrue, infrav1.ReplicasManagedByModuleReason},
		{"false", metav1.ConditionTrue, infrav1.ReplicasManagedByModuleReason},
	} {
		t.Run(tt.value, func(t *testing.T) {
			t.Parallel()
			tmp := testTMP()
			a := newTestAdapter(t, tmp, &stateReader{})
			a.setAutoscalingActive(testMP(autoscaled, managedBy(tt.value)))
			got := conditions.Get(tmp, infrav1.AutoscalingActiveCondition)
			if got == nil || got.Status != tt.status || got.Reason != tt.reason {
				t.Fatalf("condition = %+v, want %s/%s", got, tt.status, tt.reason)
			}
			if tt.value == "other" && !strings.Contains(got.Message, `"other"`) {
				t.Errorf("message %q does not name the value", got.Message)
			}
		})
	}
}

// TestForgedOwnerNeverWritesVictimReplicas is the regression test for the
// confused-deputy attack this package's owner check closes: an attacker
// who can create a TerraformMachinePool forges its ownerRef at an existing
// victim MachinePool (testMP, which actually owns a different
// TerraformMachinePool, "c1-mp-0-xyz") and reports observed replicas.
// Before the back-reference check, adapter.Owner would resolve the victim
// MachinePool and the reconciler's SyncReplicas call would patch its
// spec.replicas and replicas-managed-by annotation. Now Owner must gate
// with OwnerMismatch and resolve no MachinePool at all, so SyncReplicas
// (called with owner.MachinePool, exactly as the reconciler calls it) must
// leave the victim untouched.
func TestForgedOwnerNeverWritesVictimReplicas(t *testing.T) {
	t.Parallel()
	victim := testMP(autoscaled, specReplicas(2)) // owns "c1-mp-0-xyz", not the forged object below.
	forged := testTMP(observed(9), func(p *infrav1.TerraformMachinePool) { p.Name = "evil-tmp" })
	a := newTestAdapter(t, forged, &stateReader{}, victim)

	owner, err := a.Owner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if owner.Gate == nil || owner.Gate.Reason != infrav1.OwnerMismatchReason || owner.MachinePool != nil {
		t.Fatalf("owner = %+v, want an OwnerMismatch gate and no MachinePool", owner)
	}

	rec := &recorder{}
	if err := SyncReplicas(t.Context(), shared.Deps{Client: a.d.Client, Recorder: rec}, owner.MachinePool, forged); err != nil {
		t.Fatal(err)
	}
	if n := rec.count(shared.EventReplicasWrittenBack); n != 0 {
		t.Errorf("events = %d, want none: the victim MachinePool must never be touched", n)
	}
	got := &clusterv1.MachinePool{}
	if err := a.d.Client.Get(t.Context(), client.ObjectKeyFromObject(victim), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 2 {
		t.Errorf("victim MachinePool spec.replicas = %v, want unchanged at 2", got.Spec.Replicas)
	}
	if _, ok := got.Annotations[clusterv1.ReplicasManagedByAnnotation]; ok {
		t.Errorf("victim MachinePool annotations = %v, want no replicas-managed-by annotation", got.Annotations)
	}
}
