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

package shared

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/hash"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// secretValue is a variable value that must never reach a message.
const secretValue = "s3cr3t-value"

// varsLabel is the label a ConfigMap or Secret needs to be read as a
// variables source.
var varsLabel = map[string]string{infrav1.VariablesSourceLabel: "true"}

// configMap returns a ConfigMap named name with labels and data.
func configMap(name string, labels map[string]string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name, Labels: labels}, Data: data}
}

// varSecret returns a Secret named name with labels, its string data
// converted to Secret bytes.
func varSecret(name string, labels map[string]string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name, Labels: labels}, Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

// cmRef returns a VariablesSource that references the ConfigMap named name.
func cmRef(name string) infrav1.VariablesSource {
	return infrav1.VariablesSource{ConfigMapRef: infrav1.VariablesSourceReference{Name: name}}
}

// secretRef returns a VariablesSource that references the Secret named
// name.
func secretRef(name string) infrav1.VariablesSource {
	return infrav1.VariablesSource{SecretRef: infrav1.VariablesSourceReference{Name: name}}
}

// resolve returns the variables, gate and error ResolveVariables computes
// for spec and role against a fake client seeded with objs, failing t on
// error.
func resolve(t *testing.T, objs []client.Object, role contract.Role, spec VariablesSpec) (contract.Variables, *Gate, error) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	return ResolveVariables(t.Context(), c, testNS, role, spec)
}

// TestResolveVariablesMerge: sources in list order, a later one winning;
// inline over all; String passes strings, JSON parses; a variable is
// sensitive exactly when its winning value came from a Secret.
func TestResolveVariablesMerge(t *testing.T) {
	t.Parallel()
	objs := []client.Object{
		configMap("base", varsLabel, map[string]string{"instance_type": "t3.large", "disk_gib": "20", "region": "eu-west-1", "password": "from-cm"}),
		varSecret("creds", varsLabel, map[string]string{"password": secretValue, "token": "tok", "region": "us-east-1"}),
		configMap("json", varsLabel, map[string]string{"subnet_ids": `["a","b"]`, "token": `"public"`, "ratio": "0.5"}),
	}
	jsonSrc := cmRef("json")
	jsonSrc.Format = infrav1.VariablesFormatJSON
	vars, gate, err := resolve(t, objs, contract.RoleMachine, VariablesSpec{
		From:   []infrav1.VariablesSource{cmRef("base"), secretRef("creds"), jsonSrc},
		Inline: runtime.RawExtension{Raw: []byte(`{"instance_type":"m5.xlarge","tags":{"team":"a"}}`)},
	})
	if err != nil || gate != nil {
		t.Fatalf("ResolveVariables: %v, %+v", err, gate)
	}
	want := map[string]struct {
		value     string
		sensitive bool
	}{
		"instance_type": {`"m5.xlarge"`, false}, // inline wins over base
		"disk_gib":      {`"20"`, false},        // String format: a string
		"region":        {`"us-east-1"`, true},  // the Secret wins over base
		"password":      {`"` + secretValue + `"`, true},
		"token":         {`"public"`, false},  // the later ConfigMap wins over the Secret
		"subnet_ids":    {`["a","b"]`, false}, // JSON format: parsed
		"ratio":         {`0.5`, false},
		"tags":          {`{"team":"a"}`, false},
	}
	if got := vars.Names(); len(got) != len(want) {
		t.Errorf("names = %v", got)
	}
	for name, w := range want {
		v, ok := vars[name]
		if !ok || string(v.Value) != w.value || v.Sensitive != w.sensitive {
			t.Errorf("%s = %s (sensitive %v), want %s (sensitive %v)", name, v.Value, v.Sensitive, w.value, w.sensitive)
		}
	}
}

// TestResolveVariablesNone proves ResolveVariables returns no variables and
// no gate when the spec names nothing, and that an optional source that is
// missing or unlabeled contributes nothing (also no gate).
func TestResolveVariablesNone(t *testing.T) {
	t.Parallel()
	vars, gate, err := resolve(t, nil, contract.RoleCluster, VariablesSpec{})
	if vars != nil || gate != nil || err != nil {
		t.Errorf("no variables: %v, %+v, %v", vars, gate, err)
	}
	// An optional source that is missing or unlabeled contributes nothing.
	optional := func(s infrav1.VariablesSource) infrav1.VariablesSource { s.Optional = new(true); return s }
	vars, gate, err = resolve(t, []client.Object{configMap("unlabeled", nil, map[string]string{"a": "1"})}, contract.RoleCluster,
		VariablesSpec{From: []infrav1.VariablesSource{optional(cmRef("missing")), optional(cmRef("unlabeled")), optional(secretRef("gone"))}})
	if vars != nil || gate != nil || err != nil {
		t.Errorf("optional sources: %v, %+v, %v", vars, gate, err)
	}
}

// TestResolveVariablesGates: a missing or unlabeled source is
// VariablesSourceNotFound; a bad key or value is VariablesInvalid. Every
// message names the source and key and never a value.
func TestResolveVariablesGates(t *testing.T) {
	t.Parallel()
	jsonRef := func(s infrav1.VariablesSource) infrav1.VariablesSource {
		s.Format = infrav1.VariablesFormatJSON
		return s
	}
	binary := configMap("bin", varsLabel, nil)
	binary.BinaryData = map[string][]byte{"blob": {0xff, 0xfe}}
	objs := []client.Object{
		configMap("unlabeled", map[string]string{infrav1.VariablesSourceLabel: "false"}, map[string]string{"a": "1"}),
		configMap("dotted", varsLabel, map[string]string{"has.dot": secretValue}),
		varSecret("reserved", varsLabel, map[string]string{"bootstrap_data": secretValue}),
		varSecret("badjson", varsLabel, map[string]string{"db": `{"password":"` + secretValue}),
		binary,
	}
	for _, tt := range []struct {
		name   string
		role   contract.Role
		spec   VariablesSpec
		reason string
		frags  []string
	}{
		{"missing", contract.RoleCluster, VariablesSpec{From: []infrav1.VariablesSource{cmRef("nope")}},
			infrav1.VariablesSourceNotFoundReason, []string{"ConfigMap nope (spec.variablesFrom[0]) not found"}},
		{"missing secret", contract.RoleCluster, VariablesSpec{From: []infrav1.VariablesSource{cmRef("dotted"), secretRef("nope")}},
			infrav1.VariablesInvalidReason, []string{"ConfigMap dotted (spec.variablesFrom[0])", `"has.dot"`}},
		{"unlabeled", contract.RoleCluster, VariablesSpec{From: []infrav1.VariablesSource{cmRef("unlabeled")}},
			infrav1.VariablesSourceNotFoundReason, []string{"does not carry the label captf.io/variables=true"}},
		{"reserved key", contract.RoleMachine, VariablesSpec{From: []infrav1.VariablesSource{secretRef("reserved")}},
			infrav1.VariablesInvalidReason, []string{"Secret reserved (spec.variablesFrom[0])", "bootstrap_data", "contract input of the machine role"}},
		{"reserved only for its role", contract.RoleCluster, VariablesSpec{From: []infrav1.VariablesSource{secretRef("reserved"), cmRef("nope")}},
			infrav1.VariablesSourceNotFoundReason, []string{"spec.variablesFrom[1]"}},
		{"invalid JSON", contract.RoleCluster, VariablesSpec{From: []infrav1.VariablesSource{jsonRef(secretRef("badjson"))}},
			infrav1.VariablesInvalidReason, []string{`key "db" is not valid JSON (format JSON)`}},
		{"not UTF-8", contract.RoleCluster, VariablesSpec{From: []infrav1.VariablesSource{cmRef("bin")}},
			infrav1.VariablesInvalidReason, []string{`key "blob" is not UTF-8`}},
		{"inline not an object", contract.RoleCluster, VariablesSpec{Inline: runtime.RawExtension{Raw: []byte(`["` + secretValue + `"]`)}},
			infrav1.VariablesInvalidReason, []string{"spec.variables: variables must be a JSON object"}},
		{"inline reserved", contract.RoleCluster, VariablesSpec{Inline: runtime.RawExtension{Raw: []byte(`{"captf_x":"` + secretValue + `"}`)}},
			infrav1.VariablesInvalidReason, []string{"spec.variables", "captf_x"}},
	} {
		_, gate, err := resolve(t, objs, tt.role, tt.spec)
		if err != nil || gate == nil || gate.Reason != tt.reason || gate.Status != metav1.ConditionFalse {
			t.Errorf("%s: gate %+v, err %v; want %s", tt.name, gate, err, tt.reason)
			continue
		}
		for _, f := range tt.frags {
			if !strings.Contains(gate.Message, f) {
				t.Errorf("%s: message %q does not mention %q", tt.name, gate.Message, f)
			}
		}
		if strings.Contains(gate.Message, secretValue) {
			t.Errorf("%s: message carries a value: %q", tt.name, gate.Message)
		}
	}
}

// TestResolveVariablesReadError proves ResolveVariables returns the
// client's read error and no gate when a source's Get fails.
func TestResolveVariablesReadError(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("etcd unavailable")
		},
	}).Build()
	if _, gate, err := ResolveVariables(t.Context(), c, testNS, contract.RoleCluster,
		VariablesSpec{From: []infrav1.VariablesSource{secretRef("s")}}); err == nil || gate != nil {
		t.Errorf("read error: gate %+v, err %v", gate, err)
	}
}

// TestVariablesSourceIndexAndMappers: a labeled source enqueues exactly the
// objects in its namespace that name it, of the same kind; machines only
// until they are provisioned.
func TestVariablesSourceIndexAndMappers(t *testing.T) {
	t.Parallel()
	tc := func(ns, name string, from ...infrav1.VariablesSource) *infrav1.TerraformCluster {
		return &infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: infrav1.TerraformClusterSpec{WorkspaceSpec: infrav1.WorkspaceSpec{VariablesFrom: from}}}
	}
	tm := func(name string, provisioned bool, from ...infrav1.VariablesSource) *infrav1.TerraformMachine {
		m := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name}, Spec: infrav1.TerraformMachineSpec{WorkspaceSpec: infrav1.WorkspaceSpec{VariablesFrom: from}}}
		if provisioned {
			m.Status.Initialization.Provisioned = new(true)
		}
		return m
	}
	tp := func(name string, provisioned bool, from ...infrav1.VariablesSource) *infrav1.TerraformMachinePool {
		p := &infrav1.TerraformMachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name}, Spec: infrav1.TerraformMachinePoolSpec{WorkspaceSpec: infrav1.WorkspaceSpec{VariablesFrom: from}}}
		if provisioned {
			p.Status.Initialization.Provisioned = new(true)
		}
		return p
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(
			tc(testNS, "uses-cm", cmRef("vars")),
			tc(testNS, "uses-secret", secretRef("vars")),
			tc(testNS, "uses-both", secretRef("other"), cmRef("vars")),
			tc(testNS, "uses-none"),
			tc("other-ns", "uses-cm", cmRef("vars")),
			tm("m-new", false, cmRef("vars")),
			tm("m-done", true, cmRef("vars")),
			tm("m-secret", false, secretRef("vars")),
			tm("m-none", false),
			tp("p-new", false, cmRef("vars")),
			tp("p-done", true, cmRef("vars")),
			tp("p-secret", false, secretRef("vars")),
		).
		WithIndex(&infrav1.TerraformCluster{}, VariablesSourceIndex, ClusterVariablesSourceIndexer).
		WithIndex(&infrav1.TerraformMachine{}, VariablesSourceIndex, MachineVariablesSourceIndexer).
		WithIndex(&infrav1.TerraformMachinePool{}, VariablesSourceIndex, PoolVariablesSourceIndexer).Build()

	cm := configMap("vars", varsLabel, nil)
	sec := varSecret("vars", varsLabel, nil)
	if got := names(VariablesSourceToClusters(c)(t.Context(), cm)); !slices.Equal(got, []string{testNS + "/uses-both", testNS + "/uses-cm"}) {
		t.Errorf("ConfigMap -> clusters = %v", got)
	}
	if got := names(VariablesSourceToClusters(c)(t.Context(), sec)); !slices.Equal(got, []string{testNS + "/uses-secret"}) {
		t.Errorf("Secret -> clusters = %v", got)
	}
	if got := names(VariablesSourceToMachines(c)(t.Context(), cm)); !slices.Equal(got, []string{testNS + "/m-new"}) {
		t.Errorf("ConfigMap -> machines = %v", got)
	}
	if got := names(VariablesSourceToMachines(c)(t.Context(), sec)); !slices.Equal(got, []string{testNS + "/m-secret"}) {
		t.Errorf("Secret -> machines = %v", got)
	}
	// A pool is mutable: provisioned or not, a changed source re-applies it.
	if got := names(VariablesSourceToPools(c)(t.Context(), cm)); !slices.Equal(got, []string{testNS + "/p-done", testNS + "/p-new"}) {
		t.Errorf("ConfigMap -> pools = %v", got)
	}
	if got := names(VariablesSourceToPools(c)(t.Context(), sec)); !slices.Equal(got, []string{testNS + "/p-secret"}) {
		t.Errorf("Secret -> pools = %v", got)
	}
	other := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "vars"}}
	if VariablesSourceToClusters(c)(t.Context(), other) != nil || VariablesSourceToMachines(c)(t.Context(), other) != nil ||
		VariablesSourceToPools(c)(t.Context(), other) != nil {
		t.Error("a non-source object maps to requests")
	}
	if ClusterVariablesSourceIndexer(&infrav1.TerraformMachine{}) != nil || MachineVariablesSourceIndexer(&infrav1.TerraformCluster{}) != nil ||
		PoolVariablesSourceIndexer(&infrav1.TerraformMachine{}) != nil {
		t.Error("indexer accepted the wrong kind")
	}
	if got := VariablesSourceKeys([]infrav1.VariablesSource{cmRef("a"), secretRef("b"), {}}); !slices.Equal(got, []string{"ConfigMap/a", "Secret/b"}) {
		t.Errorf("keys = %v", got)
	}
}

// TestVariablesSourceWatches proves VariablesSourceWatches returns no
// watches when Deps carries no VariablesCache, and returns one watch each
// for ConfigMap and Secret when it does.
func TestVariablesSourceWatches(t *testing.T) {
	t.Parallel()
	mapFn := VariablesSourceToClusters(fake.NewClientBuilder().WithScheme(testScheme(t)).Build())
	if got := VariablesSourceWatches(Deps{}, mapFn); got != nil {
		t.Errorf("watches without a cache = %v", got)
	}
	if got := VariablesSourceWatches(Deps{VariablesCache: &informertest.FakeInformers{}}, mapFn); len(got) != 2 {
		t.Errorf("watches = %d, want ConfigMap and Secret", len(got))
	}
}

// varKind is a fakeKind whose BuildInputs resolves the object's variables
// against the env's client, as the real adapters do, and counts the calls.
type varKind struct {
	*fakeKind
	c     client.Reader
	calls int
}

// BuildInputs counts the call in k.calls, resolves k's object's variables
// against k.c using ctx, and returns MachineInputs carrying them, or the
// gate or error ResolveVariables reports.
func (k *varKind) BuildInputs(ctx context.Context, _ OwnerInfo, _ *inputs.Durable) (any, *Gate, error) {
	k.calls++
	vars, gate, err := ResolveVariables(ctx, k.c, testNS, contract.RoleMachine,
		VariablesSpec{Inline: k.obj.Spec.Variables, From: k.obj.Spec.VariablesFrom})
	if err != nil || gate != nil {
		return nil, gate, err
	}
	in := machineIn()
	in.Variables = vars
	return in, nil, nil
}

// withVarsFrom sets m's spec.variablesFrom to the "vars" ConfigMap.
func withVarsFrom(m *infrav1.TerraformMachine) {
	m.Spec.VariablesFrom = []infrav1.VariablesSource{cmRef("vars")}
}

// appliedHash returns the inputs hash an apply with instance_type=value
// records, failing t on error.
func appliedHash(t *testing.T, value string) string {
	t.Helper()
	in := machineIn()
	in.Variables = contract.Variables{"instance_type": {Value: []byte(`"` + value + `"`)}}
	h, err := hash.Inputs(contract.RoleMachine, "registry.example/mod:1.0", in)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// createdApplies returns the names of e's created Jobs that are applies.
func createdApplies(e *env) []string {
	return slices.DeleteFunc(slices.Clone(e.runner.created), func(n string) bool { return !strings.Contains(n, "-apply-") })
}

// TestReconcileMutableReappliesOnSourceChange: a mutable kind (the
// TerraformCluster) hashes its variables, so changing a referenced
// ConfigMap's value re-applies; the same value does not.
func TestReconcileMutableReappliesOnSourceChange(t *testing.T) {
	t.Parallel()
	cm := configMap("vars", varsLabel, map[string]string{"instance_type": "t3.large"})
	// Drift and refresh just ran, so the first pass has nothing to do.
	checked := func(m *infrav1.TerraformMachine) {
		now := metav1.NewTime(t0)
		m.Status.LastDriftCheck, m.Status.LastRefresh = &now, &now
		m.Status.Initialization.Provisioned = new(true)
	}
	e := newEnv(t, world(machine(withFinalizer, notPaused, withVarsFrom, checked), cm)...)
	e.state.st = &state.State{InputsHash: appliedHash(t, "t3.large")}
	k := &varKind{fakeKind: e.kindFor(t, readyOwner), c: e.c}
	k.mutable, k.asCluster = true, true
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	if _, err := Reconcile(t.Context(), e.d, k); err != nil {
		t.Fatal(err)
	}
	if got := createdApplies(e); len(got) != 0 {
		t.Fatalf("unchanged variables started %v", got)
	}

	cm.Data["instance_type"] = "t3.xlarge"
	if err := e.c.Update(t.Context(), cm); err != nil {
		t.Fatal(err)
	}
	k.fakeKind = e.kindFor(t, readyOwner)
	k.mutable, k.asCluster = true, true
	if _, err := Reconcile(t.Context(), e.d, k); err != nil {
		t.Fatal(err)
	}
	applies := createdApplies(e)
	if len(applies) != 1 {
		t.Fatalf("created %v, want one apply for the changed ConfigMap", e.runner.created)
	}
	var job = e.runner.jobs[len(e.runner.jobs)-1]
	if h := job.Annotations[state.InputsHashAnnotation]; h != appliedHash(t, "t3.xlarge") {
		t.Errorf("apply hash = %s, want the new variables' hash", h)
	}
	if e.rec.count(EventInputsChanged) != 1 {
		t.Errorf("events = %v, want one InputsChanged", e.rec.reasons)
	}
}

// TestReconcileProvisionedMachineIgnoresSourceChange: an immutable kind
// never builds inputs after provisioning, so a changed (or even deleted)
// source neither re-applies nor gates it.
func TestReconcileProvisionedMachineIgnoresSourceChange(t *testing.T) {
	t.Parallel()
	provisioned := func(m *infrav1.TerraformMachine) { m.Status.Initialization.Provisioned = new(true) }
	e := newEnv(t, world(machine(withFinalizer, notPaused, withVarsFrom, provisioned))...) // the ConfigMap is gone
	e.state.st = &state.State{InputsHash: appliedHash(t, "t3.large")}
	k := &varKind{fakeKind: e.kindFor(t, readyOwner), c: e.c}
	k.health = &contract.Health{State: contract.HealthRunning, Healthy: true}
	if _, err := Reconcile(t.Context(), e.d, k); err != nil {
		t.Fatal(err)
	}
	if k.calls != 0 || len(createdApplies(e)) != 0 {
		t.Errorf("BuildInputs calls %d, applies %v; want none", k.calls, createdApplies(e))
	}
	if c := conditions.Get(e.get(t), infrav1.DependenciesReadyCondition); c != nil && c.Reason == infrav1.VariablesSourceNotFoundReason {
		t.Errorf("DependenciesReady = %+v", c)
	}
}

// TestReconcileMissingSourceGates: before provisioning a missing source is
// DependenciesReady False/VariablesSourceNotFound and no Job starts; once
// it exists, the apply renders its values.
func TestReconcileMissingSourceGates(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(withFinalizer, notPaused, withVarsFrom))...)
	k := &varKind{fakeKind: e.kindFor(t, readyOwner), c: e.c}
	if _, err := Reconcile(t.Context(), e.d, k); err != nil {
		t.Fatal(err)
	}
	c := conditions.Get(e.get(t), infrav1.DependenciesReadyCondition)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != infrav1.VariablesSourceNotFoundReason || len(e.runner.created) != 0 {
		t.Fatalf("DependenciesReady = %+v, created %v", c, e.runner.created)
	}

	if err := e.c.Create(t.Context(), configMap("vars", varsLabel, map[string]string{"instance_type": "t3.large"})); err != nil {
		t.Fatal(err)
	}
	k.fakeKind = e.kindFor(t, readyOwner)
	if _, err := Reconcile(t.Context(), e.d, k); err != nil {
		t.Fatal(err)
	}
	if len(createdApplies(e)) != 1 {
		t.Fatalf("created %v, want the first apply", e.runner.created)
	}
	durable, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(durable.Files.TFVars), `"instance_type": "t3.large"`) || !strings.Contains(string(durable.Files.MainTF), `"instance_type": "${var.instance_type}"`) {
		t.Errorf("durable inputs lack the variable:\n%s\n%s", durable.Files.MainTF, durable.Files.TFVars)
	}
}

// TestReconcileVariablesTooLarge: variables count toward the rendered
// inputs limit; the apply is refused with InputsTooLarge and the size
// still reaches captf_inputs_bytes.
func TestReconcileVariablesTooLarge(t *testing.T) {
	t.Parallel()
	big := configMap("vars", varsLabel, map[string]string{"blob": strings.Repeat("x", 1_000_001)})
	e := newEnv(t, world(machine(withFinalizer, notPaused, withVarsFrom), big)...)
	k := &varKind{fakeKind: e.kindFor(t, readyOwner), c: e.c}
	if _, err := Reconcile(t.Context(), e.d, k); err != nil {
		t.Fatal(err)
	}
	c := conditions.Get(e.get(t), infrav1.ApplyJobSucceededCondition)
	if c == nil || c.Reason != infrav1.InputsTooLargeReason || !strings.Contains(c.Message, "variables") || len(e.runner.created) != 0 {
		t.Errorf("ApplyJobSucceeded = %+v, created %v", c, e.runner.created)
	}
	if strings.Contains(c.Message, "xxxx") {
		t.Error("the condition carries a value")
	}
}
