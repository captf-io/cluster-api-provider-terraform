//go:build e2e

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

package noop

import (
	"context"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/objects"
)

// The pool's replica counts: created with poolReplicas, then scaled to
// poolScaled.
const (
	// poolReplicas is the MachinePool's initial spec.replicas.
	poolReplicas = 3
	// poolScaled is spec.replicas after the scale down.
	poolScaled = 2
)

// refreshWait bounds a refresh Job's appearance and completion; the
// refresh after an apply starts at once.
const refreshWait = 3 * time.Minute

// pool is stage 4: a MachinePool of 3 replicas and its
// TerraformMachinePool on the OpenTofu noop-machinepool image. It proves
// the apply, the provider IDs, replicas, instances and readiness the
// outputs produce and the provisioned flag CAPI copies; then a scale down
// to 2 through the MachinePool, which runs a new apply; then that the
// pinned digest is pullable, through the refresh Job that follows the
// apply on the pinned reference.
// It runs under ctx and fails t on any problem.
func (s *suite) pool(ctx context.Context, t *testing.T) {
	const kind = objects.KindTerraformMachinePool
	start := time.Now()
	s.create(ctx, t, objects.TerraformMachinePoolGVR, objects.TerraformMachinePool(s.ns, poolName, objects.TerraformMachinePoolOpts{
		Image: s.poolImg.ref, ActiveDeadlineSeconds: jobDeadline,
	}))
	s.create(ctx, t, objects.MachinePoolGVR, objects.MachinePool(s.ns, poolName, clusterName, objects.MachinePoolOpts{
		BootstrapSecret: bootstrapName, Replicas: poolReplicas,
	}))
	hint := s.hint(kind, poolName)
	s.waitApplied(ctx, t, kind, poolName, applyWait)
	job := s.jobComplete(ctx, t, kind, poolName, "apply", start, time.Minute)
	expectEqual(t, "apply Job "+job.Name+" source image", jobImage(job), s.poolImg.ref, hint)
	s.checkPoolMembers(ctx, t, poolReplicas)

	tmp := s.get(ctx, t, objects.TerraformMachinePoolGVR, poolName)
	what := "TerraformMachinePool " + poolName
	expectField(t, what, tmp, "spec.providerID", "noop-group:///"+s.ns+"/"+poolName, hint)
	expectField(t, what, tmp, "status.source.imageDigest", s.poolImg.Pinned(), hint)
	expectRuntime(t, what, tmp, s.poolImg, hint)
	sec, vars := s.applied(ctx, t, kind, poolName)
	inputsHint := s.kubectlNS("get secret " + sec.Name + " -o jsonpath='{.data.terraform\\.tfvars\\.json}' | base64 -d")
	expectEqual(t, "applied inputs "+sec.Name+" annotation "+imageDigestAnnotation, sec.Annotations[imageDigestAnnotation], s.poolImg.Pinned(), inputsHint)
	expectEqual(t, "pool inputs replicas", vars["replicas"], poolReplicas, inputsHint)
	outputs, _ := vars["captf_cluster_outputs"].(map[string]any)
	expectEqual(t, "pool inputs captf_cluster_outputs.backend_id (the cluster state's exports.backend_id)", outputs["backend_id"], s.backendID, inputsHint)

	scaled := time.Now()
	patch := fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, poolScaled)
	if _, err := s.c.Dynamic.Resource(objects.MachinePoolGVR).Namespace(s.ns).Patch(ctx, poolName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatalf("scale MachinePool %s/%s to %d: %v", s.ns, poolName, poolScaled, err)
	}
	job = s.jobComplete(ctx, t, kind, poolName, "apply", scaled, applyWait)
	t.Logf("scale-down apply Job %s completed", job.Name)
	s.checkPoolMembers(ctx, t, poolScaled)

	refresh := s.jobComplete(ctx, t, kind, poolName, "refresh", job.CreationTimestamp.Time, refreshWait)
	expectEqual(t, "refresh Job "+refresh.Name+" source image (the pinned digest)", jobImage(refresh), s.poolImg.Pinned(), hint)
}

// checkPoolMembers waits until the TerraformMachinePool reports n
// members (sorted providerIDList, status.replicas, n running instances,
// ready) and the MachinePool reads infrastructureProvisioned, failing t
// otherwise. The MachinePool's spec.providerIDList stays empty here:
// Cluster API copies it only after it reaches the workload cluster
// through its ClusterCache, and the noop cluster's endpoint never
// resolves.
// It waits under ctx.
func (s *suite) checkPoolMembers(ctx context.Context, t *testing.T, n int) {
	t.Helper()
	ids := make([]any, 0, n)
	instances := make([]any, 0, n)
	for i := range n {
		id := fmt.Sprintf("noop:///%s/%s/%d", s.ns, poolName, i)
		ids = append(ids, id)
		instances = append(instances, map[string]any{"providerID": id, "state": "running"})
	}
	s.waitFields(ctx, t, objects.TerraformMachinePoolGVR, poolName, []fieldCheck{
		{"spec.providerIDList", ids},
		{"status.replicas", n},
		{"status.instances", instances},
		{"status.ready", true},
		{"status.initialization.provisioned", true},
	}, copyWait, s.hint(objects.KindTerraformMachinePool, poolName))
	s.waitFields(ctx, t, objects.MachinePoolGVR, poolName, []fieldCheck{
		{"status.initialization.infrastructureProvisioned", true},
	}, copyWait, s.kubectlNS("get machinepool "+poolName+" -o yaml"))
	t.Logf("pool %s: %d members %v", poolName, n, ids)
}
