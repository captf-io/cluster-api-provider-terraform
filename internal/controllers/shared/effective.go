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

package shared

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
)

// EffectiveConfig is the configuration after inheritance. Nothing is
// persisted; the webhook defaults nothing.
type EffectiveConfig struct {
	// IdentityName is the resolved identity; "" when none is set.
	IdentityName string
	// Jobs is the object's jobs policy, merged field by field over the
	// cluster's defaults.jobs for kinds that inherit them. internal/jobs
	// applies the built-in per-field defaults.
	Jobs infrav1.JobPolicy
	// DriftInterval is the drift policy's intervalSeconds (for inheriting
	// kinds, the machine's or pool's else the cluster's defaults.drift),
	// else the manager's --drift-default-interval. Zero disables drift
	// checks; an inherited zero does not disable a pool's (resolvePool).
	DriftInterval time.Duration
	// DriftAction is the cluster's drift.action, or a pool's inherited one
	// (resolvePool), else Report; always Report for immutable kinds
	// (machines never auto-apply drift).
	DriftAction infrav1.DriftAction
	// Remediation is a TerraformMachine's remediation policy, merged field
	// by field over the cluster's defaults.remediation (MergeRemediation);
	// zero for every other kind. Unset fields keep their zero value: their
	// readers apply the built-in defaults.
	Remediation infrav1.MachineRemediation
	// HealthCheckInterval is Remediation.healthCheckIntervalSeconds
	// (default DefaultHealthCheckInterval) while Remediation.annotateMachine
	// is true, else 0: health is then sampled only at the drift or refresh
	// cadence.
	HealthCheckInterval time.Duration
	// ApplyPolicy is the TerraformCluster's applyPolicy, else Automatic.
	ApplyPolicy infrav1.ApplyPolicy
	// DeletionPolicy is the object's own deletionPolicy, else (for a kind
	// that inherits defaults) the cluster's defaults.deletionPolicy, else
	// the TerraformCluster's own deletionPolicy, else Destroy.
	DeletionPolicy infrav1.DeletionPolicy
	// MembershipRefreshInterval is a TerraformMachinePool's
	// membershipRefreshIntervalSeconds, else the cluster's
	// defaults.membershipRefreshIntervalSeconds, else
	// DefaultMembershipRefreshInterval; 0 for every other kind.
	MembershipRefreshInterval time.Duration
}

// DefaultHealthCheckInterval is remediation.healthCheckIntervalSeconds'
// default.
const DefaultHealthCheckInterval = 300 * time.Second

// DefaultMembershipRefreshInterval is a TerraformMachinePool's
// membershipRefreshIntervalSeconds default.
const DefaultMembershipRefreshInterval = 60 * time.Second

// Resolve applies the inheritance rule. A kind that inherits defaults (a
// TerraformMachine or TerraformMachinePool) merges its jobs, drift and
// remediation policies field by field over cluster's spec.defaults, and
// resolves its identity as identity.EffectiveName does: its own field,
// then spec.defaults, then the cluster's own field of the same name where
// it has one, then the built-in default. A TerraformCluster uses only its
// own fields: spec.defaults are for its machines and pools. Unset fields
// get the compiled defaults (driftDefault for the drift interval). Module
// inputs (source, variables, variablesFrom) are never inherited. A pool
// (spec.PoolDrift non-nil) also takes its drift action and its membership
// refresh interval (resolvePool); a machine its remediation. mutable is
// false for kinds whose drift is always reported, never auto-applied
// (machines). It returns the resolved EffectiveConfig.
func Resolve(spec SpecView, cluster *infrav1.TerraformCluster, driftDefault time.Duration, mutable bool) EffectiveConfig {
	e := EffectiveConfig{
		DriftInterval: driftDefault, DriftAction: infrav1.DriftActionReport, ApplyPolicy: cmp.Or(spec.ApplyPolicy, infrav1.ApplyPolicyAutomatic),
		DeletionPolicy: cmp.Or(spec.DeletionPolicy, infrav1.DeletionPolicyDestroy),
	}
	var interval *int32
	if spec.InheritsDefaults {
		defaults := clusterDefaults(cluster)
		e.IdentityName, _ = identity.EffectiveName(spec.IdentityRef, cluster)
		var clusterPolicy infrav1.DeletionPolicy
		if cluster != nil {
			clusterPolicy = cluster.Spec.DeletionPolicy
		}
		e.DeletionPolicy = cmp.Or(spec.DeletionPolicy, defaults.DeletionPolicy, clusterPolicy, infrav1.DeletionPolicyDestroy)
		e.Jobs = MergeJobPolicy(spec.Jobs, defaults.Jobs)
		if spec.PoolDrift != nil {
			interval = e.resolvePool(spec, cluster)
		} else {
			interval = MergeMachineDriftPolicy(spec.MachineDrift, defaults.Drift).IntervalSeconds
			e.Remediation = MergeRemediation(spec.Remediation, defaults.Remediation)
		}
	} else {
		e.IdentityName = spec.IdentityRef.Name
		if spec.Jobs != nil {
			e.Jobs = *spec.Jobs.DeepCopy()
		}
		if spec.Drift != nil {
			interval = spec.Drift.IntervalSeconds
			if spec.Drift.Action != "" {
				e.DriftAction = spec.Drift.Action
			}
		}
	}
	if interval != nil {
		e.DriftInterval = time.Duration(*interval) * time.Second
	}
	if !mutable {
		e.DriftAction = infrav1.DriftActionReport
	}
	if r := e.Remediation; r.AnnotateMachine != nil && *r.AnnotateMachine {
		e.HealthCheckInterval = DefaultHealthCheckInterval
		if r.HealthCheckIntervalSeconds > 0 {
			e.HealthCheckInterval = time.Duration(r.HealthCheckIntervalSeconds) * time.Second
		}
	}
	return e
}

// clusterDefaults returns cluster's spec.defaults, or zero defaults when
// cluster is nil (not known yet) or sets none.
func clusterDefaults(cluster *infrav1.TerraformCluster) infrav1.TerraformClusterDefaults {
	if cluster == nil || cluster.Spec.Defaults == nil {
		return infrav1.TerraformClusterDefaults{}
	}
	return *cluster.Spec.Defaults
}

// resolvePool sets e's drift action and membership refresh interval from
// spec, a TerraformMachinePool's (spec.PoolDrift non-nil), and cluster,
// its TerraformCluster (nil when not known), and returns its drift
// interval in seconds: its own drift.intervalSeconds when set, else the
// cluster's defaults.drift one when positive, else nil for the manager's
// default. A pool's drift is never disabled
// (infrav1.MachinePoolDriftPolicy), so an inherited 0, which disables a
// machine's drift, does not disable the pool's. The action is the pool's
// own, else defaults.drift.action, else the cluster's spec.drift.action,
// else Report; the membership refresh interval the pool's own, else
// defaults.membershipRefreshIntervalSeconds, else
// DefaultMembershipRefreshInterval.
func (e *EffectiveConfig) resolvePool(spec SpecView, cluster *infrav1.TerraformCluster) *int32 {
	p := spec.PoolDrift
	defaults := clusterDefaults(cluster)
	var defaultAction, clusterAction infrav1.DriftAction
	if defaults.Drift != nil {
		defaultAction = defaults.Drift.Action
	}
	if cluster != nil && cluster.Spec.Drift != nil {
		clusterAction = cluster.Spec.Drift.Action
	}
	e.DriftAction = cmp.Or(p.Action, defaultAction, clusterAction, infrav1.DriftActionReport)
	e.MembershipRefreshInterval = cmp.Or(spec.MembershipRefreshInterval,
		time.Duration(defaults.MembershipRefreshIntervalSeconds)*time.Second, DefaultMembershipRefreshInterval)
	if p.IntervalSeconds > 0 {
		return &p.IntervalSeconds
	}
	if d := MergeMachineDriftPolicy(nil, defaults.Drift).IntervalSeconds; d != nil && *d > 0 {
		return d
	}
	return nil
}

// MergeJobPolicy merges own over defaults field by field: a field own sets
// wins, else the defaults' value. env is merged by name (own wins on the
// same name, own's entries first); imagePullSecrets is the union without
// duplicates (own's first); resources, securityContext and
// podSecurityContext are replaced as a whole. The result shares no memory
// with either argument; both may be nil. It returns the merged JobPolicy.
func MergeJobPolicy(own, defaults *infrav1.JobPolicy) infrav1.JobPolicy {
	var o, d infrav1.JobPolicy
	if own != nil {
		o = *own.DeepCopy()
	}
	if defaults != nil {
		d = *defaults.DeepCopy()
	}
	return infrav1.JobPolicy{
		SuccessfulJobsHistoryLimit: firstSet(o.SuccessfulJobsHistoryLimit, d.SuccessfulJobsHistoryLimit),
		FailedJobsHistoryLimit:     firstSet(o.FailedJobsHistoryLimit, d.FailedJobsHistoryLimit),
		ActiveDeadlineSeconds:      cmp.Or(o.ActiveDeadlineSeconds, d.ActiveDeadlineSeconds),
		ServiceAccountName:         firstNonEmpty(o.ServiceAccountName, d.ServiceAccountName),
		LockTimeoutSeconds:         firstSet(o.LockTimeoutSeconds, d.LockTimeoutSeconds),
		ImagePullSecrets:           unionPullSecrets(o.ImagePullSecrets, d.ImagePullSecrets),
		Resources:                  firstSet(o.Resources, d.Resources),
		Env:                        mergeEnv(o.Env, d.Env),
		SecurityContext:            firstSet(o.SecurityContext, d.SecurityContext),
		PodSecurityContext:         firstSet(o.PodSecurityContext, d.PodSecurityContext),
	}
}

// ValidateEffectiveJobPolicy checks the merged Job policy p (MergeJobPolicy's
// result) the way the Job builder applies it: an unset activeDeadlineSeconds
// or lockTimeoutSeconds takes jobs.DefaultActiveDeadlineSeconds or
// jobs.DefaultLockTimeoutSeconds. It returns an error naming both effective
// values, and whether each is configured or the built-in default, when the
// lock timeout is not shorter than the deadline: the Job would reach its
// deadline while still waiting for the backend lock. The admission webhook
// cannot see this when the two values come from different policies.
func ValidateEffectiveJobPolicy(p infrav1.JobPolicy) error {
	deadline, deadlineSrc := jobs.DefaultActiveDeadlineSeconds, "built-in default"
	if p.ActiveDeadlineSeconds > 0 {
		deadline, deadlineSrc = p.ActiveDeadlineSeconds, "configured"
	}
	lock, lockSrc := int64(jobs.DefaultLockTimeoutSeconds), "built-in default"
	if p.LockTimeoutSeconds != nil {
		lock, lockSrc = int64(*p.LockTimeoutSeconds), "configured"
	}
	if lock >= deadline {
		return fmt.Errorf("effective lockTimeoutSeconds %d (%s) must be less than effective activeDeadlineSeconds %d (%s): the Job would reach its deadline while waiting for the state lock; lower lockTimeoutSeconds or raise activeDeadlineSeconds",
			lock, lockSrc, deadline, deadlineSrc)
	}
	return nil
}

// MergeMachineDriftPolicy merges own over defaults, the cluster's
// defaults.drift, field by field: its intervalSeconds only, since a
// machine's drift is never remediated and so takes no action. It returns
// the merged MachineDriftPolicy.
func MergeMachineDriftPolicy(own *infrav1.MachineDriftPolicy, defaults *infrav1.DriftPolicy) infrav1.MachineDriftPolicy {
	var o infrav1.MachineDriftPolicy
	var d infrav1.DriftPolicy
	if own != nil {
		o = *own.DeepCopy()
	}
	if defaults != nil {
		d = *defaults.DeepCopy()
	}
	return infrav1.MachineDriftPolicy{IntervalSeconds: firstSet(o.IntervalSeconds, d.IntervalSeconds)}
}

// MergeRemediation merges own, a TerraformMachine's remediation policy,
// over defaults, the cluster's defaults.remediation, field by field: a
// field own sets (annotateMachine non-nil, a threshold or interval
// non-zero) wins, else the defaults' value. Either may be nil, and the
// result shares no memory with them. It returns the merged
// MachineRemediation.
func MergeRemediation(own, defaults *infrav1.MachineRemediation) infrav1.MachineRemediation {
	var o, d infrav1.MachineRemediation
	if own != nil {
		o = *own.DeepCopy()
	}
	if defaults != nil {
		d = *defaults.DeepCopy()
	}
	return infrav1.MachineRemediation{
		AnnotateMachine:            firstSet(o.AnnotateMachine, d.AnnotateMachine),
		UnhealthyThreshold:         cmp.Or(o.UnhealthyThreshold, d.UnhealthyThreshold),
		HealthCheckIntervalSeconds: cmp.Or(o.HealthCheckIntervalSeconds, d.HealthCheckIntervalSeconds),
	}
}

// firstSet returns own when set, else def.
func firstSet[T any](own, def *T) *T {
	if own != nil {
		return own
	}
	return def
}

// firstNonEmpty returns own when non-empty, else def.
func firstNonEmpty(own, def string) string {
	if own != "" {
		return own
	}
	return def
}

// mergeEnv returns own followed by def's variables whose names own does
// not set. nil when both are empty.
func mergeEnv(own, def []corev1.EnvVar) []corev1.EnvVar {
	out := slices.Clone(own)
	for _, e := range def {
		if !slices.ContainsFunc(own, func(o corev1.EnvVar) bool { return o.Name == e.Name }) {
			out = append(out, e)
		}
	}
	return out
}

// unionPullSecrets returns own followed by def's secrets own does not
// name, without duplicates. nil when both are empty.
func unionPullSecrets(own, def []corev1.LocalObjectReference) []corev1.LocalObjectReference {
	var out []corev1.LocalObjectReference
	for _, r := range slices.Concat(own, def) {
		if !slices.ContainsFunc(out, func(o corev1.LocalObjectReference) bool { return o.Name == r.Name }) {
			out = append(out, r)
		}
	}
	return out
}
