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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Source is the deliverable: one OCI image that bundles the role module's
// Terraform/OpenTofu code and the runtime binary. There is no separate module
// source and no separate runtime image. The image layout is a fixed-path
// contract: /captf/module, /captf/runtime and an optional /captf/providers
// mirror. The runner always execs /captf/runtime;
// pull secrets for the image are jobs.imagePullSecrets.
type Source struct {
	// image is the OCI image reference, registry/repo:tag or
	// registry/repo@sha256:digest. The tag or digest is the module version.
	// Referencing an image grants its publisher Secret-read and cloud-credential
	// access in this namespace.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image,omitempty"`

	// imagePullPolicy for the image. Defaults to IfNotPresent, applied when the
	// Job is built; Always is recommended for mutable tags.
	// +optional
	// +kubebuilder:validation:Enum=IfNotPresent;Always;Never
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
}

// JobPolicy tunes the Kubernetes Jobs that run the module. Every field is
// optional. On a TerraformMachine or TerraformMachinePool the policy is
// merged field by field with TerraformCluster.spec.defaults.jobs, then
// with the TerraformCluster's own spec.jobs: a field the machine or pool
// sets wins, an unset one comes from the defaults, then from the
// cluster's own policy, and a field none sets gets the controller's
// built-in default. env is
// merged by name (the machine's or pool's wins on the same name) and
// imagePullSecrets is the union (the machine's or pool's first); resources,
// securityContext and podSecurityContext are replaced as a whole. Defaults
// are resolved at reconcile time and never persisted, so a provider upgrade
// reaches existing objects. Jobs never retry pods (backoffLimit 0) and never
// get a TTL: the controller owns retries and prunes finished Jobs itself.
type JobPolicy struct {
	// successfulJobsHistoryLimit is how many succeeded Jobs to keep per object
	// and operation. The newest succeeded Job of each operation is kept even
	// at 0. Defaults to 3, applied at reconcile.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	SuccessfulJobsHistoryLimit *int32 `json:"successfulJobsHistoryLimit,omitempty"`

	// failedJobsHistoryLimit is how many failed Jobs to keep per object and
	// operation. The newest failed Job of an operation is kept even at 0
	// while no newer Job of that operation succeeded: retry backoff counts
	// it. Defaults to 3, applied at reconcile.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	FailedJobsHistoryLimit *int32 `json:"failedJobsHistoryLimit,omitempty"`

	// activeDeadlineSeconds bounds a Job's run time, in seconds, at most one
	// day. Defaults to 3600, applied at reconcile when unset (0). When a
	// policy sets both, lockTimeoutSeconds must be less than
	// activeDeadlineSeconds.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	ActiveDeadlineSeconds int64 `json:"activeDeadlineSeconds,omitempty"`

	// serviceAccountName overrides the runner ServiceAccount. When unset the
	// controller creates captf-runner, bound to the static captf-runner
	// ClusterRole. An override ServiceAccount must exist and carry the label
	// captf.io/runner=true, or no Job is created.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// lockTimeoutSeconds is passed to the runtime as -lock-timeout, in seconds.
	// Defaults to 300, applied at reconcile.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	LockTimeoutSeconds *int32 `json:"lockTimeoutSeconds,omitempty"`

	// imagePullSecrets for the Job pod: they cover the source image and the
	// runner init image.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=10
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// resources of the main container.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// env adds environment variables to the main container. It cannot override
	// the TF_* and KUBE_* variables the runner sets.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Env []corev1.EnvVar `json:"env,omitempty"`

	// securityContext of the main container. Defaults, applied when the Job is
	// built: seccompProfile RuntimeDefault, capabilities drop ALL,
	// allowPrivilegeEscalation false, readOnlyRootFilesystem true. runAsNonRoot
	// is not defaulted. The webhook rejects privileged: true,
	// allowPrivilegeEscalation: true and any capabilities.add: the container
	// holds cloud credentials.
	// +optional
	SecurityContext *corev1.SecurityContext `json:"securityContext,omitempty"`

	// podSecurityContext of the Job pod. Defaults, applied when the Job is
	// built: seccompProfile RuntimeDefault.
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`
}

// DeletionPolicy is what deleting a TerraformCluster, TerraformMachine or
// TerraformMachinePool does with the infrastructure it manages.
// +kubebuilder:validation:Enum=Destroy;Retain
type DeletionPolicy string

const (
	// DeletionPolicyDestroy runs a destroy Job, then deletes the state, its
	// backups and the durable inputs, and removes the finalizer.
	DeletionPolicyDestroy DeletionPolicy = "Destroy"
	// DeletionPolicyRetain removes the finalizer without a destroy: the
	// infrastructure keeps running. The state Secrets, the state backups
	// and the durable inputs are kept, without owner references and
	// labeled captf.io/retained-from-uid with the object's uid, so a later
	// object of the same kind, namespace and name can adopt them
	// (adoptRetainedState).
	DeletionPolicyRetain DeletionPolicy = "Retain"
)

// DriftAction is what the controller does when a drift check finds changes.
// +kubebuilder:validation:Enum=Report;Remediate
type DriftAction string

const (
	// DriftActionReport records drift in the DriftDetected condition only.
	DriftActionReport DriftAction = "Report"
	// DriftActionRemediate applies the current inputs to remove the drift.
	DriftActionRemediate DriftAction = "Remediate"
)

// DriftPolicy configures periodic drift detection of a TerraformCluster.
type DriftPolicy struct {
	// intervalSeconds between drift checks, in seconds. Defaults to the
	// manager's --drift-default-interval (30m), applied at reconcile. 0
	// disables drift checks, and with them every health sample after
	// provisioning: health is re-read only by a refresh or drift run.
	// +optional
	// +kubebuilder:validation:Minimum=0
	IntervalSeconds *int32 `json:"intervalSeconds,omitempty"`

	// action taken when drift is found: Report or Remediate. Defaults to
	// Report, applied at reconcile. Remediate applies the current inputs
	// automatically, reverting every out-of-band change.
	// +optional
	Action DriftAction `json:"action,omitempty"`
}

// MachineDriftPolicy configures periodic drift detection of a
// TerraformMachine. Drift on a machine is always reported, never remediated:
// the machine is immutable infrastructure, replaced by a rollout.
type MachineDriftPolicy struct {
	// intervalSeconds between drift checks, in seconds. Defaults to the
	// manager's --drift-default-interval (30m), applied at reconcile. 0
	// disables drift checks. Unless remediation.annotateMachine is true
	// (which refreshes at remediation.healthCheckIntervalSeconds), that also
	// stops every health sample after provisioning.
	// +optional
	// +kubebuilder:validation:Minimum=0
	IntervalSeconds *int32 `json:"intervalSeconds,omitempty"`
}

// MachinePoolDriftPolicy configures periodic drift detection of a
// TerraformMachinePool. Unlike MachineDriftPolicy, 0 is rejected by the CRD
// schema: for a pool it is membership refresh
// (TerraformMachinePoolSpec.MembershipRefreshIntervalSeconds), not drift,
// that keeps status fresh (https://captf.io/docs/module-author/contract/v1alpha1/machinepool.html
// "Membership refresh"), and the drift Job itself feeds the refreshed
// replicas into its plan (machinepool.md "Drift order"), so disabling
// drift would also stop that refresh from ever reaching a plan.
type MachinePoolDriftPolicy struct {
	// intervalSeconds between drift checks, in seconds. Defaults to the
	// manager's --drift-default-interval (30m), applied at reconcile when
	// unset (0). Unlike a machine's or the cluster's, a pool's drift cannot
	// be disabled, so 0 always means "use the default", never "disabled".
	// +optional
	// +kubebuilder:validation:Minimum=1
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

	// action taken when drift is found: Report or Remediate. Defaults to
	// Report, applied at reconcile. Unlike a machine's, a pool's drift may
	// be remediated: the group's instances are not immutable infrastructure.
	// +optional
	Action DriftAction `json:"action,omitempty"`
}

// MachineRemediation configures how a TerraformMachine signals an unhealthy
// instance to Cluster API beyond its Ready condition.
type MachineRemediation struct {
	// annotateMachine sets cluster.x-k8s.io/remediate-machine on the owner
	// Machine once the instance has been unhealthy for unhealthyThreshold
	// consecutive samples, or at once when it is terminated. CAPTF removes
	// the annotation it set once the instance reads Healthy again and the
	// Machine is not being deleted; an annotation set by anyone else is left
	// alone. It has an effect only when a MachineHealthCheck selects the
	// Machine; a single-replica control plane refuses the remediation.
	// Defaults to false.
	// +optional
	AnnotateMachine *bool `json:"annotateMachine,omitempty"`

	// unhealthyThreshold is the number of consecutive unhealthy health samples
	// before the Machine is annotated. A sample is one completed refresh or
	// drift Job. Defaults to 3, applied at reconcile when unset (0). A
	// terminated instance counts on the first sample.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	UnhealthyThreshold int32 `json:"unhealthyThreshold,omitempty"`

	// healthCheckIntervalSeconds is how often a provisioned machine is
	// refreshed to sample its health while annotateMachine is true,
	// independent of drift.intervalSeconds. Defaults to 300, applied at
	// reconcile when unset (0). With annotateMachine false it is ignored and
	// health is re-read only at the drift (or refresh) cadence, so
	// drift.intervalSeconds 0 then stops health sampling after provisioning.
	// +optional
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=86400
	HealthCheckIntervalSeconds int32 `json:"healthCheckIntervalSeconds,omitempty"`
}

// IdentityReference names a cluster-scoped TerraformClusterIdentity.
type IdentityReference struct {
	// name of the TerraformClusterIdentity.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name,omitempty"`
}

// SecretReference names a Secret in a given namespace.
type SecretReference struct {
	// name of the Secret.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name,omitempty"`

	// namespace of the Secret.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace,omitempty"`
}

// VariablesSourceLabel must be set to "true" on every ConfigMap and Secret
// named by spec.variablesFrom. The manager reads and watches only labeled
// sources; an unlabeled one counts as missing.
const VariablesSourceLabel = "captf.io/variables"

// VariablesFormat is how the data values of a variablesFrom source are
// passed to the module.
// +kubebuilder:validation:Enum=String;JSON
type VariablesFormat string

const (
	// VariablesFormatString passes each data value as a string. The module's
	// declared variable type converts it ("3" to a number, "true" to a bool).
	VariablesFormatString VariablesFormat = "String"
	// VariablesFormatJSON parses each data value as JSON, for lists, maps
	// and objects. A value that is not valid JSON is VariablesInvalid.
	VariablesFormatJSON VariablesFormat = "JSON"
)

// VariablesSourceReference names a ConfigMap or Secret in the object's own
// namespace.
type VariablesSourceReference struct {
	// name of the ConfigMap or Secret.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name,omitempty"`
}

// VariablesSource reads module variables from the data of one ConfigMap or
// Secret in the object's namespace: every data key becomes a variable of the
// same name. The source must carry the label captf.io/variables=true.
// Variables from a Secret are declared sensitive in the generated root, so
// Terraform redacts them in plan and apply output; they are still stored in
// the inputs Secrets and in state, like every input.
// +kubebuilder:validation:ExactlyOneOf=configMapRef;secretRef
type VariablesSource struct {
	// configMapRef names a ConfigMap. Exactly one of configMapRef and
	// secretRef is set.
	// +optional
	ConfigMapRef VariablesSourceReference `json:"configMapRef,omitempty,omitzero"`

	// secretRef names a Secret. Exactly one of configMapRef and secretRef is
	// set.
	// +optional
	SecretRef VariablesSourceReference `json:"secretRef,omitempty,omitzero"`

	// optional makes a missing or unlabeled source contribute nothing
	// instead of holding the object at DependenciesReady False
	// (VariablesSourceNotFound). Defaults to false.
	// +optional
	Optional *bool `json:"optional,omitempty"`

	// format of the data values: String passes each value as a string, JSON
	// parses each as JSON. Defaults to String, applied at reconcile.
	// +optional
	Format VariablesFormat `json:"format,omitempty"`
}

// Initialization holds the v1beta2 contract's initialization status.
// +kubebuilder:validation:MinProperties=1
type Initialization struct {
	// provisioned is true once the infrastructure is provisioned: derived from
	// state until it first holds, then latched for the object's life.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// Operation is one of the operations a Job runs.
// +kubebuilder:validation:Enum=apply;destroy;drift;refresh;restore;plan
type Operation string

const (
	// OperationApply creates or updates the infrastructure.
	OperationApply Operation = "apply"
	// OperationDestroy destroys the infrastructure.
	OperationDestroy Operation = "destroy"
	// OperationDrift refreshes state and plans to detect drift.
	OperationDrift Operation = "drift"
	// OperationRefresh refreshes state and outputs only.
	OperationRefresh Operation = "refresh"
	// OperationRestore pushes a state backup back into the backend
	// (RestoreStateAnnotation).
	OperationRestore Operation = "restore"
	// OperationPlan plans a TerraformCluster's change for review and
	// applies nothing (applyPolicy Manual).
	OperationPlan Operation = "plan"
)

// MaxStateBackups caps status.stateBackups.
const MaxStateBackups = 16

// StateBackup is one versioned copy of the object's Terraform state that
// the controller keeps in a captf-state-backup-* Secret.
type StateBackup struct {
	// serial is the state serial the backup holds; set it as the
	// captf.io/restore-state annotation to restore it.
	// +required
	// +kubebuilder:validation:Minimum=1
	Serial int64 `json:"serial,omitempty"`

	// takenAt is when the controller copied the state.
	// +required
	TakenAt *metav1.Time `json:"takenAt,omitempty"`

	// bytes is the compressed size of the backup summed over its Secrets.
	// +required
	// +kubebuilder:validation:Minimum=1
	Bytes int64 `json:"bytes,omitempty"`
}

// ActiveJob identifies the Job currently running for an object.
type ActiveJob struct {
	// name of the Job.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name,omitempty"`

	// operation the Job runs.
	// +required
	Operation Operation `json:"operation,omitempty"`

	// attempt is the operation's Job sequence number, the a<N> in the Job
	// name, starting at 1. It counts every Job of the operation still
	// retained, not retries: the 40th refresh is attempt 40.
	// +required
	// +kubebuilder:validation:Minimum=1
	Attempt int32 `json:"attempt,omitempty"`

	// startTime of the Job.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
}

// RunStep is one runtime command the runner executed.
type RunStep struct {
	// name of the step, e.g. init, validate, plan, apply, apply-refresh-only.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name,omitempty"`

	// exitCode of the step.
	// +required
	ExitCode *int32 `json:"exitCode,omitempty"`

	// durationMilliseconds is the step's wall time, in milliseconds.
	// +optional
	// +kubebuilder:validation:Minimum=0
	DurationMilliseconds *int64 `json:"durationMilliseconds,omitempty"`
}

// RunErrorKind classifies a failed run.
// +kubebuilder:validation:Enum=image-layout;step;interrupted;blocked;plan-changed
type RunErrorKind string

const (
	// RunErrorKindImageLayout means the image does not follow the image contract.
	RunErrorKindImageLayout RunErrorKind = "image-layout"
	// RunErrorKindStep means a runtime step failed.
	RunErrorKindStep RunErrorKind = "step"
	// RunErrorKindInterrupted means the step was stopped from outside (the
	// pod got SIGTERM: a drain, an eviction, a Job deletion or its deadline),
	// not that the module failed.
	RunErrorKindInterrupted RunErrorKind = "interrupted"
	// RunErrorKindBlocked means a TerraformCluster apply, or a
	// TerraformMachinePool apply of a change of the cluster's exports,
	// stopped before a plan that deletes or replaces resources that no
	// approved TerraformPlan covers. Nothing was changed; the plan waits for
	// its approval as a TerraformPlan.
	RunErrorKindBlocked RunErrorKind = "blocked"
	// RunErrorKindPlanChanged means an apply approved for one plan (an
	// approved TerraformPlan) planned other changes and stopped before
	// applying them. Nothing was changed; the plan is Failed, and under
	// applyPolicy Manual the new plan waits for its own approval as a new
	// TerraformPlan.
	RunErrorKindPlanChanged RunErrorKind = "plan-changed"
)

// RunError describes why a run failed.
type RunError struct {
	// kind of failure.
	// +required
	Kind RunErrorKind `json:"kind,omitempty"`

	// step that failed, for kind step.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Step string `json:"step,omitempty"`

	// summary is the runner's short description of the failure, at most 512
	// bytes. It is not raw stderr: status is readable by everyone who can get
	// the object, so the full output stays in the Job's logs.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Summary string `json:"summary,omitempty"`
}

// DriftSummary summarizes the plan of a drift run that found changes.
// +kubebuilder:validation:MinProperties=1
type DriftSummary struct {
	// create is the number of resources the plan would create.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Create *int32 `json:"create,omitempty"`

	// update is the number of resources the plan would update in place.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Update *int32 `json:"update,omitempty"`

	// replace is the number of resources the plan would replace: delete and
	// create again. A replacement counts here only.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Replace *int32 `json:"replace,omitempty"`

	// delete is the number of resources the plan would delete, not counting
	// replacements.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Delete *int32 `json:"delete,omitempty"`

	// resources are the addresses of the drifted resources, at most 20.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=512
	Resources []string `json:"resources,omitempty"`
}

// LastRun is the result of the most recent completed Job, copied from the
// runner's termination message.
type LastRun struct {
	// job is the name of the Job.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Job string `json:"job,omitempty"`

	// operation the Job ran.
	// +required
	Operation Operation `json:"operation,omitempty"`

	// steps the runner executed, in order.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Steps []RunStep `json:"steps,omitempty"`

	// error is set when the run failed.
	// +optional
	Error RunError `json:"error,omitempty,omitzero"`

	// drift is set when a drift run found changes.
	// +optional
	Drift DriftSummary `json:"drift,omitempty,omitzero"`
}

// SourceStatus records what the last Job actually ran.
// +kubebuilder:validation:MinProperties=1
type SourceStatus struct {
	// image is the reference that was run last, as given in the spec.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image,omitempty"`

	// imageDigest is the digest the container runtime resolved the image to
	// (pod status imageID). Informational: the pinned copy lives on the durable
	// inputs Secret as captf.io/image-digest.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	ImageDigest string `json:"imageDigest,omitempty"`

	// runtimeVersion reported by `<command> version -json`.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	RuntimeVersion string `json:"runtimeVersion,omitempty"`
}
