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

package terraformmachinepool

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// MachinePoolInputs builds the machinepool role's inputs for tmp, owned by
// mp on cluster. exports is the cluster's exports output verbatim ({} for
// an externally managed TerraformCluster) and clusterFDs the cluster's
// failure-domain names; bootstrap_data is the base64 of bootstrap's raw
// value bytes, as for machines. Autoscaling is parsed from mp's autoscaler
// annotations (ParseAutoscaling). Replicas is MachinePool.spec.replicas, 1
// when unset (CAPI's default), while autoscaling is disabled; enabled, it
// is the observed replicas output (status.replicas) once one exists, else
// still spec.replicas for the first apply, clamped into
// [Autoscaling.Min, Autoscaling.Max] so a render never asks the cloud for
// an out-of-range desired count (the write-back that patches spec.replicas
// from the raw observed value is unaffected by this clamp). It returns the
// built inputs.
func MachinePoolInputs(cluster *clusterv1.Cluster, mp *clusterv1.MachinePool, tmp *infrav1.TerraformMachinePool,
	exports json.RawMessage, clusterFDs []string, bootstrap *corev1.Secret,
) contract.MachinePoolInputs {
	autoscaling, _, _ := ParseAutoscaling(mp)
	in := contract.MachinePoolInputs{
		CommonInputs: contract.NewCommonInputs(cluster.Name, cluster.Namespace, state.KindTerraformMachinePool, tmp.Name, tmp.Namespace,
			tmp.Annotations[clusterv1.TemplateClonedFromNameAnnotation]),
		ClusterOutputs:        slices.Clone(exports),
		MachinePoolName:       mp.Name,
		Replicas:              poolReplicas(mp, tmp, autoscaling),
		BootstrapData:         shared.BootstrapData(bootstrap),
		BootstrapFormat:       shared.BootstrapFormat(bootstrap),
		FailureDomains:        slices.Clone(mp.Spec.FailureDomains),
		ClusterFailureDomains: slices.Clone(clusterFDs),
		NodeLabels:            maps.Clone(mp.Spec.Template.ObjectMeta.Labels),
		Autoscaling:           autoscaling,
	}
	if v := mp.Spec.Template.Spec.Version; v != "" {
		in.KubernetesVersion = &v
	}
	return in
}

// poolReplicas returns the rendered replicas input: mp.Spec.Replicas (1
// when unset) while autoscaling is disabled. Enabled, it prefers
// tmp.Status.Replicas — the observed desired capacity from the last
// refresh, a pointer so a real 0 (scale-to-zero) is distinguished from "no
// observation yet" — falling back to mp.Spec.Replicas for the first apply,
// before any refresh has run; either way the result is clamped into
// [autoscaling.Min, autoscaling.Max].
func poolReplicas(mp *clusterv1.MachinePool, tmp *infrav1.TerraformMachinePool, autoscaling contract.Autoscaling) int32 {
	if !autoscaling.Enabled {
		return ptr.Deref(mp.Spec.Replicas, 1)
	}
	r := ptr.Deref(tmp.Status.Replicas, ptr.Deref(mp.Spec.Replicas, 1))
	return min(max(r, autoscaling.Min), autoscaling.Max)
}

// ParseAutoscaling reads mp's autoscaler min/max-size annotations
// (clusterv1.AutoscalerMinSizeAnnotation/AutoscalerMaxSizeAnnotation) and
// returns the Autoscaling input the module renders, plus the
// AutoscalingActive reason and message the caller sets on the condition.
// Enabled is true, with Min and Max parsed, iff both annotations are
// present, both parse as base-10 32-bit non-negative integers and
// min <= max (reason ReplicasManagedByModuleReason). Neither annotation
// present returns {false,0,0} with AutoscalingDisabledReason: spec.replicas
// is the sole source of desired capacity. Any other case (one annotation
// only, an unparsable or negative value, or min > max) returns
// {false,0,0} with AutoscalingAnnotationsInvalidReason and a message
// naming the problem.
func ParseAutoscaling(mp *clusterv1.MachinePool) (autoscaling contract.Autoscaling, reason, message string) {
	minRaw, hasMin := mp.Annotations[clusterv1.AutoscalerMinSizeAnnotation]
	maxRaw, hasMax := mp.Annotations[clusterv1.AutoscalerMaxSizeAnnotation]
	if !hasMin && !hasMax {
		return contract.Autoscaling{}, infrav1.AutoscalingDisabledReason, ""
	}
	if !hasMin {
		return contract.Autoscaling{}, infrav1.AutoscalingAnnotationsInvalidReason,
			"annotation " + clusterv1.AutoscalerMinSizeAnnotation + " is missing"
	}
	if !hasMax {
		return contract.Autoscaling{}, infrav1.AutoscalingAnnotationsInvalidReason,
			"annotation " + clusterv1.AutoscalerMaxSizeAnnotation + " is missing"
	}
	minSize, err := strconv.ParseInt(minRaw, 10, 32)
	if err != nil || minSize < 0 {
		return contract.Autoscaling{}, infrav1.AutoscalingAnnotationsInvalidReason,
			"annotation " + clusterv1.AutoscalerMinSizeAnnotation + " is not a non-negative integer: " + minRaw
	}
	maxSize, err := strconv.ParseInt(maxRaw, 10, 32)
	if err != nil || maxSize < 0 {
		return contract.Autoscaling{}, infrav1.AutoscalingAnnotationsInvalidReason,
			"annotation " + clusterv1.AutoscalerMaxSizeAnnotation + " is not a non-negative integer: " + maxRaw
	}
	if minSize > maxSize {
		return contract.Autoscaling{}, infrav1.AutoscalingAnnotationsInvalidReason,
			"annotation " + clusterv1.AutoscalerMinSizeAnnotation + " is greater than " + clusterv1.AutoscalerMaxSizeAnnotation
	}
	return contract.Autoscaling{Enabled: true, Min: int32(minSize), Max: int32(maxSize)},
		infrav1.ReplicasManagedByModuleReason, ""
}
