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

package tfstate

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// stateJSON is a version 4 state file with string, sensitive and object
// outputs and one resource.
const stateJSON = `{"version":4,"terraform_version":"1.9.8","serial":7,"lineage":"abc",
"outputs":{"endpoint":{"value":"noop-tc.invalid:6443","type":"string"},
"secret":{"value":"s","sensitive":true},
"exports":{"value":{"backend_id":"noop-backend-1"}}},
"resources":[{"mode":"managed","type":"random_id","name":"x"}]}`

// gz returns the gzip compression of s, failing t on a write error.
func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return b.Bytes()
}

// secret returns a state Secret named name in ns holding data.
func secret(ns, name string, data []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       map[string][]byte{DataKey: data},
	}
}

// TestSuffixFor checks SuffixFor against vectors computed with the product's
// state.Suffix, and its handling of an unknown kind.
func TestSuffixFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ kind, want string }{
		{KindTerraformCluster, "6a911fda117d8ebf-c"},
		{KindTerraformMachine, "f7f0624bd7735fd5-m"},
		{KindTerraformMachinePool, "646e4c3e4bcae995-mp"},
		{"Machine", ""},
	} {
		if got := SuffixFor("e2e-noop-abc", tc.kind, "c1"); got != tc.want {
			t.Errorf("SuffixFor(%s) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// TestSecretName checks the base Secret name.
func TestSecretName(t *testing.T) {
	t.Parallel()
	if got := SecretName("0123456789abcdef-m"); got != "tfstate-default-0123456789abcdef-m" {
		t.Errorf("SecretName = %q", got)
	}
}

// TestReadSingle reads a state held by one Secret.
func TestReadSingle(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset(secret("ns", SecretName("s-c"), gz(t, stateJSON)))
	st, err := Read(context.Background(), kube, "ns", "s-c")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if st.Serial != 7 || st.Lineage != "abc" || st.TerraformVersion != "1.9.8" {
		t.Errorf("header: %+v", st)
	}
	if got := st.Outputs["endpoint"].Value; got != "noop-tc.invalid:6443" {
		t.Errorf("endpoint = %#v", got)
	}
	if !st.Outputs["secret"].Sensitive || st.Outputs["endpoint"].Sensitive {
		t.Errorf("sensitive flags: %+v", st.Outputs)
	}
	exports, _ := st.Outputs["exports"].Value.(map[string]any)
	if exports["backend_id"] != "noop-backend-1" {
		t.Errorf("exports = %#v", exports)
	}
	if len(st.Resources) != 1 {
		t.Errorf("resources = %#v", st.Resources)
	}
}

// TestReadChunked reads a state split over a base Secret and two parts, and
// ignores a stale trailing fragment after the gzip stream.
func TestReadChunked(t *testing.T) {
	t.Parallel()
	all := append(gz(t, stateJSON), []byte("stale-fragment")...)
	a, b, c := all[:10], all[10:25], all[25:]
	kube := fake.NewClientset(
		secret("ns", SecretName("s-c"), a),
		secret("ns", SecretName("s-c")+"-part-1", b),
		secret("ns", SecretName("s-c")+"-part-2", c),
	)
	st, err := Read(context.Background(), kube, "ns", "s-c")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if st.Serial != 7 {
		t.Errorf("serial = %d", st.Serial)
	}
}

// TestReadEmptyOutputs checks that a state without outputs gets an empty,
// non-nil map.
func TestReadEmptyOutputs(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset(secret("ns", SecretName("s-m"), gz(t, `{"version":4,"serial":1}`)))
	st, err := Read(context.Background(), kube, "ns", "s-m")
	if err != nil || st.Outputs == nil || len(st.Outputs) != 0 {
		t.Fatalf("Read = %+v, %v", st, err)
	}
}

// TestReadMissing checks that a missing base Secret wraps ErrNoState.
func TestReadMissing(t *testing.T) {
	t.Parallel()
	_, err := Read(context.Background(), fake.NewClientset(), "ns", "s-c")
	if !errors.Is(err, ErrNoState) {
		t.Fatalf("err = %v, want ErrNoState", err)
	}
}

// TestReadCorrupt checks the corrupt-data errors.
func TestReadCorrupt(t *testing.T) {
	t.Parallel()
	for name, data := range map[string][]byte{
		"not gzip":      []byte("plain text"),
		"truncated":     gz(t, stateJSON)[:12],
		"not JSON":      gz(t, "not json"),
		"empty payload": nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			kube := fake.NewClientset(secret("ns", SecretName("s-c"), data))
			if _, err := Read(context.Background(), kube, "ns", "s-c"); !errors.Is(err, ErrCorrupt) {
				t.Errorf("err = %v, want ErrCorrupt", err)
			}
		})
	}
}

// TestReadMissingKey checks a Secret without the tfstate key.
func TestReadMissingKey(t *testing.T) {
	t.Parallel()
	s := secret("ns", SecretName("s-c"), nil)
	s.Data = map[string][]byte{"other": []byte("x")}
	if _, err := Read(context.Background(), fake.NewClientset(s), "ns", "s-c"); !errors.Is(err, ErrCorrupt) {
		t.Errorf("err = %v, want ErrCorrupt", err)
	}
}

// TestReadTooManyChunks checks the chunk limit.
func TestReadTooManyChunks(t *testing.T) {
	t.Parallel()
	objs := []runtime.Object{secret("ns", SecretName("s-c"), []byte("x"))}
	for i := 1; i <= maxChunks; i++ {
		objs = append(objs, secret("ns", SecretName("s-c")+"-part-"+strconv.Itoa(i), []byte("x")))
	}
	if _, err := Read(context.Background(), fake.NewClientset(objs...), "ns", "s-c"); !errors.Is(err, ErrCorrupt) {
		t.Errorf("err = %v, want ErrCorrupt", err)
	}
}

// TestReadAPIError checks that a non-NotFound API error is wrapped.
func TestReadAPIError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	kube := fake.NewClientset()
	kube.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})
	_, err := Read(context.Background(), kube, "ns", "s-c")
	if !errors.Is(err, boom) || errors.Is(err, ErrNoState) {
		t.Errorf("err = %v, want wrapped boom", err)
	}
}
