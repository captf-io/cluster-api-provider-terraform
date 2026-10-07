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
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// TestStartJobAdoptsOnlyOwnRunning: a create that finds a Job of the same
// name takes it as the started one only when this object controls it and
// it still runs. This object's finished Job (a cache that lagged its
// finish) and another object's Job (an earlier object of the same name,
// its garbage collection pending) defer the start instead: the other
// object's Job is deleted once it finished, and left alone while it runs.
func TestStartJobAdoptsOnlyOwnRunning(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		ours        bool
		outcome     jobs.Outcome
		wantAdopted bool
		wantDeleted bool
	}{
		{"own running: adopted", true, jobs.Running, true, false},
		{"own finished: deferred", true, jobs.Failed, false, false},
		{"another object's finished: deleted and deferred", false, jobs.Succeeded, false, true},
		{"another object's running: deferred", false, jobs.Running, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, world(machine(withFinalizer, notPaused))...)
			suffix := suffixOf(t, state.KindTerraformMachine, testName)
			req := JobRequest{
				Op: jobs.OpApply, Files: renderMachine(t), InputsHash: "h1:x",
				Source: infrav1.Source{Image: "registry.example/mod:1.0"}, Identity: testIdentity,
				Suffix: suffix, ClusterName: "c1", Attempt: 1,
			}
			k := e.kindFor(t, readyOwner())
			existing := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: JobName(k, req), UID: "existing-uid"}}
			owner := client.Object(k.obj)
			if !tt.ours {
				earlier := k.obj.DeepCopy()
				earlier.UID = types.UID("earlier-uid")
				owner = earlier
			}
			if err := controllerutil.SetControllerReference(owner, existing, e.c.Scheme()); err != nil {
				t.Fatal(err)
			}
			if tt.outcome != jobs.Running {
				existing.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
				if tt.outcome == jobs.Succeeded {
					existing.Status.Conditions[0].Type = batchv1.JobComplete
				}
			}
			if err := e.c.Create(t.Context(), existing); err != nil {
				t.Fatal(err)
			}
			e.runner.createErr = apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"}, existing.Name)

			got, err := StartJob(t.Context(), e.d, k, req)
			switch {
			case tt.wantAdopted && (err != nil || got == nil || got.UID != existing.UID):
				t.Errorf("StartJob = %v, %v; want the existing Job adopted", got, err)
			case !tt.wantAdopted && !errors.Is(err, ErrStartDeferred):
				t.Errorf("StartJob error = %v, want ErrStartDeferred", err)
			}
			if tt.wantAdopted != (k.obj.Status.ActiveJob.Name == existing.Name) {
				t.Errorf("activeJob = %+v", k.obj.Status.ActiveJob)
			}
			err = e.c.Get(t.Context(), client.ObjectKeyFromObject(existing), &batchv1.Job{})
			if deleted := apierrors.IsNotFound(err); deleted != tt.wantDeleted {
				t.Errorf("existing Job deleted = %v (get: %v), want %v", deleted, err, tt.wantDeleted)
			}
		})
	}
}

// TestCreatePlanAlreadyExistsInserts: a create of a TerraformPlan that
// finds it existing takes the live one, read past the cache, when this
// object controls it, so the rest of the pass sees it; one another object
// of the same name controls is an error, retried until it is gone.
func TestCreatePlanAlreadyExistsInserts(t *testing.T) {
	t.Parallel()
	for _, ours := range []bool{true, false} {
		t.Run(map[bool]string{true: "ours", false: "another object's"}[ours], func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, world(machine(withFinalizer, notPaused))...)
			k := e.kindFor(t, readyOwner())
			r := &reconciler{d: e.d, k: k, obj: k.obj, st: k.Status()}
			f := &finished{
				job:    &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "captf-m-m1-plan-a1-abcdef"}},
				ok:     true,
				result: &jobs.Result{Plan: &runner.Plan{Hash: "p2:abc", Create: 1}},
			}
			name := planName(testName, f.job.Name, "p2:abc")
			uid := k.obj.UID
			if !ours {
				uid = "earlier-uid"
			}
			existing := &infrav1.TerraformPlan{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name, OwnerReferences: []metav1.OwnerReference{{
					APIVersion: infrav1.GroupVersion.String(), Kind: k.Kind(), Name: testName, UID: uid, Controller: new(true),
				}}},
				Spec: infrav1.TerraformPlanSpec{
					TargetRef: infrav1.PlanTargetRef{Kind: infrav1.PlanTargetKind(k.Kind()), Name: testName},
					PlanHash:  "p2:abc", InputsHash: "h1:x", Reason: infrav1.PlanReasonManual,
				},
			}
			if err := e.c.Create(t.Context(), existing); err != nil {
				t.Fatal(err)
			}

			p, created, err := r.createPlan(t.Context(), f, madePlan{reason: infrav1.PlanReasonManual, inputsHash: "h1:x"})
			if created {
				t.Error("createPlan reported a plan that existed as created")
			}
			if !ours {
				if err == nil {
					t.Errorf("createPlan took another object's plan: %+v", p)
				}
				if len(r.plans) != 0 {
					t.Errorf("plans = %d, want none", len(r.plans))
				}
				return
			}
			if err != nil || p == nil || p.UID != existing.UID {
				t.Fatalf("createPlan = %v, %v; want the existing plan", p, err)
			}
			if r.planNamed(name) == nil || r.plans[0].Name != name {
				t.Errorf("plans = %v, want %s first", r.plans, name)
			}
		})
	}
}
