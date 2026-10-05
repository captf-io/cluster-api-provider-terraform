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

package shared

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	cbmetrics "k8s.io/component-base/metrics"
	"k8s.io/klog/v2/ktesting"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/locks"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// provisioned sets m's status.initialization.provisioned to true.
func provisioned(m *infrav1.TerraformMachine) { m.Status.Initialization.Provisioned = new(true) }

// jobNamed returns the runner's Job called name, failing t when none
// matches.
func (e *env) jobNamed(t *testing.T, name string) *batchv1.Job {
	t.Helper()
	for i := range e.runner.jobs {
		if e.runner.jobs[i].Name == name {
			return &e.runner.jobs[i]
		}
	}
	t.Fatalf("no Job %s", name)
	return nil
}

// TestHealthSamplesPerRefresh: a steadily unhealthy instance whose refresh
// changes nothing keeps its state serial; each completed refresh is still
// one sample, so the threshold is reachable. A reconcile without a newly
// completed refresh adds none.
func TestHealthSamplesPerRefresh(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused, provisioned, func(m *infrav1.TerraformMachine) {
		m.Status.ObservedStateSerial = 5
	}))...)
	e.state.st = &state.State{Serial: 5, InputsHash: "h1:x"}
	e.runner.jobs = append(e.runner.jobs, job("r1", jobs.OpRefresh, jobs.Succeeded, t0.Add(-2*time.Minute)))
	sick := &contract.Health{State: contract.HealthRunning, Healthy: false}
	reconcile := func() int32 {
		t.Helper()
		k := e.kindFor(t, readyOwner)
		k.health = sick
		if _, err := reconcileOnce(t, e, k); err != nil {
			t.Fatal(err)
		}
		return e.get(t).Status.UnhealthySamples
	}
	if n := reconcile(); n != 1 {
		t.Fatalf("after the first refresh: %d samples, want 1", n)
	}
	e.runner.jobs = append(e.runner.jobs, job("r2", jobs.OpRefresh, jobs.Succeeded, t0.Add(-time.Minute)))
	if n := reconcile(); n != 2 {
		t.Fatalf("after the second refresh with the same serial: %d samples, want 2", n)
	}
	if n := reconcile(); n != 2 {
		t.Errorf("a reconcile without a new refresh counted: %d samples, want 2", n)
	}
}

// TestHealthCheckInterval: with annotateMachine a provisioned machine is
// refreshed at remediation.healthCheckIntervalSeconds (default 300) even
// with drift off; without it, health follows drift, here never.
func TestHealthCheckInterval(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		annotate bool
		want     int
	}{
		{"annotateMachine: refreshed at the health interval", true, 1},
		{"no annotateMachine: health waits for drift", false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, world(machine(withFinalizer, notPaused, provisioned, func(m *infrav1.TerraformMachine) {
				m.Spec.Drift = &infrav1.MachineDriftPolicy{IntervalSeconds: new(int32(0))}
				m.Spec.Remediation = &infrav1.MachineRemediation{AnnotateMachine: new(tt.annotate)}
				m.Status.LastRefresh = &metav1.Time{Time: t0.Add(-10 * time.Minute)}
			}))...)
			k := e.kindFor(t, readyOwner)
			if err := inputs.Write(t.Context(), e.c, k.obj, renderMachine(t), inputs.Meta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
				t.Fatal(err)
			}
			e.state.st = &state.State{InputsHash: "h1:x"}
			k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
			if _, err := reconcileOnce(t, e, k); err != nil {
				t.Fatal(err)
			}
			if len(e.runner.created) != tt.want {
				t.Fatalf("created %v, want %d Job(s)", e.runner.created, tt.want)
			}
			if tt.want == 1 && jobs.OpOf(e.jobNamed(t, e.runner.created[0])) != jobs.OpRefresh {
				t.Errorf("started %s, want a refresh", e.runner.created[0])
			}
		})
	}
}

// TestResolveHealthCheckInterval proves Resolve's HealthCheckInterval falls
// back to DefaultHealthCheckInterval when remediation.annotateMachine is on
// but the interval is unset, honors an explicit interval, and is 0 when
// annotateMachine is off.
func TestResolveHealthCheckInterval(t *testing.T) {
	t.Parallel()
	on := &infrav1.MachineRemediation{AnnotateMachine: new(true)}
	if got := Resolve(SpecView{InheritsDefaults: true, Remediation: on}, nil, 0, false).HealthCheckInterval; got != DefaultHealthCheckInterval {
		t.Errorf("default = %s, want %s", got, DefaultHealthCheckInterval)
	}
	set := &infrav1.MachineRemediation{AnnotateMachine: new(true), HealthCheckIntervalSeconds: 600}
	if got := Resolve(SpecView{InheritsDefaults: true, Remediation: set}, nil, 0, false).HealthCheckInterval; got != 10*time.Minute {
		t.Errorf("set = %s, want 10m", got)
	}
	off := &infrav1.MachineRemediation{AnnotateMachine: new(false), HealthCheckIntervalSeconds: 600}
	if got := Resolve(SpecView{InheritsDefaults: true, Remediation: off}, nil, 0, false).HealthCheckInterval; got != 0 {
		t.Errorf("annotateMachine false = %s, want 0", got)
	}
}

// TestRevertAfterFailedApply: apply A succeeded, apply B failed, the spec
// went back to A. The inputs equal the state's hash, but the newest apply
// failed (B may have half-applied): A is applied again. A failed drift
// remediation, by contrast, stays under the remediation cap.
func TestRevertAfterFailedApply(t *testing.T) {
	t.Parallel()
	hA, err := hash.Inputs(contract.RoleMachine, "registry.example/mod:1.0", machineIn())
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name        string
		remediation bool
		want        int
	}{
		{"reverted spec re-applies", false, 1},
		{"a failed remediation is not retried as unconverged", true, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, world(machine(withFinalizer, notPaused, provisioned, func(m *infrav1.TerraformMachine) {
				m.Spec.Drift = &infrav1.MachineDriftPolicy{IntervalSeconds: new(int32(0))}
			}))...)
			a := job("a", jobs.OpApply, jobs.Succeeded, t0.Add(-2*time.Hour))
			a.Annotations = map[string]string{state.InputsHashAnnotation: hA}
			b := job("b", jobs.OpApply, jobs.Failed, t0.Add(-10*time.Minute))
			b.Annotations = map[string]string{state.InputsHashAnnotation: "h1:b"}
			if tt.remediation {
				b.Annotations[RemediationAnnotation] = "true"
			}
			e.runner.jobs = append(e.runner.jobs, a, b)
			e.state.st = &state.State{InputsHash: hA}
			k := e.kindFor(t, readyOwner)
			k.mutable = true
			k.in = machineIn()
			k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
			if _, err := reconcileOnce(t, e, k); err != nil {
				t.Fatal(err)
			}
			if len(e.runner.created) != tt.want {
				t.Fatalf("created %v, want %d", e.runner.created, tt.want)
			}
			if tt.want == 1 && jobs.OpOf(e.jobNamed(t, e.runner.created[0])) != jobs.OpApply {
				t.Errorf("started %s, want an apply", e.runner.created[0])
			}
		})
	}
}

// TestStartJobMarksRemediation: a remediation apply carries
// RemediationAnnotation; an ordinary one does not.
func TestStartJobMarksRemediation(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	suffix, err := state.Suffix(testNS, state.KindTerraformMachine, testName)
	if err != nil {
		t.Fatal(err)
	}
	for i, remediation := range []bool{false, true} {
		req := JobRequest{
			Op: jobs.OpApply, Files: renderMachine(t), InputsHash: "h1:x", Source: infrav1.Source{Image: "registry.example/mod:1.0"},
			Identity: testIdentity, Suffix: suffix, ClusterName: "c1", Attempt: int32(i + 1), Remediation: remediation,
		}
		created := e.startLeased(t, e.kindFor(t, readyOwner), req)
		if got := created.Annotations[RemediationAnnotation] == "true"; got != remediation {
			t.Errorf("remediation %v: annotation %v", remediation, got)
		}
	}
}

// TestPinDigestSkipsAnotherImage: a successful apply that ran v1 does not
// pin v1's digest onto a durable Secret that records v2 (the image changed
// and inputs.Write cleared the pin).
func TestPinDigestSkipsAnotherImage(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	k := e.kindFor(t, readyOwner)
	k.mutable = true
	if err := inputs.Write(t.Context(), e.c, k.obj, renderMachine(t), inputs.Meta{Image: "registry.example/mod:2.0", Identity: testIdentity}); err != nil {
		t.Fatal(err)
	}
	durable, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil {
		t.Fatal(err)
	}
	old := job("a", jobs.OpApply, jobs.Succeeded, t0)
	old.Spec.Template.Spec.Containers = []corev1.Container{{Name: jobs.SourceContainer, Image: "registry.example/mod:1.0"}}
	pod := corev1.Pod{}
	pod.Spec.Containers = []corev1.Container{{Name: jobs.SourceContainer, Image: "registry.example/mod:1.0"}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: jobs.SourceContainer, ImageID: "registry.example/mod@sha256:" + strings.Repeat("a", 64)}}
	pinned, err := pinDigest(t.Context(), e.d, k, &finished{job: &old, ok: true, pod: &pod}, durable)
	if err != nil || pinned != "" {
		t.Fatalf("pinDigest = %q, %v; want nothing pinned", pinned, err)
	}
	if d, _ := inputs.Read(t.Context(), e.c, testNS, "m", testName); d.Meta.ImageDigest != "" {
		t.Errorf("digest %s pinned onto the v2 Secret", d.Meta.ImageDigest)
	}
}

// TestIdentityPinnedForDestroy: a provisioned machine's destroy runs with
// the identity its apply recorded, even after the cluster's
// defaults.identityRef moved to another identity (here one that does not
// even exist).
func TestIdentityPinnedForDestroy(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(deleting, notPaused, provisioned, func(m *infrav1.TerraformMachine) {
		m.Spec.IdentityRef = infrav1.IdentityReference{}
	}))...)
	k := e.kindFor(t, OwnerInfo{HasOwnerRef: true, Cluster: cluster(false), InfraCluster: &infrav1.TerraformCluster{
		Spec: infrav1.TerraformClusterSpec{Defaults: &infrav1.TerraformClusterDefaults{IdentityRef: infrav1.IdentityReference{Name: "other"}}},
	}})
	if err := inputs.Write(t.Context(), e.c, k.obj, renderMachine(t), inputs.Meta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
		t.Fatal(err)
	}
	e.state.st = &state.State{InputsHash: "h1:x"}
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	if len(e.runner.created) != 1 {
		t.Fatalf("created %v, want the destroy", e.runner.created)
	}
	destroy := e.jobNamed(t, e.runner.created[0])
	raw, err := json.Marshal(destroy.Spec.Template.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if jobs.OpOf(destroy) != jobs.OpDestroy || !strings.Contains(string(raw), identity.MirrorName(testIdentity)) || strings.Contains(string(raw), identity.MirrorName("other")) {
		t.Errorf("destroy %s mounts the wrong identity: %s", destroy.Name, raw)
	}
}

// TestStateLost: a provisioned object whose state Secret vanished, or whose
// state lost its inputs hash, gets StateReadable False/StateLost; no Job
// renders nil inputs, and the reconcile is not an error.
func TestStateLost(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		st   *state.State
	}{
		{"state Secret missing", nil},
		{"state without an inputs hash", &state.State{Serial: 3}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, world(machine(withFinalizer, notPaused, provisioned))...)
			e.state.st = tt.st
			k := e.kindFor(t, readyOwner)
			k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
			requeue, err := reconcileOnce(t, e, k)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			c := conditions.Get(e.get(t), infrav1.StateReadableCondition)
			if c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.StateLostReason || requeue != StateRequeue || len(e.runner.created) != 0 {
				t.Errorf("StateReadable = %+v, requeue %s, created %v", c, requeue, e.runner.created)
			}
		})
	}
}

// TestBookkeptJobCostsNoCalls: a finished Job is bookkept once (its pods
// read, its per-run Secret deleted, the Job annotated); afterwards its pods
// are not listed and its Secret is not deleted again, and the conditions
// and lastRun it set stay, although its pod can no longer be read.
func TestBookkeptJobCostsNoCalls(t *testing.T) {
	t.Parallel()
	runSecret := func() *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: inputs.RunName("a")}}
	}
	e := newEnv(t, world(machine(withFinalizer, notPaused), runSecret())...)
	failed := job("a", jobs.OpApply, jobs.Failed, t0.Add(-10*time.Second))
	if err := e.c.Create(t.Context(), failed.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	e.runner.jobs = append(e.runner.jobs, failed)
	e.runner.pods["a"] = []corev1.Pod{*podWith("", "ImagePullBackOff")}
	r, reg := recorder(t)
	e.d.Metrics = r

	k := e.kindFor(t, readyOwner)
	k.in = machineIn()
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	stored := &batchv1.Job{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "a"}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Annotations[BookkeptAnnotation] != "true" || !slices.Equal(e.runner.podLists, []string{"a"}) {
		t.Fatalf("annotations %v, pod lists %v", stored.Annotations, e.runner.podLists)
	}
	if c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition); c == nil || c.Reason != infrav1.ImagePullFailedReason {
		t.Fatalf("ApplyJobSucceeded = %+v", c)
	}

	// The next reconcile sees the mark: no pod list, no Secret delete (a
	// Secret put back is left alone), and the pull failure is still
	// reported although the pod is not read.
	e.runner.jobs[0].Annotations = stored.Annotations
	e.runner.podLists = nil
	if err := e.c.Create(t.Context(), runSecret()); err != nil {
		t.Fatal(err)
	}
	k = e.kindFor(t, readyOwner)
	k.in = machineIn()
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	if len(e.runner.podLists) != 0 {
		t.Errorf("pods listed for a bookkept Job: %v", e.runner.podLists)
	}
	if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(runSecret()), &corev1.Secret{}); err != nil {
		t.Errorf("per-run Secret of a bookkept Job deleted again: %v", err)
	}
	m := e.get(t)
	if c := conditions.Get(m, infrav1.ApplyJobSucceededCondition); c == nil || c.Reason != infrav1.ImagePullFailedReason || m.Status.LastRun.Job != "a" {
		t.Errorf("ApplyJobSucceeded = %+v, lastRun %+v", c, m.Status.LastRun)
	}
	if n, err := counterValue(reg, metrics.JobsTotalName); err != nil || n != 1 {
		t.Errorf("jobs counted %v times (%v), want once", n, err)
	}
}

// TestBookkeptOnlyAfterStatusPatch: when the status patch fails, the Job is
// not marked bookkept, so what its pod said is read again next time instead
// of being lost.
func TestBookkeptOnlyAfterStatusPatch(t *testing.T) {
	t.Parallel()
	boom := errors.New("apiserver unavailable")
	e := newEnvWith(t, interceptor.Funcs{SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
		return boom
	}}, world(machine(withFinalizer, notPaused))...)
	failed := job("a", jobs.OpApply, jobs.Failed, t0.Add(-10*time.Second))
	if err := e.c.Create(t.Context(), failed.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	e.runner.jobs = append(e.runner.jobs, failed)
	k := e.kindFor(t, readyOwner)
	k.in = machineIn()
	if _, err := reconcileOnce(t, e, k); !errors.Is(err, boom) {
		t.Fatalf("Reconcile = %v, want the patch error", err)
	}
	stored := &batchv1.Job{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "a"}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Annotations[BookkeptAnnotation] != "" {
		t.Errorf("Job marked bookkept although its results were not persisted: %v", stored.Annotations)
	}
}

// TestDurableReadOncePerReconcile: the durable inputs Secret is read once
// per reconcile and handed to bookkeeping and the adapter, not re-read by
// each of them.
func TestDurableReadOncePerReconcile(t *testing.T) {
	t.Parallel()
	reads := 0
	e := newEnvWith(t, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.Secret); ok && key.Name == inputs.Name("m", testName) {
			reads++
		}
		return c.Get(ctx, key, obj, opts...)
	}}, world(machine(withFinalizer, notPaused, provisioned))...)
	k := e.kindFor(t, readyOwner)
	k.mutable = true
	k.in = machineIn()
	if err := inputs.Write(t.Context(), e.c, k.obj, renderMachine(t), inputs.Meta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
		t.Fatal(err)
	}
	h, err := hash.Inputs(contract.RoleMachine, "registry.example/mod:1.0", machineIn())
	if err != nil {
		t.Fatal(err)
	}
	applied := job("a", jobs.OpApply, jobs.Succeeded, t0.Add(-time.Minute))
	applied.Annotations = map[string]string{state.InputsHashAnnotation: h}
	e.runner.jobs = append(e.runner.jobs, applied)
	e.state.st = &state.State{InputsHash: h}
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	reads = 0
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Errorf("durable Secret read %d times in one reconcile, want 1", reads)
	}
}

// counterValue returns the sum of the counter family name in reg (or of a
// histogram family's sample sums), and any Gather error.
func counterValue(reg cbmetrics.KubeRegistry, name string) (float64, error) {
	fams, err := reg.Gather()
	if err != nil {
		return 0, err
	}
	var sum float64
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			sum += m.GetCounter().GetValue() + m.GetHistogram().GetSampleSum()
		}
	}
	return sum, nil
}

// TestJobAttemptsIsTheRetryNumber: a success after two failures of the
// same op records 3, whatever its lifetime sequence number (a40).
func TestJobAttemptsIsTheRetryNumber(t *testing.T) {
	t.Parallel()
	var objs []client.Object
	for _, n := range []string{"f1", "f2", "ok"} {
		objs = append(objs, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: inputs.RunName(n)}})
	}
	e := newEnv(t, world(append(objs, machine(withFinalizer, notPaused))...)...)
	ok := job("ok", jobs.OpApply, jobs.Succeeded, t0.Add(-time.Hour))
	ok.Labels[jobs.AttemptLabel] = "40"
	e.runner.jobs = append(e.runner.jobs,
		job("f1", jobs.OpApply, jobs.Failed, t0.Add(-3*time.Hour)), job("f2", jobs.OpApply, jobs.Failed, t0.Add(-2*time.Hour)), ok)
	r, reg := recorder(t)
	e.d.Metrics = r
	k := e.kindFor(t, readyOwner)
	k.in = machineIn()
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	if sum, err := counterValue(reg, metrics.JobAttemptsName); err != nil || sum != 3 {
		t.Errorf("captf_job_attempts sum = %v (%v), want 3", sum, err)
	}
}

// TestRetryNumber proves retryNumber counts, for the finished entry at each
// index, the consecutive same-op failures immediately before it (in the
// newest-first list), stopping at the first success or a different op.
func TestRetryNumber(t *testing.T) {
	t.Parallel()
	done := []finished{ // newest first
		done("a4", jobs.OpApply, true, t0, nil),
		done("d1", jobs.OpDrift, false, t0.Add(-time.Minute), nil),
		done("a3", jobs.OpApply, false, t0.Add(-2*time.Minute), nil),
		done("a2", jobs.OpApply, false, t0.Add(-3*time.Minute), nil),
		done("a1", jobs.OpApply, true, t0.Add(-4*time.Minute), nil),
	}
	for i, want := range []int{3, 1, 2, 1, 1} {
		if got := retryNumber(done, i); got != want {
			t.Errorf("%s: retry %d, want %d", done[i].job.Name, got, want)
		}
	}
}

// TestJitter proves Jitter returns 0 for an empty UID or a non-positive
// spread, is deterministic for a given UID and interval, stays under a
// tenth of the interval, and varies across UIDs.
func TestJitter(t *testing.T) {
	t.Parallel()
	if Jitter("", time.Hour) != 0 || Jitter("uid", 5) != 0 {
		t.Error("jitter without a uid or spread")
	}
	a, b := Jitter("uid-1", 30*time.Minute), Jitter("uid-1", 30*time.Minute)
	if a != b || a < 0 || a >= 3*time.Minute {
		t.Errorf("jitter %s, %s: want equal and under 10%%", a, b)
	}
	spread := map[time.Duration]bool{}
	for _, uid := range []string{"a", "b", "c", "d", "e"} {
		spread[Jitter(uid, 30*time.Minute)] = true
	}
	if len(spread) < 4 {
		t.Errorf("jitter barely varies across UIDs: %v", spread)
	}
}

// TestFailedRemediationMessage: after a failed remediation apply,
// DriftDetected goes back to DriftPending with the drift summary and says
// the Job failed; it no longer claims the Job applies anything.
func TestFailedRemediationMessage(t *testing.T) {
	t.Parallel()
	m := machine()
	summary := "Job d: 0 to add, 1 to change, 0 to destroy"
	conditions.Set(m, metav1.Condition{Type: infrav1.DriftDetectedCondition, Status: metav1.ConditionTrue, Reason: infrav1.DriftPendingReason, Message: summary})
	running := job("a", jobs.OpApply, jobs.Running, t0)
	setDriftRemediating(m, infrav1.DriftActionRemediate, &running)
	if c := conditions.Get(m, infrav1.DriftDetectedCondition); !strings.HasPrefix(c.Message, summary) || !strings.Contains(c.Message, "Job a applies") {
		t.Fatalf("remediating message = %q", c.Message)
	}
	failed := job("a", jobs.OpApply, jobs.Failed, t0)
	m.Status.LastDriftCheck = &metav1.Time{Time: t0.Add(-time.Hour)}
	st := CommonStatus{WorkspaceStatus: &m.Status.WorkspaceStatus}
	setDriftResults(m, st, infrav1.DriftActionRemediate, []finished{{job: &failed}}, &failed, false, false)
	c := conditions.Get(m, infrav1.DriftDetectedCondition)
	if c.Reason != infrav1.DriftPendingReason || !strings.HasPrefix(c.Message, summary) || strings.Contains(c.Message, "applies") ||
		!strings.Contains(c.Message, "Job a failed") {
		t.Errorf("DriftDetected = %s %q", c.Reason, c.Message)
	}
}

// TestDriftEventOncePerCheck: DriftPending ↔ DriftRemediating flips of one
// drift check emit no further DriftDetected event; a new check does.
func TestDriftEventOncePerCheck(t *testing.T) {
	t.Parallel()
	rec := &fakeRecorder{}
	d := Deps{Recorder: rec}
	m := machine()
	logger, _ := ktesting.NewTestContext(t)
	set := func(reason, msg string) {
		conditions.Set(m, metav1.Condition{Type: infrav1.DriftDetectedCondition, Status: metav1.ConditionTrue, Reason: reason, Message: msg})
	}
	step := func(reason, msg string) {
		before := snapshot(m)
		set(reason, msg)
		emitTransitions(d, logger, state.KindTerraformMachine, m, before, nil)
	}
	summary := "Job d1: 0 to add, 1 to change, 0 to destroy"
	step(infrav1.DriftPendingReason, summary)
	step(infrav1.DriftRemediatingReason, summary+remediationMarker+"a applies the current inputs")
	step(infrav1.DriftPendingReason, summary+remediationMarker+"a failed")
	step(infrav1.DriftRemediatingReason, summary+remediationMarker+"a2 applies the current inputs")
	if n := rec.count(EventDriftDetected); n != 1 {
		t.Errorf("DriftDetected events = %d over one check, want 1", n)
	}
	step(infrav1.DriftPendingReason, "Job d2: 0 to add, 2 to change, 0 to destroy")
	if n := rec.count(EventDriftDetected); n != 2 {
		t.Errorf("DriftDetected events = %d after a new check, want 2", n)
	}
}

// TestClusterStateSecretToProvisionedMachines: a cluster state write wakes
// only the machines that still build inputs.
func TestClusterStateSecretToProvisionedMachines(t *testing.T) {
	t.Parallel()
	mk := func(name string, prov bool) *infrav1.TerraformMachine {
		m := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name, Labels: map[string]string{clusterv1.ClusterNameLabel: "c1"}}}
		if prov {
			m.Status.Initialization.Provisioned = new(true)
		}
		return m
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(mk("new", false), mk("done", true)).Build()
	suffix, err := state.Suffix(testNS, state.KindTerraformCluster, "c1")
	if err != nil {
		t.Fatal(err)
	}
	base := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: state.SecretName(suffix), Labels: map[string]string{
		state.OwnerKindLabel: state.KindTerraformCluster, clusterv1.ClusterNameLabel: "c1", state.BackendSuffixLabel: suffix,
	}}}
	if got := names(ClusterStateSecretToMachines(c)(t.Context(), base)); !slices.Equal(got, []string{"team-a/new"}) {
		t.Errorf("ClusterStateSecretToMachines = %v, want only the unprovisioned machine", got)
	}
}

// TestMirrorUpdateConflictRetried: a conflict on the shared mirror's Update
// is retried instead of aborting the reconcile before bookkeeping.
func TestMirrorUpdateConflictRetried(t *testing.T) {
	t.Parallel()
	mirror := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: testNS, Name: identity.MirrorName(testIdentity),
		Labels:      map[string]string{identity.MirroredLabel: "true"},
		Annotations: map[string]string{inputs.IdentityAnnotation: testIdentity, identity.SourceHashAnnotation: "stale"},
	}}
	conflicts := 0
	e := newEnvWith(t, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*corev1.Secret); ok && obj.GetName() == mirror.Name && conflicts == 0 {
			conflicts++
			return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, obj.GetName(), errors.New("modified by another reconcile"))
		}
		return c.Update(ctx, obj, opts...)
	}}, world(machine(withFinalizer, notPaused), mirror)...)
	k := e.kindFor(t, readyOwner)
	k.in = machineIn()
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if c := conditions.Get(e.get(t), infrav1.CredentialsMirroredCondition); conflicts != 1 || c == nil || c.Status != metav1.ConditionTrue || len(e.runner.created) != 1 {
		t.Errorf("conflicts %d, CredentialsMirrored %+v, created %v", conflicts, c, e.runner.created)
	}
}

// TestErrorWithoutRequeueAfter: a failing reconcile returns no RequeueAfter
// next to its error (controller-runtime would ignore it with a warning).
func TestErrorWithoutRequeueAfter(t *testing.T) {
	t.Parallel()
	boom := errors.New("apiserver unavailable")
	e := newEnvWith(t, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.Secret); ok && key.Name == inputs.RunName("running") {
			return boom
		}
		return c.Get(ctx, key, obj, opts...)
	}}, world(machine(withFinalizer, notPaused))...)
	e.runner.jobs = append(e.runner.jobs, job("running", jobs.OpApply, jobs.Running, t0))
	k := e.kindFor(t, readyOwner)
	k.in = machineIn()
	res, err := Reconcile(t.Context(), e.d, k)
	if !errors.Is(err, boom) || res.RequeueAfter != 0 {
		t.Errorf("Reconcile = %+v, %v; want the error alone", res, err)
	}
}

// TestStateLockedVisible: a state lock held from a workstation is reported
// as StateReadable False/StateLocked naming the holder, instead of only
// failing Jobs after lockTimeoutSeconds.
func TestStateLockedVisible(t *testing.T) {
	t.Parallel()
	suffix, err := state.Suffix(testNS, state.KindTerraformMachine, testName)
	if err != nil {
		t.Fatal(err)
	}
	holder := "lock-1"
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: state.LeaseName(suffix), Annotations: map[string]string{
			locks.LockInfoAnnotation: `{"ID":"lock-1","Who":"steven@laptop","Operation":"OperationTypeApply","Created":"2026-09-25T11:00:00Z"}`,
		}},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
	e := newEnv(t, world(machine(withFinalizer, notPaused, provisioned), lease)...)
	e.state.st = &state.State{InputsHash: "h1:x"}
	k := e.kindFor(t, readyOwner)
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	c := conditions.Get(e.get(t), infrav1.StateReadableCondition)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.StateLockedReason ||
		!strings.Contains(c.Message, "steven@laptop") || !strings.Contains(c.Message, "2026-09-25T11:00:00Z") {
		t.Errorf("StateReadable = %+v", c)
	}
	if e.rec.count(EventStateLocked) != 1 || e.rec.count(EventStateUnreadable) != 0 {
		t.Errorf("events = %v, want one StateLocked", e.rec.reasons)
	}
}

// TestInputsTooLarge: inputs that render past the Secret size limit are
// reported on ApplyJobSucceeded, not returned as a reconcile error, and no
// Job starts.
func TestInputsTooLarge(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	k := e.kindFor(t, readyOwner)
	big := machineIn()
	big.BootstrapData = base64.StdEncoding.EncodeToString(make([]byte, 1<<20))
	k.in = big
	requeue, err := reconcileOnce(t, e, k)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.InputsTooLargeReason || len(e.runner.created) != 0 || requeue != RetryMax {
		t.Errorf("ApplyJobSucceeded = %+v, created %v, requeue %s", c, e.runner.created, requeue)
	}
}

// withShortDeadline sets m's Job deadline to 60 seconds with no lock
// timeout, so the built-in lock timeout is not shorter than the deadline.
func withShortDeadline(m *infrav1.TerraformMachine) {
	m.Spec.Jobs = &infrav1.JobPolicy{ActiveDeadlineSeconds: 60}
}

// TestInvalidJobPolicy: an effective Job policy whose lock timeout is not
// below its deadline starts no Job and reports JobPolicyInvalid, but a
// destroy still starts so a teardown never wedges on the check.
func TestInvalidJobPolicy(t *testing.T) {
	t.Parallel()
	t.Run("provisioning is refused", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(withFinalizer, notPaused, withShortDeadline))...)
		k := e.kindFor(t, readyOwner)
		k.in = machineIn()
		requeue, err := reconcileOnce(t, e, k)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.JobPolicyInvalidReason || len(e.runner.created) != 0 || requeue != RetryMax {
			t.Errorf("ApplyJobSucceeded = %+v, created %v, requeue %s", c, e.runner.created, requeue)
		}
		if c != nil && !strings.Contains(c.Message, "lockTimeoutSeconds") {
			t.Errorf("message %q does not name the invalid field", c.Message)
		}
	})
	t.Run("destroy still starts", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, world(machine(deleting, notPaused, withShortDeadline))...)
		if err := inputs.Write(t.Context(), e.c, machine(), renderMachine(t), inputs.Meta{Image: "registry.example/mod:1.0", Identity: testIdentity}); err != nil {
			t.Fatal(err)
		}
		e.state.st = &state.State{InputsHash: "h1:x"}
		reconcileMachine(t, e, readyOwner)
		if len(e.runner.created) != 1 {
			t.Errorf("created %v, want one destroy Job", e.runner.created)
		}
	})
}

// TestSetInitialDriftNotChecked: through the reconcile, a fresh object
// reports DriftDetected Unknown/DriftNotChecked, not False/NoDrift.
func TestSetInitialDriftNotChecked(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	k := e.kindFor(t, readyOwner)
	k.in = machineIn()
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	if c := conditions.Get(e.get(t), infrav1.DriftDetectedCondition); c == nil || c.Status != metav1.ConditionUnknown || c.Reason != infrav1.DriftNotCheckedReason {
		t.Errorf("DriftDetected = %+v", c)
	}
}
