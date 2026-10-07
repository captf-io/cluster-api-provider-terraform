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

package webhooks

import (
	"errors"
	"fmt"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
)

// SetupWebhooks registers the validating webhooks of all eight kinds with
// mgr. There are no defaulting or mutating webhooks: every default is
// resolved at reconcile time instead. The TerraformMachine delete check
// lists Machines and reads Clusters, and the TerraformClusterIdentity
// delete check lists TerraformClusters, TerraformMachines and
// TerraformMachinePools, through the
// uncached API reader; the identity create/update check creates
// SubjectAccessReviews.
// managerUser is the manager's ServiceAccount username
// (system:serviceaccount:<namespace>:<name>), the only caller that may set
// providerID on an existing TerraformMachine and change the plan-phase label
// of a TerraformPlan; empty refuses those updates for everyone. schemas is
// where the workload webhooks look up the variables schema of an image they
// have already seen, reading a missing one within SchemaFetchTimeout (nil
// checks none). It
// returns nil once every webhook is registered, or an error
// from the first registration or scheme check that fails.
func SetupWebhooks(mgr ctrl.Manager, managerUser string, schemas SchemaLookup) error {
	// Without CAPI core types every owned TerraformMachine delete would fail
	// with a 500; fail at startup instead.
	if !mgr.GetScheme().Recognizes(clusterv1.GroupVersion.WithKind("Machine")) {
		return errors.New("webhooks: the manager scheme lacks cluster.x-k8s.io Machine, needed by the TerraformMachine delete webhook")
	}
	setups := []struct {
		kind  string
		setup func(ctrl.Manager) error
	}{
		{terraformClusterKind, (&TerraformCluster{Schemas: schemas}).SetupWebhookWithManager},
		{terraformClusterTemplateKind, (&TerraformClusterTemplate{Schemas: schemas}).SetupWebhookWithManager},
		{terraformMachineKind, (&TerraformMachine{Schemas: schemas, Reader: mgr.GetAPIReader(), ManagerUser: managerUser}).SetupWebhookWithManager},
		{terraformMachineTemplateKind, (&TerraformMachineTemplate{Schemas: schemas}).SetupWebhookWithManager},
		{terraformMachinePoolKind, (&TerraformMachinePool{Schemas: schemas}).SetupWebhookWithManager},
		{terraformMachinePoolTemplateKind, (&TerraformMachinePoolTemplate{Schemas: schemas}).SetupWebhookWithManager},
		{terraformClusterIdentityKind, (&TerraformClusterIdentity{Client: mgr.GetClient(), Reader: mgr.GetAPIReader()}).SetupWebhookWithManager},
		{terraformPlanKind, (&TerraformPlan{ManagerUser: managerUser}).SetupWebhookWithManager},
	}
	for _, s := range setups {
		if err := s.setup(mgr); err != nil {
			return fmt.Errorf("webhooks: set up %s webhook: %w", s.kind, err)
		}
	}
	return nil
}
