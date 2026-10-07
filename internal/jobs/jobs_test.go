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
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/cmd/runner/app"
	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// testScheme returns a runtime.Scheme with the client-go and CAPTF types
// registered, failing t if registration errors.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, infrav1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return s
}

// spec returns a fixture Spec for op, of TerraformMachine
// prod-md-0-abcde in Cluster prod.
func spec(op Op) Spec {
	return Spec{
		OwnerKind: state.KindTerraformMachine, OwnerName: "prod-md-0-abcde", Namespace: "team-a", ClusterName: "prod", KindShort: "m",
		Op: op, Attempt: 1, InputsHash: "h1:abc", DriftTick: "",
		ImageRef: "ghcr.io/example/machine-module:v1.2.3", PullPolicy: corev1.PullIfNotPresent,
		Policy:         infrav1.JobPolicy{ImagePullSecrets: []corev1.LocalObjectReference{{Name: "source-pull"}}},
		ServiceAccount: "captf-runner", CredsSecret: "captf-creds-aws",
		Suffix:        "0123456789abcdef-m",
		BackendLabels: state.BackendLabels(state.KindTerraformMachine, "prod-md-0-abcde", "prod"),
		Events:        true, OwnerUID: "5f1d3c2a-0000-4000-8000-00000000000a",
	}
}

// clusterSpec returns spec(op) adjusted for a TerraformCluster.
func clusterSpec(op Op) Spec {
	s := spec(op)
	s.OwnerKind, s.OwnerName, s.KindShort = state.KindTerraformCluster, "prod", "c"
	s.ImageRef = "ghcr.io/example/cluster-module:v1.2.3"
	s.Suffix = "0123456789abcdef-c"
	s.BackendLabels = state.BackendLabels(state.KindTerraformCluster, "prod", "prod")
	return s
}

// TestGuardDeletesArgs: only a TerraformCluster's apply guards against a
// destructive plan, passes the inputs hash it renders, and passes the
// approval only when one is set; a plan or apply Job with a plan key Secret
// passes the key file the runner parses back.
func TestGuardDeletesArgs(t *testing.T) {
	t.Parallel()
	parse := func(s Spec) runner.Options {
		t.Helper()
		j, _ := Build(s, "runner:img")
		o, _, err := app.ParseRunFlags(j.Spec.Template.Spec.Containers[0].Args)
		if err != nil {
			t.Fatalf("runner rejects Build's arguments: %v", err)
		}
		return o
	}
	for _, op := range []Op{OpApply, OpDestroy, OpRefresh, OpDrift, OpPlan} {
		m := spec(op)
		m.AllowDeletesHash, m.ExpectPlan = m.InputsHash, "p1:approved"
		m.PlanKeySecret = "captf-plan-key-m"
		c := clusterSpec(op)
		c.AllowDeletesHash, c.ExpectPlan = "h1:approved", "p1:approved"
		mo, co := parse(m), parse(c)
		if mo.GuardDeletes || mo.InputsHash != "" || mo.AllowDeletesHash != "" || mo.ExpectPlan != "" {
			t.Errorf("machine %s: %+v, want no guard flags", op, mo)
		}
		if (op == OpPlan || op == OpApply) && mo.PlanKeyFile != "/captf/plan-key/key" {
			t.Errorf("machine %s: PlanKeyFile = %q, want /captf/plan-key/key", op, mo.PlanKeyFile)
		}
		if want := op == OpApply; co.GuardDeletes != want {
			t.Errorf("cluster %s: guard %v, want %v", op, co.GuardDeletes, want)
		}
		if op == OpApply && (co.InputsHash != c.InputsHash || co.AllowDeletesHash != "h1:approved" || co.ExpectPlan != "p1:approved") {
			t.Errorf("cluster apply: %+v", co)
		}
		if op != OpApply && (co.InputsHash != "" || co.AllowDeletesHash != "" || co.ExpectPlan != "") {
			t.Errorf("cluster %s passes hashes: %+v", op, co)
		}
	}
	j, _ := Build(clusterSpec(OpApply), "runner:img")
	for _, a := range j.Spec.Template.Spec.Containers[0].Args {
		if strings.HasPrefix(a, "--allow-deletes-hash") {
			t.Errorf("an unapproved cluster apply passes %s", a)
		}
	}
}

// TestPoolGuardArgs: a TerraformMachinePool apply is guarded only with an
// approval hash, which the runner then compares with the approval in
// place of the inputs hash; the Job still records and is named after the
// inputs hash. Without one, and for every other op, no guard flag passes.
func TestPoolGuardArgs(t *testing.T) {
	t.Parallel()
	pool := func(op Op) Spec {
		s := spec(op)
		s.OwnerKind, s.OwnerName, s.KindShort = state.KindTerraformMachinePool, "prod-mp-0", "mp"
		s.AllowDeletesHash = "h2:approval"
		return s
	}
	for _, op := range []Op{OpApply, OpDestroy, OpRefresh, OpDrift} {
		unguarded, guarded := pool(op), pool(op)
		guarded.ApprovalHash = "h2:approval"
		for name, s := range map[string]Spec{"unguarded": unguarded, "guarded": guarded} {
			j, _ := Build(s, "runner:img")
			o, _, err := app.ParseRunFlags(j.Spec.Template.Spec.Containers[0].Args)
			if err != nil {
				t.Fatalf("runner rejects Build's arguments: %v", err)
			}
			want := op == OpApply && name == "guarded"
			if o.GuardDeletes != want {
				t.Errorf("pool %s %s: guard %v, want %v", name, op, o.GuardDeletes, want)
			}
			if want && (o.InputsHash != "h2:approval" || o.AllowDeletesHash != "h2:approval") {
				t.Errorf("pool %s %s: inputs hash %q, allow %q; want both the approval hash", name, op, o.InputsHash, o.AllowDeletesHash)
			}
			if !want && (o.InputsHash != "" || o.AllowDeletesHash != "") {
				t.Errorf("pool %s %s passes hashes: %+v", name, op, o)
			}
			if op == OpApply && (j.Annotations[state.InputsHashAnnotation] != s.InputsHash || j.Name != unguarded.Name()) {
				t.Errorf("pool %s apply: annotations %v, name %s; want the inputs hash's", name, j.Annotations, j.Name)
			}
		}
	}
}

// TestGoldenJobs pins the Job of every op (and a force-unlock) as YAML;
// UPDATE_SNAPSHOTS=1 rewrites them.
func TestGoldenJobs(t *testing.T) {
	t.Parallel()
	ser := kjson.NewSerializerWithOptions(kjson.DefaultMetaFactory, nil, nil, kjson.SerializerOptions{Yaml: true})
	cases := map[string]Spec{}
	for _, op := range []Op{OpApply, OpDestroy, OpRefresh, OpDrift} {
		cases[string(op)] = spec(op)
	}
	force := spec(OpApply)
	force.Attempt = 2
	force.ForceUnlockID = "9f1c0c5e-0000-4000-8000-000000000001"
	cases["apply-force-unlock"] = force
	cases["cluster-apply"] = clusterSpec(OpApply)
	approved := clusterSpec(OpApply)
	approved.Attempt = 2
	approved.AllowDeletesHash = approved.InputsHash
	cases["cluster-apply-approved"] = approved
	cases["cluster-plan"] = clusterSpec(OpPlan)
	planned := clusterSpec(OpApply)
	planned.Attempt = 3
	planned.ExpectPlan = "p1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases["cluster-apply-expect-plan"] = planned
	quiet := spec(OpApply)
	quiet.Events = false
	cases["apply-no-events"] = quiet
	cases["restore"] = restoreSpec()
	for name, s := range cases {
		job, dropped := Build(s, "ghcr.io/captf-io/cluster-api-provider-terraform:v0.1.0")
		if len(dropped) != 0 {
			t.Errorf("%s: dropped %v", name, dropped)
		}
		job.TypeMeta = metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"}
		var buf bytes.Buffer
		if err := ser.Encode(job, &buf); err != nil {
			t.Fatalf("encode: %v", err)
		}
		path := filepath.Join("testdata", name+".yaml")
		if os.Getenv("UPDATE_SNAPSHOTS") == "1" {
			if err := os.MkdirAll("testdata", 0o750); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
				t.Fatalf("write golden: %v", err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read golden (UPDATE_SNAPSHOTS=1 creates it): %v", err)
		}
		if !bytes.Equal(buf.Bytes(), want) {
			t.Errorf("%s differs from the golden file; rerun with UPDATE_SNAPSHOTS=1 and review the diff", path)
		}
	}
}

// restoreSpec returns a machine's restore of a two-chunk backup.
func restoreSpec() Spec {
	s := spec(OpRestore)
	s.DriftTick = "restore/captf-state-backup-0123456789abcdef-m-7"
	s.Restore = &Restore{
		Serial:           7,
		Secrets:          []string{"captf-state-backup-0123456789abcdef-m-7", "captf-state-backup-0123456789abcdef-m-7-part-1"},
		ManagedResources: 3,
	}
	return s
}

// TestRestoreJob: the restore Job projects the per-run Secret and every
// backup chunk into its config volume (chunks under restore/, which the
// runner's root copy skips), tells the runner how many chunks there are and
// how many resources to expect, and records the serial and inputs hash.
func TestRestoreJob(t *testing.T) {
	t.Parallel()
	s := restoreSpec()
	j, _ := Build(s, "runner:img")
	if !strings.Contains(j.Name, "-restore-a1-") || j.Labels[OpLabel] != "restore" {
		t.Errorf("name %s, labels %v", j.Name, j.Labels)
	}
	if j.Annotations[RestoreSerialAnnotation] != "7" || j.Annotations[state.InputsHashAnnotation] != s.InputsHash {
		t.Errorf("annotations = %v", j.Annotations)
	}
	var config *corev1.Volume
	for i := range j.Spec.Template.Spec.Volumes {
		if v := &j.Spec.Template.Spec.Volumes[i]; v.Name == "config" {
			config = v
		}
	}
	if config == nil || config.Projected == nil || len(config.Projected.Sources) != 3 {
		t.Fatalf("config volume = %+v", config)
	}
	src := config.Projected.Sources
	if src[0].Secret.Name != "captf-run-"+j.Name || src[0].Secret.Items != nil {
		t.Errorf("first source = %+v, want the whole per-run Secret", src[0].Secret)
	}
	for i, name := range s.Restore.Secrets {
		p := src[i+1].Secret
		if p.Name != name || len(p.Items) != 1 || p.Items[0].Key != state.DataKey || p.Items[0].Path != RestoreChunkDir+"/"+strconv.Itoa(i) {
			t.Errorf("chunk %d source = %+v", i, p)
		}
	}
	o, _, err := app.ParseRunFlags(j.Spec.Template.Spec.Containers[0].Args)
	if err != nil || o.Op != runner.OpRestore || o.RestoreChunks != 2 || o.RestoreResources != 3 || o.GuardDeletes {
		t.Errorf("runner options = %+v, %v", o, err)
	}
	// Another op mounts the per-run Secret itself.
	a, _ := Build(spec(OpApply), "runner:img")
	for _, v := range a.Spec.Template.Spec.Volumes {
		if v.Name == "config" && (v.Secret == nil || v.Projected != nil) {
			t.Errorf("apply config volume = %+v", v)
		}
	}
}

// TestNames checks that Name stays within MaxNameLength and produces a
// valid, collision-resistant DNS label across kinds and long object names,
// that Hash6 changes with each of its inputs, and that Labels produces
// valid label values.
func TestNames(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("n", 200)
	for _, op := range []Op{OpApply, OpDestroy, OpRefresh, OpDrift, OpRestore} {
		for _, name := range []string{"m", long, strings.Repeat("x", 40)} {
			for _, ks := range []string{"c", "m", "mp"} {
				n := Name(ks, name, op, 2147483647, Hash6("h1:x", op, 1, ""))
				if len(n) > MaxNameLength || len(validation.IsDNS1123Subdomain(n)) != 0 || len(validation.IsValidLabelValue(n)) != 0 {
					t.Errorf("Name = %q (%d chars)", n, len(n))
				}
				// The pod name <job>-<5> must be a full hostname (<= 63).
				if len(n)+6 > 63 {
					t.Errorf("pod names of %q would be truncated as hostnames", n)
				}
			}
		}
	}
	if got := Name("m", "web", OpApply, 3, "abcdef"); got != "captf-m-web-apply-a3-abcdef" {
		t.Errorf("Name = %s", got)
	}
	if Name("m", long, OpApply, 1, "aaaaaa") == Name("m", long+"x", OpApply, 1, "aaaaaa") {
		t.Error("long names collide")
	}
	h := Hash6("h1:x", OpDrift, 1, "t1")
	for _, other := range []string{Hash6("h1:y", OpDrift, 1, "t1"), Hash6("h1:x", OpRefresh, 1, "t1"), Hash6("h1:x", OpDrift, 2, "t1"), Hash6("h1:x", OpDrift, 1, "t2")} {
		if other == h || len(other) != 6 {
			t.Errorf("Hash6 did not change: %s vs %s", h, other)
		}
	}
	l := Labels(state.KindTerraformMachine, long, long, OpDrift, 12)
	for k, v := range l {
		if len(validation.IsValidLabelValue(v)) != 0 || len(validation.IsQualifiedName(k)) != 0 {
			t.Errorf("label %s=%s invalid", k, v)
		}
	}
	if l[AttemptLabel] != "12" || l[OpLabel] != "drift" || l[state.ManagedLabel] != "true" {
		t.Errorf("Labels = %v", l)
	}
}

// TestOwnsPod checks OwnsPod against pod names it owns and ones it must
// reject: another kind, another object, a missing pod suffix, an unknown
// op, a malformed attempt and unrelated names.
func TestOwnsPod(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("n", 60)
	for _, tt := range []struct {
		name, pod string
		want      bool
	}{
		{"web", "captf-m-web-apply-a1-abc123-x7k2p", true},
		{"web-1", "captf-m-web-1-drift-a12-abc123-x7k2p", true},
		{"web", "captf-m-web-restore-a2-abc123-x7k2p", true}, // a restore holds the lock too
		{long, Name("m", long, OpDestroy, 3, "abc123") + "-x7k2p", true},
		{"web", "captf-c-web-apply-a1-abc123-x7k2p", false},   // another kind
		{"web", "captf-m-web-1-apply-a1-abc123-x7k2p", false}, // another object
		{"web", "captf-m-web-apply-a1-abc123", false},         // no pod suffix
		{"web", "captf-m-web-plan-a1-abc123-x7k2p", true},     // a plan holds the lock too
		{"web", "captf-m-web-bogus-a1-abc123-x7k2p", false},   // unknown op
		{"web", "captf-m-web-apply-1-abc123-x7k2p", false},    // no a<attempt>
		{"web", "captf-m-web-apply-ax-abc123-x7k2p", false},
		{"web", "laptop", false},
		{"web", "", false},
	} {
		if got := OwnsPod("m", tt.name, tt.pod); got != tt.want {
			t.Errorf("OwnsPod(m, %s, %s) = %v", tt.name, tt.pod, got)
		}
	}
}

// TestBuild checks Build's env, security context, resource and volume
// output against a fully populated JobPolicy and against the defaults an
// empty one leaves in place.
func TestBuild(t *testing.T) {
	t.Parallel()
	s := spec(OpApply)
	uid := int64(1000)
	s.Policy = infrav1.JobPolicy{
		ImagePullSecrets:      []corev1.LocalObjectReference{{Name: "a"}, {Name: ""}, {Name: "b"}, {Name: "a"}, {Name: "c"}},
		Env:                   []corev1.EnvVar{{Name: "AWS_REGION", Value: "eu-west-1"}, {Name: "TF_LOG", Value: "DEBUG"}, {Name: "KUBE_NAMESPACE", Value: "x"}},
		SecurityContext:       &corev1.SecurityContext{RunAsUser: &uid},
		PodSecurityContext:    &corev1.PodSecurityContext{FSGroup: &uid},
		ActiveDeadlineSeconds: 600,
		LockTimeoutSeconds:    new(int32(60)),
	}
	job, dropped := Build(s, "runner:img")
	if !slices.Equal(dropped, []string{"TF_LOG", "KUBE_NAMESPACE"}) {
		t.Errorf("dropped = %v", dropped)
	}
	pod := job.Spec.Template.Spec
	if got := pod.ImagePullSecrets; len(got) != 3 || got[0].Name != "a" || got[2].Name != "c" {
		t.Errorf("pull secrets = %v, want a, b, c", got)
	}
	src := pod.Containers[0]
	envNames := []string{}
	for _, e := range src.Env {
		envNames = append(envNames, e.Name)
	}
	if !slices.Contains(envNames, "AWS_REGION") || slices.Contains(envNames, "TF_LOG") || countOf(envNames, "KUBE_NAMESPACE") != 1 {
		t.Errorf("env = %v", envNames)
	}
	if slices.Contains(envNames, "TF_DATA_DIR") {
		t.Error("TF_DATA_DIR is the runner's to set; the Job must not")
	}
	checkpoint := false
	for _, e := range src.Env {
		if e.Name == "CHECKPOINT_DISABLE" {
			checkpoint = e.Value == "1"
		}
	}
	if !checkpoint {
		t.Error("CHECKPOINT_DISABLE=1 missing from the Job env")
	}
	sc := src.SecurityContext
	if *sc.RunAsUser != 1000 || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || sc.Capabilities.Drop[0] != "ALL" || sc.RunAsNonRoot != nil {
		t.Errorf("merged container securityContext = %+v", sc)
	}
	if pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || *pod.SecurityContext.FSGroup != 1000 {
		t.Errorf("pod securityContext = %+v", pod.SecurityContext)
	}
	// The controller owns retries and pruning: never pod retries, never a TTL.
	if *job.Spec.BackoffLimit != 0 || job.Spec.TTLSecondsAfterFinished != nil || *job.Spec.ActiveDeadlineSeconds != 600 || !slices.Contains(src.Args, "--lock-timeout=60s") {
		t.Errorf("policy not applied: backoff %d ttl %v deadline %d args %v", *job.Spec.BackoffLimit, job.Spec.TTLSecondsAfterFinished, *job.Spec.ActiveDeadlineSeconds, src.Args)
	}
	if !slices.Contains(src.Args, "--bin="+DefaultBin) || countPrefix(src.Args, "--bin=") != 1 {
		t.Errorf("args = %v, want exactly --bin=%s (the image contract)", src.Args, DefaultBin)
	}
	// Defaults.
	def, _ := Build(spec(OpDrift), "runner:img")
	if *def.Spec.BackoffLimit != 0 || *def.Spec.ActiveDeadlineSeconds != 3600 || def.Spec.TTLSecondsAfterFinished != nil ||
		def.Spec.Template.Spec.Containers[0].SecurityContext.RunAsNonRoot != nil || def.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("defaults wrong: %+v", def.Spec)
	}
	// fsGroup always defaults to 65532 when unset, regardless of
	// runAsNonRoot: defaulting it only under runAsNonRoot would miss the
	// common non-root image setup (USER 65532 with no pod-level
	// runAsNonRoot). It never overrides the user's.
	if g := def.Spec.Template.Spec.SecurityContext.FSGroup; g == nil || *g != 65532 {
		t.Errorf("fsGroup default = %v, want 65532", g)
	}
	nonRoot := spec(OpApply)
	nonRoot.Policy.PodSecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: new(true)}
	nr, _ := Build(nonRoot, "runner:img")
	if g := nr.Spec.Template.Spec.SecurityContext.FSGroup; g == nil || *g != 65532 {
		t.Errorf("fsGroup under runAsNonRoot = %v, want 65532", g)
	}
	nonRoot.Policy.PodSecurityContext.FSGroup = &uid
	if nr, _ := Build(nonRoot, "runner:img"); *nr.Spec.Template.Spec.SecurityContext.FSGroup != 1000 {
		t.Error("fsGroup default overrides the user's")
	}
	creds := def.Spec.Template.Spec.Volumes[4]
	if creds.Secret.SecretName != "captf-creds-aws" || *creds.Secret.DefaultMode != 0o440 {
		t.Errorf("creds volume = %+v", creds)
	}
	if def.Spec.Template.Spec.Volumes[3].Secret.SecretName != "captf-run-"+def.Name {
		t.Error("config volume is not the per-run Secret")
	}
	init := def.Spec.Template.Spec.InitContainers[0]
	if !*init.SecurityContext.RunAsNonRoot || *init.SecurityContext.RunAsUser != 65532 || init.Image != "runner:img" {
		t.Errorf("init container = %+v", init)
	}
	// The init container's resources are fixed, never configurable; the main
	// container defaults when the policy sets none (BestEffort is exactly
	// wrong for a Terraform process).
	wantQty := func(t *testing.T, list corev1.ResourceList, name corev1.ResourceName, want string) {
		t.Helper()
		got, ok := list[name]
		if !ok || got.Cmp(resource.MustParse(want)) != 0 {
			t.Errorf("%s = %v, want %s", name, got, want)
		}
	}
	wantQty(t, init.Resources.Requests, corev1.ResourceCPU, InitContainerCPURequest)
	wantQty(t, init.Resources.Requests, corev1.ResourceMemory, InitContainerMemoryRequest)
	wantQty(t, init.Resources.Limits, corev1.ResourceCPU, InitContainerCPULimit)
	wantQty(t, init.Resources.Limits, corev1.ResourceMemory, InitContainerMemoryLimit)
	defSrc := def.Spec.Template.Spec.Containers[0]
	wantQty(t, defSrc.Resources.Requests, corev1.ResourceCPU, DefaultSourceCPURequest)
	wantQty(t, defSrc.Resources.Requests, corev1.ResourceMemory, DefaultSourceMemory)
	wantQty(t, defSrc.Resources.Limits, corev1.ResourceMemory, DefaultSourceMemory)
	if _, ok := defSrc.Resources.Limits[corev1.ResourceCPU]; ok {
		t.Error("default resources set a CPU limit, want none")
	}
	if defSrc.Resources.Requests.Memory().Cmp(*defSrc.Resources.Limits.Memory()) != 0 {
		t.Error("default memory request differs from its limit")
	}
	// A policy that sets resources replaces the default entirely, never merges.
	custom := spec(OpApply)
	custom.Policy.Resources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}
	cj, _ := Build(custom, "runner:img")
	cres := cj.Spec.Template.Spec.Containers[0].Resources
	if cres.Requests.Cpu().Cmp(resource.MustParse("1")) != 0 || cres.Limits != nil {
		t.Errorf("custom resources = %+v, want exactly the policy's", cres)
	}
	if def.Spec.Template.Labels[state.OwnerKindLabel] != state.KindTerraformMachine {
		t.Error("pod template lacks the owner labels")
	}
	for _, e := range def.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "TF_CLI_CONFIG_FILE" {
			t.Error("TF_CLI_CONFIG_FILE is the runner's to set")
		}
	}
}

// countPrefix returns how many elements of s start with prefix.
func countPrefix(s []string, prefix string) int {
	n := 0
	for _, e := range s {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

// countOf returns how many elements of s equal v.
func countOf(s []string, v string) int {
	n := 0
	for _, e := range s {
		if e == v {
			n++
		}
	}
	return n
}

// TestHCLMap checks HCLMap's key sorting and quoting against a fixed map.
func TestHCLMap(t *testing.T) {
	t.Parallel()
	got := HCLMap(map[string]string{"b": "2", "a/x.y": "", "c": `q"`})
	if got != `{"a/x.y"="","b"="2","c"="q\""}` {
		t.Errorf("HCLMap = %s", got)
	}
}

// job returns a fixture Job named name, of TerraformMachine m in Cluster c,
// labeled for op and attempt, created at created, with conditions giving it
// outcome.
func job(name string, op Op, attempt int, outcome Outcome, created time.Time) batchv1.Job {
	j := batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "team-a", CreationTimestamp: metav1.NewTime(created),
		Labels: Labels(state.KindTerraformMachine, "m", "c", op, int32(attempt)),
	}}
	switch outcome {
	case Succeeded:
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	case Failed:
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	}
	return j
}

// TestAttemptActivePrune checks that Attempt skips names still retained
// after pruning, that Active finds the newest running Job, and that Prune
// deletes finished Jobs beyond the given per-outcome limits while keeping
// each op's newest success and newest unresolved failure.
func TestAttemptActivePrune(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	// Failed apply attempts 3, 4, 5 retained (1 and 2 pruned): the next
	// attempt must be 6, not count+1 = 4, which is still retained.
	jobs := []batchv1.Job{
		job("f3", OpApply, 3, Failed, t0.Add(3*time.Minute)),
		job("f4", OpApply, 4, Failed, t0.Add(4*time.Minute)),
		job("f5", OpApply, 5, Failed, t0.Add(5*time.Minute)),
		job("d1", OpDrift, 1, Failed, t0),
		job("s1", OpApply, 1, Succeeded, t0),
	}
	if got := Attempt(jobs, OpApply); got != 6 {
		t.Errorf("Attempt = %d, want 6", got)
	}
	for _, j := range jobs {
		if j.Labels[OpLabel] == "apply" && OutcomeOf(&j) == Failed && j.Labels[AttemptLabel] == "6" {
			t.Error("retry collides with a retained failed Job")
		}
	}
	if Attempt(jobs, OpDestroy) != 1 || Attempt(jobs, OpDrift) != 2 {
		t.Error("Attempt per op")
	}
	// Only a succeeded a1 retained: a repeat apply with the same inputs
	// must not reuse its name.
	onlySuccess := []batchv1.Job{job("s1", OpApply, 1, Succeeded, t0)}
	next := Attempt(onlySuccess, OpApply)
	s := spec(OpApply)
	s.Attempt = 1
	retained := s.Name()
	s.Attempt = next
	if next != 2 || s.Name() == retained {
		t.Errorf("Attempt after a retained success = %d (name %s)", next, s.Name())
	}
	if _, ok := Active(jobs); ok {
		t.Error("Active with no running Job")
	}
	jobs = append(jobs, job("r1", OpApply, 6, Running, t0.Add(6*time.Minute)), job("r0", OpApply, 7, Running, t0.Add(time.Minute)))
	if a, ok := Active(jobs); !ok || a.Name != "r1" {
		t.Errorf("Active = %v", a)
	}

	var objs []client.Object
	for i := range jobs {
		objs = append(objs, &jobs[i])
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	r := NewRunner(c, c)
	one, zero := int32(1), int32(0)
	if err := Prune(context.Background(), r, jobs, &zero, &one); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	var left batchv1.JobList
	if err := c.List(context.Background(), &left); err != nil {
		t.Fatalf("List: %v", err)
	}
	var names []string
	for _, j := range left.Items {
		names = append(names, j.Name)
	}
	slices.Sort(names)
	// Per op: the newest failed apply (f5) and drift (d1), the newest
	// successful apply (s1) despite a limit of 0, and both running Jobs.
	if !slices.Equal(names, []string{"d1", "f5", "r0", "r1", "s1"}) {
		t.Errorf("after Prune: %v", names)
	}
	if err := Prune(context.Background(), r, left.Items, nil, nil); err != nil {
		t.Errorf("Prune with defaults: %v", err)
	}
}

// TestPrunePerOp shows limits apply per op; the newest success of each op,
// and its newest failure while no success is newer, survive any limit.
func TestPrunePerOp(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	zero, one := int32(0), int32(1)
	for _, tt := range []struct {
		name               string
		jobs               []batchv1.Job
		successful, failed *int32
		want               []string
	}{
		{
			// Refresh successes no longer push out the last successful
			// apply, so the older failed apply cannot pose as the latest.
			name: "refreshes do not evict the apply",
			jobs: []batchv1.Job{
				job("apply-f", OpApply, 1, Failed, at(0)),
				job("apply-s", OpApply, 2, Succeeded, at(1)),
				job("ref-1", OpRefresh, 1, Succeeded, at(2)),
				job("ref-2", OpRefresh, 2, Succeeded, at(3)),
				job("ref-3", OpRefresh, 3, Succeeded, at(4)),
				job("ref-4", OpRefresh, 4, Succeeded, at(5)),
			},
			want: []string{"apply-f", "apply-s", "ref-2", "ref-3", "ref-4"},
		},
		{
			// failedJobsHistoryLimit 0 keeps the newest unresolved failure,
			// so backoff still sees it.
			name:   "failed limit 0 keeps the unresolved failure",
			failed: &zero,
			jobs: []batchv1.Job{
				job("apply-s", OpApply, 1, Succeeded, at(0)),
				job("apply-f1", OpApply, 2, Failed, at(1)),
				job("apply-f2", OpApply, 3, Failed, at(2)),
			},
			want: []string{"apply-f2", "apply-s"},
		},
		{
			name:   "failed limit 0 drops a resolved failure",
			failed: &zero,
			jobs: []batchv1.Job{
				job("apply-f", OpApply, 1, Failed, at(0)),
				job("apply-s", OpApply, 2, Succeeded, at(1)),
			},
			want: []string{"apply-s"},
		},
		{
			name:       "per-op limits",
			successful: &one,
			failed:     &one,
			jobs: []batchv1.Job{
				job("apply-s1", OpApply, 1, Succeeded, at(0)),
				job("apply-s2", OpApply, 2, Succeeded, at(1)),
				job("drift-f1", OpDrift, 1, Failed, at(2)),
				job("drift-f2", OpDrift, 2, Failed, at(3)),
				job("drift-s", OpDrift, 3, Succeeded, at(4)),
			},
			want: []string{"apply-s2", "drift-f2", "drift-s"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			objs := make([]client.Object, 0, len(tt.jobs))
			for i := range tt.jobs {
				objs = append(objs, &tt.jobs[i])
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
			if err := Prune(context.Background(), NewRunner(c, c), tt.jobs, tt.successful, tt.failed); err != nil {
				t.Fatalf("Prune: %v", err)
			}
			var left batchv1.JobList
			if err := c.List(context.Background(), &left); err != nil {
				t.Fatalf("List: %v", err)
			}
			var names []string
			for _, j := range left.Items {
				names = append(names, j.Name)
			}
			slices.Sort(names)
			if !slices.Equal(names, tt.want) {
				t.Errorf("after Prune: %v, want %v", names, tt.want)
			}
		})
	}
}

// TestRunner exercises the client-backed Runner end to end: Create sets
// the controller reference, List and Pods find what was created, Delete is
// idempotent, and a second Create with the same Job (and a now-stale
// resourceVersion) fails.
func TestRunner(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	owner := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Name: "prod-md-0-abcde", Namespace: "team-a", UID: "uid-1"}}
	j, _ := Build(spec(OpApply), "runner:img")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: j.Name + "-x7k2p", Namespace: "team-a", Labels: map[string]string{batchv1.JobNameLabel: j.Name}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, pod).Build()
	r := NewRunner(c, c)
	ctx := context.Background()
	if err := r.Create(ctx, owner, j); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ref := metav1.GetControllerOf(j); ref == nil || ref.UID != "uid-1" {
		t.Errorf("controller ref = %v", ref)
	}
	list, err := r.List(ctx, owner, state.KindTerraformMachine)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %d (err %v)", len(list), err)
	}
	pods, err := r.Pods(ctx, j)
	if err != nil || len(pods) != 1 {
		t.Errorf("Pods = %d (err %v)", len(pods), err)
	}
	for range 2 {
		if err := r.Delete(ctx, j); err != nil {
			t.Errorf("Delete: %v", err)
		}
	}
	if err := r.Create(ctx, owner, j); err == nil {
		t.Error("Create with a stale resourceVersion succeeded")
	}
}

// podWith returns a fixture pod whose source container has msg as its
// termination message (when not "") and imageID as its ImageID, and whose
// runner init container has one waiting status per reason in waiting.
func podWith(msg, imageID string, waiting ...string) *corev1.Pod {
	p := &corev1.Pod{}
	cs := corev1.ContainerStatus{Name: SourceContainer, ImageID: imageID}
	if msg != "" {
		cs.State.Terminated = &corev1.ContainerStateTerminated{Message: msg}
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "sidecar"}, cs}
	for _, w := range waiting {
		p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, corev1.ContainerStatus{Name: RunnerContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: w}}})
	}
	return p
}

// TestParseResult checks ParseResult against a valid success document, a
// valid error document, and pods whose termination message is missing,
// absent, truncated, garbage, or the wrong version.
func TestParseResult(t *testing.T) {
	t.Parallel()
	valid := `{"version":1,"op":"drift","image":{"ref":"r:v1","providersMirror":true},"runtime":{"command":["/captf/runtime"],"version":"1.16.4"},` +
		`"steps":[{"name":"init","exit":0,"seconds":4.1},{"name":"plan","exit":2,"seconds":9.8}],` +
		`"drift":{"detected":true,"create":0,"update":1,"replace":0,"delete":0,"resources":["module.role.x"]},"error":null}`
	r, err := ParseResult(podWith(valid, ""))
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	if r.Op != "drift" || !r.Image.ProvidersMirror || r.Runtime.Version != "1.16.4" || len(r.Steps) != 2 || r.Steps[1].Exit != 2 || !r.Drift.Detected || r.Error != nil {
		t.Errorf("Result = %+v", r)
	}
	failed := `{"version":1,"op":"apply","image":{"ref":"r"},"runtime":{},"steps":[],"drift":null,"error":{"kind":"image-layout","step":null,"tail":"no module"}}`
	if r, err := ParseResult(podWith(failed, "")); err != nil || r.Error.Kind != "image-layout" || r.Error.Step != nil {
		t.Errorf("error result = %+v (err %v)", r, err)
	}
	cases := []struct {
		name string
		pod  *corev1.Pod
		want error
	}{
		{"no status", &corev1.Pod{}, ErrNoResult},
		{"not terminated", podWith("", ""), ErrNoResult},
		{"truncated", podWith(`{"version":1,"op":"apply","steps":[`+strings.Repeat(`{"name":"x","exit":0,"seconds":1},`, 200), ""), ErrResultTruncated},
		{"garbage", podWith("panic: boom", ""), ErrResultInvalid},
		{"wrong version", podWith(`{"version":2}`, ""), ErrResultInvalid},
	}
	for _, c := range cases {
		msg := ""
		if cs, ok := sourceStatus(c.pod); ok && cs.State.Terminated != nil {
			msg = cs.State.Terminated.Message
			if c.want == ErrResultTruncated {
				cs.State.Terminated.Message = msg[:4096]
				c.pod.Status.ContainerStatuses[1] = cs
			}
		}
		if _, err := ParseResult(c.pod); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

// TestImageDigestAndPullFailed checks ImageDigest's repository-pinning
// rules (docker-pullable prefix, an imageID naming a mirror repository, a
// bare config ID, a tag-only imageID) and PullFailed's waiting-reason
// match.
func TestImageDigestAndPullFailed(t *testing.T) {
	t.Parallel()
	d := strings.Repeat("a", 64)
	withSpecImage := func(image, imageID string) *corev1.Pod {
		p := podWith("", imageID)
		if image != "" {
			p.Spec.Containers = []corev1.Container{{Name: SourceContainer, Image: image}}
		}
		return p
	}
	cases := []struct {
		name    string
		image   string
		imageID string
		want    string
	}{
		{"docker-pullable prefix", "ghcr.io/x/m:v1", "docker-pullable://ghcr.io/x/m@sha256:" + d, "ghcr.io/x/m@sha256:" + d},
		{"imageID already repo@digest", "ghcr.io/x/m:v1", "ghcr.io/x/m@sha256:" + d, "ghcr.io/x/m@sha256:" + d},
		{"spec image already pinned", "ghcr.io/x/m@sha256:" + d, "ghcr.io/x/m@sha256:" + d, "ghcr.io/x/m@sha256:" + d},
		{"host:port and tag in spec image", "registry:5000/ns/m:v1", "registry:5000/ns/m@sha256:" + d, "registry:5000/ns/m@sha256:" + d},
		// The CRI resolved a mirror that pulled the same content under a
		// different repository: pin the spec's own repository, not the
		// imageID's.
		{"imageID names a different repository", "ghcr.io/x/m:v1", "mirror.internal/x/m@sha256:" + d, "ghcr.io/x/m@sha256:" + d},
		{"bare config id, no repository at all", "ghcr.io/x/m:v1", "sha256:" + d, ""},
		{"tag-only imageID", "ghcr.io/x/m:v1", "ghcr.io/x/m:v1", ""},
		{"not a valid digest", "ghcr.io/x/m:v1", "ghcr.io/x/m@sha256:" + strings.Repeat("A", 64), ""},
		{"empty imageID", "ghcr.io/x/m:v1", "", ""},
		{"no image in the pod spec", "", "ghcr.io/x/m@sha256:" + d, ""},
	}
	for _, c := range cases {
		got, ok := ImageDigest(withSpecImage(c.image, c.imageID))
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s: ImageDigest(image=%q, imageID=%q) = %q, %v; want %q", c.name, c.image, c.imageID, got, ok, c.want)
		}
	}
	if _, ok := ImageDigest(&corev1.Pod{}); ok {
		t.Error("ImageDigest on a pod without a source status")
	}
	for reason, want := range map[string]bool{"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true, "ContainerCreating": false} {
		if got := PullFailed(podWith("", "", reason)); got != want {
			t.Errorf("PullFailed(%s) = %v", reason, got)
		}
	}
}

// TestArgsParseInRunner keeps Build's arguments and the runner's flag set in
// step: every argument Build emits must parse, with the intended values.
func TestArgsParseInRunner(t *testing.T) {
	t.Parallel()
	s := spec(OpDrift)
	s.ForceUnlockID = "lock-1"
	s.Policy.LockTimeoutSeconds = new(int32(90))
	j, _ := Build(s, "runner:img")
	o, result, err := app.ParseRunFlags(j.Spec.Template.Spec.Containers[0].Args)
	if err != nil {
		t.Fatalf("runner rejects Build's arguments: %v", err)
	}
	if o.Op != "drift" || !slices.Equal(o.Bin, []string{DefaultBin}) || o.Image != s.ImageRef || o.ForceUnlockID != "lock-1" ||
		o.LockTimeout != 90*time.Second || len(o.BackendConfig) != 4 || result != app.DefaultResultPath {
		t.Errorf("parsed = %+v, result %s", o, result)
	}
	if !strings.HasPrefix(o.BackendConfig[3], `labels={"captf.infrastructure.cluster.x-k8s.io/owner-kind"="TerraformMachine"`) {
		t.Errorf("labels = %s", o.BackendConfig[3])
	}
	ref, err := runner.ParseObjectRef(o.EventObject)
	want := runner.ObjectRef{APIVersion: infrav1.GroupVersion.String(), Kind: "TerraformMachine", Namespace: "team-a", Name: s.OwnerName, UID: string(s.OwnerUID)}
	if err != nil || ref != want || o.JobName != j.Name {
		t.Errorf("event object = %+v (%v), job %q; want %+v, %q", ref, err, o.JobName, want, j.Name)
	}
	// --runner-events=false, or no owner UID: the runner emits nothing.
	for _, mut := range []func(*Spec){func(s *Spec) { s.Events = false }, func(s *Spec) { s.OwnerUID = "" }} {
		q := spec(OpApply)
		mut(&q)
		j, _ := Build(q, "runner:img")
		for _, a := range j.Spec.Template.Spec.Containers[0].Args {
			if strings.HasPrefix(a, "--event-object") || strings.HasPrefix(a, "--job-name") {
				t.Errorf("events off, but the Job passes %s", a)
			}
		}
	}
}

// TestBuildSafeToEvict checks that only the Jobs that mutate infrastructure
// carry the cluster-autoscaler safe-to-evict=false pod annotation.
func TestBuildSafeToEvict(t *testing.T) {
	t.Parallel()
	for op, want := range map[Op]bool{OpApply: true, OpDestroy: true, OpRestore: true, OpPlan: false, OpRefresh: false, OpDrift: false} {
		job, _ := Build(spec(op), "runner:img")
		got, ok := job.Spec.Template.Annotations[SafeToEvictAnnotation]
		if ok != want || (ok && got != "false") {
			t.Errorf("%s: annotation = %q (present %v), want present %v", op, got, ok, want)
		}
	}
}

// TestBuildReservedEnvWins checks that user env named like a variable the
// Job sets itself is dropped, so the built-in value is the only one.
func TestBuildReservedEnvWins(t *testing.T) {
	t.Parallel()
	s := spec(OpApply)
	s.Policy.Env = []corev1.EnvVar{
		{Name: "HOME", Value: "/evil"}, {Name: "TMPDIR", Value: "/evil"},
		{Name: "KUBERNETES_SERVICE_HOST", Value: "evil"}, {Name: "CHECKPOINT_DISABLE", Value: "0"},
		{Name: "AWS_REGION", Value: "x"},
	}
	job, dropped := Build(s, "runner:img")
	if len(dropped) != 4 {
		t.Errorf("dropped = %v, want 4 names", dropped)
	}
	counts := map[string]int{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		counts[e.Name]++
		if e.Value == "evil" || (e.Name == "CHECKPOINT_DISABLE" && e.Value != "1") {
			t.Errorf("env %s = %q overrides the built-in", e.Name, e.Value)
		}
	}
	if counts["HOME"] != 1 || counts["TMPDIR"] != 1 || counts["AWS_REGION"] != 1 || counts["KUBERNETES_SERVICE_HOST"] != 0 {
		t.Errorf("env counts = %v", counts)
	}
}
