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
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/plankey"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// planKeyVolume returns the name of the Secret job mounts at
// plankey.MountPath, or "" if it mounts none.
func planKeyVolume(job *batchv1.Job) string {
	spec := job.Spec.Template.Spec
	var volume string
	for _, c := range spec.Containers {
		for _, m := range c.VolumeMounts {
			if m.MountPath == plankey.MountPath {
				volume = m.Name
			}
		}
	}
	for _, v := range spec.Volumes {
		if v.Name == volume && v.Secret != nil {
			return v.Secret.SecretName
		}
	}
	return ""
}

// TestStartJobMountsPlanKey: a plan Job and the approved apply that plans
// again mount the same per-object key Secret, which StartJob creates; an
// apply without an approved plan mounts none.
func TestStartJobMountsPlanKey(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	suffix, err := state.Suffix(testNS, state.KindTerraformMachine, testName)
	if err != nil {
		t.Fatal(err)
	}
	want := plankey.Name("m", testName)
	tests := []struct {
		name       string
		op         jobs.Op
		expectPlan string
		secret     string
	}{
		{"plan", jobs.OpPlan, "", want},
		{"approved apply", jobs.OpApply, runner.PlanHashPrefix + "x", want},
		{"plain apply", jobs.OpApply, "", ""},
	}
	for i, tt := range tests {
		req := JobRequest{
			Op: tt.op, Files: renderMachine(t), InputsHash: "h1:x", Source: infrav1.Source{Image: "registry.example/mod:1.0"},
			Identity: testIdentity, Suffix: suffix, ClusterName: "c1", Attempt: int32(i + 1), ExpectPlan: tt.expectPlan,
		}
		created := e.startLeased(t, e.kindFor(t, readyOwner), req)
		if got := planKeyVolume(created); got != tt.secret {
			t.Errorf("%s: plan key Secret %q, want %q", tt.name, got, tt.secret)
		}
	}
	if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: want}, &corev1.Secret{}); err != nil {
		t.Errorf("plan key Secret: %v", err)
	}
}
