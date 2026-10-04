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

package contract

import (
	"errors"
	"fmt"
)

// Role is the part a module plays: the image's module is written for exactly
// one role.
type Role string

const (
	// RoleCluster provisions the shared infrastructure of a TerraformCluster.
	RoleCluster Role = "cluster"
	// RoleMachine provisions one TerraformMachine.
	RoleMachine Role = "machine"
	// RoleMachinePool provisions one TerraformMachinePool. Replicas is
	// MachinePool.spec.replicas with autoscaling disabled, or the observed
	// replicas output once enabled, in which case it is excluded from the
	// inputs hash (https://captf.io/docs/module-author/contract/v1alpha1/machinepool.html; MachinePoolInputs
	// HashView).
	RoleMachinePool Role = "machinepool"
)

// ErrUnknownRole is returned for a string that is no role.
var ErrUnknownRole = errors.New("contract: unknown role")

// Validate reports whether r is a role v1alpha1 implements.
func (r Role) Validate() error {
	switch r {
	case RoleCluster, RoleMachine, RoleMachinePool:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnknownRole, string(r))
	}
}
