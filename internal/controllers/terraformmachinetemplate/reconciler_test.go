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
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/imageinspect"
)

// image is the source image every test template fixture starts with.
const image = "registry.example/machine:1.0"

// fakeInspector is an imageinspect.Inspector stub returning a fixed config
// or error and counting its calls.
type fakeInspector struct {
	cfg   *imageinspect.Config
	err   error
	calls int
	// schemas is the shared schema cache of the reconcilers the test
	// builds around f, as the manager has one; it follows f so that
	// consecutive runs see each other's cached images.
	schemas *imageinspect.SchemaCache
	// now is the schema cache's clock; nil is the wall clock.
	now func() time.Time
}

// Config records the call and returns f's fixed config and error. A
// config without a digest gets one, as a registry always supplies it.
func (f *fakeInspector) Config(context.Context, string, authn.Keychain, *v1.Platform) (*imageinspect.Config, error) {
	f.calls++
	if f.cfg != nil && f.cfg.Digest == "" {
		cfg := *f.cfg
		cfg.Digest = "sha256:fake"
		return &cfg, f.err
	}
	return f.cfg, f.err
}

// cache returns f's schema cache, creating it on first use.
func (f *fakeInspector) cache() *imageinspect.SchemaCache {
	if f.schemas == nil {
		f.schemas = imageinspect.NewSchemaCacheWithClock(func() time.Time {
			if f.now != nil {
				return f.now()
			}
			return time.Now()
		})
	}
	return f.schemas
}

// t0 is the fixed time every test's clock reports.
var t0 = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

// template returns a TerraformMachineTemplate fixture with a fixed image
// and a missing pull Secret, with each mut applied in order; it returns
// the built template.
func template(mut ...func(*infrav1.TerraformMachineTemplate)) *infrav1.TerraformMachineTemplate {
	t := &infrav1.TerraformMachineTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "tpl"}}
	t.Spec.Template.Spec.Source = infrav1.Source{Image: image}
	t.Spec.Template.Spec.Jobs = &infrav1.JobPolicy{ImagePullSecrets: []corev1.LocalObjectReference{{Name: "missing-cred"}}}
	for _, f := range mut {
		f(t)
	}
	return t
}

// run reconciles tpl with insp and no event recorder, failing t on error;
// it returns the reconciled template and Result (see runRecorded).
func run(t *testing.T, tpl *infrav1.TerraformMachineTemplate, insp *fakeInspector) (*infrav1.TerraformMachineTemplate, ctrl.Result) {
	t.Helper()
	return runRecorded(t, tpl, insp, nil)
}

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

// TestCapacityResolvedEvent: CapacityResolved when the capacity changes, and
// not when an image declares none.
func TestCapacityResolvedEvent(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	insp := &fakeInspector{cfg: &imageinspect.Config{Labels: map[string]string{imageinspect.CapacityLabel: `{"cpu":"4","memory":"16Gi"}`}}}
	got, _ := runRecorded(t, template(), insp, rec)
	if !slices.Equal(rec.reasons, []string{shared.EventCapacityResolved}) {
		t.Fatalf("events = %v, want one CapacityResolved", rec.reasons)
	}
	// A new image with the same capacity: resolved again, no event.
	got.Spec.Template.Spec.Source.Image = "registry.example/machine:1.1"
	got, _ = runRecorded(t, got, insp, rec)
	if len(rec.reasons) != 1 {
		t.Errorf("same capacity emitted %v", rec.reasons)
	}
	// A new image with other capacity: an event.
	got.Spec.Template.Spec.Source.Image = "registry.example/machine:2.0"
	insp.cfg = &imageinspect.Config{Labels: map[string]string{imageinspect.CapacityLabel: `{"cpu":"8","memory":"16Gi"}`}}
	runRecorded(t, got, insp, rec)
	if len(rec.reasons) != 2 {
		t.Errorf("changed capacity: events %v", rec.reasons)
	}

	quiet := &recorder{}
	runRecorded(t, template(), &fakeInspector{cfg: &imageinspect.Config{}}, quiet)
	if len(quiet.reasons) != 0 {
		t.Errorf("an image without capacity labels emitted %v", quiet.reasons)
	}
}

// runRecorded reconciles tpl through a Reconciler wired to insp, a fixed
// clock at t0, and rec (if non-nil) as the event recorder, failing t on
// setup or reconcile error. It returns the template's state afterward and
// the Result Reconcile returned.
func runRecorded(t *testing.T, tpl *infrav1.TerraformMachineTemplate, insp *fakeInspector, rec *recorder) (*infrav1.TerraformMachineTemplate, ctrl.Result) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := infrav1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(tpl).WithStatusSubresource(tpl).Build()
	r := &Reconciler{Deps: shared.Deps{Client: c, APIReader: c, Inspector: insp, Schemas: insp.cache(), Clock: testingclock.NewFakePassiveClock(t0)}}
	if rec != nil {
		r.Deps.Recorder = rec
	}
	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tpl)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &infrav1.TerraformMachineTemplate{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(tpl), got); err != nil {
		t.Fatal(err)
	}
	return got, res
}

// reasonOf returns t's CapacityResolved condition reason, or "" if it has
// none.
func reasonOf(t *infrav1.TerraformMachineTemplate) string {
	if c := conditions.Get(t, infrav1.CapacityResolvedCondition); c != nil {
		return c.Reason
	}
	return ""
}

// TestReconcileResolved proves Reconcile sets CapacityResolved, capacity
// and node info from the image's labels, and does not re-inspect an
// already-resolved template with the same image.
func TestReconcileResolved(t *testing.T) {
	t.Parallel()
	insp := &fakeInspector{cfg: &imageinspect.Config{Labels: map[string]string{
		imageinspect.CapacityLabel: `{"cpu":"4","memory":"16Gi"}`,
		imageinspect.NodeInfoLabel: `{"architecture":"amd64","operatingSystem":"linux"}`,
	}}}
	got, res := run(t, template(), insp)
	if reasonOf(got) != infrav1.CapacityResolvedReason || !got.Status.Capacity.Memory().Equal(resource.MustParse("16Gi")) ||
		got.Status.NodeInfo.Architecture != infrav1.ArchitectureAmd64 || got.Status.CapacitySource.Image != image ||
		got.Status.CapacitySource.Source != infrav1.CapacitySourceImage || res.RequeueAfter != imageinspect.SchemaTagTTL {
		t.Errorf("status = %+v, result %+v", got.Status, res)
	}

	// Same image and resolved: no second inspection.
	insp.calls = 0
	if _, _ = run(t, got, insp); insp.calls != 0 {
		t.Error("a resolved template was inspected again")
	}
}

// TestReconcileSpecCapacity proves spec.capacity wins entirely over the
// image's capacity label (even an invalid one), records source Spec, keeps
// node info from the image, and is re-resolved when it changes.
func TestReconcileSpecCapacity(t *testing.T) {
	t.Parallel()
	insp := &fakeInspector{cfg: &imageinspect.Config{Labels: map[string]string{
		imageinspect.CapacityLabel: `{"cpu":"lots"}`,
		imageinspect.NodeInfoLabel: `{"architecture":"arm64"}`,
	}}}
	tpl := template(func(tpl *infrav1.TerraformMachineTemplate) {
		tpl.Spec.Capacity = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
	})
	got, _ := run(t, tpl, insp)
	if reasonOf(got) != infrav1.CapacityResolvedReason || len(got.Status.Capacity) != 1 || !got.Status.Capacity.Cpu().Equal(resource.MustParse("2")) ||
		got.Status.CapacitySource.Source != infrav1.CapacitySourceSpec || got.Status.NodeInfo.Architecture != infrav1.ArchitectureArm64 {
		t.Errorf("status = %+v", got.Status)
	}

	insp.calls = 0
	if _, _ = run(t, got, insp); insp.calls != 0 {
		t.Error("a resolved template was inspected again")
	}
	got.Spec.Capacity = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8")}
	got, _ = run(t, got, insp)
	if insp.calls != 1 || !got.Status.Capacity.Cpu().Equal(resource.MustParse("8")) {
		t.Errorf("changed spec.capacity not re-resolved: calls %d, status %+v", insp.calls, got.Status)
	}
	got.Spec.Capacity = nil
	got, _ = run(t, got, insp)
	if reasonOf(got) != infrav1.CapacityLabelInvalidReason || got.Status.CapacitySource.Source != infrav1.CapacitySourceImage || len(got.Status.Capacity) != 0 {
		t.Errorf("removing the override did not fall back to the image: %+v", got.Status)
	}
}

// TestReconcileNotDeclaredClears proves Reconcile clears a stale capacity
// and sets CapacityNotDeclared when the new image's labels declare none.
func TestReconcileNotDeclaredClears(t *testing.T) {
	t.Parallel()
	tpl := template(func(tpl *infrav1.TerraformMachineTemplate) {
		tpl.Status.Capacity = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8")}
		tpl.Status.CapacitySource.Image = "registry.example/machine:0.9"
	})
	got, _ := run(t, tpl, &fakeInspector{cfg: &imageinspect.Config{Labels: map[string]string{}}})
	if reasonOf(got) != infrav1.CapacityNotDeclaredReason || len(got.Status.Capacity) != 0 || got.Status.CapacitySource.Image != image {
		t.Errorf("status = %+v", got.Status)
	}
}

// TestReconcileLabelInvalid proves Reconcile sets CapacityLabelInvalid
// and clears capacity for an unparsable capacity label while still
// setting node info, and does not re-inspect the same invalid image.
func TestReconcileLabelInvalid(t *testing.T) {
	t.Parallel()
	insp := &fakeInspector{cfg: &imageinspect.Config{Labels: map[string]string{
		imageinspect.CapacityLabel: `{"cpu":"lots"}`,
		imageinspect.NodeInfoLabel: `{"architecture":"arm64"}`,
	}}}
	got, _ := run(t, template(), insp)
	if reasonOf(got) != infrav1.CapacityLabelInvalidReason || len(got.Status.Capacity) != 0 || got.Status.NodeInfo.Architecture != infrav1.ArchitectureArm64 {
		t.Errorf("status = %+v", got.Status)
	}
	// Invalid for the same image: not retried.
	insp.calls = 0
	if _, _ = run(t, got, insp); insp.calls != 0 {
		t.Error("an invalid label was re-inspected for the same image")
	}
}

// TestReconcileInspectFailed proves a failed inspection keeps the
// previous capacity, sets ImageInspectFailed with a message naming the
// missing pull Secret and the failure class but never the raw registry
// body, leaves CapacitySource.Image unset, and requeues after RetryFloor.
func TestReconcileInspectFailed(t *testing.T) {
	t.Parallel()
	prev := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
	tpl := template(func(tpl *infrav1.TerraformMachineTemplate) { tpl.Status.Capacity = prev })
	got, res := run(t, tpl, &fakeInspector{err: &transport.Error{StatusCode: http.StatusUnauthorized, Errors: []transport.Diagnostic{{Message: "raw body"}}}})
	c := conditions.Get(got, infrav1.CapacityResolvedCondition)
	if c == nil || c.Reason != infrav1.ImageInspectFailedReason || !got.Status.Capacity.Cpu().Equal(prev[corev1.ResourceCPU]) ||
		!strings.Contains(c.Message, "missing-cred") || !strings.Contains(c.Message, "unauthorized") || strings.Contains(c.Message, "raw body") ||
		res.RequeueAfter != RetryFloor || got.Status.CapacitySource.Image != "" {
		t.Errorf("condition %+v, status %+v, result %+v", c, got.Status, res)
	}
}

// TestReconcileInspectFailedImageChanged proves a failed inspection of a
// new image clears the capacity and nodeInfo resolved from the old one,
// which would size nodes for the wrong instance type, and reports False.
func TestReconcileInspectFailedImageChanged(t *testing.T) {
	t.Parallel()
	tpl := template(func(tpl *infrav1.TerraformMachineTemplate) {
		tpl.Status.Capacity = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
		tpl.Status.NodeInfo = infrav1.NodeInfo{Architecture: infrav1.ArchitectureArm64}
		tpl.Status.CapacitySource = infrav1.CapacitySource{Source: infrav1.CapacitySourceImage, Image: "registry.example/machine:0.9"}
	})
	insp := &fakeInspector{err: &transport.Error{StatusCode: http.StatusServiceUnavailable}}
	got, _ := run(t, tpl, insp)
	c := conditions.Get(got, infrav1.CapacityResolvedCondition)
	if c == nil || c.Status != metav1.ConditionFalse || len(got.Status.Capacity) != 0 || got.Status.NodeInfo.Architecture != "" || got.Status.CapacitySource.Image != "" {
		t.Errorf("condition %+v, status %+v; want False and the old image's capacity cleared", c, got.Status)
	}
}

// TestReconcileInspectRecheckFailed proves a failed re-check of the image
// the capacity was resolved from keeps the capacity and CapacityResolved
// True, saying the re-check failed.
func TestReconcileInspectRecheckFailed(t *testing.T) {
	t.Parallel()
	now := t0
	insp := &fakeInspector{now: func() time.Time { return now }, cfg: &imageinspect.Config{Labels: map[string]string{imageinspect.CapacityLabel: `{"cpu":"2"}`}}}
	got, _ := run(t, template(), insp)
	if reasonOf(got) != infrav1.CapacityResolvedReason {
		t.Fatalf("first resolution: %+v", got.Status)
	}
	// Past the tag's TTL, the image is read again, and that fails.
	now = now.Add(imageinspect.SchemaTagTTL + time.Second)
	insp.cfg, insp.err = nil, &transport.Error{StatusCode: http.StatusServiceUnavailable}
	got, _ = run(t, got, insp)
	c := conditions.Get(got, infrav1.CapacityResolvedCondition)
	if c == nil || c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "could not be inspected again") || !got.Status.Capacity.Cpu().Equal(resource.MustParse("2")) {
		t.Errorf("condition %+v, capacity %v; want True and the capacity kept", c, got.Status.Capacity)
	}
}

// TestReconcileSpecCapacityInspectFailed proves spec.capacity is applied
// when the image cannot be inspected: source Spec, no image recorded, the
// last known nodeInfo kept, CapacityResolved True with a message noting the
// image was not inspected, VariablesValid Unknown (no schema without the
// image), and a retry scheduled for the image.
func TestReconcileSpecCapacityInspectFailed(t *testing.T) {
	t.Parallel()
	tpl := template(func(tpl *infrav1.TerraformMachineTemplate) {
		tpl.Spec.Capacity = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
		tpl.Status.NodeInfo = infrav1.NodeInfo{Architecture: infrav1.ArchitectureArm64}
	})
	got, res := run(t, tpl, &fakeInspector{err: &transport.Error{StatusCode: http.StatusNotFound}})
	c := conditions.Get(got, infrav1.CapacityResolvedCondition)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != infrav1.CapacityResolvedReason || !strings.HasPrefix(c.Message, notInspected) ||
		!got.Status.Capacity.Cpu().Equal(resource.MustParse("2")) || got.Status.CapacitySource.Source != infrav1.CapacitySourceSpec ||
		got.Status.CapacitySource.Image != "" || got.Status.NodeInfo.Architecture != infrav1.ArchitectureArm64 || res.RequeueAfter != RetryFloor {
		t.Errorf("condition %+v, status %+v, result %+v", c, got.Status, res)
	}
	if v := conditions.Get(got, infrav1.VariablesValidCondition); v == nil || v.Status != metav1.ConditionUnknown || v.Reason != infrav1.VariablesSchemaUnavailableReason {
		t.Errorf("VariablesValid = %+v, want Unknown/%s", v, infrav1.VariablesSchemaUnavailableReason)
	}
	if capacityCurrent(got) {
		t.Error("a template whose image was not inspected counts as current")
	}
}

// TestInspectFailure: the condition message names the image and a class
// of failure, never the registry's or the network's error text.
func TestInspectFailure(t *testing.T) {
	t.Parallel()
	body := "internal-registry-detail-9f3"
	_, badName := name.ParseReference("Not A Reference!")
	if badName == nil {
		t.Fatal("expected a parse error")
	}
	registry := func(code int) error {
		return fmt.Errorf("imageinspect: get x: %w", &transport.Error{StatusCode: code, Errors: []transport.Diagnostic{{Code: "DENIED", Message: body}}})
	}
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"unauthorized", registry(http.StatusUnauthorized), "unauthorized"},
		{"forbidden", registry(http.StatusForbidden), "unauthorized"},
		{"not found", registry(http.StatusNotFound), "not found"},
		{"bad reference", fmt.Errorf("imageinspect: parse: %w", badName), "invalid"},
		{"no linux image", fmt.Errorf("%w: x", imageinspect.ErrNoLinuxImage), "invalid"},
		{"timeout", fmt.Errorf("get: %w: %s", context.DeadlineExceeded, body), "unreachable"},
		{"dial", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New(body)}, "unreachable"},
		{"server error", registry(http.StatusBadGateway), "HTTP 502"},
		{"other", errors.New(body), "manager log"},
	} {
		msg := InspectFailure(image, tt.err)
		if !strings.Contains(msg, image) || !strings.Contains(msg, tt.want) || strings.Contains(msg, body) {
			t.Errorf("%s: %q, want the image and %q without the error text", tt.name, msg, tt.want)
		}
	}
}

// TestReconcileInspectFailedJobsPullSecrets: every one of
// spec.template.spec.jobs's imagePullSecrets feeds image inspection, so each
// missing pull Secret is reported.
func TestReconcileInspectFailedJobsPullSecrets(t *testing.T) {
	t.Parallel()
	tpl := template(func(tpl *infrav1.TerraformMachineTemplate) {
		tpl.Spec.Template.Spec.Jobs.ImagePullSecrets = append(tpl.Spec.Template.Spec.Jobs.ImagePullSecrets, corev1.LocalObjectReference{Name: "jobs-cred"})
	})
	got, _ := run(t, tpl, &fakeInspector{err: errors.New("401 Unauthorized")})
	c := conditions.Get(got, infrav1.CapacityResolvedCondition)
	if c == nil || !strings.Contains(c.Message, "missing-cred") || !strings.Contains(c.Message, "jobs-cred") {
		t.Errorf("condition message %q does not list both Jobs pull Secrets", c.Message)
	}
}

// TestFailedRetry proves failedRetry clamps the time since the failure
// began to [RetryFloor, RetryCeiling], and returns RetryFloor for a
// template with no recorded failure.
func TestFailedRetry(t *testing.T) {
	t.Parallel()
	failing := func(since time.Duration) *infrav1.TerraformMachineTemplate {
		return template(func(tpl *infrav1.TerraformMachineTemplate) {
			tpl.Status.Conditions = []metav1.Condition{
				// An old success: it must not date the failures.
				{Type: infrav1.CapacityResolvedCondition, Status: metav1.ConditionTrue, Reason: infrav1.CapacityResolvedReason, LastTransitionTime: metav1.NewTime(t0.Add(-100 * time.Hour))},
				{Type: infrav1.VariablesValidCondition, Status: metav1.ConditionUnknown, Reason: infrav1.VariablesSchemaUnavailableReason, LastTransitionTime: metav1.NewTime(t0.Add(-since))},
			}
		})
	}
	for _, tt := range []struct {
		since, want time.Duration
	}{{time.Second, RetryFloor}, {3 * time.Minute, 3 * time.Minute}, {2 * time.Hour, RetryCeiling}} {
		if got := failedRetry(failing(tt.since), t0); got != tt.want {
			t.Errorf("failing for %s: retry %s, want %s", tt.since, got, tt.want)
		}
	}
	if got := failedRetry(template(), t0); got != RetryFloor {
		t.Errorf("first failure: %s", got)
	}
}

// TestReconcileNotFound proves Reconcile returns no error and no requeue
// for a request naming a template that does not exist.
func TestReconcileNotFound(t *testing.T) {
	t.Parallel()
	s := runtime.NewScheme()
	if err := infrav1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	r := &Reconciler{Deps: shared.Deps{Client: c}}
	if res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "ns", Name: "gone"}}); err != nil || res.RequeueAfter != 0 {
		t.Errorf("NotFound = %+v, %v", res, err)
	}
}

// variablesValid returns t's VariablesValid condition, or nil.
func variablesValid(t *infrav1.TerraformMachineTemplate) *metav1.Condition {
	return conditions.Get(t, infrav1.VariablesValidCondition)
}

// TestVariablesValid: the template's inline variables are checked against
// the image's schema into VariablesValid, and the schema is remembered in
// the shared cache.
func TestVariablesValid(t *testing.T) {
	t.Parallel()
	schema := `{"type":"object","properties":{"instance_type":{"type":"string"}},"required":["instance_type"],"additionalProperties":false}`
	insp := &fakeInspector{cfg: &imageinspect.Config{Digest: "sha256:aa", Labels: map[string]string{imageinspect.VariablesSchemaLabel: schema}}}
	withVars := func(raw string) func(*infrav1.TerraformMachineTemplate) {
		return func(tpl *infrav1.TerraformMachineTemplate) {
			tpl.Spec.Template.Spec.Variables = runtime.RawExtension{Raw: []byte(raw)}
		}
	}

	got, res := run(t, template(withVars(`{"instance_type":"m5"}`)), insp)
	if c := variablesValid(got); c == nil || c.Status != metav1.ConditionTrue || c.Reason != infrav1.VariablesValidReason || res.RequeueAfter != imageinspect.SchemaTagTTL {
		t.Errorf("valid variables: %+v, %+v", c, res)
	}
	if !capacityCurrent(got) {
		t.Error("a template with valid variables has current capacity")
	}

	got, _ = run(t, template(withVars(`{"instance_type":"m5","instnce":1}`)), insp)
	c := variablesValid(got)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.VariablesRejectedReason || !strings.Contains(c.Message, `"instnce" is not declared`) {
		t.Errorf("invalid variables: %+v", c)
	}

	got, _ = run(t, template(), &fakeInspector{cfg: &imageinspect.Config{}})
	if c := variablesValid(got); c == nil || c.Status != metav1.ConditionTrue || c.Reason != infrav1.VariablesSchemaNotDeclaredReason {
		t.Errorf("no schema: %+v", c)
	}

	got, res = run(t, template(func(tpl *infrav1.TerraformMachineTemplate) {
		tpl.Spec.Template.Spec.VariablesFrom = []infrav1.VariablesSource{{ConfigMapRef: infrav1.VariablesSourceReference{Name: "nope"}}}
	}), insp)
	if c := variablesValid(got); c == nil || c.Status != metav1.ConditionUnknown || c.Reason != infrav1.VariablesSourcePendingReason || res.RequeueAfter != RetryFloor {
		t.Errorf("missing source: %+v, %+v", c, res)
	}

	got, _ = run(t, template(), &fakeInspector{err: errors.New("down")})
	if c := variablesValid(got); c == nil || c.Status != metav1.ConditionUnknown || c.Reason != infrav1.VariablesSchemaUnavailableReason {
		t.Errorf("inspect failure: %+v", c)
	}
}
