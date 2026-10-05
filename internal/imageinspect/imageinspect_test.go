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
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestParseCapacity proves ParseCapacity decodes a valid capacity label and
// returns ErrInvalidLabel for an empty object, a non-string quantity, an
// unparsable quantity, a non-object document, trailing data or an empty
// resource name.
func TestParseCapacity(t *testing.T) {
	t.Parallel()
	got, err := ParseCapacity(`{"cpu":"4","memory":"16Gi","nvidia.com/gpu":"1"}`)
	if err != nil || len(got) != 3 || !got.Memory().Equal(resource.MustParse("16Gi")) || got.Cpu().Value() != 4 {
		t.Fatalf("ParseCapacity = %v, %v", got, err)
	}
	for _, bad := range []string{`{}`, `{"cpu":4}`, `{"cpu":"four"}`, `[]`, `{"cpu":"1"} x`, `{"":"1"}`} {
		if _, err := ParseCapacity(bad); !errors.Is(err, ErrInvalidLabel) {
			t.Errorf("ParseCapacity(%s) = %v, want ErrInvalidLabel", bad, err)
		}
	}
}

// TestParseNodeInfo proves ParseNodeInfo decodes a valid node-info label,
// accepts one key set and the other omitted, and returns ErrInvalidLabel
// for an all-empty object, an unknown architecture, an unrecognized field,
// a non-object document or an operatingSystem over 64 characters.
func TestParseNodeInfo(t *testing.T) {
	t.Parallel()
	got, err := ParseNodeInfo(`{"architecture":"arm64","operatingSystem":"linux"}`)
	if err != nil || got.Architecture != infrav1.ArchitectureArm64 || got.OperatingSystem != "linux" {
		t.Fatalf("ParseNodeInfo = %+v, %v", got, err)
	}
	if got, err := ParseNodeInfo(`{"operatingSystem":"linux"}`); err != nil || got.Architecture != "" {
		t.Errorf("one key = %+v, %v", got, err)
	}
	for _, bad := range []string{`{}`, `{"architecture":"x86"}`, `{"arch":"amd64"}`, `"amd64"`, `{"operatingSystem":"` + strings.Repeat("x", 65) + `"}`} {
		if _, err := ParseNodeInfo(bad); !errors.Is(err, ErrInvalidLabel) {
			t.Errorf("ParseNodeInfo(%s) = %v, want ErrInvalidLabel", bad, err)
		}
	}
}

// TestResolve proves Resolve sets CapacityNotDeclared when neither label
// is present, CapacityResolved when every present label is valid, and
// CapacityLabelInvalid, with the invalid field unset and a valid other
// field kept, when one label is invalid.
func TestResolve(t *testing.T) {
	t.Parallel()
	validCap := `{"cpu":"2"}`
	validNI := `{"architecture":"amd64"}`
	tests := []struct {
		name    string
		labels  map[string]string
		status  metav1.ConditionStatus
		reason  string
		cap, ni bool
	}{
		{"no labels", map[string]string{"other": "x"}, metav1.ConditionTrue, infrav1.CapacityNotDeclaredReason, false, false},
		{"both valid", map[string]string{CapacityLabel: validCap, NodeInfoLabel: validNI}, metav1.ConditionTrue, infrav1.CapacityResolvedReason, true, true},
		{"only capacity", map[string]string{CapacityLabel: validCap}, metav1.ConditionTrue, infrav1.CapacityResolvedReason, true, false},
		{"capacity invalid, node info kept", map[string]string{CapacityLabel: `{`, NodeInfoLabel: validNI}, metav1.ConditionFalse, infrav1.CapacityLabelInvalidReason, false, true},
		{"node info invalid, capacity kept", map[string]string{CapacityLabel: validCap, NodeInfoLabel: `{}`}, metav1.ConditionFalse, infrav1.CapacityLabelInvalidReason, true, false},
	}
	for _, tt := range tests {
		r := Resolve(tt.labels)
		if r.Condition.Status != tt.status || r.Condition.Reason != tt.reason || (len(r.Capacity) > 0) != tt.cap || (r.NodeInfo != infrav1.NodeInfo{}) != tt.ni {
			t.Errorf("%s: %+v", tt.name, r)
		}
	}
}

// scheme returns a runtime.Scheme with the client-go types registered, for
// building fake clients in t's tests.
func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestPullSecretsKeychain proves PullSecretsKeychain reads dockerconfigjson
// and dockercfg pull Secrets into per-host credentials the returned
// Keychain resolves correctly, reports a missing Secret by name instead of
// failing, and rejects a Secret that is not a Docker config Secret.
func TestPullSecretsKeychain(t *testing.T) {
	t.Parallel()
	b64 := base64.StdEncoding.EncodeToString([]byte("robot:s3cret"))
	jsonSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "regcred"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{
			"https://registry.example:5000/v2/":{"auth":"` + b64 + `"},
			"https://index.docker.io/v1/":{"username":"hub","password":"pw"}}}`)},
	}
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "legacy"},
		Type:       corev1.SecretTypeDockercfg,
		Data:       map[string][]byte{corev1.DockerConfigKey: []byte(`{"ghcr.io":{"username":"gh","password":"tok"}}`)},
	}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(jsonSecret, legacy).Build()
	k, missing, err := PullSecretsKeychain(t.Context(), c, "ns", []string{"regcred", "legacy", "nope"})
	if err != nil || len(missing) != 1 || missing[0] != "nope" {
		t.Fatalf("PullSecretsKeychain = %v, %v", missing, err)
	}
	for _, tt := range []struct{ ref, user, pass string }{
		{"registry.example:5000/org/mod:1", "robot", "s3cret"},
		{"docker.io/library/alpine:3", "hub", "pw"},
		{"ghcr.io/org/mod:1", "gh", "tok"},
		{"quay.io/org/mod:1", "", ""},
	} {
		ref, err := name.ParseReference(tt.ref)
		if err != nil {
			t.Fatal(err)
		}
		a, err := k.Resolve(ref.Context())
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := a.Authorization()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Username != tt.user || cfg.Password != tt.pass {
			t.Errorf("%s: got %q/%q", tt.ref, cfg.Username, cfg.Password)
		}
	}

	bad := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "opaque"}, Data: map[string][]byte{"x": nil}}
	c = fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(bad).Build()
	if _, _, err := PullSecretsKeychain(t.Context(), c, "ns", []string{"opaque"}); err == nil {
		t.Error("an Opaque Secret was accepted as a pull secret")
	}
}

// labeled uses t to build a random image whose config carries labels, on
// platform (unset when platform is nil), and returns that image.
func labeled(t *testing.T, labels map[string]string, platform *v1.Platform) v1.Image {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.Config.Labels = labels
	cf.Config.User = "65532"
	if platform != nil {
		cf.OS, cf.Architecture = platform.OS, platform.Architecture
	}
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// TestRemoteConfig proves Remote.Config reads a single image's config
// directly, selects an index's linux image matching the requested
// platform, falls back to the first linux image while skipping a buildx
// attestation manifest, returns ErrNoLinuxImage for an attestation-only
// index, and reports DefaultPlatform as linux with a non-empty
// architecture.
func TestRemoteConfig(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	push := func(repo string, write func(name.Reference) error) string {
		t.Helper()
		ref, err := name.ParseReference(host+"/"+repo, name.Insecure)
		if err != nil {
			t.Fatal(err)
		}
		if err := write(ref); err != nil {
			t.Fatal(err)
		}
		return ref.String()
	}
	image := func(img v1.Image) func(name.Reference) error {
		return func(ref name.Reference) error { return remote.Write(ref, img) }
	}
	index := func(entries ...mutate.IndexAddendum) func(name.Reference) error {
		return func(ref name.Reference) error {
			return remote.WriteIndex(ref, mutate.AppendManifests(empty.Index, entries...))
		}
	}
	entry := func(label string, p v1.Platform) mutate.IndexAddendum {
		return mutate.IndexAddendum{Add: labeled(t, map[string]string{CapacityLabel: label}, &p), Descriptor: v1.Descriptor{Platform: &p}}
	}
	amd, arm := v1.Platform{OS: "linux", Architecture: "amd64"}, v1.Platform{OS: "linux", Architecture: "arm64"}
	attestation := v1.Platform{OS: "unknown", Architecture: "unknown"}

	single := push("single:1", image(labeled(t, map[string]string{CapacityLabel: `{"cpu":"1"}`}, nil)))
	multi := push("multi:1", index(entry("attest", attestation), entry("arm", arm), entry("amd", amd)))
	armOnly := push("armonly:1", index(entry("attest", attestation), entry("arm", arm)))
	noLinux := push("nolinux:1", index(entry("attest", attestation)))

	r := Remote{Insecure: true}
	anon := authn.NewMultiKeychain()
	cfg, err := r.Config(t.Context(), single, anon, nil)
	if err != nil || cfg.Labels[CapacityLabel] != `{"cpu":"1"}` || cfg.User != "65532" || cfg.Platform != "" || !strings.HasPrefix(cfg.Digest, "sha256:") {
		t.Errorf("single = %+v, %v", cfg, err)
	}
	cfg, err = r.Config(t.Context(), multi, anon, &amd)
	if err != nil || cfg.Labels[CapacityLabel] != "amd" || cfg.Platform != "linux/amd64" {
		t.Errorf("multi on amd64 = %+v, %v", cfg, err)
	}
	cfg, err = r.Config(t.Context(), armOnly, anon, &amd)
	if err != nil || cfg.Labels[CapacityLabel] != "arm" || cfg.Platform != "linux/arm64" {
		t.Errorf("fallback to the first linux image, skipping the attestation = %+v, %v", cfg, err)
	}
	if _, err := r.Config(t.Context(), noLinux, anon, &amd); !errors.Is(err, ErrNoLinuxImage) {
		t.Errorf("attestation-only index = %v", err)
	}
	if _, err := r.Config(t.Context(), host+"/missing:1", anon, nil); err == nil {
		t.Error("missing image resolved")
	}
	if _, err := r.Config(t.Context(), "Not A Ref", anon, nil); err == nil {
		t.Error("invalid reference accepted")
	}
	if p := DefaultPlatform(); p.OS != "linux" || p.Architecture == "" {
		t.Errorf("DefaultPlatform = %+v", p)
	}
}
