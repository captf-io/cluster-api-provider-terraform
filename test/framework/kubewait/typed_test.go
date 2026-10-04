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

package kubewait

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// v1Event returns an events.k8s.io/v1 Event named evName in ns regarding
// name, with reason reason.
func v1Event(ns, evName, name, reason string) *eventsv1.Event {
	return &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: evName},
		Regarding:  corev1.ObjectReference{Name: name},
		Reason:     reason,
	}
}

// coreEvent returns a core/v1 Event named evName in ns involving name, with
// reason reason.
func coreEvent(ns, evName, name, reason string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: ns, Name: evName},
		InvolvedObject: corev1.ObjectReference{Name: name},
		Reason:         reason,
	}
}

// TestEventSeen checks matching in either API and the timeout observation.
func TestEventSeen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if err := EventSeen(ctx, fake.NewClientset(v1Event("ns", "e1", "tc", "JobCreated")), "ns", "tc", "JobCreated", fast(nil)); err != nil {
		t.Errorf("events.k8s.io: %v", err)
	}
	if err := EventSeen(ctx, fake.NewClientset(coreEvent("ns", "e1", "tc", "JobCreated")), "ns", "tc", "JobCreated", fast(nil)); err != nil {
		t.Errorf("core: %v", err)
	}
	kube := fake.NewClientset(
		v1Event("ns", "e1", "tc", "JobCreated"),
		coreEvent("ns", "e2", "tc", "DigestPinned"),
		v1Event("ns", "e3", "other", "Provisioned"),
		coreEvent("ns", "e4", "other", "Provisioned"),
	)
	err := EventSeen(ctx, kube, "ns", "tc", "Provisioned", fast(nil))
	if err == nil || !strings.Contains(err.Error(), "[DigestPinned JobCreated]") || !strings.Contains(err.Error(), "event ns/tc reason Provisioned") {
		t.Errorf("timeout: %v", err)
	}
}

// TestEventSeenListErrors checks that list errors are reported and that one
// API failing does not hide a match in the other.
func TestEventSeenListErrors(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset()
	kube.PrependReactor("list", "events", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("no events")
	})
	err := EventSeen(context.Background(), kube, "ns", "tc", "X", fast(nil))
	if err == nil || !strings.Contains(err.Error(), "events.k8s.io: no events") || !strings.Contains(err.Error(), "core: no events") {
		t.Errorf("both failing: %v", err)
	}

	half := fake.NewClientset(coreEvent("ns", "e", "tc", "X"))
	half.PrependReactor("list", "events", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Group == "events.k8s.io" {
			return true, nil, errors.New("v1 down")
		}
		return false, nil, nil
	})
	if err := EventSeen(context.Background(), half, "ns", "tc", "X", fast(nil)); err != nil {
		t.Errorf("core match with events.k8s.io down: %v", err)
	}
}

// TestJobsFor checks the selector labels.
func TestJobsFor(t *testing.T) {
	t.Parallel()
	if got := JobsFor("TerraformMachine", "m1", "apply").String(); got !=
		"captf.infrastructure.cluster.x-k8s.io/op=apply,captf.infrastructure.cluster.x-k8s.io/owner-kind=TerraformMachine,captf.infrastructure.cluster.x-k8s.io/owner-name=m1" {
		t.Errorf("selector = %s", got)
	}
	if got := JobsFor("TerraformCluster", "c", "").String(); strings.Contains(got, "/op") {
		t.Errorf("empty op must not select on op: %s", got)
	}
}

// job returns a Job in ns named name with the labels of JobsFor(kind, owner,
// op), created at created and carrying condition cond (none when "").
func job(ns, name, owner, op string, created time.Time, cond batchv1.JobConditionType) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(created),
		Labels: map[string]string{OwnerKindLabel: "TerraformCluster", OwnerNameLabel: owner, OpLabel: op},
	}}
	if cond != "" {
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobSuspended, Status: corev1.ConditionFalse}, {Type: cond, Status: corev1.ConditionTrue}}
	}
	return j
}

// TestJobFinished checks the newest-Job rule, both outcomes, selection by
// label and the timeout observations.
func TestJobFinished(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sel := JobsFor("TerraformCluster", "tc", "apply")

	kube := fake.NewClientset(
		job("ns", "old", "tc", "apply", base, batchv1.JobFailed),
		job("ns", "new", "tc", "apply", base.Add(time.Minute), batchv1.JobComplete),
		job("ns", "destroy", "tc", "destroy", base.Add(time.Hour), ""),
		job("ns", "elsewhere", "other", "apply", base.Add(time.Hour), ""),
	)
	got, err := JobFinished(ctx, kube, "ns", sel, fast(nil))
	if err != nil || got.Name != "new" {
		t.Fatalf("JobFinished = %v, %v", got, err)
	}

	failed := fake.NewClientset(job("ns", "j", "tc", "apply", base, batchv1.JobFailed))
	if got, err := JobFinished(ctx, failed, "ns", sel, fast(nil)); err != nil || got.Name != "j" {
		t.Errorf("failed job: %v, %v", got, err)
	}

	// A running newest Job outweighs an older finished one; equal times
	// fall back to the name.
	running := fake.NewClientset(
		job("ns", "a", "tc", "apply", base, batchv1.JobComplete),
		job("ns", "b", "tc", "apply", base.Add(time.Minute), ""),
		job("ns", "c", "tc", "apply", base.Add(time.Minute), batchv1.JobComplete),
	)
	got, err = JobFinished(ctx, running, "ns", sel, fast(nil))
	if err != nil || got.Name != "c" {
		t.Errorf("same-time tie break: %v, %v", got, err)
	}
	running = fake.NewClientset(
		job("ns", "a", "tc", "apply", base, batchv1.JobComplete),
		job("ns", "b", "tc", "apply", base.Add(time.Minute), ""),
	)
	_, err = JobFinished(ctx, running, "ns", sel, fast(nil))
	if err == nil || !strings.Contains(err.Error(), "newest Job b running") || !strings.Contains(err.Error(), "2 matching") {
		t.Errorf("running newest: %v", err)
	}

	_, err = JobFinished(ctx, fake.NewClientset(), "ns", sel, fast(nil))
	if err == nil || !strings.Contains(err.Error(), "no matching Job") {
		t.Errorf("no jobs: %v", err)
	}

	broken := fake.NewClientset()
	broken.PrependReactor("list", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	_, err = JobFinished(ctx, broken, "ns", sel, fast(nil))
	if err == nil || !strings.Contains(err.Error(), "list failed: boom") {
		t.Errorf("list error: %v", err)
	}
}
