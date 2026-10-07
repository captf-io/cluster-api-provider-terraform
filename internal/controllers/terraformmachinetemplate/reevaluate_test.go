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

package terraformmachinetemplate

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	testingclock "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/imageinspect"
)

// env is one fake cluster with a long-lived Reconciler, so a test can
// reconcile the same template repeatedly while changing the world around it.
type env struct {
	t     *testing.T
	c     client.Client
	r     *Reconciler
	clock *testingclock.FakeClock
	key   client.ObjectKey
}

// newEnv returns an env of test t holding tpl and objs, reconciling through
// insp.
func newEnv(t *testing.T, insp *fakeInspector, tpl *infrav1.TerraformMachineTemplate, objs ...client.Object) *env {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := infrav1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, tpl)...).WithStatusSubresource(tpl).Build()
	clk := testingclock.NewFakeClock(t0)
	return &env{t: t, c: c, clock: clk, key: client.ObjectKeyFromObject(tpl),
		r: &Reconciler{Deps: shared.Deps{Client: c, APIReader: c, Inspector: insp, Schemas: insp.cache(), Clock: clk}}}
}

// reconcile runs one reconcile and returns the template afterward and the
// Result.
func (e *env) reconcile() (*infrav1.TerraformMachineTemplate, ctrl.Result) {
	e.t.Helper()
	res, err := e.r.Reconcile(e.t.Context(), ctrl.Request{NamespacedName: e.key})
	if err != nil {
		e.t.Fatalf("Reconcile: %v", err)
	}
	got := &infrav1.TerraformMachineTemplate{}
	if err := e.c.Get(e.t.Context(), e.key, got); err != nil {
		e.t.Fatal(err)
	}
	return got, res
}

// strictSchema declares one string variable and nothing else.
const strictSchema = `{"type":"object","properties":{"instance_type":{"type":"string"}},"additionalProperties":false}`

// TestVariablesRevalidatedWhenSourceFixed proves a template whose variables
// were rejected is re-checked, without reading the registry again, when the
// ConfigMap it names is corrected, and that a source broken after the
// fact turns a valid template invalid.
func TestVariablesRevalidatedWhenSourceFixed(t *testing.T) {
	t.Parallel()
	insp := &fakeInspector{cfg: &imageinspect.Config{Labels: map[string]string{imageinspect.VariablesSchemaLabel: strictSchema}}}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "vars", Labels: map[string]string{infrav1.VariablesSourceLabel: "true"}},
		Data:       map[string]string{"instnce_type": "m5"},
	}
	tpl := template(func(tpl *infrav1.TerraformMachineTemplate) {
		tpl.Spec.Template.Spec.VariablesFrom = []infrav1.VariablesSource{{ConfigMapRef: infrav1.VariablesSourceReference{Name: "vars"}}}
	})
	e := newEnv(t, insp, tpl, cm)

	got, _ := e.reconcile()
	if v := variablesValid(got); v == nil || v.Status != metav1.ConditionFalse || v.Reason != infrav1.VariablesRejectedReason {
		t.Fatalf("misspelled key: %+v", v)
	}
	if got, _ = e.reconcile(); variablesValid(got).Status != metav1.ConditionFalse || insp.calls != 1 {
		t.Fatalf("an unchanged template: %+v, %d registry reads", variablesValid(got), insp.calls)
	}

	cm.Data = map[string]string{"instance_type": "m5"}
	if err := e.c.Update(t.Context(), cm); err != nil {
		t.Fatal(err)
	}
	got, _ = e.reconcile()
	if v := variablesValid(got); v.Status != metav1.ConditionTrue || v.Reason != infrav1.VariablesValidReason {
		t.Errorf("after the fix: %+v", v)
	}
	if insp.calls != 1 {
		t.Errorf("registry reads = %d, want 1: the schema comes from the cache", insp.calls)
	}

	cm.Data = map[string]string{"nope": "x"}
	if err := e.c.Update(t.Context(), cm); err != nil {
		t.Fatal(err)
	}
	if got, _ = e.reconcile(); variablesValid(got).Status != metav1.ConditionFalse {
		t.Errorf("a source broken after the fact: %+v", variablesValid(got))
	}
}

// TestMutableTagReinspected proves a template on a mutable tag is read
// again once the tag binding lapses (a re-pushed tag changes the capacity
// label), requeues to do so, and that a digest reference is never read
// again.
func TestMutableTagReinspected(t *testing.T) {
	t.Parallel()
	now := t0
	insp := &fakeInspector{now: func() time.Time { return now },
		cfg: &imageinspect.Config{Digest: "sha256:aa", Labels: map[string]string{imageinspect.CapacityLabel: `{"cpu":"2"}`}}}
	e := newEnv(t, insp, template())

	got, res := e.reconcile()
	if res.RequeueAfter != imageinspect.SchemaTagTTL || got.Status.Capacity.Cpu().Value() != 2 {
		t.Fatalf("first read: %+v, %+v", got.Status.Capacity, res)
	}
	insp.cfg = &imageinspect.Config{Digest: "sha256:bb", Labels: map[string]string{imageinspect.CapacityLabel: `{"cpu":"8"}`}}
	if got, _ = e.reconcile(); insp.calls != 1 || got.Status.Capacity.Cpu().Value() != 2 {
		t.Fatalf("within the TTL the tag is not polled: %d reads, %+v", insp.calls, got.Status.Capacity)
	}
	now = now.Add(imageinspect.SchemaTagTTL + time.Second)
	if got, _ = e.reconcile(); insp.calls != 2 || got.Status.Capacity.Cpu().Value() != 8 {
		t.Errorf("after the TTL the re-pushed tag is read: %d reads, %+v", insp.calls, got.Status.Capacity)
	}

	pinned := "registry.example/machine@sha256:" + "aa"
	insp2 := &fakeInspector{now: func() time.Time { return now }, cfg: &imageinspect.Config{Digest: "sha256:aa"}}
	e2 := newEnv(t, insp2, template(func(tpl *infrav1.TerraformMachineTemplate) { tpl.Spec.Template.Spec.Source.Image = pinned }))
	if _, res = e2.reconcile(); res.RequeueAfter != 0 {
		t.Errorf("a digest reference is not polled: %+v", res)
	}
	now = now.Add(24 * time.Hour)
	e2.reconcile()
	if insp2.calls != 1 {
		t.Errorf("a digest reference was read %d times", insp2.calls)
	}
}

// TestFailedRetryFromFirstFailure proves the retry doubles from the first
// failure of the inspection even for a template with spec.capacity, whose
// CapacityResolved condition keeps the transition time of an older success.
func TestFailedRetryFromFirstFailure(t *testing.T) {
	t.Parallel()
	insp := &fakeInspector{err: &transport.Error{StatusCode: http.StatusServiceUnavailable}}
	tpl := template(func(tpl *infrav1.TerraformMachineTemplate) {
		tpl.Spec.Capacity = corev1.ResourceList{corev1.ResourceCPU: *resourceQty("2")}
		tpl.Status.Conditions = []metav1.Condition{{
			Type: infrav1.CapacityResolvedCondition, Status: metav1.ConditionTrue, Reason: infrav1.CapacityResolvedReason,
			LastTransitionTime: metav1.NewTime(t0.Add(-100 * time.Hour)),
		}}
	})
	e := newEnv(t, insp, tpl)
	var got []time.Duration
	for range 4 {
		_, res := e.reconcile()
		got = append(got, res.RequeueAfter)
		e.clock.Step(res.RequeueAfter)
	}
	// The first failure retries at the floor; each later one at the time
	// since the first failure, so the delays grow rather than jump to the
	// ceiling.
	want := []time.Duration{RetryFloor, RetryFloor, 2 * RetryFloor, 4 * RetryFloor}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("retries = %v, want %v", got, want)
		}
	}
}

// resourceQty returns the resource quantity s parses as.
func resourceQty(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}
