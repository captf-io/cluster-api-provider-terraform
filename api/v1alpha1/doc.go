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

// Package v1alpha1 contains the API Schema definitions for CAPTF (the
// cluster-api-provider-terraform), the infrastructure.cluster.x-k8s.io
// v1alpha1 group version. CAPTF turns a Cluster API cluster, machine or
// machine pool into one Terraform or OpenTofu module run: TerraformCluster,
// TerraformMachine and TerraformMachinePool carry the desired state that
// CAPI's core controllers create and drive, TerraformClusterTemplate,
// TerraformMachineTemplate and TerraformMachinePoolTemplate each hold a
// template (their spec.template) from which a TerraformCluster,
// TerraformMachine or TerraformMachinePool is created — the first typically
// referenced by a ClusterClass, the second by a MachineDeployment, MachineSet
// or a control-plane provider, the third by a MachinePool — and
// TerraformClusterIdentity holds the cloud credentials a cluster's module run
// is allowed to use, mirrored into the namespaces that reference it. Seven
// kinds in all, each registered, with its List type, in
// groupversion_info.go.
//
// Every object's spec.source names the one OCI image that bundles the
// module's Terraform/OpenTofu code and the runtime binary that runs it (the
// image-contract types, Source and JobPolicy, live in common_types.go), and
// spec.identityRef, spec.variables and spec.variablesFrom feed that module's
// inputs. The controllers in internal/controllers render those inputs, run
// the image as a Kubernetes Job and translate its outputs and health into the
// object's status: status.conditions (see conditions_consts.go for every
// condition type and reason CAPTF sets, and their Ready-summarization rules)
// and the run-tracking status types common_types.go shares across the
// TerraformCluster, TerraformMachine and TerraformMachinePool kinds —
// ActiveJob, LastRun (with its RunStep and RunError), SourceStatus and
// StateBackup — alongside the drift and remediation policy types DriftPolicy
// (cluster), MachineDriftPolicy (also reused for a cluster's per-machine and
// per-pool defaults), MachinePoolDriftPolicy (pool, never fully disabled) and
// MachineRemediation (machine).
//
// zz_generated.deepcopy.go is controller-gen output; regenerate it with `make
// generate`, never hand-edit it. groupversion_info.go registers the group
// version and every kind with the runtime scheme.
package v1alpha1
