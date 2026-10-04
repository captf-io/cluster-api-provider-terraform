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

package outputs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Violation is one output that breaks a rule.
type Violation struct {
	// Output is the output name.
	Output string
	// Reason is the OutputsValid reason: OutputsInvalid,
	// FailureDomainMismatch or ProviderIDChanged.
	Reason string
	// Message names the output and the rule; it never quotes a free-form
	// value, because outputs may be sensitive.
	Message string
}

// Result is the outcome of decoding, which becomes the OutputsValid
// condition.
type Result struct {
	// Missing lists required outputs that are absent and may not be null.
	Missing []string
	// Pending lists required outputs that are still null.
	Pending []string
	// Violations lists outputs that break a rule.
	Violations []Violation
	// Truncated lists required outputs that were shortened to fit a
	// status limit (the machinepool role's instances output only, capped
	// at MaxInstances): OutputsValid still reports True, with reason
	// InstancesTruncated instead of OutputsValid. Empty for the cluster
	// and machine roles, which never truncate.
	Truncated []string
}

// reasonOrder is the precedence of the OutputsValid reasons: the most severe
// cause is reported.
var reasonOrder = []string{
	infrav1.OutputsInvalidReason,
	infrav1.FailureDomainMismatchReason,
	infrav1.ProviderIDChangedReason,
}

// Reason returns the OutputsValid reason: OutputsMissing, then
// OutputsInvalid, FailureDomainMismatch, ProviderIDChanged, then
// OutputsPending, then InstancesTruncated when nothing else is wrong but
// Truncated is non-empty, else OutputsValid.
func (r Result) Reason() string {
	if len(r.Missing) > 0 {
		return infrav1.OutputsMissingReason
	}
	for _, reason := range reasonOrder {
		for _, v := range r.Violations {
			if v.Reason == reason {
				return reason
			}
		}
	}
	if len(r.Violations) > 0 {
		return r.Violations[0].Reason
	}
	if len(r.Pending) > 0 {
		return infrav1.OutputsPendingReason
	}
	if len(r.Truncated) > 0 {
		return infrav1.InstancesTruncatedReason
	}
	return infrav1.OutputsValidReason
}

// Status returns the OutputsValid condition status for Reason.
// InstancesTruncated is True: a truncated instances list is not a defect,
// it is the controller's own size cap, and the pool still provisions.
func (r Result) Status() metav1.ConditionStatus {
	switch r.Reason() {
	case infrav1.OutputsValidReason, infrav1.InstancesTruncatedReason:
		return metav1.ConditionTrue
	case infrav1.OutputsPendingReason:
		return metav1.ConditionUnknown
	default:
		return metav1.ConditionFalse
	}
}

// Valid reports whether the OutputsValid condition would be True: every
// required output is present and satisfies the contract, whether or not
// Truncated is set. A truncated pool still provisions.
func (r Result) Valid() bool {
	return r.Status() == metav1.ConditionTrue
}

// Concerns reports whether r names output as missing, pending or invalid:
// the controller then keeps the previous mapped value rather than writing a
// bad one.
func (r Result) Concerns(output string) bool {
	if slices.Contains(r.Missing, output) || slices.Contains(r.Pending, output) {
		return true
	}
	return slices.ContainsFunc(r.Violations, func(v Violation) bool { return v.Output == output })
}

// Message describes every problem, naming the outputs; it returns that
// combined description, empty if r has none.
func (r Result) Message() string {
	var parts []string
	if len(r.Missing) > 0 {
		parts = append(parts, "missing outputs: "+strings.Join(r.Missing, ", "))
	}
	for _, v := range r.Violations {
		parts = append(parts, v.Message)
	}
	if len(r.Pending) > 0 {
		parts = append(parts, "outputs still null: "+strings.Join(r.Pending, ", "))
	}
	if len(r.Truncated) > 0 {
		parts = append(parts, "outputs truncated: "+strings.Join(r.Truncated, ", "))
	}
	return strings.Join(parts, "; ")
}

// value returns the raw value of output name in s, or nil when it is
// absent or null.
func value(s *state.State, name string) json.RawMessage {
	o, ok := s.Outputs[name]
	if !ok || len(o.Value) == 0 || bytes.Equal(bytes.TrimSpace(o.Value), []byte("null")) {
		return nil
	}
	return o.Value
}

// decode unmarshals raw, the non-null value of output name, into v,
// recording a violation on res naming want (the expected shape) on a type
// mismatch. It returns whether decoding succeeded.
func decode(res *Result, name, want string, raw json.RawMessage, v any) bool {
	if err := json.Unmarshal(raw, v); err != nil {
		res.Violations = append(res.Violations, invalid(name, "is not %s", want))
		return false
	}
	return true
}

// healthJSON is the wire shape of the health output, with Healthy left a
// pointer so a missing boolean can be told apart from false.
type healthJSON struct {
	State   contract.HealthState `json:"state"`
	Healthy *bool                `json:"healthy"`
	Message *string              `json:"message"`
	Reasons []string             `json:"reasons"`
}

// decodeHealth decodes and validates the health output of s, recording any
// problem on res; it returns the decoded contract.Health, zero-valued if
// health is missing or malformed.
func decodeHealth(s *state.State, res *Result) contract.Health {
	raw := value(s, "health")
	if raw == nil {
		res.Missing = append(res.Missing, "health")
		return contract.Health{}
	}
	var h healthJSON
	if !decode(res, "health", "an object {state, healthy, message, reasons}", raw, &h) {
		return contract.Health{}
	}
	out := contract.Health{State: h.State, Message: h.Message, Reasons: h.Reasons}
	if out.Reasons == nil {
		out.Reasons = []string{}
	}
	if h.Healthy == nil {
		res.Violations = append(res.Violations, invalid("health", "healthy must be a boolean"))
	} else {
		out.Healthy = *h.Healthy
	}
	if v := ValidateHealth(out); v != nil {
		res.Violations = append(res.Violations, *v)
	}
	return out
}

// decodeProviderID decodes and validates the provider_id output of s, the
// same for the machine and machinepool roles. It records a missing, empty
// or undecodable value in res (Pending, or a decode violation) and an
// invalid one as a violation, and returns the normalized provider ID, or
// nil when there is none yet or it was rejected.
func decodeProviderID(s *state.State, res *Result) *string {
	raw := value(s, "provider_id")
	if raw == nil {
		res.Pending = append(res.Pending, "provider_id")
		return nil
	}
	var id *string
	if !decode(res, "provider_id", "a string", raw, &id) {
		return nil
	}
	if id = NormalizeProviderID(id); id == nil {
		res.Pending = append(res.Pending, "provider_id")
		return nil
	}
	if v := ValidateProviderID("provider_id", *id); v != nil {
		res.Violations = append(res.Violations, *v)
		return nil
	}
	return id
}

// DecodeMachine decodes and validates the machine role's outputs from s.
// FailureDomainMismatch and ProviderIDChanged need the Machine and the
// current spec: the controller adds them with CheckPlacement and
// ProviderIDChanged. It returns the decoded outputs (best-effort even on
// error, so the caller can keep prior values it Concerns) and the decode
// Result.
func DecodeMachine(s *state.State) (*contract.MachineOutputs, Result) {
	var res Result
	out := &contract.MachineOutputs{Health: decodeHealth(s, &res)}

	// provider_id: null (or "") until known.
	out.ProviderID = decodeProviderID(s, &res)

	// addresses: null is an empty list.
	out.Addresses = []contract.Address{}
	if raw := value(s, "addresses"); raw != nil {
		var addrs []contract.Address
		if decode(&res, "addresses", "a list of {type, address}", raw, &addrs) {
			if vs := ValidateAddresses(addrs); len(vs) > 0 {
				res.Violations = append(res.Violations, vs...)
			} else if sorted := Dedupe(SortAddresses(addrs)); ValidateAddressCount(len(sorted)) != nil {
				res.Violations = append(res.Violations, *ValidateAddressCount(len(sorted)))
			} else {
				out.Addresses = sorted
			}
		}
	}

	// failure_domain: null is a valid final value.
	if raw := value(s, "failure_domain"); raw != nil {
		var fd string
		if decode(&res, "failure_domain", "a string", raw, &fd) {
			if v := ValidateFailureDomainName("failure_domain", fd); v != nil {
				res.Violations = append(res.Violations, *v)
			} else {
				out.FailureDomain = &fd
			}
		}
	}

	// interruptible: null is false.
	interruptible := false
	if raw := value(s, "interruptible"); raw != nil {
		decode(&res, "interruptible", "a boolean", raw, &interruptible)
	}
	out.Interruptible = &interruptible
	return out, res
}

// MaxInstances is the largest number of instances entries DecodeMachinePool
// keeps in the decoded output; a longer list is truncated to the first
// MaxInstances entries and Result.Truncated records "instances"
// (machinepool.md "instances": "the controller caps status.instances at
// 1000 entries ... reported as OutputsValid=True/InstancesTruncated when
// it does").
const MaxInstances = 1000

// DecodeMachinePool decodes and validates the machinepool role's outputs
// from s. FailureDomainMismatch and ProviderIDChanged are not produced
// here: MachinePool instances have no per-Machine placement request or
// prior providerID to compare against (machinepool.md). It returns the
// decoded outputs (best-effort even on error, so the caller can keep prior
// values it Concerns) and the decode Result.
func DecodeMachinePool(s *state.State) (*contract.MachinePoolOutputs, Result) {
	var res Result
	out := &contract.MachinePoolOutputs{Health: decodeHealth(s, &res)}

	// provider_id: null (or "") until known, exactly like the machine's.
	out.ProviderID = decodeProviderID(s, &res)

	// provider_id_list: required and never null (unlike provider_id): a
	// missing or null value is Missing, not Pending.
	if raw := value(s, "provider_id_list"); raw == nil {
		res.Missing = append(res.Missing, "provider_id_list")
	} else {
		var ids []string
		if decode(&res, "provider_id_list", "a list of strings", raw, &ids) {
			if vs := ValidateProviderIDList(ids); len(vs) > 0 {
				res.Violations = append(res.Violations, vs...)
			} else if sorted := slices.Compact(slices.Sorted(slices.Values(ids))); ValidateProviderIDListCount(len(sorted)) != nil {
				res.Violations = append(res.Violations, *ValidateProviderIDListCount(len(sorted)))
			} else {
				out.ProviderIDList = sorted
			}
		}
	}
	if out.ProviderIDList == nil {
		out.ProviderIDList = []string{}
	}

	// replicas: null until the first refresh, like provider_id.
	if raw := value(s, "replicas"); raw == nil {
		res.Pending = append(res.Pending, "replicas")
	} else {
		var n int32
		if decode(&res, "replicas", "a non-negative integer", raw, &n) {
			if n < 0 {
				res.Violations = append(res.Violations, invalid("replicas", "must be >= 0, got %d", n))
			} else {
				out.Replicas = &n
			}
		}
	}

	// instances: required and never null, like provider_id_list. Every
	// entry is validated before the MaxInstances cap is applied, so a
	// violation among the dropped entries is still reported.
	out.Instances = []contract.PoolInstance{}
	if raw := value(s, "instances"); raw == nil {
		res.Missing = append(res.Missing, "instances")
	} else {
		var instances []contract.PoolInstance
		want := "a list of {provider_id, instance_id, addresses, failure_domain, state}"
		if decode(&res, "instances", want, raw, &instances) {
			if vs := ValidateInstances(instances); len(vs) > 0 {
				res.Violations = append(res.Violations, vs...)
			} else if len(instances) > MaxInstances {
				out.Instances = instances[:MaxInstances]
				res.Truncated = append(res.Truncated, "instances")
			} else {
				out.Instances = instances
			}
		}
	}
	return out, res
}

// DecodeCluster decodes and validates the cluster role's outputs from s;
// it returns the decoded outputs (best-effort even on error, so the
// caller can keep prior values it Concerns) and the decode Result.
func DecodeCluster(s *state.State) (*contract.ClusterOutputs, Result) {
	var res Result
	out := &contract.ClusterOutputs{Health: decodeHealth(s, &res)}

	// control_plane_endpoint: null is valid (no endpoint gate on
	// provisioned). host "" with port 0 is also treated as null:
	// nothing would be written either way. One side set is invalid.
	if raw := value(s, "control_plane_endpoint"); raw != nil {
		var e contract.Endpoint
		if decode(&res, "control_plane_endpoint", "an object {host, port}", raw, &e) && (e.Host != "" || e.Port != 0) {
			if v := ValidateEndpoint("control_plane_endpoint", e); v != nil {
				res.Violations = append(res.Violations, *v)
			} else {
				out.ControlPlaneEndpoint = &e
			}
		}
	}

	// failure_domains: null and [] both clear the field.
	if raw := value(s, "failure_domains"); raw != nil {
		var fds []contract.FailureDomain
		if decode(&res, "failure_domains", "a list of {name, control_plane, attributes}", raw, &fds) {
			if vs := ValidateFailureDomains(fds); len(vs) > 0 {
				res.Violations = append(res.Violations, vs...)
			} else {
				out.FailureDomains = fds
			}
		}
	}

	// exports: null is {}. It becomes every machine's hashed
	// captf_cluster_outputs input, so it must be hashable (integers only).
	out.Exports = json.RawMessage("{}")
	if raw := value(s, "exports"); raw != nil {
		if _, err := hash.Canonical(raw); err != nil {
			reason := "cannot be hashed"
			if errors.Is(err, hash.ErrNonInteger) {
				reason = "contains a fractional or exponent number; export numbers as integers or strings"
			}
			res.Violations = append(res.Violations, invalid("exports", "%s", reason))
		} else {
			out.Exports = raw
		}
	}
	return out, res
}

// String is for debugging; it returns the violation's message and reason,
// and never includes output values.
func (v Violation) String() string {
	return fmt.Sprintf("%s (%s)", v.Message, v.Reason)
}
