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

package terraformmachinepool

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
)

// ReplicasManagedByValue is the cluster.x-k8s.io/replicas-managed-by value
// CAPTF writes on an autoscaled pool's MachinePool, and the only value it
// ever removes. CAPI treats any value but "false" as set.
const ReplicasManagedByValue = "captf"

// foreignReplicasOwner returns the cluster.x-k8s.io/replicas-managed-by
// value of mp and true when it names a controller other than CAPTF: any
// value but "false", ReplicasManagedByValue and the absent annotation.
func foreignReplicasOwner(mp *clusterv1.MachinePool) (string, bool) {
	v, ok := mp.Annotations[clusterv1.ReplicasManagedByAnnotation]
	return v, ok && v != "false" && v != ReplicasManagedByValue
}

// SyncReplicas keeps mp, the MachinePool owning tmp, in line with tmp's
// autoscaling, patching through d's client using ctx. With autoscaling
// enabled (ParseAutoscaling) it sets cluster.x-k8s.io/replicas-managed-by
// to ReplicasManagedByValue when the annotation is absent or "false" and,
// when status.replicas is known and its value clamped into the autoscaler
// min/max differs from spec.replicas, writes that value to spec.replicas,
// in one patch. A foreign truthy annotation value is kept and nothing is
// written to spec.replicas: another controller owns the replicas (the
// adapter's AutoscalingActive condition reports it, and emits
// shared.EventReplicasManagedExternally once on entering that state).
// Otherwise it removes the annotation when it
// carries ReplicasManagedByValue; another owner's value is never touched,
// and spec.replicas is never written. A nil mp, a pool or MachinePool
// being deleted, and a paused or externally managed pool are skipped. It
// returns an error only from a failed patch; a MachinePool gone by then
// is not one.
func SyncReplicas(ctx context.Context, d shared.Deps, mp *clusterv1.MachinePool, tmp *infrav1.TerraformMachinePool) error {
	if mp == nil || !mp.DeletionTimestamp.IsZero() || !tmp.DeletionTimestamp.IsZero() ||
		conditions.IsTrue(tmp, clusterv1.PausedCondition) || annotations.IsExternallyManaged(tmp) {
		return nil
	}
	current, annotated := mp.Annotations[clusterv1.ReplicasManagedByAnnotation]
	after := mp.DeepCopy()
	autoscaling, _, _ := ParseAutoscaling(mp)
	var written bool
	_, isForeign := foreignReplicasOwner(mp)
	switch {
	case autoscaling.Enabled && isForeign:
		// Nothing is written: another controller owns the replicas.
	case autoscaling.Enabled:
		// Only an absent or "false" annotation is claimed; a foreign truthy
		// value is handled above.
		if !annotated || current == "false" {
			if after.Annotations == nil {
				after.Annotations = map[string]string{}
			}
			after.Annotations[clusterv1.ReplicasManagedByAnnotation] = ReplicasManagedByValue
		}
		if tmp.Status.Replicas != nil {
			observed := min(max(*tmp.Status.Replicas, autoscaling.Min), autoscaling.Max)
			if mp.Spec.Replicas == nil || *mp.Spec.Replicas != observed {
				after.Spec.Replicas = new(observed)
				written = true
			}
		}
	case annotated && current == ReplicasManagedByValue:
		delete(after.Annotations, clusterv1.ReplicasManagedByAnnotation)
	}
	if current == after.Annotations[clusterv1.ReplicasManagedByAnnotation] && !written {
		return nil
	}
	if err := d.Client.Patch(ctx, after, client.MergeFrom(mp), client.FieldOwner(shared.FieldOwner)); err != nil {
		if client.IgnoreNotFound(err) == nil {
			klog.FromContext(ctx).V(shared.LogDebug).Info("MachinePool is gone, skipped the replicas write-back", "MachinePool", klog.KObj(mp))
			return nil
		}
		return fmt.Errorf("write back the replicas of MachinePool %s: %w", mp.Name, err)
	}
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "MachinePool", klog.KObj(mp))
	if !written {
		logger.V(shared.LogFlow).Info("Updated the replicas-managed-by annotation", "value", after.Annotations[clusterv1.ReplicasManagedByAnnotation])
	} else {
		logger.V(shared.LogFlow).Info("Wrote the observed replicas back", "from", replicasText(mp.Spec.Replicas), "to", *after.Spec.Replicas)
		d.Emit(tmp, corev1.EventTypeNormal, shared.EventReplicasWrittenBack, "WriteBackReplicas",
			"MachinePool spec.replicas %s → %d", replicasText(mp.Spec.Replicas), *after.Spec.Replicas)
	}
	mp.Annotations, mp.Spec.Replicas = after.Annotations, after.Spec.Replicas
	return nil
}

// replicasText returns n as text, or "unset" when nil.
func replicasText(n *int32) string {
	if n == nil {
		return "unset"
	}
	return strconv.Itoa(int(*n))
}
