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

package terraformmachine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	cbmetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/testutil"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
)

// recorder records event reasons.
type recorder struct {
	mu      sync.Mutex
	reasons []string
}

// Eventf records reason from an events.EventRecorder.Eventf call.
func (r *recorder) Eventf(_, _ runtime.Object, _, reason, _, _ string, _ ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reasons = append(r.reasons, reason)
}

// metricsRecorder builds a metrics.Recorder registered against a fresh
// component-base metrics.KubeRegistry, failing t on a registration error;
// it returns both.
func metricsRecorder(t *testing.T) (*metrics.Recorder, cbmetrics.KubeRegistry) {
	t.Helper()
	reg := cbmetrics.NewKubeRegistry()
	m := metrics.New()
	if err := m.Register(reg); err != nil {
		t.Fatal(err)
	}
	return m, reg
}

// checkRemediations checks captf_remediation_requests_total in reg against
// the expected requested and withdrawn counts, failing t on a mismatch.
func checkRemediations(t *testing.T, reg cbmetrics.KubeRegistry, requested, withdrawn int) {
	t.Helper()
	want := ""
	if requested+withdrawn > 0 {
		want = "# HELP captf_remediation_requests_total [ALPHA] cluster.x-k8s.io/remediate-machine annotations set on (requested) or removed from (withdrawn) a Machine.\n" +
			"# TYPE captf_remediation_requests_total counter\n"
	}
	if requested > 0 {
		want += fmt.Sprintf("captf_remediation_requests_total{action=\"requested\"} %d\n", requested)
	}
	if withdrawn > 0 {
		want += fmt.Sprintf("captf_remediation_requests_total{action=\"withdrawn\"} %d\n", withdrawn)
	}
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), metrics.RemediationRequestsName); err != nil {
		t.Error(err)
	}
}

// unhealthy returns a provisioned machine opted in to annotation, with its
// InfrastructureHealthy condition set to reason and status.unhealthySamples
// set to samples, with each mut applied in order; it returns the built
// machine.
func unhealthy(reason string, samples int32, mut ...func(*infrav1.TerraformMachine)) *infrav1.TerraformMachine {
	tm := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "m1"}}
	tm.Spec.Remediation = &infrav1.MachineRemediation{AnnotateMachine: new(true)}
	tm.Status.Initialization.Provisioned = new(true)
	tm.Status.UnhealthySamples = samples
	conditions.Set(tm, metav1.Condition{Type: infrav1.InfrastructureHealthyCondition, Status: metav1.ConditionFalse, Reason: reason})
	for _, f := range mut {
		f(tm)
	}
	return tm
}

// TestRemediationReason proves RemediationReason returns a reason only
// when opted in, provisioned, not deleting, and either terminated (one
// sample) or unhealthy/degraded/stopped for at least the threshold's
// consecutive samples.
func TestRemediationReason(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		tm   *infrav1.TerraformMachine
		want bool
	}{
		{"opted out by default", unhealthy(infrav1.InstanceTerminatedReason, 0, func(tm *infrav1.TerraformMachine) { tm.Spec.Remediation = nil }), false},
		{"opted out explicitly", unhealthy(infrav1.InstanceTerminatedReason, 0, func(tm *infrav1.TerraformMachine) {
			tm.Spec.Remediation.AnnotateMachine = new(false)
		}), false},
		{"not provisioned", unhealthy(infrav1.InstanceTerminatedReason, 0, func(tm *infrav1.TerraformMachine) {
			tm.Status.Initialization.Provisioned = nil
		}), false},
		{"deleting", unhealthy(infrav1.InstanceTerminatedReason, 0, func(tm *infrav1.TerraformMachine) {
			tm.DeletionTimestamp = new(metav1.Now())
		}), false},
		{"no health yet", unhealthy("", 0, func(tm *infrav1.TerraformMachine) { tm.Status.Conditions = nil }), false},
		{"terminated: one sample is enough", unhealthy(infrav1.InstanceTerminatedReason, 0), true},
		{"healthy", unhealthy(infrav1.HealthyReason, 0), false},
		{"pending never counts", unhealthy(infrav1.InstancePendingReason, 9), false},
		{"degraded twice", unhealthy(infrav1.InstanceDegradedReason, 2), false},
		{"degraded three times", unhealthy(infrav1.InstanceDegradedReason, 3), true},
		{"stopped past the threshold", unhealthy(infrav1.InstanceStoppedReason, 4), true},
		{"unhealthy with threshold 1", unhealthy(infrav1.InstanceUnhealthyReason, 1, func(tm *infrav1.TerraformMachine) {
			tm.Spec.Remediation.UnhealthyThreshold = 1
		}), true},
	} {
		if got := RemediationReason(tt.tm) != ""; got != tt.want {
			t.Errorf("%s: annotate = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestSyncRemediation proves SyncRemediation annotates the Machine once
// (idempotent on a second call), with the expected field owner, event and
// metric, and does nothing for a nil Machine or an already-healthy one.
func TestSyncRemediation(t *testing.T) {
	t.Parallel()
	machine := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "m1"}}
	var owner string
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(machine).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			po := &client.PatchOptions{}
			po.ApplyOptions(opts)
			owner = po.FieldManager
			return c.Patch(ctx, obj, p, opts...)
		},
	}).Build()
	rec := &recorder{}
	m, reg := metricsRecorder(t)
	d := shared.Deps{Client: c, Recorder: rec, Metrics: m}
	tm := unhealthy(infrav1.InstanceTerminatedReason, 0)
	t.Cleanup(func() {
		// Counted once, though synced twice.
		checkRemediations(t, reg, 1, 0)
	})

	cached := machine.DeepCopy()
	for range 2 { // the second call finds the annotation and does nothing
		if err := SyncRemediation(t.Context(), d, cached, tm); err != nil {
			t.Fatal(err)
		}
	}
	got := &clusterv1.Machine{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(machine), got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Annotations[clusterv1.RemediateMachineAnnotation]; !ok || owner != FieldOwner {
		t.Errorf("annotations %v, field owner %q", got.Annotations, owner)
	}
	if len(rec.reasons) != 1 || rec.reasons[0] != shared.EventRemediationRequested {
		t.Errorf("events = %v, want one RemediationRequested", rec.reasons)
	}

	// Nothing to do: no Machine, or a healthy machine.
	if err := SyncRemediation(t.Context(), d, nil, tm); err != nil {
		t.Error(err)
	}
	if err := SyncRemediation(t.Context(), d, machine.DeepCopy(), unhealthy(infrav1.HealthyReason, 0)); err != nil || len(rec.reasons) != 1 {
		t.Errorf("healthy: %v, events %v", err, rec.reasons)
	}
}

// TestMaybeAnnotateError: unlike a gone Machine, a genuine Patch failure is
// an error (the reconcile requeues instead of silently dropping the
// remediation request), and no event is emitted.
func TestMaybeAnnotateError(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errors.New("etcd unavailable")
		},
	}).Build()
	rec := &recorder{}
	machine := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "m1"}}
	err := SyncRemediation(t.Context(), shared.Deps{Client: c, Recorder: rec}, machine, unhealthy(infrav1.InstanceTerminatedReason, 0))
	if err == nil {
		t.Error("MaybeAnnotate swallowed a genuine Patch error")
	}
	if len(rec.reasons) != 0 {
		t.Errorf("events = %v, want none on failure", rec.reasons)
	}
}

// TestMaybeAnnotateMachineGone: the cached Machine was deleted in the
// meantime; NotFound is not an error.
func TestMaybeAnnotateMachineGone(t *testing.T) {
	t.Parallel()
	gone := apierrors.NewNotFound(schema.GroupResource{Group: clusterv1.GroupVersion.Group, Resource: "machines"}, "m1")
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return gone
		},
	}).Build()
	rec := &recorder{}
	m, reg := metricsRecorder(t)
	machine := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "m1"}}
	if err := SyncRemediation(t.Context(), shared.Deps{Client: c, Recorder: rec, Metrics: m}, machine, unhealthy(infrav1.InstanceTerminatedReason, 0)); err != nil {
		t.Errorf("MaybeAnnotate = %v", err)
	}
	if len(rec.reasons) != 0 {
		t.Errorf("events = %v", rec.reasons)
	}
	checkRemediations(t, reg, 0, 0)
}

// TestSyncRemediationWithdraws: once the instance is Healthy again, the
// annotation CAPTF set is removed (a recovered blip must not get the
// Machine replaced later), but never one someone else set, never on a
// Machine being deleted, and not while the instance is still unhealthy.
func TestSyncRemediationWithdraws(t *testing.T) {
	t.Parallel()
	ours := map[string]string{clusterv1.RemediateMachineAnnotation: "", RequestedByAnnotation: "the instance is terminated"}
	theirs := map[string]string{clusterv1.RemediateMachineAnnotation: ""}
	healthy := func(tm *infrav1.TerraformMachine) {
		conditions.Set(tm, metav1.Condition{Type: infrav1.InfrastructureHealthyCondition, Status: metav1.ConditionTrue, Reason: infrav1.HealthyReason})
	}
	for _, tt := range []struct {
		name        string
		annotations map[string]string
		deleting    bool
		tm          *infrav1.TerraformMachine
		removed     bool
	}{
		{"healthy again: withdrawn", ours, false, unhealthy(infrav1.InstanceTerminatedReason, 0, healthy), true},
		{"someone else's annotation: kept", theirs, false, unhealthy(infrav1.InstanceTerminatedReason, 0, healthy), false},
		{"Machine being deleted: kept", ours, true, unhealthy(infrav1.InstanceTerminatedReason, 0, healthy), false},
		{"still unhealthy below the threshold: kept", ours, false, unhealthy(infrav1.InstanceUnhealthyReason, 1), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			machine := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "m1", Annotations: maps.Clone(tt.annotations)}}
			if tt.deleting {
				machine.Finalizers = []string{"test"}
				machine.DeletionTimestamp = new(metav1.Now())
			}
			c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(machine.DeepCopy()).Build()
			rec := &recorder{}
			m, reg := metricsRecorder(t)
			if err := SyncRemediation(t.Context(), shared.Deps{Client: c, Recorder: rec, Metrics: m}, machine, tt.tm); err != nil {
				t.Fatal(err)
			}
			withdrawn := 0
			if tt.removed {
				withdrawn = 1
			}
			checkRemediations(t, reg, 0, withdrawn)
			got := &clusterv1.Machine{}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(machine), got); err != nil {
				t.Fatal(err)
			}
			_, has := got.Annotations[clusterv1.RemediateMachineAnnotation]
			if has == tt.removed {
				t.Errorf("annotations %v, removed wanted %v", got.Annotations, tt.removed)
			}
			if tt.removed && (len(rec.reasons) != 1 || rec.reasons[0] != shared.EventRemediationWithdrawn || got.Annotations[RequestedByAnnotation] != "") {
				t.Errorf("events %v, annotations %v", rec.reasons, got.Annotations)
			}
		})
	}
}

// TestForgedOwnerNeverRemediatesVictim is the regression test for the
// confused-deputy attack this package's owner check closes: an attacker
// who can create a TerraformMachine forges its ownerRef at an existing
// victim Machine (testMachine, which actually owns a different
// TerraformMachine, "c1-md-0-xyz") and reports a terminated instance.
// Before the back-reference check, adapter.Owner would resolve the victim
// Machine and the reconciler's SyncRemediation call would annotate it for
// remediation, and CAPI would delete it. Now Owner must gate with
// OwnerMismatch and resolve no Machine at all, so SyncRemediation (called
// with owner.Machine, exactly as the reconciler calls it) must leave the
// victim untouched.
func TestForgedOwnerNeverRemediatesVictim(t *testing.T) {
	t.Parallel()
	victim := testMachine() // owns "c1-md-0-xyz", not the forged object below.
	forged := testTM(func(tm *infrav1.TerraformMachine) {
		tm.Name = "evil-tm"
		tm.Spec.Remediation = &infrav1.MachineRemediation{AnnotateMachine: new(true)}
		tm.Status.Initialization.Provisioned = new(true)
		conditions.Set(tm, metav1.Condition{Type: infrav1.InfrastructureHealthyCondition, Status: metav1.ConditionFalse, Reason: infrav1.InstanceTerminatedReason})
	})
	a := newTestAdapter(t, forged, stateReader{}, victim)

	owner, err := a.Owner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if owner.Gate == nil || owner.Gate.Reason != infrav1.OwnerMismatchReason || owner.Machine != nil {
		t.Fatalf("owner = %+v, want an OwnerMismatch gate and no Machine", owner)
	}
	if got := RemediationReason(forged); got == "" {
		t.Fatal("fixture is not actually reporting a remediation-worthy terminated instance")
	}

	rec := &recorder{}
	m, reg := metricsRecorder(t)
	d := shared.Deps{Client: a.d.Client, Recorder: rec, Metrics: m}
	if err := SyncRemediation(t.Context(), d, owner.Machine, forged); err != nil {
		t.Fatal(err)
	}
	checkRemediations(t, reg, 0, 0)
	if len(rec.reasons) != 0 {
		t.Errorf("events = %v, want none: the victim Machine must never be touched", rec.reasons)
	}
	got := &clusterv1.Machine{}
	if err := a.d.Client.Get(t.Context(), client.ObjectKeyFromObject(victim), got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Annotations[clusterv1.RemediateMachineAnnotation]; ok {
		t.Errorf("victim Machine annotations = %v, want no remediate-machine annotation", got.Annotations)
	}
}
