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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	celschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// loadCRD reads the generated CRD file name from config/crd/bases and
// returns it converted to the internal type the apiserver validates. Test t
// fails when the file cannot be read or parsed.
func loadCRD(t *testing.T, name string) *apiextensions.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", name))
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}
	v1crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.UnmarshalStrict(raw, v1crd); err != nil {
		t.Fatalf("parse CRD: %v", err)
	}
	crd := &apiextensions.CustomResourceDefinition{}
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(v1crd, crd, nil); err != nil {
		t.Fatalf("convert CRD: %v", err)
	}
	return crd
}

// TestCRDsPassAPIServerValidation runs every generated CRD through the
// apiserver's own CRD validation, which compiles every x-kubernetes-
// validations rule and rejects one whose estimated cost is over budget, so
// a rule that the apiserver would refuse to install fails here instead.
func TestCRDsPassAPIServerValidation(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("..", "..", "config", "crd", "bases", "*.yaml"))
	if err != nil || len(files) != 8 {
		t.Fatalf("CRD files = %v, %v; want 8", files, err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			t.Parallel()
			crd := loadCRD(t, filepath.Base(f))
			crd.ResourceVersion = "1"
			crd.Status.StoredVersions = []string{"v1alpha1"}
			errs := apiextvalidation.ValidateCustomResourceDefinition(context.Background(), crd)
			if len(errs) > 0 {
				t.Fatalf("apiserver CRD validation: %v", errs.ToAggregate())
			}
		})
	}
}

// celCase is one transition of an object (old to new, both as unstructured
// maps) and the CEL error substring it must produce; an empty want is a
// valid transition.
type celCase struct {
	name     string
	old, new map[string]any
	want     string
}

// runCELCases validates every case against the schema of the CRD file crdFile
// with the apiserver's CEL validator, old being nil for a create. Test t
// fails on a case whose outcome differs from its want; cases are the
// transitions to check.
func runCELCases(t *testing.T, crdFile string, cases []celCase) {
	t.Helper()
	crd := loadCRD(t, crdFile)
	validation := crd.Spec.Validation
	if validation == nil {
		validation = crd.Spec.Versions[0].Schema
	}
	props := validation.OpenAPIV3Schema
	s, err := structuralschema.NewStructural(props)
	if err != nil {
		t.Fatalf("structural schema: %v", err)
	}
	v := celschema.NewValidator(s, true, celconfig.PerCallLimit)
	if v == nil {
		t.Fatal("schema has no CEL rules")
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			errs, _ := v.Validate(context.Background(), field.NewPath(""), s, tt.new, tt.old, celconfig.RuntimeCELCostBudget)
			got := errs.ToAggregate()
			switch {
			case tt.want == "" && got != nil:
				t.Fatalf("unexpected error: %v", got)
			case tt.want != "" && (got == nil || !strings.Contains(got.Error(), tt.want)):
				t.Fatalf("error = %v, want one containing %q", got, tt.want)
			}
		})
	}
}

// celMachine returns a TerraformMachine object whose spec is base overlaid with
// extra.
func celMachine(extra map[string]any) map[string]any {
	spec := map[string]any{
		"source":      map[string]any{"image": "ghcr.io/captf-io/noop-machine:v1"},
		"identityRef": map[string]any{"name": "id"},
		"variables":   map[string]any{"a": "1", "nested": map[string]any{"b": []any{"x", "y"}}},
		"variablesFrom": []any{
			map[string]any{"configMapRef": map[string]any{"name": "vars"}},
		},
	}
	for k, val := range extra {
		if val == nil {
			delete(spec, k)
			continue
		}
		spec[k] = val
	}
	return map[string]any{"spec": spec}
}

// TestTerraformMachineCEL proves the CRD backs the webhook's immutability of
// source, identityRef, variables and variablesFrom and the set-once providerID.
func TestTerraformMachineCEL(t *testing.T) {
	t.Parallel()
	withPID := celMachine(map[string]any{"providerID": "aws:///i-1"})
	runCELCases(t, "infrastructure.cluster.x-k8s.io_terraformmachines.yaml", []celCase{
		{name: "create", new: celMachine(nil)},
		{name: "unchanged", old: celMachine(nil), new: celMachine(nil)},
		{name: "operational policy changes", old: celMachine(nil),
			new: celMachine(map[string]any{"jobs": map[string]any{"deadlineSeconds": int64(60)}})},
		{name: "source", old: celMachine(nil),
			new:  celMachine(map[string]any{"source": map[string]any{"image": "ghcr.io/captf-io/noop-machine:v2"}}),
			want: "spec.source is immutable"},
		{name: "identityRef", old: celMachine(nil),
			new: celMachine(map[string]any{"identityRef": map[string]any{"name": "other"}}), want: "spec.identityRef is immutable"},
		{name: "identityRef removed", old: celMachine(nil), new: celMachine(map[string]any{"identityRef": nil}),
			want: "spec.identityRef is immutable"},
		{name: "identityRef added", old: celMachine(map[string]any{"identityRef": nil}), new: celMachine(nil),
			want: "spec.identityRef is immutable"},
		{name: "variables", old: celMachine(nil),
			new: celMachine(map[string]any{"variables": map[string]any{"a": "2"}}), want: "spec.variables is immutable"},
		{name: "variables removed", old: celMachine(nil), new: celMachine(map[string]any{"variables": nil}),
			want: "spec.variables is immutable"},
		{name: "variablesFrom", old: celMachine(nil),
			new:  celMachine(map[string]any{"variablesFrom": []any{map[string]any{"secretRef": map[string]any{"name": "vars"}}}}),
			want: "spec.variablesFrom is immutable"},
		{name: "variablesFrom removed", old: celMachine(nil), new: celMachine(map[string]any{"variablesFrom": nil}),
			want: "spec.variablesFrom is immutable"},
		{name: "providerID set once", old: celMachine(nil), new: withPID},
		{name: "providerID unchanged", old: withPID, new: withPID},
		{name: "providerID changed", old: withPID, new: celMachine(map[string]any{"providerID": "aws:///i-2"}),
			want: "providerID can only be set once"},
		{name: "providerID cleared", old: withPID, new: celMachine(nil), want: "providerID can only be set once"},
	})
}

// celCluster returns a TerraformCluster object with the given identityRef
// (none when nil) and the given controlPlaneEndpoint endpoint (none when nil).
func celCluster(identityRef, endpoint map[string]any) map[string]any {
	spec := map[string]any{"source": map[string]any{"image": "ghcr.io/captf-io/noop-cluster:v1"}}
	if identityRef != nil {
		spec["identityRef"] = identityRef
	}
	if endpoint != nil {
		spec["controlPlaneEndpoint"] = endpoint
	}
	return map[string]any{"spec": spec}
}

// TestTerraformClusterCEL proves the CRD requires identityRef and keeps a
// complete controlPlaneEndpoint immutable, as the webhook does.
func TestTerraformClusterCEL(t *testing.T) {
	t.Parallel()
	id := map[string]any{"name": "id"}
	ep := map[string]any{"host": "api.example.com", "port": int64(6443)}
	runCELCases(t, "infrastructure.cluster.x-k8s.io_terraformclusters.yaml", []celCase{
		{name: "create", new: celCluster(id, nil)},
		{name: "create with endpoint", new: celCluster(id, ep)},
		{name: "create without identityRef", new: celCluster(nil, nil), want: "an identity is mandatory"},
		{name: "controller sets the endpoint", old: celCluster(id, nil), new: celCluster(id, ep)},
		{name: "half-set endpoint completed", old: celCluster(id, map[string]any{"host": "h"}), new: celCluster(id, ep)},
		{name: "endpoint unchanged", old: celCluster(id, ep), new: celCluster(id, ep)},
		{name: "host changed", old: celCluster(id, ep), new: celCluster(id, map[string]any{"host": "other", "port": int64(6443)}),
			want: "controlPlaneEndpoint is immutable"},
		{name: "port changed", old: celCluster(id, ep), new: celCluster(id, map[string]any{"host": "api.example.com", "port": int64(443)}),
			want: "controlPlaneEndpoint is immutable"},
		{name: "endpoint removed", old: celCluster(id, ep), new: celCluster(id, nil), want: "controlPlaneEndpoint is immutable"},
		{name: "source stays mutable", old: celCluster(id, ep),
			new: map[string]any{"spec": map[string]any{
				"source": map[string]any{"image": "ghcr.io/captf-io/noop-cluster:v2"}, "identityRef": id, "controlPlaneEndpoint": ep}}},
	})
}

// celPlan returns a TerraformPlan object with the given approved flag (unset
// when nil) and approvedBy (unset when empty).
func celPlan(approved *bool, approvedBy string) map[string]any {
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
	return map[string]any{"spec": spec}
}

// TestTerraformPlanCEL proves the CRD backs the webhook's rules that
// spec fields but the approval are immutable and an approval is permanent.
func TestTerraformPlanCEL(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	mutate := func(m map[string]any, f func(spec map[string]any)) map[string]any {
		f(m["spec"].(map[string]any))
		return m
	}
	runCELCases(t, "infrastructure.cluster.x-k8s.io_terraformplans.yaml", []celCase{
		{name: "create", new: celPlan(nil, "")},
		{name: "create approved", new: celPlan(&yes, "alice")},
		{name: "approve", old: celPlan(nil, ""), new: celPlan(&yes, "alice")},
		{name: "approve over an explicit false", old: celPlan(&no, ""), new: celPlan(&yes, "alice")},
		{name: "approved without approvedBy", new: celPlan(&yes, ""), want: "approvedBy is required when approved is true"},
		{name: "approvedBy without approved", new: celPlan(nil, "alice"), want: "approvedBy can only be set when approved is true"},
		{name: "approvedBy with approved false", new: celPlan(&no, "alice"), want: "approvedBy can only be set when approved is true"},
		{name: "approval unchanged", old: celPlan(&yes, "alice"), new: celPlan(&yes, "alice")},
		{name: "withdraw", old: celPlan(&yes, "alice"), new: celPlan(&no, ""), want: "neither withdrawn nor changed"},
		{name: "withdraw by removal", old: celPlan(&yes, "alice"), new: celPlan(nil, ""), want: "neither withdrawn nor changed"},
		{name: "re-attribute", old: celPlan(&yes, "alice"), new: celPlan(&yes, "bob"), want: "neither withdrawn nor changed"},
		{name: "planHash", old: celPlan(nil, ""), new: mutate(celPlan(nil, ""), func(s map[string]any) { s["planHash"] = "p2:other" }),
			want: "spec.planHash is immutable"},
		{name: "inputsHash", old: celPlan(nil, ""), new: mutate(celPlan(nil, ""), func(s map[string]any) { s["inputsHash"] = "h2:other" }),
			want: "spec.inputsHash is immutable"},
		{name: "reason", old: celPlan(nil, ""), new: mutate(celPlan(nil, ""), func(s map[string]any) { s["reason"] = "Manual" }),
			want: "spec.reason is immutable"},
		{name: "targetRef", old: celPlan(nil, ""), new: mutate(celPlan(nil, ""), func(s map[string]any) {
			s["targetRef"] = map[string]any{"kind": "TerraformCluster", "name": "other"}
		}), want: "spec.targetRef is immutable"},
		{name: "summary", old: celPlan(nil, ""), new: mutate(celPlan(nil, ""), func(s map[string]any) {
			s["summary"] = map[string]any{"replace": int64(2)}
		}), want: "spec.summary is immutable"},
	})
}
