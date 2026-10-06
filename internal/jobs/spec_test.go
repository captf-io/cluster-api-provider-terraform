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

package jobs

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/captf-io/cluster-api-provider-terraform/internal/plankey"
)

// volumeNamed returns the volume called name among vols, or nil when
// absent.
func volumeNamed(vols []corev1.Volume, name string) *corev1.Volume {
	for i := range vols {
		if vols[i].Name == name {
			return &vols[i]
		}
	}
	return nil
}

// TestPlanKeyMount: the plan key Secret is mounted (key file only, 0440,
// read-only) and passed to the runner for plan and apply Jobs that set
// PlanKeySecret, and for no other op and when the field is empty.
func TestPlanKeyMount(t *testing.T) {
	t.Parallel()
	for _, op := range []Op{OpPlan, OpApply, OpDestroy, OpRefresh, OpDrift, OpRestore} {
		for _, secret := range []string{"", "captf-plan-key-c"} {
			t.Run(string(op)+"/"+secret, func(t *testing.T) {
				t.Parallel()
				s := spec(op)
				if op == OpRestore {
					s = restoreSpec()
				}
				s.PlanKeySecret = secret
				j, _ := Build(s, "runner:img")
				pod := j.Spec.Template.Spec
				vol := volumeNamed(pod.Volumes, "plan-key")
				var mount *corev1.VolumeMount
				for i := range pod.Containers[0].VolumeMounts {
					if m := &pod.Containers[0].VolumeMounts[i]; m.Name == "plan-key" {
						mount = m
					}
				}
				var flag string
				for _, a := range pod.Containers[0].Args {
					if strings.HasPrefix(a, "--plan-key-file") {
						flag = a
					}
				}
				if secret == "" || (op != OpPlan && op != OpApply) {
					if vol != nil || mount != nil || flag != "" {
						t.Errorf("volume %+v, mount %+v, flag %q; want none", vol, mount, flag)
					}
					return
				}
				if vol == nil || vol.Secret == nil || vol.Secret.SecretName != secret || *vol.Secret.DefaultMode != 0o440 ||
					len(vol.Secret.Items) != 1 || vol.Secret.Items[0].Key != plankey.KeyFile || vol.Secret.Items[0].Path != plankey.KeyFile {
					t.Errorf("volume = %+v", vol)
				}
				if mount == nil || mount.MountPath != plankey.MountPath || !mount.ReadOnly {
					t.Errorf("mount = %+v", mount)
				}
				if want := "--plan-key-file=" + plankey.MountPath + "/" + plankey.KeyFile; flag != want {
					t.Errorf("flag = %q, want %q", flag, want)
				}
			})
		}
	}
}

// TestSourceSecurityContextDropsAll: a user capabilities object keeps its
// own entries but never loses the default drop of ALL, and the default
// read-only root filesystem survives a context that does not set it.
func TestSourceSecurityContextDropsAll(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		user *corev1.SecurityContext
		want []corev1.Capability
	}{
		{name: "nil", user: nil, want: []corev1.Capability{"ALL"}},
		{name: "empty capabilities", user: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{}}, want: []corev1.Capability{"ALL"}},
		{name: "other drop", user: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"NET_RAW"}}}, want: []corev1.Capability{"ALL", "NET_RAW"}},
		{name: "already ALL", user: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}, want: []corev1.Capability{"ALL"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sc := sourceSecurityContext(tt.user)
			if !slices.Equal(sc.Capabilities.Drop, tt.want) {
				t.Errorf("drop = %v, want %v", sc.Capabilities.Drop, tt.want)
			}
			if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
				t.Error("readOnlyRootFilesystem default lost")
			}
		})
	}
}

// TestSecretVolumeModes: every Secret-backed volume (credentials, the
// config Secret carrying the module's variables, a restore's projected
// config) is mounted 0440, never the 0644 default.
func TestSecretVolumeModes(t *testing.T) {
	t.Parallel()
	apply, _ := Build(spec(OpApply), "runner:img")
	cfg := volumeNamed(apply.Spec.Template.Spec.Volumes, "config")
	if cfg == nil || cfg.Secret == nil || cfg.Secret.DefaultMode == nil || *cfg.Secret.DefaultMode != 0o440 {
		t.Errorf("config volume = %+v, want Secret with defaultMode 0440", cfg)
	}
	restore, _ := Build(restoreSpec(), "runner:img")
	cfg = volumeNamed(restore.Spec.Template.Spec.Volumes, "config")
	if cfg == nil || cfg.Projected == nil || cfg.Projected.DefaultMode == nil || *cfg.Projected.DefaultMode != 0o440 {
		t.Errorf("restore config volume = %+v, want projected with defaultMode 0440", cfg)
	}
}

// TestFingerprints: a plan Job, a guarded apply (a cluster's, or a pool's
// that carries an ApprovalHash) and an approved apply fingerprint their
// plan; no other Job does.
func TestFingerprints(t *testing.T) {
	t.Parallel()
	pool := spec(OpApply)
	pool.ApprovalHash = "h1:exports"
	approved := spec(OpApply)
	approved.ExpectPlan = "p2:x"
	for name, c := range map[string]struct {
		s    Spec
		want bool
	}{
		"plan":            {spec(OpPlan), true},
		"cluster apply":   {clusterSpec(OpApply), true},
		"pool guarded":    {pool, true},
		"approved apply":  {approved, true},
		"machine apply":   {spec(OpApply), false},
		"cluster destroy": {clusterSpec(OpDestroy), false},
		"cluster drift":   {clusterSpec(OpDrift), false},
		"cluster refresh": {clusterSpec(OpRefresh), false},
	} {
		if got := c.s.Fingerprints(); got != c.want {
			t.Errorf("%s: Fingerprints() = %v, want %v", name, got, c.want)
		}
	}
}
