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

package state

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// ns is the namespace these tests create objects in.
const ns = "team-a"

// testScheme returns a scheme with the client-go and CAPTF API types
// registered; t fails the test if a scheme registration errors.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, infrav1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return s
}

// TestSuffix proves Suffix is deterministic, collision-free across
// namespace, kind and name, never ends in "-<digits>", always fits a label
// value, and produces valid Secret and Lease names; and that KindShort
// rejects an unknown kind.
func TestSuffix(t *testing.T) {
	t.Parallel()
	numericEnd := regexp.MustCompile(`-[0-9]+$`)
	for _, kind := range []string{KindTerraformCluster, KindTerraformMachine, KindTerraformMachinePool} {
		for _, name := range []string{"a", "prod-md-0-abcde", strings.Repeat("x", 253), "123"} {
			s, err := Suffix(ns, kind, name)
			if err != nil {
				t.Fatalf("Suffix: %v", err)
			}
			if numericEnd.MatchString(s) {
				t.Errorf("Suffix(%s, %s) = %s ends in -<digits>", kind, name, s)
			}
			if len(s) > 63 || len(validation.IsValidLabelValue(s)) != 0 {
				t.Errorf("Suffix %s is not a valid label value", s)
			}
			for _, n := range []string{SecretName(s), SecretName(s) + "-part-99", LeaseName(s)} {
				if errs := validation.IsDNS1123Subdomain(n); len(errs) != 0 {
					t.Errorf("name %s invalid: %v", n, errs)
				}
			}
			again, _ := Suffix(ns, kind, name)
			if again != s {
				t.Errorf("Suffix is not deterministic")
			}
		}
	}
	a, _ := Suffix(ns, KindTerraformMachine, "m")
	for _, other := range [][3]string{{"team-b", KindTerraformMachine, "m"}, {ns, KindTerraformCluster, "m"}, {ns, KindTerraformMachine, "n"}} {
		if b, _ := Suffix(other[0], other[1], other[2]); b == a {
			t.Errorf("Suffix(%v) collides with Suffix(%s,m)", other, ns)
		}
	}
	if !strings.HasSuffix(a, "-m") || len(a) != 18 {
		t.Errorf("Suffix = %s, want 16 hex + -m", a)
	}
	if _, err := Suffix(ns, "Machine", "m"); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("unknown kind: err = %v", err)
	}
	if SecretName("x") != "tfstate-default-x" || LeaseName("x") != "lock-tfstate-default-x" {
		t.Error("SecretName/LeaseName changed")
	}
}

// TestLabels proves LabelValue passes through a name that fits and hashes
// one that does not, and that BackendLabels and Selector produce valid,
// consistent label keys and values.
func TestLabels(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("n", 64)
	if LabelValue("short") != "short" || LabelValue(strings.Repeat("n", 63)) != strings.Repeat("n", 63) {
		t.Error("LabelValue changed a name that fits")
	}
	if v := LabelValue(long); len(v) != 16 || v == LabelValue(long+"x") {
		t.Errorf("LabelValue(64 chars) = %s, want a distinct 16-char hash", v)
	}
	l := BackendLabels(KindTerraformMachine, long, "prod")
	for k, v := range l {
		if errs := validation.IsQualifiedName(k); len(errs) != 0 {
			t.Errorf("label key %s: %v", k, errs)
		}
		if errs := validation.IsValidLabelValue(v); len(errs) != 0 {
			t.Errorf("label %s=%s: %v", k, v, errs)
		}
	}
	if len(l) != 5 || l[ManagedLabel] != "true" || l["clusterctl.cluster.x-k8s.io/move"] != "" || l["cluster.x-k8s.io/cluster-name"] != "prod" {
		t.Errorf("BackendLabels = %v", l)
	}
	if got := Selector("abc-m").String(); got != "tfstate=true,tfstateSecretSuffix=abc-m,tfstateWorkspace=default" {
		t.Errorf("Selector = %s", got)
	}
	if errs := validation.IsQualifiedName(InputsHashAnnotation); len(errs) != 0 {
		t.Errorf("annotation key: %v", errs)
	}
}

// gz returns s gzip-compressed; t fails the test if compression errors.
func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	return b.Bytes()
}

// secret returns a state Secret named name with the backend labels of
// suffix and data under DataKey.
func secret(suffix, name string, data []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{
			BackendStateLabel: "true", BackendSuffixLabel: suffix, BackendWorkspaceLabel: Workspace,
		}},
		Data: map[string][]byte{DataKey: data},
	}
}

// chunked splits the gzip of state into n Secrets of suffix, named like
// Terraform's, and returns them; t is used by gz.
func chunked(t *testing.T, suffix, state string, n int) []client.Object {
	t.Helper()
	payload := gz(t, state)
	size := (len(payload) + n - 1) / n
	var objs []client.Object
	for i := range n {
		end := min((i+1)*size, len(payload))
		name := SecretName(suffix)
		if i > 0 {
			name = fmt.Sprintf("%s-part-%d", name, i)
		}
		objs = append(objs, secret(suffix, name, payload[i*size:end]))
	}
	return objs
}

// stateJSON is a minimal Terraform state v4 document used across these
// tests.
const stateJSON = `{"version":4,"terraform_version":"1.16.4","serial":7,"lineage":"abc","outputs":{"provider_id":{"value":"stub://m","type":"string"},"health":{"value":{"state":"running"},"type":["object",{"state":"string"}],"sensitive":true}},"resources":[]}`

// read builds a fake client seeded with objs and returns the result of
// reading suffix's state from it; t supplies the test scheme.
func read(t *testing.T, suffix string, objs ...client.Object) (*State, error) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	return NewReader(c).Read(context.Background(), ns, suffix)
}

// TestReader proves Read reassembles chunked state in order, decodes
// serial, lineage, version, outputs and the inputs-hash annotation, counts
// managed resources and reports the compressed byte size, and reads only
// the named suffix.
func TestReader(t *testing.T) {
	t.Parallel()
	const s = "0123456789abcdef-m"
	// Twelve chunks: -part-10 and -part-11 must sort after -part-2.
	st, err := read(t, s, chunked(t, s, stateJSON, 12)...)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if st.Serial != 7 || st.Lineage != "abc" || st.TerraformVersion != "1.16.4" || len(st.Secrets) != 12 || st.Secrets[0].Name != SecretName(s) {
		t.Errorf("State = %+v", st)
	}
	// The metadata follows the chunk order, with what a patch locks to.
	if len(st.Metadata) != len(st.Secrets) {
		t.Fatalf("%d metadata for %d Secrets", len(st.Metadata), len(st.Secrets))
	}
	for i, m := range st.Metadata {
		if m.Name != st.Secrets[i].Name || m.ResourceVersion == "" || m.Labels[BackendSuffixLabel] != s {
			t.Errorf("Metadata[%d] = %s (rv %q), want %s", i, m.Name, m.ResourceVersion, st.Secrets[i].Name)
		}
	}
	if !st.Outputs["health"].Sensitive || string(st.Outputs["provider_id"].Value) != `"stub://m"` {
		t.Errorf("outputs = %v", st.Outputs)
	}
	if st.InputsHash != "" {
		t.Errorf("InputsHash = %q, want empty", st.InputsHash)
	}

	empty, err := read(t, s, secret(s, SecretName(s), gz(t, `{"version":4,"serial":1,"lineage":"x","outputs":{}}`)))
	if err != nil || len(empty.Outputs) != 0 || empty.Outputs == nil {
		t.Errorf("empty pre-apply state = %+v (err %v), want an empty outputs map", empty, err)
	}
	noOutputs, err := read(t, s, secret(s, SecretName(s), gz(t, `{"version":4,"serial":1,"lineage":"x"}`)))
	if err != nil || noOutputs.Outputs == nil {
		t.Errorf("state without outputs key = %+v (err %v)", noOutputs, err)
	}

	withHash := secret(s, SecretName(s), gz(t, stateJSON))
	withHash.Annotations = map[string]string{InputsHashAnnotation: "h1:abc"}
	if st, err := read(t, s, withHash); err != nil || st.InputsHash != "h1:abc" {
		t.Errorf("InputsHash = %v (err %v)", st, err)
	}
	// Data sources are not managed resources; a counted resource is one.
	withResources := `{"version":4,"serial":1,"lineage":"x","resources":[` +
		`{"mode":"managed","type":"t","name":"a","instances":[{"index_key":0},{"index_key":1}]},` +
		`{"mode":"data","type":"t","name":"d","instances":[{}]},{"mode":"managed","type":"t","name":"b","instances":[{}]}]}`
	if st, err := read(t, s, secret(s, SecretName(s), gz(t, withResources))); err != nil || st.ManagedResources != 2 || st.Bytes != len(gz(t, withResources)) {
		t.Errorf("state with resources = %+v (err %v), want 2 managed resources", st, err)
	}
	if st.ManagedResources != 0 || st.Bytes != len(gz(t, stateJSON)) {
		t.Errorf("chunked: managed %d, bytes %d", st.ManagedResources, st.Bytes)
	}
	// Other suffixes and workspaces are not read.
	other := secret("ffffffffffffffff-m", SecretName("ffffffffffffffff-m"), gz(t, stateJSON))
	if _, err := read(t, s, other); !errors.Is(err, ErrNoState) {
		t.Errorf("other suffix only: err = %v, want ErrNoState", err)
	}
}

// TestParseLimit: a gzip bomb (a small payload that expands past the limit)
// is corrupt state, not a read that exhausts the manager's memory. Anyone who
// can write Secrets in the namespace, or any module image, can plant one.
func TestParseLimit(t *testing.T) {
	t.Parallel()
	const limit = 64 << 10
	valid := gz(t, stateJSON)
	if _, err := parse(valid, len(stateJSON)); err != nil {
		t.Errorf("state exactly at the limit: %v", err)
	}
	bomb := gz(t, strings.Repeat("\x00", 16*limit))
	if len(bomb) > limit/8 {
		t.Fatalf("bomb is %d bytes; not a bomb", len(bomb))
	}
	if _, err := parse(bomb, limit); !errors.Is(err, ErrStateCorrupt) || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("bomb: err = %v, want ErrStateCorrupt (exceeds)", err)
	}
}

// TestReaderStaleTrailingChunk: item 12. A shrinking Put() that fails to
// delete a surplus -part-N Secret leaves it stale but contiguous (no gap):
// its leftover bytes are a fragment of the old, larger gzip stream, not a
// second member. The reader must still recover the real, complete state and
// ignore the trailing garbage, not report StateCorrupt.
func TestReaderStaleTrailingChunk(t *testing.T) {
	t.Parallel()
	const s = "0123456789abcdef-m"
	base := secret(s, SecretName(s), gz(t, stateJSON))
	// A believable leftover: a slice from the middle of some other, larger
	// compressed stream (not itself a valid gzip member). Random bytes barely
	// compress, so the gzip of them is long enough to slice from safely.
	oldData := make([]byte, 4096)
	if _, err := rand.Read(oldData); err != nil {
		t.Fatalf("rand: %v", err)
	}
	old := gz(t, string(oldData))
	stale := secret(s, SecretName(s)+"-part-1", old[100:400])
	st, err := read(t, s, base, stale)
	if err != nil || st.Serial != 7 || st.Lineage != "abc" || len(st.Secrets) != 2 {
		t.Fatalf("Read with a stale trailing chunk = %+v (err %v)", st, err)
	}
}

// TestReaderErrors proves Read reports the right sentinel error for every
// way a chunk set or its payload can be wrong (missing, gapped, malformed
// names, missing data key, not gzip, truncated, not JSON, encrypted, an
// unsupported version, or too many chunks), and wraps a List failure.
func TestReaderErrors(t *testing.T) {
	t.Parallel()
	const s = "0123456789abcdef-m"
	parts := chunked(t, s, stateJSON, 3)
	cases := []struct {
		name string
		objs []client.Object
		want error
	}{
		{"none", nil, ErrNoState},
		{"gap", []client.Object{parts[0], parts[2]}, ErrStateInconsistent},
		{"missing base", []client.Object{parts[1], parts[2]}, ErrStateInconsistent},
		{"odd name", []client.Object{parts[0], secret(s, SecretName(s)+"-part-x", nil)}, ErrStateInconsistent},
		{"zero-padded index", []client.Object{parts[0], secret(s, SecretName(s)+"-part-01", nil)}, ErrStateInconsistent},
		{"foreign name", []client.Object{secret(s, "other", nil)}, ErrStateInconsistent},
		{"no data key", []client.Object{&corev1.Secret{ObjectMeta: secret(s, SecretName(s), nil).ObjectMeta}}, ErrStateInconsistent},
		{"not gzip", []client.Object{secret(s, SecretName(s), []byte(stateJSON))}, ErrStateCorrupt},
		{"truncated gzip", []client.Object{parts[0]}, ErrStateCorrupt},
		{"not JSON", []client.Object{secret(s, SecretName(s), gz(t, "not json"))}, ErrStateCorrupt},
		{"encrypted", []client.Object{secret(s, SecretName(s), gz(t, `{"meta":{},"encrypted_data":"eA==","encryption_version":"v0"}`))}, ErrStateEncrypted},
		{"version 3", []client.Object{secret(s, SecretName(s), gz(t, `{"version":3,"serial":1}`))}, ErrUnsupportedStateVersion},
		{"no version", []client.Object{secret(s, SecretName(s), gz(t, `{"serial":1}`))}, ErrUnsupportedStateVersion},
	}
	// More chunks than any real state needs.
	tooMany := []client.Object{secret(s, SecretName(s), nil)}
	for i := 1; i <= MaxChunks; i++ {
		tooMany = append(tooMany, secret(s, fmt.Sprintf("%s-part-%d", SecretName(s), i), nil))
	}
	cases = append(cases,
		struct {
			name string
			objs []client.Object
			want error
		}{"too many chunks", tooMany, ErrStateCorrupt},
	)
	for _, c := range cases {
		if _, err := read(t, s, c.objs...); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	failing := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("boom")
		},
	}).Build()
	if _, err := NewReader(failing).Read(context.Background(), ns, s); err == nil || errors.Is(err, ErrNoState) {
		t.Errorf("list failure: err = %v", err)
	}
}

// TestAdoptAndCleanup proves Adopt is idempotent, sets the ownerReference
// and inputs-hash annotation on the base Secret only, reports ErrNoState
// for an unknown suffix, and that Cleanup removes every state Secret and
// the lock Lease and tolerates being called again.
func TestAdoptAndCleanup(t *testing.T) {
	t.Parallel()
	const s = "0123456789abcdef-m"
	owner := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: ns, UID: "uid-1"}}
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: LeaseName(s), Namespace: ns}}
	objs := append(chunked(t, s, stateJSON, 3), owner, lease)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	ctx := context.Background()

	for range 2 { // idempotent
		if err := Adopt(ctx, c, owner, s, "h1:first"); err != nil {
			t.Fatalf("Adopt: %v", err)
		}
	}
	if err := Adopt(ctx, c, owner, s, "h1:second"); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	var list corev1.SecretList
	if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, sec := range list.Items {
		refs := sec.OwnerReferences
		if len(refs) != 1 || refs[0].UID != "uid-1" || refs[0].Kind != "TerraformMachine" || refs[0].BlockOwnerDeletion != nil {
			t.Errorf("%s ownerRefs = %+v", sec.Name, refs)
		}
		_, has := sec.Annotations[InputsHashAnnotation]
		if has != (sec.Name == SecretName(s)) {
			t.Errorf("%s: inputs hash annotation present = %v", sec.Name, has)
		}
		// Only the base Secret is protected: Terraform deletes surplus parts.
		if Protected(&sec.ObjectMeta) != (sec.Name == SecretName(s)) {
			t.Errorf("%s: finalizers = %v", sec.Name, sec.Finalizers)
		}
	}
	// Cleanup takes the protected backups too.
	if _, _, err := TakeBackup(ctx, c, backupOpts(owner, s, backupT0)); err != nil {
		t.Fatalf("TakeBackup: %v", err)
	}
	if h, ok := HasInputsHash(list.Items, s); !ok || h != "h1:second" {
		t.Errorf("HasInputsHash = %q, %v", h, ok)
	}
	if _, ok := HasInputsHash(nil, s); ok {
		t.Error("HasInputsHash(nil) = true")
	}
	if err := Adopt(ctx, c, owner, "ffffffffffffffff-m", "h1:x"); !errors.Is(err, ErrNoState) {
		t.Errorf("Adopt without state: err = %v", err)
	}

	for range 2 { // NotFound tolerated
		if err := Cleanup(ctx, c, ns, s); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}
	}
	if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil || len(list.Items) != 0 {
		t.Errorf("Secrets after cleanup = %d (err %v)", len(list.Items), err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(lease), &coordinationv1.Lease{}); err == nil {
		t.Error("Lease survived cleanup")
	}
}
