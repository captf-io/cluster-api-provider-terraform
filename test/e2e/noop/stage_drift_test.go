//go:build e2e

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

package noop

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/objects"
)

// Stage 5's waits.
const (
	// driftWait bounds the next drift Job of the main cluster: its 60s
	// interval, the jitter on top and the run itself.
	driftWait = 4 * time.Minute
	// failWait bounds the failing apply, from creation to its failed
	// condition.
	failWait = 5 * time.Minute
)

// extraneousProperty is Terraform's diagnostic for a module argument the
// module does not declare, as the JSON syntax of the rendered
// main.tf.json reports it (the native syntax says "Unsupported
// argument"). Terraform reports it at init, which loads the
// configuration.
const extraneousProperty = "Extraneous JSON object property"

// drift is stage 5. Drift: the main cluster's next drift Job runs on the
// pinned image and succeeds; DriftJobSucceeded=True, DriftDetected=False
// and lastDriftCheck set. Failure: a second Cluster and TerraformCluster
// set a variable the module does not declare, so the apply fails with
// ApplyJobSucceeded=False (ApplyFailed), lastRun.error a failed init step
// naming the extraneous property, no provisioned and a JobFailed Warning
// event.
// Recovery: removing the variable runs a new apply that succeeds.
// It runs under ctx and fails t on any problem.
func (s *suite) drift(ctx context.Context, t *testing.T) {
	const kind = objects.KindTerraformCluster
	hint := s.hint(kind, clusterName)
	start := time.Now()
	job := s.jobComplete(ctx, t, kind, clusterName, "drift", start, driftWait)
	expectEqual(t, "drift Job "+job.Name+" source image (the pinned digest)", jobImage(job), s.clusterImg.Pinned(), hint)
	// The conditions may still describe an earlier drift Job until the
	// manager books this one: lastDriftCheck (the Job's finish time) tells.
	want := []cond{{"DriftJobSucceeded", "True", "DriftChecked"}, {"DriftDetected", "False", "NoDrift"}}
	err := eventually(ctx, t, "TerraformCluster "+clusterName+" drift check booked", copyWait, func(ctx context.Context) error {
		tc, err := resource(s.c.Dynamic, objects.TerraformClusterGVR, s.ns).Get(ctx, clusterName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		last := str(tc, "status.lastDriftCheck")
		if ts, err := time.Parse(time.RFC3339, last); err != nil || ts.Before(job.CreationTimestamp.Time) {
			return fmt.Errorf("status.lastDriftCheck %q, want at or after drift Job %s's creation %s", last, job.Name, job.CreationTimestamp.UTC().Format(time.RFC3339))
		}
		return conditionsMatch(tc, want)
	})
	if err != nil {
		t.Errorf("TerraformCluster %s: %v; inspect: %s", clusterName, err, hint)
	}

	s.failAndRecover(ctx, t)
}

// applyBooked returns a check that passes once the TerraformCluster name
// reports ApplyJobSucceeded=True (ApplySucceeded) for the Job job and is
// provisioned; the condition still names the failed Job until the manager
// books the new one.
func (s *suite) applyBooked(name, job string) func(context.Context) error {
	return func(ctx context.Context) error {
		tc, err := resource(s.c.Dynamic, objects.TerraformClusterGVR, s.ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		status, reason, msg, _ := condition(tc, "ApplyJobSucceeded")
		if status != "True" || reason != "ApplySucceeded" || msg != "Job "+job {
			return fmt.Errorf("expected ApplyJobSucceeded=True (ApplySucceeded, %q), observed %s (%s, %q)", "Job "+job, orNone(status), reason, msg)
		}
		if p, _ := field(tc, "status.initialization.provisioned"); !isTrue(p) {
			return fmt.Errorf("expected status.initialization.provisioned=true, observed %v", p)
		}
		return nil
	}
}

// failAndRecover creates the second cluster with an undeclared variable,
// checks the failed apply, removes the variable and waits for the
// recovery apply. It runs under ctx and reports through t.
func (s *suite) failAndRecover(ctx context.Context, t *testing.T) {
	t.Helper()
	const kind = objects.KindTerraformCluster
	hint := s.hint(kind, failName)
	s.create(ctx, t, objects.TerraformClusterGVR, objects.TerraformCluster(s.ns, failName, objects.TerraformClusterOpts{
		Image: s.clusterImg.ref, Identity: s.identity, ActiveDeadlineSeconds: jobDeadline,
		Variables: map[string]any{unknownVariable: "x"},
	}))
	s.create(ctx, t, objects.ClusterGVR, objects.Cluster(s.ns, failName, objects.ClusterOpts{}))
	tc := s.waitConditions(ctx, t, objects.TerraformClusterGVR, failName, []cond{{"ApplyJobSucceeded", "False", "ApplyFailed"}}, failWait, hint)
	what := "TerraformCluster " + failName
	expectField(t, what, tc, "status.lastRun.operation", "apply", hint)
	expectField(t, what, tc, "status.lastRun.error.kind", "step", hint)
	expectField(t, what, tc, "status.lastRun.error.step", "init", hint)
	if summary := str(tc, "status.lastRun.error.summary"); !strings.Contains(summary, extraneousProperty) {
		t.Errorf("%s: expected status.lastRun.error.summary to mention %q (the module declares no %s), observed %q (lastRun %s); inspect: %s",
			what, extraneousProperty, unknownVariable, summary, canonical(fieldOr(tc, "status.lastRun")), hint)
	}
	if p, _ := field(tc, "status.initialization.provisioned"); isTrue(p) {
		t.Errorf("%s: expected not provisioned after the failed apply, observed status.initialization.provisioned=true; inspect: %s", what, hint)
	}
	s.expectWarningEvent(ctx, t, failName, "JobFailed", eventWait, s.kubectlNS("get events --field-selector involvedObject.name="+failName))

	s.failFixed = time.Now()
	if _, err := s.c.Dynamic.Resource(objects.TerraformClusterGVR).Namespace(s.ns).Patch(ctx, failName, types.MergePatchType,
		[]byte(`{"spec":{"variables":null}}`), metav1.PatchOptions{}); err != nil {
		t.Fatalf("remove spec.variables from TerraformCluster %s/%s: %v", s.ns, failName, err)
	}
	job := s.jobComplete(ctx, t, kind, failName, "apply", s.failFixed, applyWait)
	if err := eventually(ctx, t, what+" recovery booked", copyWait, s.applyBooked(failName, job.Name)); err != nil {
		t.Fatalf("%s: %v; inspect: %s", what, err, hint)
	}
	tc = s.get(ctx, t, objects.TerraformClusterGVR, failName)
	expectField(t, what, tc, "spec.controlPlaneEndpoint", map[string]any{"host": "noop-" + failName + ".invalid", "port": 6443}, hint)
	t.Logf("%s recovered: apply Job %s completed after the variable was removed", what, job.Name)
}

// fieldOr returns the value at path in u, or a placeholder string when it
// is absent, for messages.
func fieldOr(u *unstructured.Unstructured, path string) any {
	v, ok := field(u, path)
	if !ok {
		return fmt.Sprintf("<%s absent>", path)
	}
	return v
}
