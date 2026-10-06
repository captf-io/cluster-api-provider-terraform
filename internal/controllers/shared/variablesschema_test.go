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

package shared

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/imageinspect"
)

// schemaInspector serves one image config and counts the calls.
type schemaInspector struct {
	labels map[string]string
	err    error
	calls  int
}

// Config counts the call and returns the fixed config or error.
func (s *schemaInspector) Config(context.Context, string, authn.Keychain, *v1.Platform) (*imageinspect.Config, error) {
	s.calls++
	return &imageinspect.Config{Digest: "sha256:abc", Labels: s.labels}, s.err
}

// gateSchema is the schema the gate tests' image publishes.
const gateSchema = `{"type":"object","properties":{"instance_type":{"type":"string"},"disk_gib":{"type":"number"}},` +
	`"required":["instance_type"],"additionalProperties":false}`

// gateDeps returns Deps whose Inspector is insp and whose Schemas cache is
// empty, using t for setup.
func gateDeps(t *testing.T, insp *schemaInspector) Deps {
	t.Helper()
	d := newEnv(t).d
	d.Inspector, d.Schemas = insp, imageinspect.NewSchemaCache()
	return d
}

// vars returns variables from kv, alternating names and JSON values.
func vars(kv ...string) contract.Variables {
	out := contract.Variables{}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = contract.Variable{Value: json.RawMessage(kv[i+1])}
	}
	return out
}

// TestVariablesSchemaGate: the variables are validated against the image's
// schema, once per image; an image without a schema, an unreadable image
// and unwired Deps validate nothing.
func TestVariablesSchemaGate(t *testing.T) {
	t.Parallel()
	insp := &schemaInspector{labels: map[string]string{imageinspect.VariablesSchemaLabel: gateSchema}}
	d := gateDeps(t, insp)
	ctx := t.Context()

	if g := VariablesSchemaGate(ctx, d, testNS, "img:v1", nil, vars("instance_type", `"m5"`, "disk_gib", `20`)); g != nil {
		t.Fatalf("valid variables: %+v", g)
	}
	g := VariablesSchemaGate(ctx, d, testNS, "img:v1", nil, vars("instance_typ", `"m5"`, "disk_gib", `"big"`))
	if g == nil || g.Reason != infrav1.VariablesInvalidReason {
		t.Fatalf("gate = %+v", g)
	}
	for _, want := range []string{`"instance_typ" is not declared`, `"disk_gib" must be a number`, `"instance_type" is required`} {
		if !strings.Contains(g.Message, want) {
			t.Errorf("message %q lacks %q", g.Message, want)
		}
	}
	if insp.calls != 1 {
		t.Errorf("the image was inspected %d times, want once", insp.calls)
	}
	many := vars("a", `1`, "b", `1`, "c", `1`, "d", `1`)
	if g := VariablesSchemaGate(ctx, d, testNS, "img:v1", nil, many); g == nil || !strings.Contains(g.Message, "(and 2 more)") {
		t.Errorf("the message counts the rest: %+v", g)
	}
	lenient := contract.Variables{"instance_type": {Value: json.RawMessage(`"m5"`), Lenient: true}, "disk_gib": {Value: json.RawMessage(`"20"`), Lenient: true}}
	if g := VariablesSchemaGate(ctx, d, testNS, "img:v1", nil, lenient); g != nil {
		t.Errorf("string-format values: %+v", g)
	}

	none := gateDeps(t, &schemaInspector{})
	if g := VariablesSchemaGate(ctx, none, testNS, "img:v1", nil, vars("anything", `1`)); g != nil {
		t.Errorf("an image without a label skips validation: %+v", g)
	}
	bad := gateDeps(t, &schemaInspector{labels: map[string]string{imageinspect.VariablesSchemaLabel: `[]`}})
	if g := VariablesSchemaGate(ctx, bad, testNS, "img:v1", nil, vars("anything", `1`)); g != nil {
		t.Errorf("an invalid label skips validation: %+v", g)
	}
	down := gateDeps(t, &schemaInspector{err: errors.New("registry down")})
	if g := VariablesSchemaGate(ctx, down, testNS, "img:v1", nil, vars("anything", `1`)); g != nil {
		t.Errorf("an unreadable image skips validation: %+v", g)
	}
	if g := VariablesSchemaGate(ctx, newEnv(t).d, testNS, "img:v1", nil, vars("anything", `1`)); g != nil {
		t.Errorf("Deps without a cache skip validation: %+v", g)
	}
}

// TestBuildGatesInvalidVariables: build sets DependenciesReady False with
// VariablesInvalid before any Job when the variables do not fit the
// schema, and passes when they do.
func TestBuildGatesInvalidVariables(t *testing.T) {
	t.Parallel()
	insp := &schemaInspector{labels: map[string]string{imageinspect.VariablesSchemaLabel: gateSchema}}
	e := newEnv(t, world(machine(withFinalizer, notPaused))...)
	e.d.Inspector, e.d.Schemas = insp, imageinspect.NewSchemaCache()
	for _, tt := range []struct {
		name string
		vars contract.Variables
		ok   bool
	}{
		{"valid", vars("instance_type", `"m5"`), true},
		{"unknown key", vars("instance_type", `"m5"`, "oops", `1`), false},
	} {
		k := e.kindFor(t, OwnerInfo{})
		k.in = contract.MachineInputs{CommonInputs: contract.CommonInputs{Variables: tt.vars}}
		k.obj.Spec.Source.Image = "img:v1"
		r := &reconciler{d: e.d, k: k, obj: k.Object(), st: k.Status()}
		_, gate, err := r.build(t.Context(), &StateView{})
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		c := conditions.Get(k.Object(), infrav1.DependenciesReadyCondition)
		switch {
		case tt.ok && (gate != nil || c == nil || c.Status != "True"):
			t.Errorf("%s: gate %+v, condition %+v", tt.name, gate, c)
		case !tt.ok && (gate == nil || gate.Reason != infrav1.VariablesInvalidReason || c == nil || c.Reason != infrav1.VariablesInvalidReason):
			t.Errorf("%s: gate %+v, condition %+v", tt.name, gate, c)
		}
	}
}
