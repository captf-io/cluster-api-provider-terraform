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

package imageinspect

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// pullSecret returns a dockerconfigjson Secret named n whose auths object
// is the JSON document auths.
func pullSecret(n, auths string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: n},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":` + auths + `}`)},
	}
}

// keychainOf uses t to build and return a Keychain from secrets, in order.
func keychainOf(t *testing.T, secrets ...*corev1.Secret) *Keychain {
	t.Helper()
	k := &Keychain{}
	for _, s := range secrets {
		if err := k.add(s); err != nil {
			t.Fatal(err)
		}
	}
	return k
}

// users uses t to return the usernames of k's candidates for image, in order.
func users(t *testing.T, k *Keychain, image string) []string {
	t.Helper()
	ref, err := name.ParseReference(image)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, a := range k.Candidates(ref.Context()) {
		cfg, err := a.Authorization()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, cfg.Username)
	}
	return out
}

// TestKeychainScopes proves credentials keep their path scope: distinct
// paths under one host get distinct credentials, overlapping prefixes put
// the longest first, a path matches on segment boundaries only, a port
// must match, and wildcard hosts match one DNS label each.
func TestKeychainScopes(t *testing.T) {
	t.Parallel()
	k := keychainOf(t, pullSecret("s", `{
		"registry.example.com/team-a":{"username":"a","password":"p"},
		"registry.example.com/team-b":{"username":"b","password":"p"},
		"registry.example.com/team-a/sub":{"username":"asub","password":"p"},
		"https://registry.example.com":{"username":"host","password":"p"},
		"*.example.com":{"username":"wild","password":"p"},
		"reg.example.com:5000":{"username":"port","password":"p"}}`))
	for _, tt := range []struct {
		image string
		want  []string
	}{
		{"registry.example.com/team-a/app:1", []string{"a", "host", "wild"}},
		{"registry.example.com/team-b/app:1", []string{"b", "host", "wild"}},
		{"registry.example.com/team-a/sub/app:1", []string{"asub", "a", "host", "wild"}},
		{"registry.example.com/team-ab/app:1", []string{"host", "wild"}},
		{"registry.example.com/other:1", []string{"host", "wild"}},
		{"reg.example.com/xx:1", []string{"wild"}},
		{"reg.example.com:5000/xx:1", []string{"port"}},
		{"a.b.example.com/xx:1", nil},
		{"quay.io/xx:1", nil},
	} {
		if got := users(t, k, tt.image); strings.Join(got, ",") != strings.Join(tt.want, ",") {
			t.Errorf("%s: candidates %v, want %v", tt.image, got, tt.want)
		}
	}
}

// TestKeychainDeterministic proves the candidate order is stable across
// builds from the same Secrets: ties go to the earlier Secret, then the
// sorted key, and a wildcard host ranks after an exact one.
func TestKeychainDeterministic(t *testing.T) {
	t.Parallel()
	first := pullSecret("first", `{"r.example.com/x":{"username":"f2","password":"p"},"https://r.example.com/x/":{"username":"f1","password":"p"}}`)
	second := pullSecret("second", `{"r.example.com/x":{"username":"s","password":"p"},"*.example.com/x":{"username":"w","password":"p"}}`)
	const want = "f1,f2,s,w"
	for i := range 50 {
		k := keychainOf(t, first, second)
		if got := strings.Join(users(t, k, "r.example.com/x/y:1"), ","); got != want {
			t.Fatalf("run %d: %s, want %s", i, got, want)
		}
	}
	a, err := keychainOf(t, first, second).Resolve(mustRepo(t, "r.example.com/x/y"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _ := a.Authorization(); cfg.Username != "f1" {
		t.Errorf("Resolve picked %q, want f1", cfg.Username)
	}
}

// mustRepo uses t to parse image and return its repository, failing on a
// parse error.
func mustRepo(t *testing.T, image string) name.Repository {
	t.Helper()
	ref, err := name.ParseReference(image)
	if err != nil {
		t.Fatal(err)
	}
	return ref.Context()
}

// TestFallbackInspector proves FallbackInspector retries the next matching
// credential after a registry answers 401, returns the success, reports
// the first refusal when every credential is refused, and does not retry
// on a non-authentication failure.
func TestFallbackInspector(t *testing.T) {
	t.Parallel()
	reg := registry.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "good" || p != "pw" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	ref, err := name.ParseReference(host+"/team/app:1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, labeled(t, map[string]string{CapacityLabel: `{"cpu":"1"}`}, nil), remote.WithAuth(&authn.Basic{Username: "good", Password: "pw"})); err != nil {
		t.Fatal(err)
	}

	f := FallbackInspector{Inner: Remote{Insecure: true}}
	k := keychainOf(t, pullSecret("s", `{
		"`+host+`/team":{"username":"stale","password":"x"},
		"`+host+`":{"username":"good","password":"pw"}}`))
	cfg, err := f.Config(t.Context(), ref.String(), k, nil)
	if err != nil || cfg.Labels[CapacityLabel] != `{"cpu":"1"}` {
		t.Fatalf("fallback after 401 = %+v, %v", cfg, err)
	}

	allBad := keychainOf(t, pullSecret("s", `{
		"`+host+`/team":{"username":"stale","password":"x"},
		"`+host+`":{"username":"old","password":"y"}}`))
	if _, err := f.Config(t.Context(), ref.String(), allBad, nil); err == nil || !authRefused(err) {
		t.Errorf("all credentials refused = %v, want a 401", err)
	}

	if _, err := f.Config(t.Context(), "Not A Ref", k, nil); err == nil {
		t.Error("invalid reference accepted")
	}
	if _, err := f.Config(t.Context(), ref.String(), authn.NewMultiKeychain(), nil); err == nil {
		t.Error("anonymous pull of an authenticated registry succeeded")
	}
}
