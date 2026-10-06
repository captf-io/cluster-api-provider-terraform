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
	"testing"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/varschema"
)

// schemas is a SchemaLookup over a fixed map; an absent image is unknown.
type schemas map[string]*varschema.Schema

// Cached returns the schema of ref and whether the image is known.
func (s schemas) Cached(ref string) (*varschema.Schema, bool) {
	v, ok := s[ref]
	return v, ok
}

// TestSchemaAtAdmission: inline variables are checked against a cached
// schema for the object's image; an unknown image, a nil lookup, an image
// without a schema and an update that changes neither image nor variables
// are all allowed. Required variables are not checked: variablesFrom may
// supply them.
func TestSchemaAtAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	schema, err := varschema.Parse(`{"type":"object","properties":{"instance_type":{"type":"string"},"disk_gib":{"type":"number"}},` +
		`"required":["instance_type","disk_gib"],"additionalProperties":false}`)
	if err != nil {
		t.Fatal(err)
	}
	known := schemas{testImage: schema}
	withVars := func(v string) *infrav1.TerraformMachine {
		m := machine("")
		m.Spec.Variables = raw(v)
		return m
	}

	_, err = (&TerraformMachine{Schemas: known}).ValidateCreate(ctx, withVars(`{"instance_type":"m5"}`))
	wantInvalid(t, err, false)
	for _, bad := range []struct{ vars, frag string }{
		{`{"instnce_type":"m5"}`, `"instnce_type" is not declared`},
		{`{"disk_gib":"big"}`, `"disk_gib" must be a number`},
	} {
		_, err = (&TerraformMachine{Schemas: known}).ValidateCreate(ctx, withVars(bad.vars))
		wantInvalid(t, err, true, bad.frag, "spec.variables", "io.captf.variables-schema")
		if _, err = (&TerraformMachine{}).ValidateCreate(ctx, withVars(bad.vars)); err != nil {
			t.Errorf("a nil lookup checks nothing: %v", err)
		}
		if _, err = (&TerraformMachine{Schemas: schemas{}}).ValidateCreate(ctx, withVars(bad.vars)); err != nil {
			t.Errorf("an unknown image is allowed: %v", err)
		}
		if _, err = (&TerraformMachine{Schemas: schemas{testImage: nil}}).ValidateCreate(ctx, withVars(bad.vars)); err != nil {
			t.Errorf("an image without a schema is allowed: %v", err)
		}
	}

	// An update that changes neither image nor variables passes whatever the
	// cache now says, so an unrelated write never fails on a stale schema.
	old := withVars(`{"instnce_type":"m5"}`)
	cur := old.DeepCopy()
	cur.Labels = map[string]string{"x": "y"}
	if _, err = (&TerraformMachine{Schemas: known}).ValidateUpdate(userContext("someone"), old, cur); err != nil {
		t.Errorf("unchanged variables on update: %v", err)
	}

	cl := &infrav1.TerraformCluster{}
	cl.Name = "c"
	cl.Spec = infrav1.TerraformClusterSpec{WorkspaceSpec: infrav1.WorkspaceSpec{
		Source: infrav1.Source{Image: testImage}, IdentityRef: infrav1.IdentityReference{Name: "id"}, Variables: raw(`{"zzz":1}`),
	}}
	_, err = (&TerraformCluster{Schemas: known}).ValidateCreate(ctx, cl)
	wantInvalid(t, err, true, `"zzz" is not declared`)
	next := cl.DeepCopy()
	next.Spec.Variables = raw(`{"instance_type":"m5","zzz":1}`)
	_, err = (&TerraformCluster{Schemas: known}).ValidateUpdate(ctx, cl, next)
	wantInvalid(t, err, true, `"zzz" is not declared`)

	p := pool("")
	p.Spec.Variables = raw(`{"nope":1}`)
	_, err = (&TerraformMachinePool{Schemas: known}).ValidateCreate(ctx, p)
	wantInvalid(t, err, true, `"nope" is not declared`)
	p2 := p.DeepCopy()
	p2.Spec.Variables = raw(`{"disk_gib":"x"}`)
	_, err = (&TerraformMachinePool{Schemas: known}).ValidateUpdate(ctx, p, p2)
	wantInvalid(t, err, true, `"disk_gib" must be a number`)

	tpl := &infrav1.TerraformMachineTemplate{}
	tpl.Name = "t"
	tpl.Spec.Template.Spec = machine("").Spec
	tpl.Spec.Template.Spec.Variables = raw(`{"nope":1}`)
	_, err = (&TerraformMachineTemplate{Schemas: known}).ValidateCreate(ctx, tpl)
	wantInvalid(t, err, true, `"nope" is not declared`, "spec.template.spec.variables")
	_, err = (&TerraformMachineTemplate{Schemas: known}).ValidateUpdate(dryRunContext(true), tpl, tpl.DeepCopy())
	wantInvalid(t, err, false)
}
