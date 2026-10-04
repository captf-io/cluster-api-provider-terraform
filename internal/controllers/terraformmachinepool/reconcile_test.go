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

package terraformmachinepool

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	testingclock "k8s.io/utils/clock/testing"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// t0 is the fixed instant the end-to-end test treats as "now".
var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// fakeRunner is an in-memory jobs.Runner.
type fakeRunner struct {
	mu   sync.Mutex
	jobs []batchv1.Job
	// pods are the pods Pods returns, by Job name.
	pods map[string][]corev1.Pod
}

var _ jobs.Runner = &fakeRunner{}

// Create stores job with a fake UID and returns a nil error.
func (r *fakeRunner) Create(_ context.Context, _ client.Object, job *batchv1.Job) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	job.UID = types.UID(fmt.Sprintf("job-uid-%d", len(r.jobs)+1))
	r.jobs = append(r.jobs, *job.DeepCopy())
	return nil
}

// List returns a copy of every Job r holds, and a nil error.
func (r *fakeRunner) List(context.Context, client.Object, string) ([]batchv1.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.jobs), nil
}

// Delete removes job from r's Jobs and returns a nil error.
func (r *fakeRunner) Delete(_ context.Context, job *batchv1.Job) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs = slices.DeleteFunc(r.jobs, func(j batchv1.Job) bool { return j.Name == job.Name })
	return nil
}

// Pods returns the pods recorded for job (none by default) and a nil
// error.
func (r *fakeRunner) Pods(_ context.Context, job *batchv1.Job) ([]corev1.Pod, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.pods[job.Name]), nil
}

// applies returns the names of r's apply Jobs.
func (r *fakeRunner) applies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for i := range r.jobs {
		if jobs.OpOf(&r.jobs[i]) == jobs.OpApply {
			out = append(out, r.jobs[i].Name)
		}
	}
	return out
}

// world returns the objects a pool needs to reconcile end to end: its
// namespace, an identity allowed there with its credentials Secret, the
// Cluster and TerraformCluster, the MachinePool and its bootstrap Secret,
// and tmp.
func world(tmp *infrav1.TerraformMachinePool) []client.Object {
	return []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&infrav1.TerraformClusterIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: "aws", UID: "id-uid"},
			Spec: infrav1.TerraformClusterIdentitySpec{
				SecretRef:         infrav1.SecretReference{Name: "aws-creds", Namespace: "captf-system"},
				AllowedNamespaces: &infrav1.AllowedNamespaces{Selector: &metav1.LabelSelector{}},
			},
		},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "captf-system", Name: "aws-creds"}, Data: map[string][]byte{"KEY": []byte("v")}},
		testCluster(), testTC(), testMP(),
		bootstrapSecret(map[string][]byte{"value": []byte("#cloud-config\n")}),
		tmp,
	}
}

// TestReconcile runs the pool through the shared core: a NotFound request
// is a no-op; a pool whose state holds its current inputs and valid,
// converged outputs is provisioned with status.ready, without a Job, and
// summarizes the pool's Ready inputs (ApplyJobSucceeded included after
// provisioning); a rotated bootstrap Secret then re-applies it
// (InputsChanged).
func TestReconcile(t *testing.T) {
	t.Parallel()
	gone := fake.NewClientBuilder().WithScheme(scheme(t)).Build()
	if res, err := (&Reconciler{Deps: shared.Deps{Client: gone}}).Reconcile(t.Context(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "gone"}}); err != nil || res.RequeueAfter != 0 {
		t.Errorf("NotFound: %+v, %v", res, err)
	}

	tmp := testTMP(func(p *infrav1.TerraformMachinePool) {
		p.Finalizers = []string{Finalizer}
		p.Status.Conditions = []metav1.Condition{{
			Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason, LastTransitionTime: metav1.NewTime(t0),
		}}
		// A refresh and a drift check just ran: nothing is due.
		now := metav1.NewTime(t0)
		p.Status.LastRefresh, p.Status.LastDriftCheck = &now, &now
	})
	s := scheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(world(tmp)...).WithStatusSubresource(&infrav1.TerraformMachinePool{}).Build()
	sr := &stateReader{}
	sr.set(t, state.KindTerraformCluster, "c1", clusterState(`{}`, `[{"name":"az-1","control_plane":true,"attributes":{}}]`))
	runner := &fakeRunner{}
	rec := &recorder{}
	d := shared.Deps{
		Client: c, APIReader: c, Scheme: s, Jobs: runner, State: sr, Recorder: rec,
		Clock: testingclock.NewFakePassiveClock(t0), RunnerImage: "registry.example/captf:dev", DriftDefault: 30 * time.Minute,
	}

	// The state holds the current inputs' hash.
	a := newAdapter(d, tmp.DeepCopy())
	owner, err := a.Owner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	in, gate, err := a.BuildInputs(t.Context(), owner, nil)
	if err != nil || gate != nil {
		t.Fatalf("BuildInputs: %+v, %v", gate, err)
	}
	h, err := hash.Inputs(contract.RoleMachinePool, tmp.Spec.Source.Image, in)
	if err != nil {
		t.Fatal(err)
	}
	st := poolState(poolOutputs(`["aws:///us-east-1a/i-2"]`, `1`))
	st.InputsHash = h
	sr.set(t, state.KindTerraformMachinePool, tmp.Name, st)

	r := &Reconciler{Deps: d}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tmp)}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	got := &infrav1.TerraformMachinePool{}
	if err := c.Get(t.Context(), req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if n := len(runner.jobs); n != 0 {
		t.Errorf("created %d Jobs for a pool already at its inputs", n)
	}
	if got.Status.Ready == nil || !*got.Status.Ready || got.Status.Initialization.Provisioned == nil || !*got.Status.Initialization.Provisioned {
		t.Errorf("ready %v, provisioned %v", got.Status.Ready, got.Status.Initialization.Provisioned)
	}
	if !slices.Equal(got.Spec.ProviderIDList, []string{"aws:///us-east-1a/i-2"}) || got.Status.Replicas == nil || *got.Status.Replicas != 1 {
		t.Errorf("providerIDList %v, replicas %v", got.Spec.ProviderIDList, got.Status.Replicas)
	}
	// The pool's after-provisioned Ready inputs hold ApplyJobSucceeded,
	// which a machine's and the cluster's do not.
	if ready := conditions.Get(got, infrav1.ReadyCondition); ready == nil || ready.Status == metav1.ConditionTrue ||
		!strings.Contains(ready.Message, infrav1.ApplyJobSucceededCondition) {
		t.Errorf("Ready = %+v, want it to summarize ApplyJobSucceeded", ready)
	}

	// The bootstrap provider rotates its data: the inputs change.
	b := &corev1.Secret{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "bootstrap-mp"}, b); err != nil {
		t.Fatal(err)
	}
	b.Data["value"] = []byte("#cloud-config\n# new token\n")
	if err := c.Update(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if got := runner.applies(); len(got) != 1 || rec.count(shared.EventInputsChanged) != 1 {
		t.Errorf("applies %v, events %v; want one InputsChanged apply", got, rec.reasons)
	}
}

// TestReconcileWritesBackReplicas: for an autoscaled pool, a replicas
// output that differs from MachinePool.spec.replicas is written back to
// the MachinePool without starting an apply (replicas are out of the
// hash), and the next pass converges: no second patch or event.
func TestReconcileWritesBackReplicas(t *testing.T) {
	t.Parallel()
	tmp := testTMP(func(p *infrav1.TerraformMachinePool) {
		p.Finalizers = []string{Finalizer}
		p.Status.Conditions = []metav1.Condition{{
			Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason, LastTransitionTime: metav1.NewTime(t0),
		}}
		now := metav1.NewTime(t0)
		p.Status.LastRefresh, p.Status.LastDriftCheck = &now, &now
	})
	objs := world(tmp)
	for _, o := range objs {
		if mp, ok := o.(*clusterv1.MachinePool); ok {
			autoscaled(mp)
			mp.Spec.Replicas = new(int32(2))
		}
	}
	s := scheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&infrav1.TerraformMachinePool{}).Build()
	sr := &stateReader{}
	sr.set(t, state.KindTerraformCluster, "c1", clusterState(`{}`, ""))
	runner := &fakeRunner{}
	rec := &recorder{}
	d := shared.Deps{
		Client: c, APIReader: c, Scheme: s, Jobs: runner, State: sr, Recorder: rec,
		Clock: testingclock.NewFakePassiveClock(t0), RunnerImage: "registry.example/captf:dev", DriftDefault: 30 * time.Minute,
	}
	a := newAdapter(d, tmp.DeepCopy())
	owner, err := a.Owner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	in, gate, err := a.BuildInputs(t.Context(), owner, nil)
	if err != nil || gate != nil {
		t.Fatalf("BuildInputs: %+v, %v", gate, err)
	}
	h, err := hash.Inputs(contract.RoleMachinePool, tmp.Spec.Source.Image, in)
	if err != nil {
		t.Fatal(err)
	}
	// The cloud autoscaler grew the group to 3.
	st := poolState(poolOutputs(`["aws:///us-east-1a/i-1","aws:///us-east-1a/i-2","aws:///us-east-1a/i-3"]`, `3`))
	st.InputsHash = h
	sr.set(t, state.KindTerraformMachinePool, tmp.Name, st)

	r := &Reconciler{Deps: d}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tmp)}
	for range 2 {
		if _, err := r.Reconcile(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	if len(runner.jobs) != 0 {
		t.Errorf("created %d Jobs for an observed replicas change", len(runner.jobs))
	}
	mp := &clusterv1.MachinePool{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(testMP()), mp); err != nil {
		t.Fatal(err)
	}
	if mp.Spec.Replicas == nil || *mp.Spec.Replicas != 3 || mp.Annotations[clusterv1.ReplicasManagedByAnnotation] != ReplicasManagedByValue {
		t.Errorf("MachinePool spec.replicas %v, annotations %v", mp.Spec.Replicas, mp.Annotations)
	}
	if n := rec.count(shared.EventReplicasWrittenBack); n != 1 {
		t.Errorf("%d ReplicasWrittenBack events over two passes, want 1: %v", n, rec.reasons)
	}
}
