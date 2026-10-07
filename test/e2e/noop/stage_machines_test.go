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
	"encoding/base64"
	"testing"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/objects"
)

// machineCase is one machine stage 3 creates.
type machineCase struct {
	// name names the Machine and its TerraformMachine.
	name string
	// img is the module image.
	img image
	// failureDomain is the Machine's spec.failureDomain, "" for none.
	failureDomain string
}

// machines is stage 3: Machine A (Terraform, failure domain fd1) and
// Machine B (OpenTofu), each with its TerraformMachine and the bootstrap
// Secret. For each it proves the apply Job, the providerID and addresses
// the outputs produce and CAPI copies onto the Machine, the pinned digest
// and runtime, and the export flow: the cluster's exports.backend_id, the
// Machine name, the bootstrap data and control_plane=false in the
// machine's inputs. No refresh follows a machine's apply (its outputs
// already read healthy), so the pinned reference is proven by the
// machine's destroy Job in stage 6.
// It runs under ctx and fails t on any problem.
func (s *suite) machines(ctx context.Context, t *testing.T) {
	cases := []machineCase{
		{name: machineA, img: s.machineAImg, failureDomain: failureDomain},
		{name: machineB, img: s.machineBImg},
	}
	start := time.Now()
	for _, m := range cases {
		s.create(ctx, t, objects.TerraformMachineGVR, objects.TerraformMachine(s.ns, m.name, objects.TerraformMachineOpts{
			Image: m.img.ref, ActiveDeadlineSeconds: jobDeadline,
		}))
		s.create(ctx, t, objects.MachineGVR, objects.Machine(s.ns, m.name, clusterName, objects.MachineOpts{
			BootstrapSecret: bootstrapName, FailureDomain: m.failureDomain,
		}))
	}
	versions := map[string]string{}
	for _, m := range cases {
		versions[m.name] = s.checkMachine(ctx, t, m, start)
	}
	if versions[machineA] == versions[machineB] {
		t.Errorf("expected machines %s (Terraform) and %s (OpenTofu) to report different runtimeVersions, observed %q for both", machineA, machineB, versions[machineA])
	}
}

// checkMachine waits for the machine m, whose objects were created at
// start, and checks its apply, status, CAPI Machine and inputs. It
// returns its runtimeVersion.
// It waits under ctx and reports through t.
func (s *suite) checkMachine(ctx context.Context, t *testing.T, m machineCase, start time.Time) string {
	t.Helper()
	const kind = objects.KindTerraformMachine
	hint := s.hint(kind, m.name)
	tm := s.waitApplied(ctx, t, kind, m.name, applyWait)
	job := s.jobComplete(ctx, t, kind, m.name, "apply", start, time.Minute)
	expectEqual(t, "apply Job "+job.Name+" source image", jobImage(job), m.img.ref, hint)

	what := "TerraformMachine " + m.name
	providerID := "noop:///" + s.ns + "/" + m.name
	addresses := []any{map[string]any{"type": "InternalIP", "address": "10.0.0.1"}}
	expectField(t, what, tm, "spec.providerID", providerID, hint)
	expectField(t, what, tm, "status.addresses", addresses, hint)
	expectField(t, what, tm, "status.initialization.provisioned", true, hint)
	expectField(t, what, tm, "status.source.imageDigest", m.img.Pinned(), hint)
	expectField(t, what, tm, "status.interruptible", false, hint)
	if m.failureDomain != "" {
		expectField(t, what, tm, "status.failureDomain", m.failureDomain, hint)
	}
	expectRuntime(t, what, tm, m.img, hint)
	s.expectEvents(ctx, t, m.name, eventWait, s.kubectlNS("get events --field-selector involvedObject.name="+m.name),
		"JobCreated", "JobSucceeded", "DigestPinned", "Provisioned", "ProviderIDSet")

	machineHint := s.kubectlNS("get machine " + m.name + " -o yaml")
	s.waitFields(ctx, t, objects.MachineGVR, m.name, []fieldCheck{
		{"spec.providerID", providerID},
		{"status.addresses", addresses},
		{"status.initialization.infrastructureProvisioned", true},
	}, copyWait, machineHint)
	s.waitConditions(ctx, t, objects.MachineGVR, m.name, []cond{{"InfrastructureReady", "True", ""}}, copyWait, machineHint)

	sec, vars := s.applied(ctx, t, kind, m.name)
	inputsHint := s.kubectlNS("get secret " + sec.Name + " -o jsonpath='{.data.terraform\\.tfvars\\.json}' | base64 -d")
	expectEqual(t, "applied inputs "+sec.Name+" annotation "+imageDigestAnnotation, sec.Annotations[imageDigestAnnotation], m.img.Pinned(), inputsHint)
	outputs, _ := vars["captf_cluster_outputs"].(map[string]any)
	expectEqual(t, "machine "+m.name+" inputs captf_cluster_outputs.backend_id (the cluster state's exports.backend_id)", outputs["backend_id"], s.backendID, inputsHint)
	expectEqual(t, "machine "+m.name+" inputs machine_name", vars["machine_name"], m.name, inputsHint)
	expectEqual(t, "machine "+m.name+" inputs bootstrap_data", vars["bootstrap_data"], base64.StdEncoding.EncodeToString([]byte(bootstrapValue)), inputsHint)
	expectEqual(t, "machine "+m.name+" inputs control_plane", vars["control_plane"], false, inputsHint)
	var fd any
	if m.failureDomain != "" {
		fd = m.failureDomain
	}
	expectEqual(t, "machine "+m.name+" inputs failure_domain", vars["failure_domain"], fd, inputsHint)
	v := str(tm, "status.source.runtimeVersion")
	t.Logf("machine %s: %s, runtime %q, digest %s", m.name, providerID, v, str(tm, "status.source.imageDigest"))
	return v
}
