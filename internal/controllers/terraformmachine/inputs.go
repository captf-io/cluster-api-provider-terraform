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
	"encoding/json"
	"slices"

	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/controllers/shared"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// MachineInputs builds the machine role's inputs for tm, owned by machine
// on cluster. exports is the cluster's exports output verbatim ({} for an
// externally managed TerraformCluster). bootstrap_data is the base64 of
// bootstrap's raw value bytes for every bootstrap provider: a gzip
// payload (CAPRKE2 gzipUserData) is not a string, so the module always
// receives base64. It returns the built inputs.
func MachineInputs(cluster *clusterv1.Cluster, machine *clusterv1.Machine, tm *infrav1.TerraformMachine, exports json.RawMessage, bootstrap *corev1.Secret) contract.MachineInputs {
	_, controlPlane := machine.Labels[clusterv1.MachineControlPlaneLabel]
	in := contract.MachineInputs{
		CommonInputs: contract.NewCommonInputs(cluster.Name, cluster.Namespace, state.KindTerraformMachine, tm.Name, tm.Namespace,
			tm.Annotations[clusterv1.TemplateClonedFromNameAnnotation]),
		ClusterOutputs:  slices.Clone(exports),
		MachineName:     machine.Name,
		BootstrapData:   shared.BootstrapData(bootstrap),
		BootstrapFormat: shared.BootstrapFormat(bootstrap),
		ControlPlane:    controlPlane,
	}
	if fd := machine.Spec.FailureDomain; fd != "" {
		in.FailureDomain = &fd
	}
	if v := machine.Spec.Version; v != "" {
		in.KubernetesVersion = &v
	}
	return in
}
