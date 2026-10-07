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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// Condition types and reasons follow Cluster API v1beta2 semantics
// (capi/docs/proposals/20240916-improve-status-in-CAPI-resources.md): a
// reason for every status, PascalCase, negative-polarity types declared
// explicitly. ConditionReasons documents the full table.
//
// Only Ready is read by Cluster API: it is mirrored into the Cluster's and the
// Machine's InfrastructureReady condition
// (capi/internal/contract/infrastructure_machine.go:59,
// infrastructure_cluster.go:71). Every other type is informational to CAPI.
//
// Paused and Deleting reuse clusterv1.PausedCondition and
// clusterv1.DeletingCondition with their clusterv1 reasons (PausedReason,
// NotPausedReason, DeletingReason, NotDeletingReason).
//
// AutoscalingActive is a pool-only, informational condition: it never
// feeds Ready and is not mirrored to the MachinePool. InstancesTruncatedReason
// (an OutputsValid True reason) is unrelated to autoscaling and is
// exercised by internal/outputs.DecodeMachinePool.

// Ready: positive polarity; the only condition Cluster API mirrors.
const (
	// ReadyCondition summarizes the object's infrastructure. It is mirrored by
	// Cluster API into Cluster/Machine InfrastructureReady.
	ReadyCondition = "Ready"

	// ReadyReason is the Ready reason when every input is healthy.
	ReadyReason = "Ready"
	// NotReadyReason is the Ready reason when an input is False.
	NotReadyReason = "NotReady"
	// ReadyUnknownReason is the Ready reason when an input is Unknown.
	ReadyUnknownReason = "ReadyUnknown"
)

// DependenciesReady: positive polarity; not mirrored. Carries the gating
// reasons Ready cannot express.
const (
	// DependenciesReadyCondition reports whether everything the object waits
	// for (owner, cluster infrastructure, exports, bootstrap data, variable
	// sources) exists.
	DependenciesReadyCondition = "DependenciesReady"

	// DependenciesReadyReason is the True reason.
	DependenciesReadyReason = "DependenciesReady"
	// OwnerNotFoundReason is the False reason when the owner object is gone.
	OwnerNotFoundReason = "OwnerNotFound"
	// WaitingForOwnerMachineReason is the False reason when a fresh
	// TerraformMachine has only a non-controller control-plane ownerRef and no
	// Machine ownerRef yet.
	WaitingForOwnerMachineReason = "WaitingForOwnerMachine"
	// WaitingForOwnerMachinePoolReason is the False reason when a
	// TerraformMachinePool has ownerRefs but no MachinePool ownerRef yet.
	WaitingForOwnerMachinePoolReason = "WaitingForOwnerMachinePool"
	// ClusterNotTerraformReason is the False reason when the owning Cluster's
	// infrastructureRef is not a TerraformCluster; nothing is rendered or run.
	ClusterNotTerraformReason = "ClusterNotTerraform"
	// OwnerMismatchReason is the False reason when an ownerRef of the
	// expected kind resolves to an object that does not reference this one
	// back (wrong or missing infrastructureRef, a UID mismatch, or a
	// cluster-name label that disagrees with the owner): the ownerRef is
	// forged or stale, so the object is treated as having no valid owner;
	// no Job runs and nothing is written to the named owner or its Cluster.
	OwnerMismatchReason = "OwnerMismatch"
	// WaitingForOwnerReason is the Unknown reason while the owner is not set.
	WaitingForOwnerReason = "WaitingForOwner"
	// WaitingForClusterInfrastructureReason is the Unknown reason while the
	// TerraformCluster is not provisioned.
	WaitingForClusterInfrastructureReason = "WaitingForClusterInfrastructure"
	// WaitingForClusterExportsReason is the Unknown reason while the cluster
	// module's exports output is not readable.
	WaitingForClusterExportsReason = "WaitingForClusterExports"
	// WaitingForBootstrapDataReason is the Unknown reason while the Machine's
	// or MachinePool's bootstrap data Secret is not set or not found.
	WaitingForBootstrapDataReason = "WaitingForBootstrapData"
	// VariablesSourceNotFoundReason is the False reason when a ConfigMap or
	// Secret named by spec.variablesFrom (not optional) is missing or does
	// not carry captf.io/variables=true; no Job starts.
	VariablesSourceNotFoundReason = "VariablesSourceNotFound"
	// VariablesInvalidReason is the False reason when a variablesFrom source
	// has a key that is not a Terraform identifier or is reserved, or a
	// value that is not UTF-8 or (format JSON) not valid JSON. The message
	// names the key, never the value; no Job starts.
	VariablesInvalidReason = "VariablesInvalid"
)

// IdentityAllowed: positive polarity; not mirrored.
const (
	// IdentityAllowedCondition reports whether the resolved
	// TerraformClusterIdentity may be used from this namespace.
	IdentityAllowedCondition = "IdentityAllowed"

	// IdentityAllowedReason is the True reason.
	IdentityAllowedReason = "IdentityAllowed"
	// IdentityNotFoundReason is the False reason when the identity does not exist.
	IdentityNotFoundReason = "IdentityNotFound"
	// NamespaceNotAllowedReason is the False reason when allowedNamespaces
	// excludes this namespace.
	NamespaceNotAllowedReason = "NamespaceNotAllowed"
	// SecretNotFoundReason is the False reason when the identity's Secret does
	// not exist.
	SecretNotFoundReason = "SecretNotFound"
	// IdentityCheckFailedReason is the Unknown reason when the check could not
	// be completed.
	IdentityCheckFailedReason = "IdentityCheckFailed"
)

// CredentialsMirrored: positive polarity; not mirrored.
const (
	// CredentialsMirroredCondition reports whether the identity Secret is
	// mirrored into this namespace.
	CredentialsMirroredCondition = "CredentialsMirrored"

	// MirroredReason is the True reason.
	MirroredReason = "Mirrored"
	// MirrorFailedReason is the False reason when mirroring failed.
	MirrorFailedReason = "MirrorFailed"
	// MirrorPendingReason is the Unknown reason before the first mirror.
	MirrorPendingReason = "MirrorPending"
)

// RunnerRBACReady: positive polarity; not mirrored.
const (
	// RunnerRBACReadyCondition reports whether the runner ServiceAccount and
	// its RoleBinding are in place.
	RunnerRBACReadyCondition = "RunnerRBACReady"

	// RBACReadyReason is the True reason.
	RBACReadyReason = "RBACReady"
	// RBACFailedReason is the False reason when creating the binding failed.
	RBACFailedReason = "RBACFailed"
	// ServiceAccountNotOptedInReason is the False reason when an override
	// ServiceAccount lacks the captf.io/runner=true label.
	ServiceAccountNotOptedInReason = "ServiceAccountNotOptedIn"
)

// ApplyJobSucceeded: positive polarity; not mirrored. Reflects the last
// completed apply or destroy Job and is never Unknown once one completed,
// except while the next apply or destroy waits for a run lease
// (WaitingForRunLease, WaitingForClusterOperation,
// WaitingForMachineOperations) or for the approval of its plan
// (PlanAwaitingApproval, PlanChanged).
const (
	// ApplyJobSucceededCondition reports the outcome of the last apply or
	// destroy Job.
	ApplyJobSucceededCondition = "ApplyJobSucceeded"

	// ApplySucceededReason is the True reason after an apply.
	ApplySucceededReason = "ApplySucceeded"
	// DestroySucceededReason is the True reason after a destroy.
	DestroySucceededReason = "DestroySucceeded"
	// ApplyFailedReason is the False reason when an apply Job failed, or
	// disappeared while it ran (deleted before it finished): an apply of
	// the current inputs is then due until one succeeds.
	ApplyFailedReason = "ApplyFailed"
	// DestroyFailedReason is the False reason when a destroy Job failed.
	DestroyFailedReason = "DestroyFailed"
	// JobDeadlineExceededReason is the False reason when the Job hit
	// activeDeadlineSeconds.
	JobDeadlineExceededReason = "JobDeadlineExceeded"
	// IdentityNotAllowedReason is the False reason when no Job could be
	// created because the identity is not allowed.
	IdentityNotAllowedReason = "IdentityNotAllowed"
	// ImageInvalidReason is the False reason when the runner reported an
	// image-layout error (missing /captf/module or a non-executable command).
	ImageInvalidReason = "ImageInvalid"
	// ImagePullFailedReason is the False reason when the pod stayed in
	// ErrImagePull/ImagePullBackOff past activeDeadlineSeconds.
	ImagePullFailedReason = "ImagePullFailed"
	// InputsTooLargeReason is the False reason when the rendered root module
	// and variables exceed the size a Secret can carry, so no Job starts.
	InputsTooLargeReason = "InputsTooLarge"
	// JobPolicyInvalidReason is the False reason when the effective job
	// policy (the object's own, merged over the cluster defaults and the
	// built-in defaults) gives a lockTimeoutSeconds that is not below
	// activeDeadlineSeconds, so no Job starts.
	JobPolicyInvalidReason = "JobPolicyInvalid"
	// DestructivePlanBlockedReason is the False reason when a
	// TerraformCluster apply (including a drift remediation) stopped before
	// a plan that deletes or replaces resources: the plan waits for its
	// approval as a TerraformPlan (status.pendingPlanRef), and no apply of
	// those inputs runs until it is approved, or the inputs change. On a
	// TerraformMachinePool it means an apply of a change of the cluster's
	// exports stopped so: the reason stays while the change's TerraformPlan
	// waits, although the pool keeps applying everything else with the
	// exports of its last successful apply, until the plan is approved, or
	// the exports change again or return to the applied ones (the condition
	// then reports the last successful apply, True). A pool that cannot
	// fall back to those exports (an earlier guarded apply failed part way,
	// or they are not recorded, or they are unknown because the pool
	// applied before this version recorded them) waits for the approval
	// instead, as a cluster does, and the message says why.
	DestructivePlanBlockedReason = "DestructivePlanBlocked"
	// NoApplyYetReason is the Unknown reason before the first apply completes.
	NoApplyYetReason = "NoApplyYet"
	// WaitingForRunLeaseReason is the Unknown reason while the object's run
	// lease is held by another live Job, for example one another manager
	// instance started: no Job starts until it finishes. It is also the
	// DriftJobSucceeded reason for a refresh or drift that waits.
	WaitingForRunLeaseReason = "WaitingForRunLease"
	// WaitingForClusterOperationReason is the Unknown reason while a
	// machine's or pool's apply or destroy waits for its TerraformCluster's
	// apply or destroy to finish.
	WaitingForClusterOperationReason = "WaitingForClusterOperation"
	// WaitingForMachineOperationsReason is the Unknown reason while a
	// TerraformCluster's apply or destroy waits for its machines' and
	// pools' applies and destroys in flight to finish; new ones wait for
	// it meanwhile.
	WaitingForMachineOperationsReason = "WaitingForMachineOperations"
	// PlanAwaitingApprovalReason is the Unknown reason while a
	// TerraformCluster with applyPolicy Manual waits for the approval of
	// its plan, a TerraformPlan (status.pendingPlanRef). Unknown, so
	// waiting never turns Ready False.
	PlanAwaitingApprovalReason = "PlanAwaitingApproval"
	// PlanChangedReason is the Unknown reason when an approved apply
	// planned other changes than the approved plan and stopped before
	// applying them; the approved TerraformPlan is Failed, and the new
	// plan waits for its approval as a new TerraformPlan.
	PlanChangedReason = "PlanChanged"
)

// TerraformPlan conditions. Ready (positive polarity) is True while the
// plan is live or applied, and False once it was superseded or failed;
// Approved is True once spec.approved is set. Pending and Approved name
// both a Ready and an Approved reason.
const (
	// PlanApprovedCondition reports whether the TerraformPlan was approved.
	PlanApprovedCondition = "Approved"

	// PlanPendingReason is the Ready True and the Approved False reason
	// while the plan waits for an approval.
	PlanPendingReason = "Pending"
	// PlanApprovedReason is the Ready True reason while the approved plan
	// waits for its apply to finish, and the Approved True reason of every
	// approved plan that was not superseded.
	PlanApprovedReason = "Approved"
	// PlanAppliedReason is the Ready True reason once the apply of the
	// approved plan succeeded.
	PlanAppliedReason = "Applied"
	// PlanSupersededReason is the Ready False reason once a newer plan of
	// the target replaced the plan, or the plan became moot (the target's
	// inputs changed, no apply is due any more, or its applyPolicy
	// changed), before it was applied.
	PlanSupersededReason = "Superseded"
	// PlanFailedReason is the Ready False reason once the apply of the
	// approved plan planned other changes and stopped before applying
	// them.
	PlanFailedReason = "Failed"
	// PlanNotApprovedReason is the Approved False reason of a plan that
	// was superseded before anyone approved it.
	PlanNotApprovedReason = "NotApproved"
	// PlanApprovalIgnoredReason is the Approved False reason of a plan
	// approved too late: it was superseded before its apply ran, so the
	// approval applied nothing.
	PlanApprovalIgnoredReason = "ApprovalIgnored"
)

// StateReadable: positive polarity; not mirrored.
const (
	// StateReadableCondition reports whether the Terraform state could be read
	// from the kubernetes backend Secrets.
	StateReadableCondition = "StateReadable"

	// StateReadReason is the True reason.
	StateReadReason = "StateRead"
	// StateEncryptedReason is the False reason for OpenTofu-encrypted state,
	// which v1 does not support.
	StateEncryptedReason = "StateEncrypted"
	// StateCorruptReason is the False reason when the state cannot be decoded.
	StateCorruptReason = "StateCorrupt"
	// StateInconsistentReason is the False reason when state chunks disagree.
	StateInconsistentReason = "StateInconsistent"
	// StateLostReason is the False reason when the state Secret of an
	// object that applied before (provisioned, or marked applied, digest
	// pinned or backed up on its own Secrets, which survive a clusterctl
	// move) is missing, or carries no inputs hash for an immutable kind: no
	// Job runs until the state is restored. A deleting object keeps its
	// finalizer until the state is restored (RestoreStateAnnotation) or the
	// object's deletionPolicy is set to Retain.
	StateLostReason = "StateLost"
	// StateLockedReason is the False reason while the state lock is held by
	// something other than the object's own runner, such as a workstation;
	// every Job waits lockTimeoutSeconds for it and then fails.
	StateLockedReason = "StateLocked"
	// StateNotFoundReason is the Unknown reason when no state exists yet.
	StateNotFoundReason = "StateNotFound"
	// RetainedStateFoundReason is the False reason when the object's state
	// Secrets, state backups or durable inputs were kept by an earlier
	// object of the same kind, namespace and name, deleted with
	// deletionPolicy Retain (labeled captf.io/retained-from-uid with its
	// uid). Retained state is never adopted silently: no Job runs until
	// spec.adoptRetainedState is true, and deleting the object removes its
	// finalizer without touching those Secrets.
	RetainedStateFoundReason = "RetainedStateFound"
)

// RestoreJobSucceeded: positive polarity; never in Ready. Set only once a
// state restore was requested (RestoreStateAnnotation): it reports the
// newest restore Job, a requested serial without a backup, or a restore
// waiting for a run lease.
const (
	// RestoreJobSucceededCondition reports the outcome of the last state
	// restore.
	RestoreJobSucceededCondition = "RestoreJobSucceeded"

	// StateRestoredReason is the True reason: the restore Job pushed the
	// backup into the backend.
	StateRestoredReason = "StateRestored"
	// RestoreFailedReason is the False reason when the restore Job failed.
	// It is not retried for the same serial (status.lastRestoredSerial): to
	// retry, remove the annotation, wait for lastRestoredSerial to clear,
	// and set it again.
	RestoreFailedReason = "RestoreFailed"
	// RestoreBackupNotFoundReason is the False reason when the annotation
	// names no existing backup (or is not a serial); no Job starts.
	RestoreBackupNotFoundReason = "RestoreBackupNotFound"
)

// OutputsValid: positive polarity; not mirrored.
const (
	// OutputsValidCondition reports whether the module's outputs satisfy the
	// contract and the Cluster API field markers.
	OutputsValidCondition = "OutputsValid"

	// OutputsValidReason is the True reason.
	OutputsValidReason = "OutputsValid"
	// OutputsMissingReason is the False reason when a required output is not
	// declared.
	OutputsMissingReason = "OutputsMissing"
	// OutputsInvalidReason is the False reason when an output violates the
	// contract or a Cluster API marker.
	OutputsInvalidReason = "OutputsInvalid"
	// FailureDomainMismatchReason is the False reason when a machine's
	// failure_domain output differs from the requested failure domain.
	FailureDomainMismatchReason = "FailureDomainMismatch"
	// ProviderIDChangedReason is the False reason when provider_id changed
	// after it was first written.
	ProviderIDChangedReason = "ProviderIDChanged"
	// OutputsPendingReason is the Unknown reason while required outputs are null.
	OutputsPendingReason = "OutputsPending"
	// InstancesTruncatedReason is a True reason: the machinepool role's
	// instances output had more entries than the controller keeps
	// (internal/outputs.MaxInstances) and was shortened. The pool still
	// provisions; nothing else about its outputs is invalid.
	InstancesTruncatedReason = "InstancesTruncated"
)

// InfrastructureHealthy: positive polarity; not mirrored itself, but the
// input that drives Ready after provisioning.
const (
	// InfrastructureHealthyCondition reports the module's health output.
	InfrastructureHealthyCondition = "InfrastructureHealthy"

	// HealthyReason is the True reason (state running, healthy true).
	HealthyReason = "Healthy"
	// ProvisioningReason is the False reason from the first apply start until
	// provisioned.
	ProvisioningReason = "Provisioning"
	// InstancePendingReason is the False reason for health state pending.
	InstancePendingReason = "InstancePending"
	// InstanceUnhealthyReason is the False reason for state running, healthy false.
	InstanceUnhealthyReason = "InstanceUnhealthy"
	// InstanceDegradedReason is the False reason for health state degraded.
	InstanceDegradedReason = "InstanceDegraded"
	// InstanceStoppedReason is the False reason for health state stopped.
	InstanceStoppedReason = "InstanceStopped"
	// InstanceTerminatedReason is the False reason for health state terminated
	// or a vanished instance.
	InstanceTerminatedReason = "InstanceTerminated"
	// WaitingForProvisioningReason is the Unknown reason before the first apply.
	WaitingForProvisioningReason = "WaitingForProvisioning"
	// HealthUnknownReason is the Unknown reason for health state unknown.
	HealthUnknownReason = "HealthUnknown"
	// ProviderIDMissingReason is the Unknown reason when provider_id turned
	// null after provisioning for the first time. The instance is reported
	// terminated only if the next sample is null as well.
	ProviderIDMissingReason = "ProviderIDMissing"
)

// DriftJobSucceeded: positive polarity; never in Ready.
const (
	// DriftJobSucceededCondition reports the outcome of the last drift or
	// refresh Job.
	DriftJobSucceededCondition = "DriftJobSucceeded"

	// DriftCheckedReason is the True reason.
	DriftCheckedReason = "DriftChecked"
	// DriftJobFailedReason is the False reason when the drift Job failed.
	DriftJobFailedReason = "DriftJobFailed"
	// DriftJobDeadlineExceededReason is the False reason when the drift Job
	// hit activeDeadlineSeconds.
	DriftJobDeadlineExceededReason = "DriftJobDeadlineExceeded"
	// DriftNotCheckedReason is the Unknown reason before the first drift check
	// (also used by DriftDetected).
	DriftNotCheckedReason = "DriftNotChecked"
	// DriftJobRunningReason is the Unknown reason while a drift Job runs.
	DriftJobRunningReason = "DriftJobRunning"
	// DurableInputsMissingReason is the Unknown reason when a refresh or
	// drift Job is due but has nothing to run against: the durable inputs
	// Secret (captf-inputs-<kindshort>-<name>) is gone, and the object
	// renders no current inputs in its place (a TerraformMachine, which is
	// immutable, never does). No refresh or drift check
	// runs until the Secret is restored, so InfrastructureHealthy keeps its
	// last reading and drift goes unchecked.
	DurableInputsMissingReason = "DurableInputsMissing"
)

// DriftDetected: NEGATIVE polarity (True is bad); never in Ready.
const (
	// DriftDetectedCondition reports whether the last drift check found the
	// infrastructure differing from the desired inputs.
	DriftDetectedCondition = "DriftDetected"

	// DriftReportedReason is the True reason with drift action Report.
	DriftReportedReason = "DriftReported"
	// DriftPendingReason is the True reason while remediation is pending, and
	// after a failed cluster re-apply.
	DriftPendingReason = "DriftPending"
	// DriftRemediatingReason is the True reason while a remediation apply runs.
	DriftRemediatingReason = "DriftRemediating"
	// NoDriftReason is the False reason.
	NoDriftReason = "NoDrift"
)

// DeletionBlocked: NEGATIVE polarity; TerraformCluster only; never in Ready.
const (
	// DeletionBlockedCondition reports whether a TerraformCluster's destroy
	// waits for TerraformMachines of the cluster to go away.
	DeletionBlockedCondition = "DeletionBlocked"

	// DependentsExistReason is the True reason.
	DependentsExistReason = "DependentsExist"
	// NotBlockedReason is the False reason.
	NotBlockedReason = "NotBlocked"
)

// EndpointAvailable: positive polarity; TerraformCluster only; never in Ready.
const (
	// EndpointAvailableCondition reports whether a valid control-plane
	// endpoint exists once the cluster is provisioned.
	EndpointAvailableCondition = "EndpointAvailable"

	// EndpointAvailableReason is the True reason.
	EndpointAvailableReason = "EndpointAvailable"
	// WaitingForEndpointReason is the False reason when the cluster is
	// provisioned and neither the module output nor Cluster.spec has a valid
	// endpoint.
	WaitingForEndpointReason = "WaitingForEndpoint"
)

// AutoscalingActive: positive polarity; TerraformMachinePool only; never in
// Ready, not mirrored to the MachinePool. Reports whether the module owns
// the pool's desired count (machinepool.md "autoscaling" input).
const (
	// AutoscalingActiveCondition reports whether the pool's autoscaler
	// min/max-size annotations are present and valid, so the module owns
	// the desired count and its scaling policy.
	AutoscalingActiveCondition = "AutoscalingActive"

	// ReplicasManagedByModuleReason is the True reason: both annotations are
	// present and valid, and the controller writes the observed replicas
	// back to MachinePool.spec.replicas.
	ReplicasManagedByModuleReason = "ReplicasManagedByModule"
	// AutoscalingDisabledReason is the False reason when neither annotation
	// is set: MachinePool.spec.replicas is the sole source of desired
	// capacity.
	AutoscalingDisabledReason = "AutoscalingDisabled"
	// AutoscalingAnnotationsInvalidReason is the False reason when an
	// annotation is present but the pair is incomplete, unparsable, or
	// min > max; the message names the problem. The pool still applies
	// without autoscaling.
	AutoscalingAnnotationsInvalidReason = "AutoscalingAnnotationsInvalid"
	// ReplicasManagedExternallyReason is the False reason when both
	// annotations are valid but the MachinePool's
	// cluster.x-k8s.io/replicas-managed-by annotation names another
	// controller: the observed replicas are not written back, so the two
	// controllers do not fight over MachinePool.spec.replicas.
	ReplicasManagedExternallyReason = "ReplicasManagedExternally"
)

// CapacityResolved: positive polarity; TerraformMachineTemplate only (it has no
// Ready condition).
const (
	// CapacityResolvedCondition reports whether the template's capacity and
	// nodeInfo were read from the image labels.
	CapacityResolvedCondition = "CapacityResolved"

	// CapacityResolvedReason is the True reason when both labels parsed.
	CapacityResolvedReason = "CapacityResolved"
	// CapacityNotDeclaredReason is the True reason when the image carries
	// neither label.
	CapacityNotDeclaredReason = "CapacityNotDeclared"
	// ImageInspectFailedReason is the False reason when the registry fetch or
	// authentication failed.
	ImageInspectFailedReason = "ImageInspectFailed"
	// CapacityLabelInvalidReason is the False reason when a label is present
	// but invalid.
	CapacityLabelInvalidReason = "CapacityLabelInvalid"
)

// Ready of a TerraformClusterIdentity: positive polarity. The identity uses
// ReadyCondition with its own reasons; the manager sets it from the
// credentials Secret.
const (
	// SecretFoundReason is the True reason when the identity's credentials
	// Secret exists.
	SecretFoundReason = "SecretFound"
	// SecretNotFoundReason (IdentityAllowed) is also the False reason when the
	// identity's credentials Secret does not exist.
)

// Ready summary inputs; the slices are passed to
// sigs.k8s.io/cluster-api/util/conditions options, which are []string.
var (
	// ReadyInputsBeforeProvisioned are the Ready inputs of every kind until
	// status.initialization.provisioned first holds.
	ReadyInputsBeforeProvisioned = []string{
		DependenciesReadyCondition,
		IdentityAllowedCondition,
		CredentialsMirroredCondition,
		RunnerRBACReadyCondition,
		ApplyJobSucceededCondition,
		StateReadableCondition,
		OutputsValidCondition,
		InfrastructureHealthyCondition,
		clusterv1.DeletingCondition,
	}

	// ReadyInputsClusterAfterProvisioned are the TerraformCluster's Ready
	// inputs after provisioning. ApplyJobSucceeded is excluded: a failed
	// re-apply must not flip Cluster InfrastructureReady, which would suspend
	// every MachineHealthCheck of the cluster.
	ReadyInputsClusterAfterProvisioned = []string{
		InfrastructureHealthyCondition,
		clusterv1.DeletingCondition,
	}

	// ReadyInputsMachineAfterProvisioned are the TerraformMachine's Ready
	// inputs after provisioning. Identical to the cluster's today; kept
	// separate because the pool list differs.
	ReadyInputsMachineAfterProvisioned = []string{
		InfrastructureHealthyCondition,
		clusterv1.DeletingCondition,
	}

	// ReadyInputsMachinePoolAfterProvisioned are the TerraformMachinePool's
	// Ready inputs after provisioning. Unlike the cluster's and the
	// machine's, ApplyJobSucceeded is included: a pool is mutable and
	// re-applied on bootstrap rotation roughly every 7.5 minutes
	// (https://captf.io/docs/module-author/contract/v1alpha1/machinepool.html "Bootstrap rotation"), so a
	// failed re-apply should be visible in Ready the way it is not for the
	// cluster (which excludes it to avoid suspending every
	// MachineHealthCheck) or the immutable machine.
	ReadyInputsMachinePoolAfterProvisioned = []string{
		InfrastructureHealthyCondition,
		ApplyJobSucceededCondition,
		clusterv1.DeletingCondition,
	}

	// NegativePolarityConditions are the negative-polarity Ready inputs
	// (NegativePolarityConditionTypes). DriftDetected and DeletionBlocked are
	// negative too but never Ready inputs.
	NegativePolarityConditions = []string{clusterv1.DeletingCondition}

	// NeverInReady are condition types that never feed Ready, whatever the
	// phase. ApplyJobSucceeded is phase-dependent and not listed: it is a
	// Ready input before provisioning only.
	NeverInReady = []string{
		clusterv1.PausedCondition,
		DriftDetectedCondition,
		DriftJobSucceededCondition,
		DeletionBlockedCondition,
		EndpointAvailableCondition,
		RestoreJobSucceededCondition,
		AutoscalingActiveCondition,
	}

	// SetOnFirstVisit are set on the first reconcile so that
	// IgnoreTypesIfMissing is only a safety net.
	SetOnFirstVisit = []string{
		clusterv1.PausedCondition,
		clusterv1.DeletingCondition,
		DriftDetectedCondition,
		DeletionBlockedCondition,
	}
)

// ConditionReasons returns, per condition type, the reasons allowed for each
// status the type can take. It is the fixture the reconcilers' tests assert
// against. A status a type cannot take is absent. Each call returns a fresh
// copy, so callers may modify it.
func ConditionReasons() map[string]map[metav1.ConditionStatus][]string {
	return map[string]map[metav1.ConditionStatus][]string{
		ReadyCondition: {
			metav1.ConditionTrue:    {ReadyReason, SecretFoundReason, PlanPendingReason, PlanApprovedReason, PlanAppliedReason},
			metav1.ConditionFalse:   {NotReadyReason, SecretNotFoundReason, PlanSupersededReason, PlanFailedReason},
			metav1.ConditionUnknown: {ReadyUnknownReason},
		},
		clusterv1.PausedCondition: {
			metav1.ConditionTrue:  {clusterv1.PausedReason},
			metav1.ConditionFalse: {clusterv1.NotPausedReason},
		},
		DependenciesReadyCondition: {
			metav1.ConditionTrue:    {DependenciesReadyReason},
			metav1.ConditionFalse:   {OwnerNotFoundReason, WaitingForOwnerMachineReason, WaitingForOwnerMachinePoolReason, ClusterNotTerraformReason, OwnerMismatchReason, VariablesSourceNotFoundReason, VariablesInvalidReason},
			metav1.ConditionUnknown: {WaitingForOwnerReason, WaitingForClusterInfrastructureReason, WaitingForClusterExportsReason, WaitingForBootstrapDataReason},
		},
		IdentityAllowedCondition: {
			metav1.ConditionTrue:    {IdentityAllowedReason},
			metav1.ConditionFalse:   {IdentityNotFoundReason, NamespaceNotAllowedReason, SecretNotFoundReason},
			metav1.ConditionUnknown: {IdentityCheckFailedReason},
		},
		CredentialsMirroredCondition: {
			metav1.ConditionTrue:    {MirroredReason},
			metav1.ConditionFalse:   {MirrorFailedReason},
			metav1.ConditionUnknown: {MirrorPendingReason},
		},
		RunnerRBACReadyCondition: {
			metav1.ConditionTrue:  {RBACReadyReason},
			metav1.ConditionFalse: {RBACFailedReason, ServiceAccountNotOptedInReason},
		},
		ApplyJobSucceededCondition: {
			metav1.ConditionTrue:    {ApplySucceededReason, DestroySucceededReason},
			metav1.ConditionFalse:   {ApplyFailedReason, DestroyFailedReason, JobDeadlineExceededReason, IdentityNotAllowedReason, ImageInvalidReason, ImagePullFailedReason, InputsTooLargeReason, JobPolicyInvalidReason, DestructivePlanBlockedReason},
			metav1.ConditionUnknown: {NoApplyYetReason, WaitingForRunLeaseReason, WaitingForClusterOperationReason, WaitingForMachineOperationsReason, PlanAwaitingApprovalReason, PlanChangedReason},
		},
		StateReadableCondition: {
			metav1.ConditionTrue:    {StateReadReason},
			metav1.ConditionFalse:   {StateEncryptedReason, StateCorruptReason, StateInconsistentReason, StateLostReason, StateLockedReason, RetainedStateFoundReason},
			metav1.ConditionUnknown: {StateNotFoundReason},
		},
		RestoreJobSucceededCondition: {
			metav1.ConditionTrue:    {StateRestoredReason},
			metav1.ConditionFalse:   {RestoreFailedReason, RestoreBackupNotFoundReason},
			metav1.ConditionUnknown: {WaitingForRunLeaseReason, WaitingForClusterOperationReason, WaitingForMachineOperationsReason},
		},
		OutputsValidCondition: {
			metav1.ConditionTrue:    {OutputsValidReason, InstancesTruncatedReason},
			metav1.ConditionFalse:   {OutputsMissingReason, OutputsInvalidReason, FailureDomainMismatchReason, ProviderIDChangedReason},
			metav1.ConditionUnknown: {OutputsPendingReason},
		},
		InfrastructureHealthyCondition: {
			metav1.ConditionTrue:    {HealthyReason},
			metav1.ConditionFalse:   {ProvisioningReason, InstancePendingReason, InstanceUnhealthyReason, InstanceDegradedReason, InstanceStoppedReason, InstanceTerminatedReason},
			metav1.ConditionUnknown: {WaitingForProvisioningReason, HealthUnknownReason, ProviderIDMissingReason},
		},
		DriftJobSucceededCondition: {
			metav1.ConditionTrue:    {DriftCheckedReason},
			metav1.ConditionFalse:   {DriftJobFailedReason, DriftJobDeadlineExceededReason},
			metav1.ConditionUnknown: {DriftNotCheckedReason, DriftJobRunningReason, WaitingForRunLeaseReason, DurableInputsMissingReason},
		},
		DriftDetectedCondition: {
			metav1.ConditionTrue:    {DriftReportedReason, DriftPendingReason, DriftRemediatingReason},
			metav1.ConditionFalse:   {NoDriftReason},
			metav1.ConditionUnknown: {DriftNotCheckedReason},
		},
		DeletionBlockedCondition: {
			metav1.ConditionTrue:  {DependentsExistReason},
			metav1.ConditionFalse: {NotBlockedReason},
		},
		EndpointAvailableCondition: {
			metav1.ConditionTrue:  {EndpointAvailableReason},
			metav1.ConditionFalse: {WaitingForEndpointReason},
		},
		AutoscalingActiveCondition: {
			metav1.ConditionTrue:  {ReplicasManagedByModuleReason},
			metav1.ConditionFalse: {AutoscalingDisabledReason, AutoscalingAnnotationsInvalidReason, ReplicasManagedExternallyReason},
		},
		clusterv1.DeletingCondition: {
			metav1.ConditionTrue:  {clusterv1.DeletingReason},
			metav1.ConditionFalse: {clusterv1.NotDeletingReason},
		},
		PlanApprovedCondition: {
			metav1.ConditionTrue:  {PlanApprovedReason},
			metav1.ConditionFalse: {PlanPendingReason, PlanNotApprovedReason, PlanApprovalIgnoredReason},
		},
		CapacityResolvedCondition: {
			metav1.ConditionTrue:  {CapacityResolvedReason, CapacityNotDeclaredReason},
			metav1.ConditionFalse: {ImageInspectFailedReason, CapacityLabelInvalidReason},
		},
	}
}
