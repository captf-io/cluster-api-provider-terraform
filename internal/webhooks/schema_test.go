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
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/imageinspect"
	"github.com/captf-io/cluster-api-provider-terraform/internal/varschema"
)

// schemas is a SchemaLookup over a fixed map; an absent image is unknown.
type schemas map[string]*varschema.Schema

// Cached returns the schema of ref and whether the image is known.
func (s schemas) Cached(_, ref string) (*varschema.Schema, bool) {
	v, ok := s[ref]
	return v, ok
}

// Fetch returns no schema and an error: the registry of these tests is
// unreachable.
func (s schemas) Fetch(context.Context, string, string, []string) (*varschema.Schema, error) {
	return nil, errors.New("registry unreachable")
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

// fakeInspector serves one image config and counts the reads.
type fakeInspector struct {
	cfg   *imageinspect.Config
	err   error
	calls atomic.Int32
	// keychain is the keychain of the last read.
	keychain authn.Keychain
}

// Config records k and the call, and returns the canned config or error.
func (f *fakeInspector) Config(_ context.Context, _ string, k authn.Keychain, _ *v1.Platform) (*imageinspect.Config, error) {
	f.calls.Add(1)
	f.keychain = k
	return f.cfg, f.err
}

// TestSchemaFetchOnMiss proves a replica whose cache is empty reads the
// registry itself and rejects invalid variables (so admission does not
// depend on which replica is the leader), caches the schema, reads each
// namespace's own pull Secret, and, on a registry failure, admits the
// object and remembers the failure so the next admission does not read the
// registry again.
func TestSchemaFetchOnMiss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	label := `{"type":"object","properties":{"instance_type":{"type":"string"}},"additionalProperties":false}`
	insp := &fakeInspector{cfg: &imageinspect.Config{Digest: "sha256:aa", Labels: map[string]string{imageinspect.VariablesSchemaLabel: label}}}
	f := SchemaFetcher{Cache: imageinspect.NewSchemaCache(), Inspector: insp, Reader: fake.NewClientBuilder().Build()}
	m := machine("")
	m.Spec.Variables = raw(`{"nope":1}`)

	_, err := (&TerraformMachine{Schemas: f}).ValidateCreate(ctx, m)
	wantInvalid(t, err, true, `"nope" is not declared`)
	_, err = (&TerraformMachine{Schemas: f}).ValidateCreate(ctx, m)
	wantInvalid(t, err, true, `"nope" is not declared`)
	if insp.calls.Load() != 1 {
		t.Errorf("registry reads = %d, want 1: the second admission is served from the cache", insp.calls.Load())
	}
	other := m.DeepCopy()
	other.Namespace = "tenant-b"
	if _, ok := f.Cached("tenant-b", other.Spec.Source.Image); ok {
		t.Error("another namespace must not be answered from the first one's read")
	}

	down := &fakeInspector{err: errors.New("registry down")}
	f = SchemaFetcher{Cache: imageinspect.NewSchemaCache(), Inspector: down, Reader: fake.NewClientBuilder().Build()}
	for range 3 {
		if _, err := (&TerraformMachine{Schemas: f}).ValidateCreate(ctx, m); err != nil {
			t.Fatalf("an unreadable image is not checkable and must be admitted: %v", err)
		}
	}
	if down.calls.Load() != 1 {
		t.Errorf("registry reads after a failure = %d, want 1: the failure is remembered", down.calls.Load())
	}
}

// TestSchemaFetchPullSecret proves the read authenticates with the pull
// Secrets the object's own jobs name, from its own namespace.
func TestSchemaFetchPullSecret(t *testing.T) {
	t.Parallel()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pull"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"ghcr.io":{"username":"u","password":"p"}}}`)},
	}
	insp := &fakeInspector{cfg: &imageinspect.Config{Digest: "sha256:aa"}}
	f := SchemaFetcher{Cache: imageinspect.NewSchemaCache(), Inspector: insp, Reader: fake.NewClientBuilder().WithObjects(sec).Build()}
	if _, err := f.Fetch(context.Background(), "ns", testImage, []string{"pull"}); err != nil {
		t.Fatal(err)
	}
	k, ok := insp.keychain.(*imageinspect.Keychain)
	if !ok || len(k.Candidates(name.MustParseReference(testImage).Context())) != 1 {
		t.Errorf("keychain = %#v, want the namespace's pull Secret", insp.keychain)
	}
}
