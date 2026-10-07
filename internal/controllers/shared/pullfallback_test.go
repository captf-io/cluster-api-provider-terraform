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
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// The images of the fallback tests: the pinned digest and tag of the
// applied record, and the machine's spec.source.image.
const (
	pinnedRef  = "registry.example/mod@sha256:abc"
	appliedTag = "registry.example/mod:0.9"
	specRef    = "registry.example/mod:1.0"
)

// TestImageCandidates proves apply and plan run only the spec reference,
// and other operations prefer the pinned digest, then the recorded tag,
// then the source and spec references, without empty or repeated ones.
func TestImageCandidates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                           string
		op                             jobs.Op
		source, pinned, recorded, spec string
		want                           []string
	}{
		{"apply", jobs.OpApply, specRef, pinnedRef, appliedTag, specRef, []string{specRef}},
		{"plan", jobs.OpPlan, specRef, pinnedRef, appliedTag, specRef, []string{specRef}},
		{"destroy, applied record", jobs.OpDestroy, specRef, pinnedRef, appliedTag, specRef, []string{pinnedRef, appliedTag, specRef}},
		{"immutable source", jobs.OpDrift, appliedTag, pinnedRef, appliedTag, specRef, []string{pinnedRef, appliedTag, specRef}},
		{"attempt record", jobs.OpDestroy, appliedTag, "", "registry.example/mod:1.1", specRef, []string{"registry.example/mod:1.1", appliedTag, specRef}},
		{"no record", jobs.OpRefresh, specRef, "", "", specRef, []string{specRef}},
	} {
		if got := ImageCandidates(tc.op, tc.source, tc.pinned, tc.recorded, tc.spec); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestChooseImage proves ChooseImage runs the first candidate not known
// unpullable, reporting a fallback past the first; the last when every
// one is; and always the spec reference for apply and plan.
func TestChooseImage(t *testing.T) {
	t.Parallel()
	all := []string{pinnedRef, appliedTag, specRef}
	for _, tc := range []struct {
		name       string
		op         jobs.Op
		candidates []string
		unpullable []string
		want       string
		fallback   bool
	}{
		{"pinned", jobs.OpDestroy, all, nil, pinnedRef, false},
		{"digest gone", jobs.OpDestroy, all, []string{pinnedRef}, appliedTag, true},
		{"tag gone too", jobs.OpRefresh, all, []string{pinnedRef, appliedTag}, specRef, true},
		{"all gone", jobs.OpDrift, all, all, specRef, true},
		{"unrelated entry", jobs.OpRestore, all, []string{"other:1"}, pinnedRef, false},
		{"single", jobs.OpDestroy, []string{specRef}, []string{specRef}, specRef, false},
		{"apply ignores the list", jobs.OpApply, []string{specRef}, []string{specRef}, specRef, false},
		{"none", jobs.OpDestroy, nil, nil, "", false},
	} {
		if ref, fb := ChooseImage(tc.op, tc.candidates, tc.unpullable); ref != tc.want || fb != tc.fallback {
			t.Errorf("%s: %s (fallback %v), want %s (%v)", tc.name, ref, fb, tc.want, tc.fallback)
		}
	}
}

// pullingPod returns a pod of job whose container of name waits with
// reason, controlled by job when it has a UID.
func pullingPod(job *batchv1.Job, name, reason string) corev1.Pod {
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: job.Name + "-pod"}}
	if job.UID != "" {
		p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: new(true)}}
	}
	waiting := func(n, r string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: n, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: r}}}
	}
	if name == jobs.SourceContainer {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{waiting(name, reason)}
		return p
	}
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{waiting(name, reason)}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{waiting(jobs.SourceContainer, "PodInitializing")}
	return p
}

// activeJob returns a copy of e's running Job of op, failing t when there
// is none.
func (e *env) activeJob(t *testing.T, op jobs.Op) batchv1.Job {
	t.Helper()
	e.runner.mu.Lock()
	defer e.runner.mu.Unlock()
	for _, j := range e.runner.jobs {
		if jobs.OpOf(&j) == op && jobs.OutcomeOf(&j) == jobs.Running {
			return *j.DeepCopy()
		}
	}
	t.Fatalf("no running %s Job among %v", op, e.runner.created)
	return batchv1.Job{}
}

// stickPull ages e's Job name to age and gives it a pod whose source
// container waits in ImagePullBackOff.
func (e *env) stickPull(name string, age time.Duration) {
	e.runner.mu.Lock()
	defer e.runner.mu.Unlock()
	for i := range e.runner.jobs {
		if j := &e.runner.jobs[i]; j.Name == name {
			j.CreationTimestamp = metav1.NewTime(t0.Add(-age))
			e.runner.pods[name] = []corev1.Pod{pullingPod(j, jobs.SourceContainer, "ImagePullBackOff")}
		}
	}
}

// unpullable returns the images e's machine's durable Secret lists as
// unpullable, failing t on a read error.
func (e *env) unpullable(t *testing.T) []string {
	t.Helper()
	d, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil {
		t.Fatal(err)
	}
	return d.Unpullable
}

// TestPullFallbackChain proves a destroy whose pinned digest cannot be
// pulled falls back, PullFailureGrace after its Job was created, to the
// applied record's tag and then to spec.source.image: each time the Job
// is deleted, its image recorded unpullable and ImagePullFallback
// emitted, and the next destroy runs the next image. On the last image
// the Job is left to its deadline, and ApplyJobSucceeded reports
// ImagePullFailed with the Retain hint. Nothing re-pins: the applied
// record keeps its digest.
func TestPullFallbackChain(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(deleting, notPaused))...)
	if err := writeInputs(t.Context(), e.c, e.get(t), renderMachine(t), testMeta{Image: appliedTag, Identity: testIdentity, ImageDigest: pinnedRef}); err != nil {
		t.Fatal(err)
	}
	e.state.st = &state.State{InputsHash: "h1:x"}
	pass := func() {
		t.Helper()
		if _, err := reconcileOnce(t, e, e.kindFor(t, readyOwner())); err != nil {
			t.Fatal(err)
		}
	}

	steps := []struct {
		image     string
		fallbacks string
		next      string
	}{
		{pinnedRef, `["` + appliedTag + `","` + specRef + `"]`, appliedTag},
		{appliedTag, `["` + specRef + `"]`, specRef},
	}
	var unpullable []string
	for i, s := range steps {
		pass()
		j := e.activeJob(t, jobs.OpDestroy)
		if img := jobs.SourceImage(&j); img != s.image || j.Annotations[ImageFallbacksAnnotation] != s.fallbacks {
			t.Fatalf("step %d: destroy runs %s with fallbacks %s; want %s, %s", i, img, j.Annotations[ImageFallbacksAnnotation], s.image, s.fallbacks)
		}
		e.stickPull(j.Name, 3*time.Minute)
		pass()
		unpullable = append(unpullable, s.image)
		if got := e.unpullable(t); !slices.Equal(got, unpullable) {
			t.Errorf("step %d: unpullable = %v, want %v", i, got, unpullable)
		}
		if !slices.Contains(e.runner.deleted, j.Name) || len(e.runner.deleted) != i+1 {
			t.Errorf("step %d: deleted %v, want %s", i, e.runner.deleted, j.Name)
		}
		if m := e.get(t); m.Status.ActiveJob.Name != "" {
			t.Errorf("step %d: activeJob = %+v", i, m.Status.ActiveJob)
		}
		evs := e.rec.only(EventImagePullFallback)
		if len(evs) != i+1 || !strings.Contains(evs[i].note, "Could not pull "+s.image+" (ImagePullBackOff)") ||
			!strings.HasSuffix(evs[i].note, "retrying destroy with "+s.next) || evs[i].eventType != corev1.EventTypeWarning {
			t.Errorf("step %d: ImagePullFallback = %+v", i, evs)
		}
	}

	// The last image: a JobCreated note says it falls back, and its pull
	// failure is reported, not acted on.
	pass()
	j := e.activeJob(t, jobs.OpDestroy)
	if img := jobs.SourceImage(&j); img != specRef || j.Annotations[ImageFallbacksAnnotation] != "" {
		t.Fatalf("last: destroy runs %s with fallbacks %q", img, j.Annotations[ImageFallbacksAnnotation])
	}
	created := e.rec.only(EventJobCreated)
	if note := created[len(created)-1].note; !strings.Contains(note, "falls back to this image, as an earlier Job could not pull "+pinnedRef) {
		t.Errorf("JobCreated = %q", note)
	}
	e.stickPull(j.Name, 3*time.Minute)
	pass()
	if len(e.runner.deleted) != 2 || e.rec.count(EventImagePullFallback) != 2 {
		t.Errorf("the last image's Job was deleted: %v", e.runner.deleted)
	}
	m := e.get(t)
	c := conditions.Get(m, infrav1.ApplyJobSucceededCondition)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.ImagePullFailedReason ||
		!strings.HasPrefix(c.Message, "Job "+j.Name+": cannot pull its module image "+specRef+" (ImagePullBackOff), and no other image is left to try") ||
		!strings.Contains(c.Message, retainHint) {
		t.Errorf("ApplyJobSucceeded = %+v", c)
	}
	if m.Status.ActiveJob.Name != j.Name || !HasBlockMove(m) {
		t.Errorf("activeJob = %+v, block-move %v", m.Status.ActiveJob, HasBlockMove(m))
	}
	d, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil || d.Applied == nil || d.Applied.Digest != pinnedRef {
		t.Errorf("applied record = %+v, %v; want the digest kept", d, err)
	}
}

// TestPullStuckLeavesAlone proves pullStuck does nothing to a Job younger
// than PullFailureGrace, to one whose init container (the runner image)
// cannot be pulled, to one with a ready pod (whose pods it does not
// read), and to an apply: no delete, no recorded image, no event and no
// ImagePullFailed.
func TestPullStuckLeavesAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		op        jobs.Op
		age       time.Duration
		container string
		ready     bool
	}{
		{"younger than the grace", jobs.OpDestroy, PullFailureGrace - time.Second, jobs.SourceContainer, false},
		{"init container", jobs.OpDestroy, 3 * time.Minute, jobs.RunnerContainer, false},
		{"apply", jobs.OpApply, 3 * time.Minute, jobs.SourceContainer, false},
		{"ready pod", jobs.OpDestroy, 3 * time.Minute, jobs.SourceContainer, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: inputs.RunName("running")}}
			e := newEnv(t, world(machine(withFinalizer, notPaused), runSecret)...)
			if err := writeInputs(t.Context(), e.c, e.get(t), renderMachine(t), testMeta{Image: appliedTag, Identity: testIdentity, ImageDigest: pinnedRef}); err != nil {
				t.Fatal(err)
			}
			j := job("running", tc.op, jobs.Running, t0)
			j.CreationTimestamp = metav1.NewTime(t0.Add(-tc.age))
			j.Spec.Template.Spec.Containers = []corev1.Container{{Name: jobs.SourceContainer, Image: pinnedRef}}
			if tc.ready {
				j.Status.Ready = new(int32(1))
			}
			e.runner.jobs = append(e.runner.jobs, j)
			e.runner.pods["running"] = []corev1.Pod{pullingPod(&j, tc.container, "ErrImagePull")}
			if _, err := reconcileOnce(t, e, e.kindFor(t, readyOwner())); err != nil {
				t.Fatal(err)
			}
			m := e.get(t)
			if len(e.runner.deleted) != 0 || m.Status.ActiveJob.Name != "running" || e.rec.count(EventImagePullFallback) != 0 || len(e.unpullable(t)) != 0 {
				t.Errorf("deleted %v, activeJob %+v, unpullable %v", e.runner.deleted, m.Status.ActiveJob, e.unpullable(t))
			}
			for _, c := range m.Status.Conditions {
				if c.Reason == infrav1.ImagePullFailedReason {
					t.Errorf("%s = %+v", c.Type, c)
				}
			}
		})
	}
}

// TestPullStuckLastImageDrift proves a drift Job on its last image whose
// pull fails sets DriftJobSucceeded False/ImagePullFailed, without the
// Retain hint (nothing is being deleted), and is left to its deadline.
func TestPullStuckLastImageDrift(t *testing.T) {
	t.Parallel()
	runSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: inputs.RunName("drift")}}
	e := newEnv(t, world(machine(withFinalizer, notPaused), runSecret)...)
	if err := writeInputs(t.Context(), e.c, e.get(t), renderMachine(t), testMeta{Image: specRef, Identity: testIdentity, ImageDigest: pinnedRef}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := inputs.AddUnpullable(t.Context(), e.c, e.get(t), nil, pinnedRef); err != nil {
		t.Fatal(err)
	}
	j := job("drift", jobs.OpDrift, jobs.Running, t0)
	j.CreationTimestamp = metav1.NewTime(t0.Add(-5 * time.Minute))
	j.Spec.Template.Spec.Containers = []corev1.Container{{Name: jobs.SourceContainer, Image: specRef}}
	e.runner.jobs = append(e.runner.jobs, j)
	e.runner.pods["drift"] = []corev1.Pod{pullingPod(&j, jobs.SourceContainer, "ErrImagePull")}
	if _, err := reconcileOnce(t, e, e.kindFor(t, readyOwner())); err != nil {
		t.Fatal(err)
	}
	c := conditions.Get(e.get(t), infrav1.DriftJobSucceededCondition)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.ImagePullFailedReason ||
		!strings.Contains(c.Message, specRef+" (ErrImagePull)") || strings.Contains(c.Message, "Retain") || len(e.runner.deleted) != 0 {
		t.Errorf("DriftJobSucceeded = %+v, deleted %v", c, e.runner.deleted)
	}
}

// TestPromotionClearsUnpullable proves a successful apply's promotion
// clears the images earlier Jobs could not pull: it pinned a new image.
func TestPromotionClearsUnpullable(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	k := e.kindFor(t, readyOwner())
	if err := writeInputs(t.Context(), e.c, k.obj, renderMachine(t), testMeta{Image: specRef, Identity: testIdentity}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := inputs.AddUnpullable(t.Context(), e.c, k.obj, nil, pinnedRef); err != nil {
		t.Fatal(err)
	}
	e.runner.jobs = append(e.runner.jobs, job(seedJob, jobs.OpApply, jobs.Succeeded, t0.Add(-time.Minute)))
	e.state.st = &state.State{Serial: 1}
	if _, err := reconcileOnce(t, e, k); err != nil {
		t.Fatal(err)
	}
	d, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil || d.Applied == nil || d.Applied.Job != seedJob || d.Unpullable != nil {
		t.Errorf("records = %+v, %v; want the apply promoted and no unpullable images", d, err)
	}
}

// TestPullStuckPaused proves a paused object's Job whose module image
// cannot be pulled is deleted, so it cannot hold clusterctl move until
// its deadline, and nothing starts while paused: with an image left, the
// image is recorded and ImagePullFallback says the next one runs once
// unpaused; on the last image, the condition reports ImagePullFailed,
// and keeps it on later paused passes although an older Job's outcome
// would otherwise show.
func TestPullStuckPaused(t *testing.T) {
	t.Parallel()
	pausedOwner := OwnerInfo{HasOwnerRef: true, Cluster: cluster(true)}
	setup := func(t *testing.T, m *infrav1.TerraformMachine, name string, op jobs.Op, image, fallbacks string) *env {
		t.Helper()
		runSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: inputs.RunName(name)}}
		e := newEnv(t, world(m, runSecret)...)
		if err := writeInputs(t.Context(), e.c, e.get(t), renderMachine(t), testMeta{Image: appliedTag, Identity: testIdentity, ImageDigest: pinnedRef}); err != nil {
			t.Fatal(err)
		}
		// A first paused pass persists the Paused condition, as the pass
		// that saw the pause did; its patch leaves the in-memory object's
		// resourceVersion behind, which releaseActiveJob would conflict on.
		if _, err := reconcileOnce(t, e, e.kindFor(t, pausedOwner)); err != nil {
			t.Fatal(err)
		}
		j := job(name, op, jobs.Running, t0)
		j.UID = "stuck-uid"
		j.CreationTimestamp = metav1.NewTime(t0.Add(-3 * time.Minute))
		j.Spec.Template.Spec.Containers = []corev1.Container{{Name: jobs.SourceContainer, Image: image}}
		if fallbacks != "" {
			j.Annotations = map[string]string{ImageFallbacksAnnotation: fallbacks}
		}
		e.runner.jobs = append(e.runner.jobs, j)
		e.runner.pods[name] = []corev1.Pod{pullingPod(&j, jobs.SourceContainer, "ImagePullBackOff")}
		// status.activeJob names the Job, as the pass that started it left it.
		// block-move and status.activeJob name the Job, as the pass that
		// started it left them.
		m = e.get(t)
		SetBlockMove(m)
		if err := e.c.Update(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		m.Status.ActiveJob = infrav1.ActiveJob{Name: name, Operation: infrav1.Operation(op)}
		if err := e.c.Status().Update(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		return e
	}
	pass := func(t *testing.T, e *env) {
		t.Helper()
		if _, err := reconcileOnce(t, e, e.kindFor(t, pausedOwner)); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("an image left: recorded, deleted, nothing starts", func(t *testing.T) {
		t.Parallel()
		e := setup(t, machine(withFinalizer), "drift", jobs.OpDrift, pinnedRef, `["`+appliedTag+`","`+specRef+`"]`)
		pass(t, e)
		m := e.get(t)
		if !slices.Equal(e.runner.deleted, []string{"drift"}) || len(e.runner.created) != 0 || m.Status.ActiveJob.Name != "" || HasBlockMove(m) {
			t.Errorf("deleted %v, created %v, activeJob %+v, block-move %v", e.runner.deleted, e.runner.created, m.Status.ActiveJob, HasBlockMove(m))
		}
		if got := e.unpullable(t); !slices.Equal(got, []string{pinnedRef}) {
			t.Errorf("unpullable = %v", got)
		}
		evs := e.rec.only(EventImagePullFallback)
		if len(evs) != 1 || !strings.HasSuffix(evs[0].note, "drift runs with "+appliedTag+" once the object is unpaused") {
			t.Errorf("ImagePullFallback = %+v", evs)
		}
	})
	t.Run("the last image: deleted, ImagePullFailed kept while paused", func(t *testing.T) {
		t.Parallel()
		e := setup(t, machine(deleting), "destroy", jobs.OpDestroy, specRef, "")
		// An older destroy whose outcome bookkeeping would report again.
		e.runner.jobs = append(e.runner.jobs, job("old", jobs.OpDestroy, jobs.Failed, t0.Add(-time.Hour)))
		check := func(when string) {
			t.Helper()
			m := e.get(t)
			c := conditions.Get(m, infrav1.ApplyJobSucceededCondition)
			if c == nil || c.Reason != infrav1.ImagePullFailedReason || !strings.HasPrefix(c.Message, "Job destroy: cannot pull its module image "+specRef) ||
				!strings.Contains(c.Message, pausedPullNote) || !strings.Contains(c.Message, retainHint) {
				t.Errorf("%s: ApplyJobSucceeded = %+v", when, c)
			}
			if !slices.Equal(e.runner.deleted, []string{"destroy"}) || len(e.runner.created) != 0 || m.Status.ActiveJob.Name != "" || HasBlockMove(m) {
				t.Errorf("%s: deleted %v, created %v, activeJob %+v, block-move %v", when, e.runner.deleted, e.runner.created, m.Status.ActiveJob, HasBlockMove(m))
			}
		}
		pass(t, e)
		check("deleting pass")
		if len(e.unpullable(t)) != 0 || e.rec.count(EventImagePullFallback) != 0 {
			t.Errorf("unpullable %v, fallback events %d", e.unpullable(t), e.rec.count(EventImagePullFallback))
		}
		pass(t, e)
		check("next paused pass")
	})
}
