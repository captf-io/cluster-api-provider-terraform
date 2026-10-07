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
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
)

// unknownWhy is what a blocked apply's condition says while the exports
// of the pool's last successful apply are unknown.
const unknownWhy = "The exports of the pool's last successful apply are unknown (it applied before this version recorded them)"

// forgetExports removes the record of the applied exports and, unless
// keepHash, its hash from the durable Secret, as a pool that applied
// before either existed has it.
func (e *holdEnv) forgetExports(keepHash bool) {
	e.t.Helper()
	s := &corev1.Secret{}
	if err := e.c.Get(e.t.Context(), client.ObjectKey{Namespace: ns, Name: inputs.Name("mp", e.req.Name)}, s); err != nil {
		e.t.Fatal(err)
	}
	delete(s.Data, inputs.AppliedClusterOutputsKey)
	if !keepHash {
		delete(s.Annotations, inputs.AppliedClusterOutputsHashAnnotation)
	}
	if err := e.c.Update(e.t.Context(), s); err != nil {
		e.t.Fatal(err)
	}
	if d := e.durable(); d.AppliedClusterOutputs != nil || (d.AppliedExportsHash != "") != keepHash {
		e.t.Fatalf("after forgetting the exports: applied %s (%q)", d.AppliedClusterOutputs, d.AppliedExportsHash)
	}
}

// seeded fails the test unless the durable Secret records exports as the
// applied ones, record and hash.
func (e *holdEnv) seeded(exports string) {
	e.t.Helper()
	if d := e.durable(); !sameExports(e.t, d.AppliedClusterOutputs, exports) || d.AppliedExportsHash != exportsHash(e.t, exports) {
		e.t.Errorf("applied exports %s (%q), want %s", d.AppliedClusterOutputs, d.AppliedExportsHash, exports)
	}
}

// guardsChange changes the cluster's exports to exports and fails the
// test unless the pool guards their apply as one whose applied exports
// are recorded: guarded, and once blocked, held.
func (e *holdEnv) guardsChange(exports string) {
	e.t.Helper()
	e.setExports(exports)
	b := e.reconcileStarts(jobs.OpApply)
	if !e.guarded(b) || b.Annotations[shared.ApprovalHashAnnotation] == "" || !sameExports(e.t, e.rendered(b).ClusterOutputs, exports) {
		e.t.Fatalf("change of the exports: args %v, annotations %v", e.args(b), b.Annotations)
	}
	e.block(b)
	e.reconcile()
	e.heldHash(b.Name)
}

// unknownBlocked fails the test unless the pool's ApplyJobSucceeded is
// job's block while the applied exports are unknown, and returns the
// approval hash it names.
func (e *holdEnv) unknownBlocked(job *batchv1.Job) string {
	e.t.Helper()
	c := e.applyCondition()
	if c.Reason != infrav1.DestructivePlanBlockedReason || !strings.HasPrefix(c.Message, "Job "+job.Name+": ") || !strings.Contains(c.Message, unknownWhy) ||
		strings.Contains(c.Message, "keeps applying") {
		e.t.Fatalf("ApplyJobSucceeded = %+v, want Job %s blocked while the applied exports are unknown", c, job.Name)
	}
	return e.approveHash(c)
}

// TestSeedFromDurable: a pool that applied before the applied exports
// were recorded, whose newest apply succeeded with the state's inputs and
// whose durable Secret still holds them, records their exports once, and
// guards a change of the exports as any pool does: held once blocked.
func TestSeedFromDurable(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	e.forgetExports(false)
	e.reconcileNoApply()
	e.seeded(exportsE0)
	e.guardsChange(exportsE1)
}

// TestSeedFromDurableRefused: when the durable Secret holds the inputs of
// an apply deleted while it ran under a controller that kept no record of
// it, they are not taken for the last successful apply's, although that
// apply is the newest retained and the state records its inputs hash:
// the exports stay unknown, and the next apply is guarded, waiting for
// approval of a destructive plan as a cluster's does, saying why.
func TestSeedFromDurableRefused(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	e.setExports(exportsE1)
	j := e.reconcileStarts(jobs.OpApply)
	e.deleteJob(j)
	p := e.pool()
	p.Status.ActiveJob = infrav1.ActiveJob{}
	if err := e.c.Status().Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	e.forgetExports(false)
	e.setExports(exportsE0)
	e.reconcileNoApply()
	if d := e.durable(); d.AppliedExportsHash != "" || d.AppliedClusterOutputs != nil {
		t.Fatalf("seeded from the deleted Job's inputs: %s (%q)", d.AppliedClusterOutputs, d.AppliedExportsHash)
	}
	e.setVersion("v1.37.0")
	r := e.reconcileStarts(jobs.OpApply)
	if !e.guarded(r) || r.Annotations[shared.ApprovalHashAnnotation] == "" {
		t.Fatalf("apply while the applied exports are unknown: args %v, annotations %v", e.args(r), r.Annotations)
	}
	e.block(r)
	e.reconcileNoApply()
	e.unknownBlocked(r)
}

// TestSeedFromCurrent: a pool that applied before the applied exports
// were recorded, with no apply Job retained, records the exports of its
// current inputs once they hash to the state's, and guards a change of
// the exports as any pool does.
func TestSeedFromCurrent(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	var l batchv1.JobList
	if err := e.c.List(t.Context(), &l, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	for i := range l.Items {
		if err := e.c.Delete(t.Context(), &l.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	e.forgetExports(false)
	e.reconcileNoApply()
	e.seeded(exportsE0)
	e.guardsChange(exportsE1)
}

// TestSeedUnknown: a pool that applied before the applied exports were
// recorded, whose newest apply failed, cannot tell which exports its last
// successful apply rendered. The apply then due is guarded: a plan that
// deletes nothing applies and records its exports, after which a change
// of them is guarded and held as any pool's; a destructive one waits for
// approval, saying the exports are unknown, and the approval applies it.
func TestSeedUnknown(t *testing.T) {
	t.Parallel()
	// unknownApply returns the apply due after a failed version roll and
	// its revert, failing t unless it is guarded.
	unknownApply := func(e *holdEnv) *batchv1.Job {
		e.t.Helper()
		e.setVersion("v1.37.0")
		f := e.reconcileStarts(jobs.OpApply)
		e.fail(f)
		e.forgetExports(false)
		e.setVersion("v1.36.2")
		r := e.reconcileStarts(jobs.OpApply)
		if !e.guarded(r) || r.Annotations[shared.ApprovalHashAnnotation] == "" || r.Annotations[shared.AfterFailedApplyAnnotation] != "true" {
			e.t.Fatalf("apply while the applied exports are unknown: args %v, annotations %v", e.args(r), r.Annotations)
		}
		if d := e.durable(); d.AppliedExportsHash != "" {
			e.t.Fatalf("seeded after a failed apply: %q", d.AppliedExportsHash)
		}
		return r
	}
	t.Run("plan deletes nothing", func(t *testing.T) {
		t.Parallel()
		e := newHoldEnv(t)
		r := unknownApply(e)
		e.succeed(r)
		e.reconcileNoApply()
		e.seeded(exportsE0)
		e.guardsChange(exportsE1)
	})
	t.Run("destructive plan", func(t *testing.T) {
		t.Parallel()
		e := newHoldEnv(t)
		r := unknownApply(e)
		e.block(r)
		e.reconcileNoApply()
		approval := e.unknownBlocked(r)
		if approval != r.Annotations[shared.ApprovalHashAnnotation] {
			t.Errorf("the condition approves %s, the blocked apply's approval hash is %s", approval, r.Annotations[shared.ApprovalHashAnnotation])
		}
		e.reconcileNoApply()
		e.approve(approval)
		a := e.reconcileStarts(jobs.OpApply)
		if e.flag(a, "--allow-deletes-hash") != approval {
			t.Fatalf("approved apply: args %v", e.args(a))
		}
		e.succeed(a)
		e.reconcileNoApply()
		e.seeded(exportsE0)
		if d := e.durable(); d.Pending != nil {
			t.Errorf("pending change after the approved apply: %+v", d.Pending)
		}
		e.guardsChange(exportsE1)
	})
}

// TestSeedRecorded: a pool whose durable Secret records the hash of its
// applied exports, though not the exports (they did not fit), is not
// seeded again: the record stays absent, and a destructive change of the
// exports waits for approval, saying only their hash is recorded.
func TestSeedRecorded(t *testing.T) {
	t.Parallel()
	e := newHoldEnv(t)
	e.forgetExports(true)
	e.reconcileNoApply()
	if d := e.durable(); d.AppliedClusterOutputs != nil || d.AppliedExportsHash != exportsHash(t, exportsE0) {
		t.Fatalf("a pool with a recorded hash was seeded: %s (%q)", d.AppliedClusterOutputs, d.AppliedExportsHash)
	}
	e.setExports(exportsE1)
	b := e.reconcileStarts(jobs.OpApply)
	e.block(b)
	e.reconcileNoApply()
	if c := e.applyCondition(); c.Reason != infrav1.DestructivePlanBlockedReason || !strings.Contains(c.Message, "are not recorded, only their hash") ||
		strings.Contains(c.Message, unknownWhy) {
		t.Errorf("ApplyJobSucceeded = %+v, want the block of a pool recording only the hash", c)
	}
}

// TestSeedNeverApplied: a pool that never applied has no exports to
// know: its first apply, and its retry after that one failed (before its
// apply step: one that failed after it with no state saved is held as
// unconfirmed), are not guarded, and nothing is seeded.
func TestSeedNeverApplied(t *testing.T) {
	t.Parallel()
	e := newPoolEnv(t)
	first := e.reconcileStarts(jobs.OpApply)
	e.failAt(first, runner.StepPlan)
	r := e.reconcileStarts(jobs.OpApply)
	if e.guarded(r) || r.Annotations[shared.ApprovalHashAnnotation] != "" {
		t.Errorf("retry of a failed first apply: args %v, annotations %v", e.args(r), r.Annotations)
	}
	if d := e.durable(); d.AppliedExportsHash != "" || d.AppliedMark || d.Applied != nil {
		t.Errorf("a pool that never applied: applied hash %q, applied %v, record %+v", d.AppliedExportsHash, d.AppliedMark, d.Applied)
	}
	if c := e.applyCondition(); c.Status == metav1.ConditionTrue {
		t.Errorf("ApplyJobSucceeded = %+v after a failed first apply", c)
	}
}
