//go:build envtest

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

package envtest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// celCase is one write to a kind: old (nil for a create) is created first,
// then new replaces its spec, and the write must be rejected as Invalid
// with a message containing want, or accepted when want is empty.
type celCase struct {
	name     string
	old, new map[string]any
	want     string
}

// runCELCases runs cases against te's API server for kind, in namespace ns
// (empty for a cluster-scoped kind), each as a parallel subtest on an
// object of its own. Test t fails on a case whose outcome differs from its
// want.
func runCELCases(t *testing.T, te *testEnv, kind, ns string, cases []celCase) {
	t.Helper()
	for i, tt := range cases {
		name := fmt.Sprintf("c%d-%s", i, strings.ToLower(kind))
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			var err error
			if tt.old == nil {
				err = te.client.Create(ctx, unstructuredOf(kind, ns, name, tt.new))
			} else {
				cur := unstructuredOf(kind, ns, name, tt.old)
				if err := te.client.Create(ctx, cur); err != nil {
					t.Fatalf("create the old object: %v", err)
				}
				cur.Object["spec"] = tt.new
				err = te.client.Update(ctx, cur)
			}
			if tt.want == "" {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}
			wantInvalid(t, err, tt.want)
		})
	}
}

// machineSpec returns a TerraformMachine spec, overlaid with extra (a nil
// value deletes the key).
func machineSpec(extra map[string]any) map[string]any {
	spec := map[string]any{
		"source":      map[string]any{"image": "ghcr.io/captf-io/noop-machine:v1"},
		"identityRef": map[string]any{"name": "id"},
		"variables":   map[string]any{"a": "1", "nested": map[string]any{"b": []any{"x", "y"}}},
		"variablesFrom": []any{
			map[string]any{"configMapRef": map[string]any{"name": "vars"}},
		},
	}
	for k, v := range extra {
		if v == nil {
			delete(spec, k)
			continue
		}
		spec[k] = v
	}
	return spec
}

// TestTerraformMachineCEL checks the rules on the TerraformMachine kind
// against a real API server with no webhook installed: the immutable
// source, identityRef, variables and variablesFrom, and the set-once
// providerID.
func TestTerraformMachineCEL(t *testing.T) {
	t.Parallel()
	ns := newNamespace(t, bare.client)
	withPID := machineSpec(map[string]any{"providerID": "aws:///i-1"})
	runCELCases(t, bare, "TerraformMachine", ns, []celCase{
		{name: "create", new: machineSpec(nil)},
		{name: "unchanged", old: machineSpec(nil), new: machineSpec(nil)},
		{name: "operational policy changes", old: machineSpec(nil),
			new: machineSpec(map[string]any{"jobs": map[string]any{"deadlineSeconds": int64(60)}})},
		{name: "source", old: machineSpec(nil),
			new:  machineSpec(map[string]any{"source": map[string]any{"image": "ghcr.io/captf-io/noop-machine:v2"}}),
			want: "spec.source is immutable"},
		{name: "identityRef", old: machineSpec(nil),
			new: machineSpec(map[string]any{"identityRef": map[string]any{"name": "other"}}), want: "spec.identityRef is immutable"},
		{name: "identityRef removed", old: machineSpec(nil), new: machineSpec(map[string]any{"identityRef": nil}),
			want: "spec.identityRef is immutable"},
		{name: "identityRef added", old: machineSpec(map[string]any{"identityRef": nil}), new: machineSpec(nil),
			want: "spec.identityRef is immutable"},
		{name: "variables", old: machineSpec(nil),
			new: machineSpec(map[string]any{"variables": map[string]any{"a": "2"}}), want: "spec.variables is immutable"},
		{name: "variables removed", old: machineSpec(nil), new: machineSpec(map[string]any{"variables": nil}),
			want: "spec.variables is immutable"},
		{name: "variables added", old: machineSpec(map[string]any{"variables": nil}), new: machineSpec(nil),
			want: "spec.variables is immutable"},
		{name: "variablesFrom", old: machineSpec(nil),
			new:  machineSpec(map[string]any{"variablesFrom": []any{map[string]any{"secretRef": map[string]any{"name": "vars"}}}}),
			want: "spec.variablesFrom is immutable"},
		{name: "variablesFrom removed", old: machineSpec(nil), new: machineSpec(map[string]any{"variablesFrom": nil}),
			want: "spec.variablesFrom is immutable"},
		{name: "variablesFrom added", old: machineSpec(map[string]any{"variablesFrom": nil}), new: machineSpec(nil),
			want: "spec.variablesFrom is immutable"},
		{name: "providerID set once", old: machineSpec(nil), new: withPID},
		{name: "providerID unchanged", old: withPID, new: withPID},
		{name: "providerID changed", old: withPID, new: machineSpec(map[string]any{"providerID": "aws:///i-2"}),
			want: "providerID can only be set once"},
		{name: "providerID cleared", old: withPID, new: machineSpec(nil), want: "providerID can only be set once"},
	})
}

// clusterSpec returns a TerraformCluster spec with the given identityRef
// (none when nil) and endpoint, its controlPlaneEndpoint (none when nil).
func clusterSpec(identityRef, endpoint map[string]any) map[string]any {
	spec := map[string]any{"source": map[string]any{"image": "ghcr.io/captf-io/noop-cluster:v1"}}
	if identityRef != nil {
		spec["identityRef"] = identityRef
	}
	if endpoint != nil {
		spec["controlPlaneEndpoint"] = endpoint
	}
	return spec
}

// TestTerraformClusterCEL checks the rules on the TerraformCluster kind
// against a real API server with no webhook installed: the required
// identityRef, the controlPlaneEndpoint that is immutable once complete,
// and its shape.
func TestTerraformClusterCEL(t *testing.T) {
	t.Parallel()
	ns := newNamespace(t, bare.client)
	id := map[string]any{"name": "id"}
	ep := map[string]any{"host": "api.example.com", "port": int64(6443)}
	runCELCases(t, bare, "TerraformCluster", ns, []celCase{
		{name: "create", new: clusterSpec(id, nil)},
		{name: "create with endpoint", new: clusterSpec(id, ep)},
		{name: "create without identityRef", new: clusterSpec(nil, nil), want: "an identity is mandatory"},
		{name: "update drops identityRef", old: clusterSpec(id, nil), new: clusterSpec(nil, nil), want: "an identity is mandatory"},
		{name: "controller sets the endpoint", old: clusterSpec(id, nil), new: clusterSpec(id, ep)},
		{name: "host and port are set together: host alone", new: clusterSpec(id, map[string]any{"host": "h"}),
			want: "host and port must be set together"},
		{name: "host and port are set together: port alone", new: clusterSpec(id, map[string]any{"port": int64(6443)}),
			want: "host and port must be set together"},
		{name: "partial endpoint may change", old: clusterSpec(id, nil), new: clusterSpec(id, ep)},
		{name: "endpoint unchanged", old: clusterSpec(id, ep), new: clusterSpec(id, ep)},
		{name: "host changed", old: clusterSpec(id, ep), new: clusterSpec(id, map[string]any{"host": "other", "port": int64(6443)}),
			want: "controlPlaneEndpoint is immutable"},
		{name: "port changed", old: clusterSpec(id, ep), new: clusterSpec(id, map[string]any{"host": "api.example.com", "port": int64(443)}),
			want: "controlPlaneEndpoint is immutable"},
		{name: "endpoint removed", old: clusterSpec(id, ep), new: clusterSpec(id, nil), want: "controlPlaneEndpoint is immutable"},
		{name: "source stays mutable", old: clusterSpec(id, ep),
			new: map[string]any{
				"source": map[string]any{"image": "ghcr.io/captf-io/noop-cluster:v2"}, "identityRef": id, "controlPlaneEndpoint": ep}},
	})
}

// planSpec returns a TerraformPlan spec with the given approved flag (unset
// when nil) and approvedBy (unset when empty).
func planSpec(approved *bool, approvedBy string) map[string]any {
	spec := map[string]any{
		"targetRef":  map[string]any{"kind": "TerraformCluster", "name": "demo"},
		"planHash":   "p2:abc",
		"inputsHash": "h2:def",
		"reason":     "Destructive",
		"summary":    map[string]any{"replace": int64(1), "resources": []any{"aws_instance.a (replace)"}},
	}
	if approved != nil {
		spec["approved"] = *approved
	}
	if approvedBy != "" {
		spec["approvedBy"] = approvedBy
	}
	return spec
}

// TestTerraformPlanCEL checks the rules on TerraformPlan against a real API
// server with no webhook installed: the approval's shape, that it is
// permanent, and the immutable spec fields.
func TestTerraformPlanCEL(t *testing.T) {
	t.Parallel()
	ns := newNamespace(t, bare.client)
	yes, no := true, false
	mutate := func(f func(spec map[string]any)) map[string]any {
		spec := planSpec(nil, "")
		f(spec)
		return spec
	}
	runCELCases(t, bare, "TerraformPlan", ns, []celCase{
		{name: "create", new: planSpec(nil, "")},
		{name: "create approved", new: planSpec(&yes, "alice")},
		{name: "approve", old: planSpec(nil, ""), new: planSpec(&yes, "alice")},
		{name: "approve over an explicit false", old: planSpec(&no, ""), new: planSpec(&yes, "alice")},
		{name: "approved without approvedBy", new: planSpec(&yes, ""), want: "approvedBy is required when approved is true"},
		{name: "approvedBy without approved", new: planSpec(nil, "alice"), want: "approvedBy can only be set when approved is true"},
		{name: "approvedBy with approved false", new: planSpec(&no, "alice"), want: "approvedBy can only be set when approved is true"},
		{name: "approval unchanged", old: planSpec(&yes, "alice"), new: planSpec(&yes, "alice")},
		{name: "withdraw", old: planSpec(&yes, "alice"), new: planSpec(&no, ""), want: "neither withdrawn nor changed"},
		{name: "withdraw by removal", old: planSpec(&yes, "alice"), new: planSpec(nil, ""), want: "neither withdrawn nor changed"},
		{name: "re-attribute", old: planSpec(&yes, "alice"), new: planSpec(&yes, "bob"), want: "neither withdrawn nor changed"},
		{name: "planHash", old: planSpec(nil, ""), new: mutate(func(s map[string]any) { s["planHash"] = "p2:other" }),
			want: "spec.planHash is immutable"},
		{name: "inputsHash", old: planSpec(nil, ""), new: mutate(func(s map[string]any) { s["inputsHash"] = "h2:other" }),
			want: "spec.inputsHash is immutable"},
		{name: "reason", old: planSpec(nil, ""), new: mutate(func(s map[string]any) { s["reason"] = "Manual" }),
			want: "spec.reason is immutable"},
		{name: "targetRef", old: planSpec(nil, ""), new: mutate(func(s map[string]any) {
			s["targetRef"] = map[string]any{"kind": "TerraformCluster", "name": "other"}
		}), want: "spec.targetRef is immutable"},
		{name: "summary", old: planSpec(nil, ""), new: mutate(func(s map[string]any) {
			s["summary"] = map[string]any{"replace": int64(2)}
		}), want: "spec.summary is immutable"},
	})
}

// TestTerraformClusterIdentityCEL checks the rules of the identity kind
// against a real API server: the secretRef its Secret type needs and the
// allowedNamespaces that are never ambiguous.
func TestTerraformClusterIdentityCEL(t *testing.T) {
	t.Parallel()
	secret := map[string]any{"name": "creds", "namespace": "captf-system"}
	spec := func(extra map[string]any) map[string]any {
		s := map[string]any{"secretRef": secret}
		for k, v := range extra {
			s[k] = v
		}
		return s
	}
	runCELCases(t, bare, "TerraformClusterIdentity", "", []celCase{
		{name: "create", new: spec(nil)},
		{name: "secretRef is required", new: map[string]any{"type": "Secret"}, want: "secretRef is required when type is Secret"},
		{name: "secretRef is required with the default type", new: map[string]any{"requiredKeys": []any{"K"}},
			want: "secretRef is required when type is Secret"},
		{name: "allowedNamespaces list", new: spec(map[string]any{"allowedNamespaces": map[string]any{"list": []any{"team-a"}}})},
		{name: "allowedNamespaces selector", new: spec(map[string]any{"allowedNamespaces": map[string]any{"selector": map[string]any{}}})},
		{name: "allowedNamespaces {} is ambiguous", new: spec(map[string]any{"allowedNamespaces": map[string]any{}}),
			want: "allowedNamespaces {} is ambiguous"},
		{name: "allowedNamespaces {} on update", old: spec(nil), new: spec(map[string]any{"allowedNamespaces": map[string]any{}}),
			want: "allowedNamespaces {} is ambiguous"},
	})
}

// TestTemplateSpecsAreEditableByCEL proves a template's spec is not
// immutable at the CRD: its immutability is the webhook's alone, so a
// ClusterClass topology dry-run can still change it.
func TestTemplateSpecsAreEditableByCEL(t *testing.T) {
	t.Parallel()
	ns := newNamespace(t, bare.client)
	machine := func(image string) map[string]any {
		return map[string]any{"template": map[string]any{"spec": map[string]any{
			"source": map[string]any{"image": image}, "identityRef": map[string]any{"name": "id"}}}}
	}
	cluster := func(image string) map[string]any {
		return map[string]any{"template": map[string]any{"spec": map[string]any{
			"source": map[string]any{"image": image}, "identityRef": map[string]any{"name": "id"}}}}
	}
	t.Run("TerraformMachineTemplate", func(t *testing.T) {
		t.Parallel()
		runCELCases(t, bare, "TerraformMachineTemplate", ns, []celCase{
			{name: "spec edit", old: machine("ghcr.io/captf-io/noop-machine:v1"), new: machine("ghcr.io/captf-io/noop-machine:v2")},
		})
	})
	t.Run("TerraformClusterTemplate", func(t *testing.T) {
		t.Parallel()
		runCELCases(t, bare, "TerraformClusterTemplate", ns, []celCase{
			{name: "spec edit", old: cluster("ghcr.io/captf-io/noop-cluster:v1"), new: cluster("ghcr.io/captf-io/noop-cluster:v2")},
		})
	})
}

// TestCapacitySourceCEL checks the rule on the status of a
// TerraformMachineTemplate: a capacity read from the image names the image.
func TestCapacitySourceCEL(t *testing.T) {
	t.Parallel()
	ns := newNamespace(t, bare.client)
	ctx := context.Background()
	tmpl := unstructuredOf("TerraformMachineTemplate", ns, "capacity", map[string]any{"template": map[string]any{"spec": map[string]any{
		"source": map[string]any{"image": "ghcr.io/captf-io/noop-machine:v1"}, "identityRef": map[string]any{"name": "id"}}}})
	if err := bare.client.Create(ctx, tmpl); err != nil {
		t.Fatalf("create: %v", err)
	}
	setStatus := func(src map[string]any) error {
		cur := &unstructured.Unstructured{}
		cur.SetGroupVersionKind(tmpl.GroupVersionKind())
		if err := bare.client.Get(ctx, clientKey(tmpl), cur); err != nil {
			t.Fatalf("get: %v", err)
		}
		cur.Object["status"] = map[string]any{"capacitySource": src}
		return bare.client.Status().Update(ctx, cur)
	}
	wantInvalid(t, setStatus(map[string]any{"source": "Image"}), "image is required when source is Image")
	if err := setStatus(map[string]any{"source": "Image", "image": "ghcr.io/captf-io/noop-machine:v1"}); err != nil {
		t.Fatalf("a capacity from the image naming it: %v", err)
	}
}
