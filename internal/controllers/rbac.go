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

package controllers

// Own kinds.
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=terraformclusters;terraformclustertemplates;terraformmachines;terraformmachinetemplates;terraformmachinepools;terraformmachinepooltemplates;terraformclusteridentities,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=terraformclusters/status;terraformclustertemplates/status;terraformmachines/status;terraformmachinetemplates/status;terraformmachinepools/status;terraformclusteridentities/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=terraformclusters/finalizers;terraformmachines/finalizers;terraformmachinepools/finalizers,verbs=update

// CAPI owners, the remediate-machine annotation, and an autoscaled pool's
// replicas write-back.
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status;machines;machines/status;machinepools;machinepools/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinepools,verbs=patch

// Credentials, inputs, state, and the runner ServiceAccount and RoleBinding.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Module variables: spec.variablesFrom ConfigMaps (Secrets are covered
// above). The manager watches only those labeled captf.io/variables=true.
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=bind,resourceNames=captf-runner

// State lock cleanup, and the run and cluster write leases (create, update:
// internal/runlease); leader election uses the namespaced Role instead.
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;delete

// Runs.
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list

// Events, legacy core and events.k8s.io/v1.
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Secured diagnostics endpoint.
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
