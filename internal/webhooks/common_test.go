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

package webhooks

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestValidateJobPolicySecurityContexts: the container and pod security
// contexts may not weaken the defaults: no root, no writable root
// filesystem, no unconfined seccomp, no unmasked /proc, no Windows host
// process. Hardened values and unset fields are admitted.
func TestValidateJobPolicySecurityContexts(t *testing.T) {
	t.Parallel()
	ctr := func(sc corev1.SecurityContext) *infrav1.JobPolicy { return &infrav1.JobPolicy{SecurityContext: &sc} }
	pod := func(sc corev1.PodSecurityContext) *infrav1.JobPolicy {
		return &infrav1.JobPolicy{PodSecurityContext: &sc}
	}
	unmasked := corev1.UnmaskedProcMount
	tests := []struct {
		name string
		p    *infrav1.JobPolicy
		want string // field path expected in the errors; empty for none
	}{
		{name: "hardened container", p: ctr(corev1.SecurityContext{
			ReadOnlyRootFilesystem: new(true), RunAsNonRoot: new(true), RunAsUser: new(int64(65532)),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			ProcMount:      new(corev1.DefaultProcMount),
		})},
		{name: "hardened pod", p: pod(corev1.PodSecurityContext{
			RunAsNonRoot: new(true), RunAsUser: new(int64(1000)),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeLocalhost},
		})},
		{name: "writable root", p: ctr(corev1.SecurityContext{ReadOnlyRootFilesystem: new(false)}), want: "spec.jobs.securityContext.readOnlyRootFilesystem"},
		{name: "container unconfined", p: ctr(corev1.SecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}}),
			want: "spec.jobs.securityContext.seccompProfile.type"},
		{name: "unmasked proc", p: ctr(corev1.SecurityContext{ProcMount: &unmasked}), want: "spec.jobs.securityContext.procMount"},
		{name: "container host process", p: ctr(corev1.SecurityContext{WindowsOptions: &corev1.WindowsSecurityContextOptions{HostProcess: new(true)}}),
			want: "spec.jobs.securityContext.windowsOptions.hostProcess"},
		{name: "container runAsNonRoot false", p: ctr(corev1.SecurityContext{RunAsNonRoot: new(false)}), want: "spec.jobs.securityContext.runAsNonRoot"},
		{name: "container uid 0", p: ctr(corev1.SecurityContext{RunAsUser: new(int64(0))}), want: "spec.jobs.securityContext.runAsUser"},
		{name: "pod unconfined", p: pod(corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}}),
			want: "spec.jobs.podSecurityContext.seccompProfile.type"},
		{name: "pod host process", p: pod(corev1.PodSecurityContext{WindowsOptions: &corev1.WindowsSecurityContextOptions{HostProcess: new(true)}}),
			want: "spec.jobs.podSecurityContext.windowsOptions.hostProcess"},
		{name: "pod runAsNonRoot false", p: pod(corev1.PodSecurityContext{RunAsNonRoot: new(false)}), want: "spec.jobs.podSecurityContext.runAsNonRoot"},
		{name: "pod uid 0", p: pod(corev1.PodSecurityContext{RunAsUser: new(int64(0))}), want: "spec.jobs.podSecurityContext.runAsUser"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			errs := validateJobPolicy(field.NewPath("spec", "jobs"), tt.p)
			if tt.want == "" {
				if len(errs) != 0 {
					t.Fatalf("errors = %v, want none", errs)
				}
				return
			}
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), tt.want+": Forbidden") {
				t.Fatalf("errors = %v, want one Forbidden at %s", errs, tt.want)
			}
		})
	}
}
