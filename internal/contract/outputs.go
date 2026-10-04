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
	"encoding/json"
)

// HealthState is health.state, a closed enum.
type HealthState string

// The health states.
const (
	HealthPending    HealthState = "pending"
	HealthRunning    HealthState = "running"
	HealthDegraded   HealthState = "degraded"
	HealthStopped    HealthState = "stopped"
	HealthTerminated HealthState = "terminated"
	HealthUnknown    HealthState = "unknown"
)

// HealthProviderIDMissing is the health state the controller derives, never
// a module output, for the first sample whose provider_id is null (or "")
// after provisioning: InfrastructureHealthy turns Unknown with
// ProviderIDMissing, and the instance is reported terminated only if the
// next sample is null too. It is not in HealthStates, the schema's enum.
const HealthProviderIDMissing HealthState = "provider_id_missing"

// HealthStates lists every HealthState in the schema's order; it returns
// HealthPending, HealthRunning, HealthDegraded, HealthStopped,
// HealthTerminated and HealthUnknown, in that order.
func HealthStates() []HealthState {
	return []HealthState{HealthPending, HealthRunning, HealthDegraded, HealthStopped, HealthTerminated, HealthUnknown}
}

// Health is the health output every role declares.
type Health struct {
	State   HealthState `json:"state"`
	Healthy bool        `json:"healthy"`
	Message *string     `json:"message"`
	Reasons []string    `json:"reasons"`
}

// FailureDomain is one entry of the cluster role's failure_domains output.
// ControlPlane (default true) and Attributes (default {}) are optional in
// the module; the controller applies the defaults.
type FailureDomain struct {
	Name         string            `json:"name"`
	ControlPlane *bool             `json:"control_plane,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

// Address is one entry of the machine role's addresses output.
type Address struct {
	Type    string `json:"type"`
	Address string `json:"address"`
}

// ClusterOutputs are the cluster role's required outputs. Outputs are
// never hashed.
type ClusterOutputs struct {
	ControlPlaneEndpoint *Endpoint       `json:"control_plane_endpoint"`
	FailureDomains       []FailureDomain `json:"failure_domains"`
	// Exports is injected verbatim into machines as captf_cluster_outputs.
	Exports json.RawMessage `json:"exports"`
	Health  Health          `json:"health"`
}

// MachineOutputs are the machine role's required outputs.
type MachineOutputs struct {
	ProviderID    *string   `json:"provider_id"`
	Addresses     []Address `json:"addresses"`
	FailureDomain *string   `json:"failure_domain"`
	Interruptible *bool     `json:"interruptible"`
	Health        Health    `json:"health"`
}

// PoolInstance is one entry of the machinepool role's instances output.
// Its shape is provider-defined (infra-machinepool.md "InfraMachinePool:
// instances"); every field but ProviderID is optional in the module.
type PoolInstance struct {
	ProviderID    string    `json:"provider_id"`
	InstanceID    *string   `json:"instance_id,omitempty"`
	Addresses     []Address `json:"addresses,omitempty"`
	FailureDomain *string   `json:"failure_domain,omitempty"`
	// State is the health.state enum (common.md), or nil when the module
	// does not report per-instance state.
	State *HealthState `json:"state,omitempty"`
}

// MachinePoolOutputs are the machinepool role's required outputs.
type MachinePoolOutputs struct {
	// ProviderID is the scaling-group id; optional in the contract, may
	// be nil for group-less implementations.
	ProviderID *string `json:"provider_id"`
	// ProviderIDList lists every non-terminated member of the group,
	// each equal to its Node's spec.providerID; may be empty.
	ProviderIDList []string `json:"provider_id_list"`
	// Replicas is the group's desired capacity as observed at refresh.
	Replicas *int32 `json:"replicas"`
	// Instances is the provider-defined member list; may be empty.
	Instances []PoolInstance `json:"instances"`
	Health    Health         `json:"health"`
}
