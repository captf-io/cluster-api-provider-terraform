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

// Package templates holds the clusterctl templates released with the
// provider: cluster-template.yaml (the default flavor: Cluster,
// TerraformCluster, KubeadmControlPlane, TerraformMachineTemplates, a
// MachineDeployment and its MachineHealthChecks), the ClusterClass flavor
// (clusterclass-noop.yaml plus cluster-template-clusterclass.yaml), and the
// TerraformClusterIdentity template admins apply once per set of
// credentials (identity.yaml). See
// templates/README.md for the full variable reference, the order of
// operations (install the provider, apply an identity, then generate a
// cluster) and how module variables reach a Terraform module through
// spec.template.spec.variables and variablesFrom.
//
// The package itself has no production Go code: its tests render each
// template with clusterctl's own variable-substitution syntax
// (clusterclass_test.go, templates_test.go), decode
// every resulting object strictly into its API type, and check the
// values README.md documents — including, for the
// ClusterClass flavor, running CAPI's own admission webhook and topology
// generator against clusterclass-noop.yaml so the patches are exercised
// the same way the management cluster exercises them. `make verify` also
// renders every file with the pinned clusterctl binary
// (hack/verify-templates.sh), so a template that references an unknown
// variable or produces invalid YAML fails before it ships.
package templates
