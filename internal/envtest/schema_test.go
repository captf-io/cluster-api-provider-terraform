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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestSchemaMarkers checks one case of each kind of schema marker
// (required, enum, pattern, minimum, maximum, minLength, maxLength,
// minItems and maxItems) against a real API server with no webhook.
func TestSchemaMarkers(t *testing.T) {
	t.Parallel()
	ns := newNamespace(t, bare.client)
	cluster := func(extra map[string]any) map[string]any {
		return clusterSpec(map[string]any{"name": "id"}, extra)
	}
	endpoint := func(host any, port any) map[string]any {
		return cluster(map[string]any{"host": host, "port": port})
	}
	runCELCases(t, bare, "TerraformCluster", ns, []celCase{
		{name: "required: source.image", new: map[string]any{"identityRef": map[string]any{"name": "id"}, "source": map[string]any{}},
			want: "spec.source.image: Required value"},
		{name: "required: source", new: map[string]any{"identityRef": map[string]any{"name": "id"}},
			want: "spec.source: Required value"},
		{name: "enum: imagePullPolicy", new: map[string]any{"identityRef": map[string]any{"name": "id"},
			"source": map[string]any{"image": "i", "imagePullPolicy": "Sometimes"}}, want: "Unsupported value: \"Sometimes\""},
		{name: "enum: a valid value", new: map[string]any{"identityRef": map[string]any{"name": "id"},
			"source": map[string]any{"image": "i", "imagePullPolicy": "Always"}}},
		{name: "minimum: port 0", new: endpoint("h", int64(0)), want: "should be greater than or equal to 1"},
		{name: "maximum: port 65536", new: endpoint("h", int64(65536)), want: "should be less than or equal to 65535"},
		{name: "minLength: empty host", new: endpoint("", int64(6443)), want: "should be at least 1 chars long"},
		{name: "maxLength: image of 513 characters", new: map[string]any{"identityRef": map[string]any{"name": "id"},
			"source": map[string]any{"image": strings.Repeat("a", 513)}}, want: "may not be more than 512"},
		{name: "type: a string port", new: endpoint("h", "6443"), want: "Invalid value: \"string\""},
	})
	runCELCases(t, bare, "TerraformClusterIdentity", "", []celCase{
		{name: "pattern: allowedNamespaces list item", new: map[string]any{"secretRef": map[string]any{"name": "s", "namespace": "n"},
			"allowedNamespaces": map[string]any{"list": []any{"Bad_NS"}}}, want: "should match"},
		{name: "pattern: secretRef namespace", new: map[string]any{"secretRef": map[string]any{"name": "s", "namespace": "UPPER"}},
			want: "should match"},
		{name: "minItems: an empty list", new: map[string]any{"secretRef": map[string]any{"name": "s", "namespace": "n"},
			"allowedNamespaces": map[string]any{"list": []any{}}}, want: "should have at least 1 items"},
		{name: "maxItems: 65 requiredKeys", new: map[string]any{"secretRef": map[string]any{"name": "s", "namespace": "n"},
			"requiredKeys": manyKeys(65)}, want: "must have at most 64 items"},
	})
}

// manyKeys returns n distinct valid key names.
func manyKeys(n int) []any {
	keys := make([]any, n)
	for i := range keys {
		keys[i] = strings.Repeat("K", 1) + strings.Repeat("a", i+1)
	}
	return keys
}

// TestDefaultingAndPruning checks that the API server fills in the schema
// defaults of a TerraformMachine (the empty name of a local object
// reference) and drops fields the schema does not
// declare, at every level, with no webhook installed.
func TestDefaultingAndPruning(t *testing.T) {
	t.Parallel()
	ns := newNamespace(t, bare.client)
	ctx := context.Background()
	spec := machineSpec(map[string]any{
		"unknownField": "dropped",
		"jobs":         map[string]any{"imagePullSecrets": []any{map[string]any{}}},
		"source":       map[string]any{"image": "ghcr.io/captf-io/noop-machine:v1", "unknownNested": "dropped"},
	})
	in := unstructuredOf("TerraformMachine", ns, "defaults", spec)
	if err := bare.client.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(in.GroupVersionKind())
	if err := bare.client.Get(ctx, clientKey(in), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, found, _ := unstructured.NestedString(got.Object, "spec", "unknownField"); found {
		t.Error("spec.unknownField survived pruning")
	}
	if _, found, _ := unstructured.NestedString(got.Object, "spec", "source", "unknownNested"); found {
		t.Error("spec.source.unknownNested survived pruning")
	}
	pulls, _, _ := unstructured.NestedSlice(got.Object, "spec", "jobs", "imagePullSecrets")
	if len(pulls) != 1 {
		t.Fatalf("jobs.imagePullSecrets = %v, want one entry", pulls)
	}
	if name, found, _ := unstructured.NestedString(pulls[0].(map[string]any), "name"); !found || name != "" {
		t.Errorf("jobs.imagePullSecrets[0].name = %q (found %v), want the default empty string", name, found)
	}
}
