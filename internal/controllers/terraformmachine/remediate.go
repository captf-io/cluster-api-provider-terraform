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

package terraformmachine

import (
	"cmp"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
)

const (
	// DefaultUnhealthyThreshold is remediation.unhealthyThreshold's default.
	DefaultUnhealthyThreshold = 3
	// FieldOwner is the field manager of CAPTF's writes to CAPI objects.
	FieldOwner = shared.FieldOwner
)

// RemediationReason says why tm's owner Machine should be remediated, or
// "": only with remediation.annotateMachine, only after provisioning, and
// only for a terminated instance (one sample) or unhealthyThreshold
// consecutive unhealthy, degraded or stopped samples. One transient
// reading must never set it: once MachineHealthCheck acts on the
// annotation, the Machine is replaced. It returns that reason, or "" when
// none applies.
func RemediationReason(tm *infrav1.TerraformMachine) string {
	r := tm.Spec.Remediation
	if r == nil || r.AnnotateMachine == nil || !*r.AnnotateMachine {
		return ""
	}
	if p := tm.Status.Initialization.Provisioned; p == nil || !*p || !tm.DeletionTimestamp.IsZero() {
		return ""
	}
	c := conditions.Get(tm, infrav1.InfrastructureHealthyCondition)
	if c == nil {
		return ""
	}
	threshold := cmp.Or(r.UnhealthyThreshold, int32(DefaultUnhealthyThreshold))
	switch c.Reason {
	case infrav1.InstanceTerminatedReason:
		return "the instance is terminated"
	case infrav1.InstanceUnhealthyReason, infrav1.InstanceDegradedReason, infrav1.InstanceStoppedReason:
		if tm.Status.UnhealthySamples >= threshold {
			return fmt.Sprintf("%s for %d consecutive samples", c.Reason, tm.Status.UnhealthySamples)
		}
	}
	return ""
}

// RequestedByAnnotation marks a cluster.x-k8s.io/remediate-machine
// annotation as CAPTF's: only an annotation carrying it is ever removed.
// Its value is why the remediation was requested.
const RequestedByAnnotation = "captf.io/remediation-requested"

// Recovered reports whether tm's instance reads Healthy again: the moment
// the remediation request CAPTF made is withdrawn.
func Recovered(tm *infrav1.TerraformMachine) bool {
	c := conditions.Get(tm, infrav1.InfrastructureHealthyCondition)
	return c != nil && c.Status == metav1.ConditionTrue && c.Reason == infrav1.HealthyReason && tm.DeletionTimestamp.IsZero()
}

// SyncRemediation keeps cluster.x-k8s.io/remediate-machine on machine in
// line with tm's instance health, patching through d's client using ctx.
// It sets the annotation, with RequestedByAnnotation, when
// RemediationReason says so, and is a no-op when the annotation is already
// there. It removes both again once the instance is Healthy (Recovered)
// and the Machine is not being deleted, so a blip that recovered does not
// get the Machine replaced the next time a MachineHealthCheck looks; an
// annotation someone else set is never removed. A nil machine is ignored.
// It returns an error only from a failed patch.
func SyncRemediation(ctx context.Context, d shared.Deps, machine *clusterv1.Machine, tm *infrav1.TerraformMachine) error {
	if machine == nil {
		return nil
	}
	_, annotated := machine.Annotations[clusterv1.RemediateMachineAnnotation]
	reason := RemediationReason(tm)
	switch {
	case reason != "" && !annotated:
		return patchRemediation(ctx, d, machine, tm, reason)
	case reason == "" && annotated && machine.Annotations[RequestedByAnnotation] != "" &&
		Recovered(tm) && machine.DeletionTimestamp.IsZero():
		return patchRemediation(ctx, d, machine, tm, "")
	}
	return nil
}

// patchRemediation patches machine, through d's client using ctx, adding
// the remediation annotations with reason, or removing them when reason is
// "", then emits the matching metric, log and event on tm. It returns an
// error only from a patch failure other than not-found.
func patchRemediation(ctx context.Context, d shared.Deps, machine *clusterv1.Machine, tm *infrav1.TerraformMachine, reason string) error {
	after := machine.DeepCopy()
	if after.Annotations == nil {
		after.Annotations = map[string]string{}
	}
	if reason != "" {
		after.Annotations[clusterv1.RemediateMachineAnnotation] = ""
		after.Annotations[RequestedByAnnotation] = reason
	} else {
		delete(after.Annotations, clusterv1.RemediateMachineAnnotation)
		delete(after.Annotations, RequestedByAnnotation)
	}
	err := d.Client.Patch(ctx, after, client.MergeFrom(machine), client.FieldOwner(FieldOwner))
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			klog.FromContext(ctx).V(shared.LogDebug).Info("Machine is gone, skipped the remediation annotation patch", "Machine", klog.KObj(machine))
			return nil
		}
		return fmt.Errorf("update the remediation annotation of Machine %s: %w", machine.Name, err)
	}
	machine.Annotations = after.Annotations
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "Machine", klog.KObj(machine))
	if reason == "" {
		d.Metrics.RemediationRequest(metrics.RemediationWithdrawn)
		logger.Info("Withdrew the remediation request: the instance is healthy again")
		d.Emit(tm, corev1.EventTypeNormal, shared.EventRemediationWithdrawn, "Remediate",
			"The instance is healthy again; removed the remediation request from Machine %s", machine.Name)
		return nil
	}
	d.Metrics.RemediationRequest(metrics.RemediationRequested)
	logger.Info("Annotated the Machine for remediation", "why", reason)
	d.Emit(tm, corev1.EventTypeWarning, shared.EventRemediationRequested, "Remediate",
		"Asked MachineHealthCheck to remediate Machine %s: %s", machine.Name, reason)
	return nil
}
