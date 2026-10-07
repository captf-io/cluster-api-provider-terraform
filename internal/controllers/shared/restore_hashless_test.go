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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// TestHashlessRestoreClearsHash restores a backup taken without an inputs
// hash (a first apply that failed partway) over a state a later apply
// recorded hash h1:newer on. The push keeps that hash on the backend's
// Secret; the restore Job records the backup's empty hash, and adopting
// it removes the newer one, so the partial state is not passed off as the
// newer apply's.
func TestHashlessRestoreClearsHash(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused, provisioned, restoring("5")))...)
	jr := &clientRunner{c: e.c}
	e.d.Jobs = jr
	e.d.State = state.NewReader(e.c)
	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	e.backupOf(t, state.KindTerraformMachine, testName, 5, "")
	e.setState(t, suffix, 6, "h1:newer")
	base := &corev1.Secret{}
	key := client.ObjectKey{Namespace: testNS, Name: state.SecretName(suffix)}
	if err := e.c.Get(t.Context(), key, base); err != nil {
		t.Fatal(err)
	}
	base.Annotations = map[string]string{state.InputsHashAnnotation: "h1:newer"}
	if err := e.c.Update(t.Context(), base); err != nil {
		t.Fatal(err)
	}

	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	restores := restoreJobs(t, e)
	if len(restores) != 1 {
		t.Fatalf("restore Jobs = %d, want 1", len(restores))
	}
	job := restores[0]
	if h, ok := job.Annotations[state.InputsHashAnnotation]; !ok || h != "" {
		t.Errorf("restore Job inputs hash = %q (present %v), want present and empty", h, ok)
	}

	// The push replaced the data and kept the newer hash on the Secret.
	e.finishJob(t, job.Name, jobs.Succeeded)
	e.setState(t, suffix, 7, "")
	for range 2 {
		if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.c.Get(t.Context(), key, base); err != nil {
		t.Fatal(err)
	}
	if h, ok := base.Annotations[state.InputsHashAnnotation]; ok {
		t.Errorf("state inputs hash = %q after a hashless restore, want none", h)
	}
	adopted := e.rec.only(EventStateAdopted)
	if len(adopted) != 1 || !strings.Contains(adopted[0].note, "without an inputs hash") {
		t.Errorf("StateAdopted events = %+v, want one naming the missing hash", adopted)
	}
}

// TestHashlessRestoreReappliesImmutable proves a provisioned immutable
// object whose restored state carries no inputs hash is not held as
// StateLost, which a restore was just the fix for: it builds no inputs,
// so it re-applies what its inputs record holds, under that record's
// hash, which records the hash in the state again.
func TestHashlessRestoreReappliesImmutable(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused, provisioned))...)
	e.d.Jobs, e.d.State = &clientRunner{c: e.c}, state.NewReader(e.c)
	e.writeDurable(t, e.get(t))
	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	e.setState(t, suffix, 7, "")
	k := func() *fakeKind {
		k := healthyKind(t, e, testName)
		k.mutable = false
		return k
	}
	for range 2 {
		if _, err := Reconcile(t.Context(), e.d, k()); err != nil {
			t.Fatal(err)
		}
	}
	if c := conditions.Get(e.get(t), infrav1.StateReadableCondition); c != nil && c.Reason == infrav1.StateLostReason {
		t.Fatalf("StateReadable = %+v, want no StateLost hold", c)
	}
	d, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil || d.AppliedOrAttempt() == nil {
		t.Fatalf("records = %+v, %v", d, err)
	}
	applies := slices.DeleteFunc(e.jobsOf(t), func(j batchv1.Job) bool { return jobs.OpOf(&j) != jobs.OpApply })
	if len(applies) != 1 || applies[0].Annotations[state.InputsHashAnnotation] != d.AppliedOrAttempt().InputsHash {
		t.Errorf("apply Jobs = %d, want one under the record's hash %q", len(applies), d.AppliedOrAttempt().InputsHash)
	}
}
