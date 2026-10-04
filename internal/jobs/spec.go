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

package jobs

import (
	"cmp"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/plankey"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// BackoffLimit is every Job's pod retry limit: the controller owns retries.
const BackoffLimit int32 = 0

// Defaults of JobPolicy fields.
const (
	DefaultActiveDeadlineSeconds int64 = 3600
	DefaultLockTimeoutSeconds    int32 = 300
)

// Fixed resources. The runner init container only copies one static binary
// and never varies with the module; its requests/limits are not
// configurable. The main container's defaults apply only when
// spec.jobs.resources is unset: BestEffort is exactly wrong for a Terraform
// process with several large providers, the first thing OOM-killed or
// evicted in a crowded node. No default CPU limit: throttling a slow apply
// is worse than a slow apply.
const (
	InitContainerCPURequest    = "10m"
	InitContainerCPULimit      = "100m"
	InitContainerMemoryRequest = "32Mi"
	InitContainerMemoryLimit   = "64Mi"

	DefaultSourceCPURequest    = "250m"
	DefaultSourceMemoryRequest = "512Mi"
	DefaultSourceMemoryLimit   = "2Gi"
)

// TerminationGracePeriodSeconds is the Job pod's grace period. A deletion,
// drain or activeDeadlineSeconds sends the runner SIGTERM; it interrupts
// the runtime, which finishes in-flight provider calls (a VM being
// created), writes the state and releases the lock. The Kubernetes default
// of 30 s would SIGKILL it mid-call: resources created but never recorded.
// The runner's --stop-timeout is this less stopMargin, which leaves time to
// write the result.
const (
	TerminationGracePeriodSeconds int64 = 600
	stopMargin                    int64 = 30
)

// Fixed paths and names of the Job pod.
const (
	RunnerContainer       = "runner"
	SourceContainer       = "source"
	RunnerBinDir          = "/captf/bin"
	RunnerBin             = "/captf/bin/runner"
	ConfigDir             = "/captf/config"
	CredentialsDir        = "/var/run/captf/credentials" // #nosec G101 -- a mount path, not a credential
	TmpDir                = "/tmp"
	DefaultBin            = render.RuntimePath
	runnerUID       int64 = 65532
	credsMode       int32 = 0o440
	planKeyVolume         = "plan-key"
)

// Spec is everything Build needs for one Job.
type Spec struct {
	// OwnerKind and OwnerName identify the Terraform* object; Namespace is
	// its namespace. Runner.Create sets the controller ownerReference.
	OwnerKind   string
	OwnerName   string
	Namespace   string
	ClusterName string
	KindShort   string

	Op         Op
	Attempt    int32
	InputsHash string
	DriftTick  string

	// ImageRef is spec.source.image, or repo@digest when pinned.
	ImageRef   string
	PullPolicy corev1.PullPolicy

	ServiceAccount string
	CredsSecret    string
	Policy         infrav1.JobPolicy

	// PlanKeySecret names the Secret holding the plan fingerprint key
	// (internal/plankey). When set, a plan or apply Job mounts it at
	// plankey.MountPath and passes the runner --plan-key-file; other ops
	// never see the key.
	PlanKeySecret string

	// Suffix and BackendLabels complete the kubernetes backend.
	Suffix        string
	BackendLabels map[string]string
	// ForceUnlockID, when set, makes the runner force-unlock this stale
	// lock after init: force-unlock needs backend initialization
	// (internal/runner.Steps).
	ForceUnlockID string
	// AllowDeletesHash is the hash approved for a destructive plan (the
	// object's captf.io/approve-destructive-plan annotation); only a
	// guarded apply passes it on (guardsDeletes).
	AllowDeletesHash string
	// ApprovalHash, when set, guards a TerraformMachinePool apply that
	// renders a change of the cluster's exports: the runner then stops
	// before a destructive plan unless AllowDeletesHash is ApprovalHash
	// (the inputs hash without bootstrap_data), passed as its
	// --inputs-hash. "" for every other Job.
	ApprovalHash string
	// ExpectPlan is the approved plan hash (a TerraformCluster's
	// captf.io/approve-plan annotation) an apply under applyPolicy Manual
	// must plan again before it applies; only a cluster apply passes it on.
	ExpectPlan string

	// Restore is the backup a restore Job pushes; nil for other ops.
	Restore *Restore

	// Events makes the runner report its progress as events about the
	// owner (--event-object, --job-name); OwnerUID completes the owner's
	// reference and must be set with it. The manager's --runner-events.
	Events   bool
	OwnerUID types.UID
}

// Restore is the state backup a restore Job pushes.
type Restore struct {
	// Serial is the backup's state serial.
	Serial int64
	// Secrets are the backup's chunk Secrets in chunk order; the config
	// volume projects each one's tfstate key as RestoreChunkDir/<index>.
	Secrets []string
	// ManagedResources is the backup's managed resource count: the runner
	// fails a restore whose state list shows none when this is not 0.
	ManagedResources int
}

// EventObject returns the runner's --event-object value for s: the owning
// Terraform* object as <apiVersion>/<kind>/<namespace>/<name>/<uid>.
func (s Spec) EventObject() string {
	return strings.Join([]string{infrav1.GroupVersion.String(), s.OwnerKind, s.Namespace, s.OwnerName, string(s.OwnerUID)}, "/")
}

// guardsDeletes reports whether the Job's apply stops before a plan that
// deletes or replaces resources: every apply of the cluster role, and a
// pool apply that renders a change of the cluster's exports (ApprovalHash
// set). A pool's other changes stay unguarded: the contract requires a
// bootstrap_data change to update in place and a kubernetes_version change
// to roll the instances. A machine's instance is immutable and replaced by
// CAPI, not by its apply.
func (s Spec) guardsDeletes() bool {
	return s.Op == OpApply && (s.OwnerKind == state.KindTerraformCluster || s.ApprovalHash != "")
}

// guardHash returns the hash the runner's guard compares the approval
// with (--inputs-hash): ApprovalHash when set, else InputsHash.
func (s Spec) guardHash() string {
	if s.ApprovalHash != "" {
		return s.ApprovalHash
	}
	return s.InputsHash
}

// Name returns the Job's name.
func (s Spec) Name() string {
	return Name(s.KindShort, s.OwnerName, s.Op, s.Attempt, Hash6(s.InputsHash, s.Op, s.Attempt, s.DriftTick))
}

// reservedEnv reports whether a jobs.env name is owned by the runner.
func reservedEnv(name string) bool {
	return strings.HasPrefix(name, "TF_") || strings.HasPrefix(name, "KUBE_")
}

// Build returns the Job for s, running runnerImage as the init container,
// and the jobs.env names it dropped because they would override the TF_*
// or KUBE_* variables the runner relies on (report them as an event).
// TF_CLI_CONFIG_FILE is not set here: only the runner can see whether the
// image has a provider mirror, so it sets that variable itself.
func Build(s Spec, runnerImage string) (*batchv1.Job, []string) {
	name := s.Name()
	labels := Labels(s.OwnerKind, s.OwnerName, s.ClusterName, s.Op, s.Attempt)

	env := []corev1.EnvVar{
		{Name: "TF_IN_AUTOMATION", Value: "1"},
		{Name: "TF_INPUT", Value: "0"},
		// TF_DATA_DIR is not set here: the runner always forces its own
		// (internal/runner.Prepare), and setting it here too only made
		// Prepare drop and warn about it on every Job.
		{Name: "HOME", Value: render.WorkDir},
		{Name: "TMPDIR", Value: TmpDir},
		{Name: "KUBE_NAMESPACE", Value: s.Namespace},
		// Terraform calls checkpoint-api.hashicorp.com on every command
		// otherwise, from a pod holding cloud credentials, and can stall on
		// its timeout when a namespace drops egress silently.
		{Name: "CHECKPOINT_DISABLE", Value: "1"},
	}
	var dropped []string
	for _, e := range s.Policy.Env {
		if reservedEnv(e.Name) {
			dropped = append(dropped, e.Name)
			continue
		}
		env = append(env, e)
	}

	deadline := cmp.Or(s.Policy.ActiveDeadlineSeconds, DefaultActiveDeadlineSeconds)
	resources := defaultSourceResources()
	if s.Policy.Resources != nil {
		resources = *s.Policy.Resources
	}

	// An apply Job records the inputs hash it renders, so the controller can
	// adopt the state with that hash once the Job succeeded.
	// A restore Job records the backup's serial and the inputs hash the
	// backup was taken with, which the controller adopts after it. A plan
	// Job records the inputs hash it planned, which status.plan names.
	var annotations map[string]string
	if (s.Op == OpApply || s.Op == OpRestore || s.Op == OpPlan) && s.InputsHash != "" {
		annotations = map[string]string{state.InputsHashAnnotation: s.InputsHash}
	}
	if s.Op == OpRestore && s.Restore != nil {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[RestoreSerialAnnotation] = strconv.FormatInt(s.Restore.Serial, 10)
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace, Labels: labels, Annotations: annotations},
		Spec: batchv1.JobSpec{
			// No pod retries and no TTL: the controller owns retries (the
			// attempt is in the Job name) and derives backoff, digest pinning
			// and conditions from retained Jobs, which only Prune deletes.
			BackoffLimit:          new(BackoffLimit),
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: maps.Clone(labels)},
				Spec: corev1.PodSpec{
					ServiceAccountName:            s.ServiceAccount,
					RestartPolicy:                 corev1.RestartPolicyNever,
					TerminationGracePeriodSeconds: new(TerminationGracePeriodSeconds),
					ImagePullSecrets:              pullSecrets(s.Policy.ImagePullSecrets),
					SecurityContext:               podSecurityContext(s.Policy.PodSecurityContext),
					Volumes:                       volumes(inputs.RunName(name), s.CredsSecret, s.restoreSecrets(), s.planKeySecret()),
					InitContainers: []corev1.Container{{
						Name:            RunnerContainer,
						Image:           runnerImage,
						Command:         []string{"/runner", "copy", RunnerBin},
						SecurityContext: runnerSecurityContext(),
						Resources:       initContainerResources(),
						VolumeMounts:    []corev1.VolumeMount{{Name: "runner", MountPath: RunnerBinDir}},
					}},
					Containers: []corev1.Container{{
						Name:                     SourceContainer,
						Image:                    s.ImageRef,
						ImagePullPolicy:          s.PullPolicy,
						Command:                  []string{RunnerBin, "run"},
						Args:                     args(s),
						Env:                      env,
						EnvFrom:                  []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: s.CredsSecret}}}},
						SecurityContext:          sourceSecurityContext(s.Policy.SecurityContext),
						Resources:                resources,
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						VolumeMounts:             s.sourceMounts(),
					}},
				},
			},
		},
	}
	return job, dropped
}

// args returns the runner flags for s (the 3.07 contract).
func args(s Spec) []string {
	lockTimeout := DefaultLockTimeoutSeconds
	if s.Policy.LockTimeoutSeconds != nil {
		lockTimeout = *s.Policy.LockTimeoutSeconds
	}
	a := []string{
		"--op=" + string(s.Op),
		"--bin=" + DefaultBin,
		"--image=" + s.ImageRef,
		"--module=" + render.ModuleDir,
		"--providers=" + render.ProvidersDir,
		"--workdir=" + render.WorkDir,
		"--config=" + ConfigDir,
		"--lock-timeout=" + strconv.Itoa(int(lockTimeout)) + "s",
		"--stop-timeout=" + strconv.FormatInt(TerminationGracePeriodSeconds-stopMargin, 10) + "s",
		"--backend-config=secret_suffix=" + s.Suffix,
		"--backend-config=namespace=" + s.Namespace,
		"--backend-config=in_cluster_config=true",
		"--backend-config=labels=" + HCLMap(s.BackendLabels),
	}
	if s.ForceUnlockID != "" {
		a = append(a, "--force-unlock="+s.ForceUnlockID)
	}
	if s.guardsDeletes() {
		a = append(a, "--guard-deletes", "--inputs-hash="+s.guardHash())
		if s.AllowDeletesHash != "" {
			a = append(a, "--allow-deletes-hash="+s.AllowDeletesHash)
		}
		if s.ExpectPlan != "" {
			a = append(a, "--expect-plan="+s.ExpectPlan)
		}
	}
	if secrets := s.restoreSecrets(); len(secrets) > 0 {
		a = append(a, "--restore-chunks="+strconv.Itoa(len(secrets)),
			"--restore-resources="+strconv.Itoa(s.Restore.ManagedResources))
	}
	if s.planKeySecret() != "" {
		a = append(a, "--plan-key-file="+path.Join(plankey.MountPath, plankey.KeyFile))
	}
	if s.Events && s.OwnerUID != "" {
		a = append(a, "--event-object="+s.EventObject(), "--job-name="+s.Name())
	}
	return a
}

// planKeySecret returns the plan key Secret s mounts: PlanKeySecret for a
// plan or apply Job, otherwise "".
func (s Spec) planKeySecret() string {
	if s.Op != OpPlan && s.Op != OpApply {
		return ""
	}
	return s.PlanKeySecret
}

// sourceMounts returns the main container's volume mounts for s.
func (s Spec) sourceMounts() []corev1.VolumeMount {
	m := []corev1.VolumeMount{
		{Name: "runner", MountPath: RunnerBinDir, ReadOnly: true},
		{Name: "work", MountPath: render.WorkDir},
		{Name: "tmp", MountPath: TmpDir},
		{Name: "config", MountPath: ConfigDir, ReadOnly: true},
		{Name: "creds", MountPath: CredentialsDir, ReadOnly: true},
	}
	if s.planKeySecret() != "" {
		m = append(m, corev1.VolumeMount{Name: planKeyVolume, MountPath: plankey.MountPath, ReadOnly: true})
	}
	return m
}

// restoreSecrets returns the backup chunks a restore Job mounts for s; nil
// for other ops.
func (s Spec) restoreSecrets() []string {
	if s.Op != OpRestore || s.Restore == nil {
		return nil
	}
	return s.Restore.Secrets
}

// HCLMap returns m rendered as an HCL object constructor with sorted,
// quoted keys: {"a"="b","c"=""}. The CLI parses -backend-config=labels=<this>
// as an HCL expression.
func HCLMap(m map[string]string) string {
	keys := slices.Sorted(maps.Keys(m))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", strconv.Quote(k), strconv.Quote(m[k])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// pullSecrets returns refs (jobs.imagePullSecrets), in order, without empty
// names or duplicates: they must cover the source image and the runner
// image.
func pullSecrets(refs []corev1.LocalObjectReference) []corev1.LocalObjectReference {
	var out []corev1.LocalObjectReference
	for _, r := range refs {
		if r.Name != "" && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// volumes returns the pod's volumes for the per-run Secret runSecret, the
// credentials Secret credsSecret and, for a restore Job, its backup chunk
// Secrets restore, and, when planKey is not empty, the plan key Secret
// (only its key file, mounted 0440). A restore Job's config volume projects the per-run
// Secret next to the backup's chunks, under RestoreChunkDir: a chunked
// state (the backend chunks above ~1 MiB compressed) cannot fit a single
// per-run Secret.
func volumes(runSecret, credsSecret string, restore []string, planKey string) []corev1.Volume {
	mode := credsMode
	empty := func(name string) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	}
	config := corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: runSecret, DefaultMode: &mode}}
	if len(restore) > 0 {
		sources := []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: runSecret}}}}
		for i, name := range restore {
			sources = append(sources, corev1.VolumeProjection{Secret: &corev1.SecretProjection{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
				Items:                []corev1.KeyToPath{{Key: state.DataKey, Path: RestoreChunkDir + "/" + strconv.Itoa(i)}},
			}})
		}
		config = corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: sources, DefaultMode: &mode}}
	}
	vols := []corev1.Volume{
		empty("runner"),
		empty("work"),
		empty("tmp"),
		{Name: "config", VolumeSource: config},
		{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: credsSecret, DefaultMode: &mode}}},
	}
	if planKey != "" {
		vols = append(vols, corev1.Volume{Name: planKeyVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  planKey,
			DefaultMode: &mode,
			Items:       []corev1.KeyToPath{{Key: plankey.KeyFile, Path: plankey.KeyFile}},
		}}})
	}
	return vols
}

// podSecurityContext returns user, or a zero value when user is nil, with
// seccomp defaulted to RuntimeDefault; runAsNonRoot is not defaulted,
// because images built FROM hashicorp/terraform run as root.
func podSecurityContext(user *corev1.PodSecurityContext) *corev1.PodSecurityContext {
	sc := &corev1.PodSecurityContext{}
	if user != nil {
		sc = user.DeepCopy()
	}
	if sc.SeccompProfile == nil {
		sc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}
	if sc.FSGroup == nil {
		// The credential files are 0440: a non-root image user reads them
		// through this group. Defaulting it only under runAsNonRoot
		// missed every image that runs non-root without setting that field
		// itself (USER 65532 with no pod-level runAsNonRoot, or a
		// container-level securityContext): the docs' own recommended
		// setup. fsGroup on a root-run pod is a no-op, so defaulting it
		// unconditionally is safe.
		sc.FSGroup = new(runnerUID)
	}
	return sc
}

// initContainerResources returns the runner init container's fixed
// requests/limits: it only copies one static binary, never varies with the
// module, and is not configurable.
func initContainerResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(InitContainerCPURequest),
			corev1.ResourceMemory: resource.MustParse(InitContainerMemoryRequest),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(InitContainerCPULimit),
			corev1.ResourceMemory: resource.MustParse(InitContainerMemoryLimit),
		},
	}
}

// defaultSourceResources returns the main container's resources when
// spec.jobs.resources is unset: no default CPU limit.
func defaultSourceResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(DefaultSourceCPURequest),
			corev1.ResourceMemory: resource.MustParse(DefaultSourceMemoryRequest),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse(DefaultSourceMemoryLimit),
		},
	}
}

// runnerSecurityContext returns the init container's fixed, locked-down
// security context: no privilege escalation, every capability dropped,
// forced non-root at runnerUID, and a read-only root filesystem.
func runnerSecurityContext() *corev1.SecurityContext {
	uid := runnerUID
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		RunAsNonRoot:             new(true),
		RunAsUser:                &uid,
		ReadOnlyRootFilesystem:   new(true),
	}
}

// sourceSecurityContext returns the defaults overlaid with every field the
// user set in jobs.securityContext. Capabilities.drop always includes ALL,
// whatever the user's capabilities object holds.
func sourceSecurityContext(user *corev1.SecurityContext) *corev1.SecurityContext {
	sc := &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		ReadOnlyRootFilesystem:   new(true),
	}
	if user == nil {
		return sc
	}
	u := user.DeepCopy()
	if u.Capabilities != nil {
		// A user capabilities object never loses the default drop of ALL.
		sc.Capabilities = u.Capabilities
		if !slices.Contains(sc.Capabilities.Drop, "ALL") {
			sc.Capabilities.Drop = append([]corev1.Capability{"ALL"}, sc.Capabilities.Drop...)
		}
	}
	if u.Privileged != nil {
		sc.Privileged = u.Privileged
	}
	if u.SELinuxOptions != nil {
		sc.SELinuxOptions = u.SELinuxOptions
	}
	if u.WindowsOptions != nil {
		sc.WindowsOptions = u.WindowsOptions
	}
	if u.RunAsUser != nil {
		sc.RunAsUser = u.RunAsUser
	}
	if u.RunAsGroup != nil {
		sc.RunAsGroup = u.RunAsGroup
	}
	if u.RunAsNonRoot != nil {
		sc.RunAsNonRoot = u.RunAsNonRoot
	}
	if u.ReadOnlyRootFilesystem != nil {
		sc.ReadOnlyRootFilesystem = u.ReadOnlyRootFilesystem
	}
	if u.AllowPrivilegeEscalation != nil {
		sc.AllowPrivilegeEscalation = u.AllowPrivilegeEscalation
	}
	if u.ProcMount != nil {
		sc.ProcMount = u.ProcMount
	}
	if u.SeccompProfile != nil {
		sc.SeccompProfile = u.SeccompProfile
	}
	if u.AppArmorProfile != nil {
		sc.AppArmorProfile = u.AppArmorProfile
	}
	return sc
}
