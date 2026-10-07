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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/objects"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/tfstate"
)

// Stage 2's waits.
const (
	// applyWait bounds one apply, from object creation to provisioned.
	applyWait = 6 * time.Minute
	// copyWait bounds CAPI copying a value from the infrastructure object.
	copyWait = 2 * time.Minute
	// eventWait bounds an expected Event's appearance once its cause is
	// done.
	eventWait = 30 * time.Second
)

// clusterDriftSeconds is the main TerraformCluster's drift interval, so
// stage 5 sees a drift Job within minutes.
const clusterDriftSeconds = 60

// cluster is stage 2: a Cluster and its TerraformCluster (drift every
// 60s, Report), on the Terraform noop-cluster image. It proves the apply
// Job, the status the outputs produce (endpoint, failure domains,
// conditions, pinned digest, runtime), the events, the values CAPI copies
// onto the Cluster, the inputs CAPTF rendered from the Cluster, the state
// the module wrote and its backup, and the credential mirror and runner
// RBAC in the namespace.
// It runs under ctx and fails t on any problem.
func (s *suite) cluster(ctx context.Context, t *testing.T) {
	const kind = objects.KindTerraformCluster
	start := time.Now()
	s.create(ctx, t, objects.TerraformClusterGVR, objects.TerraformCluster(s.ns, clusterName, objects.TerraformClusterOpts{
		Image: s.clusterImg.ref, Identity: s.identity, DriftIntervalSeconds: clusterDriftSeconds, DriftAction: "Report",
		ActiveDeadlineSeconds: jobDeadline,
	}))
	s.create(ctx, t, objects.ClusterGVR, objects.Cluster(s.ns, clusterName, objects.ClusterOpts{}))
	hint := s.hint(kind, clusterName)

	s.waitApplied(ctx, t, kind, clusterName, applyWait)
	job := s.jobComplete(ctx, t, kind, clusterName, "apply", start, time.Minute)
	expectEqual(t, "apply Job "+job.Name+" source image", jobImage(job), s.clusterImg.ref, hint)

	endpoint := map[string]any{"host": "noop-" + clusterName + ".invalid", "port": 6443}
	tc := s.waitConditions(ctx, t, objects.TerraformClusterGVR, clusterName, []cond{
		{"Ready", "True", ""},
		{"ApplyJobSucceeded", "True", "ApplySucceeded"},
		{"OutputsValid", "True", ""},
		{"InfrastructureHealthy", "True", ""},
	}, copyWait, hint)
	what := "TerraformCluster " + clusterName
	expectField(t, what, tc, "spec.controlPlaneEndpoint", endpoint, hint)
	expectField(t, what, tc, "status.failureDomains", []any{map[string]any{"name": failureDomain, "controlPlane": true}}, hint)
	expectField(t, what, tc, "status.initialization.provisioned", true, hint)
	expectField(t, what, tc, "status.source.image", s.clusterImg.ref, hint)
	expectField(t, what, tc, "status.source.imageDigest", s.clusterImg.Pinned(), hint)
	expectField(t, what, tc, "status.stateSecretSuffix", tfstate.SuffixFor(s.ns, kind, clusterName), hint)
	expectRuntime(t, what, tc, s.clusterImg, hint)
	s.expectEvents(ctx, t, clusterName, eventWait, s.kubectlNS("get events --field-selector involvedObject.name="+clusterName),
		"JobCreated", "JobSucceeded", "DigestPinned", "StateBackedUp", "Provisioned", "ControlPlaneEndpointSet", "FailureDomainsChanged")

	s.waitFields(ctx, t, objects.ClusterGVR, clusterName, []fieldCheck{
		{"status.initialization.infrastructureProvisioned", true},
		{"spec.controlPlaneEndpoint", endpoint},
		{"status.failureDomains", []any{map[string]any{"name": failureDomain, "controlPlane": true}}},
	}, copyWait, s.kubectlNS("get cluster "+clusterName+" -o yaml"))
	s.waitConditions(ctx, t, objects.ClusterGVR, clusterName, []cond{{"InfrastructureReady", "True", ""}}, copyWait, s.kubectlNS("get cluster "+clusterName+" -o yaml"))

	s.checkClusterInputs(ctx, t)
	s.checkClusterState(ctx, t)
	s.checkRunnerAccess(ctx, t)
}

// runtimeVersions are the runtime versions the pinned noop images carry
// (their terraform-base and opentofu-base FROM lines). runtimeVersion is
// the bare version `<command> version -json` reports, so the version is
// what tells the runtimes apart.
var runtimeVersions = map[framework.NoopRuntime]string{
	framework.RuntimeTerraform: "1.16.4",
	framework.RuntimeOpenTofu:  "1.12.6",
}

// expectRuntime fails t unless status.source.runtimeVersion of u, the
// object what, is the version of img's runtime (runtimeVersions); hint
// says where to look.
func expectRuntime(t *testing.T, what string, u *unstructured.Unstructured, img image, hint string) {
	t.Helper()
	want := runtimeVersions[img.Runtime]
	if v := str(u, "status.source.runtimeVersion"); v != want {
		t.Errorf("%s: expected status.source.runtimeVersion %q (the %s in %s), observed %q; inspect: %s", what, want, img.Runtime, img.Pinned(), v, hint)
	}
}

// checkClusterInputs checks the main cluster's applied inputs Secret: the
// pinned digest annotation, captf_cluster naming the Cluster, the
// cluster_network rendered from the Cluster's spec, and
// control_plane_initialized false (no control plane runs).
// It reads under ctx and reports through t.
func (s *suite) checkClusterInputs(ctx context.Context, t *testing.T) {
	t.Helper()
	sec, vars := s.applied(ctx, t, objects.KindTerraformCluster, clusterName)
	hint := s.kubectlNS("get secret " + sec.Name + " -o jsonpath='{.data.terraform\\.tfvars\\.json}' | base64 -d")
	expectEqual(t, "applied inputs "+sec.Name+" annotation "+imageDigestAnnotation, sec.Annotations[imageDigestAnnotation], s.clusterImg.Pinned(), hint)
	expectEqual(t, "inputs captf_cluster", vars["captf_cluster"], map[string]any{"name": clusterName, "namespace": s.ns}, hint)
	expectEqual(t, "inputs cluster_network", vars["cluster_network"], map[string]any{
		"pods": []any{"10.244.0.0/16"}, "services": []any{"10.96.0.0/12"}, "api_server_port": 6443, "service_domain": nil,
	}, hint)
	expectEqual(t, "inputs control_plane_initialized", vars["control_plane_initialized"], false, hint)
}

// checkClusterState checks the main cluster's state: its outputs hold the
// endpoint and exports.backend_id (noop-backend-<uuid>, kept for stage 3),
// and a state backup exists. It reads under ctx and reports through t.
func (s *suite) checkClusterState(ctx context.Context, t *testing.T) {
	t.Helper()
	const kind = objects.KindTerraformCluster
	st := s.readState(ctx, t, kind, clusterName)
	suffix := tfstate.SuffixFor(s.ns, kind, clusterName)
	hint := s.kubectlNS("get secret " + tfstate.SecretName(suffix) + " -o yaml")
	expectEqual(t, "cluster state output control_plane_endpoint", st.Outputs["control_plane_endpoint"].Value,
		map[string]any{"host": "noop-" + clusterName + ".invalid", "port": 6443}, hint)
	exports, _ := st.Outputs["exports"].Value.(map[string]any)
	id, _ := exports["backend_id"].(string)
	if !strings.HasPrefix(id, "noop-backend-") || len(id) <= len("noop-backend-") {
		t.Fatalf("cluster state: expected output exports.backend_id = noop-backend-<uuid>, observed exports %s; inspect: %s", canonical(st.Outputs["exports"].Value), hint)
	}
	s.backendID = id
	backups, err := s.secretsLeft(ctx, "", "captf-state-backup-"+suffix)
	if err != nil || len(backups) == 0 {
		t.Errorf("expected a state backup Secret captf-state-backup-%s-<serial>, observed %v (err %v); inspect: %s", suffix, backups, err, s.kubectlNS("get secrets"))
	}
	t.Logf("cluster state serial %d, backend_id %s, backups %v", st.Serial, id, backups)
}

// checkRunnerAccess checks that the namespace holds the identity's
// credential mirror and the runner ServiceAccount and RoleBinding. It
// reads under ctx and reports through t.
func (s *suite) checkRunnerAccess(ctx context.Context, t *testing.T) {
	t.Helper()
	hint := s.kubectlNS("get secrets,serviceaccounts,rolebindings")
	if _, err := s.c.Kube.CoreV1().Secrets(s.ns).Get(ctx, mirrorPrefix+s.identity, metav1.GetOptions{}); err != nil {
		t.Errorf("expected the credential mirror %s/%s%s: %v; inspect: %s", s.ns, mirrorPrefix, s.identity, err, hint)
	}
	if _, err := s.c.Kube.CoreV1().ServiceAccounts(s.ns).Get(ctx, runnerName, metav1.GetOptions{}); err != nil {
		t.Errorf("expected the runner ServiceAccount %s/%s: %v; inspect: %s", s.ns, runnerName, err, hint)
	}
	if _, err := s.c.Kube.RbacV1().RoleBindings(s.ns).Get(ctx, runnerName, metav1.GetOptions{}); err != nil {
		t.Errorf("expected the runner RoleBinding %s/%s: %v; inspect: %s", s.ns, runnerName, err, hint)
	}
}
