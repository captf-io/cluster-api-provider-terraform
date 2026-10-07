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

package identity

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestEffectiveRefFallbackChain proves the machine fallback chain works the
// same for both kinds: an own ref wins (and a Secret ref never falls
// through), else defaults.identityRef, else the cluster's identityRef.
func TestEffectiveRefFallbackChain(t *testing.T) {
	t.Parallel()
	secret := func(n string) infrav1.IdentityReference {
		return infrav1.IdentityReference{Name: n, Kind: infrav1.IdentityKindSecret}
	}
	ci := func(n string) infrav1.IdentityReference { return infrav1.IdentityReference{Name: n} }
	cluster := &infrav1.TerraformCluster{Spec: infrav1.TerraformClusterSpec{
		WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: ci("cluster")},
		Defaults:      &infrav1.TerraformClusterDefaults{IdentityRef: secret("defaults")},
	}}
	noDefaults := &infrav1.TerraformCluster{Spec: infrav1.TerraformClusterSpec{WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: secret("cluster")}}}
	tests := []struct {
		name     string
		own      infrav1.IdentityReference
		cluster  *infrav1.TerraformCluster
		want     infrav1.IdentityReference
		wantOK   bool
		wantName string
	}{
		{"own Secret wins", secret("mine"), cluster, secret("mine"), true, ""},
		{"own identity wins", ci("mine"), cluster, ci("mine"), true, "mine"},
		{"defaults Secret before cluster", infrav1.IdentityReference{}, cluster, secret("defaults"), true, ""},
		{"cluster Secret", infrav1.IdentityReference{}, noDefaults, secret("cluster"), true, ""},
		{"nothing set", infrav1.IdentityReference{}, &infrav1.TerraformCluster{}, infrav1.IdentityReference{}, false, ""},
		{"no cluster", infrav1.IdentityReference{}, nil, infrav1.IdentityReference{}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := EffectiveRef(tt.own, tt.cluster)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("EffectiveRef = %+v, %v; want %+v, %v", got, ok, tt.want, tt.wantOK)
			}
			if name, _ := EffectiveName(tt.own, tt.cluster); name != tt.wantName {
				t.Errorf("EffectiveName = %q, want %q: a Secret is no identity", name, tt.wantName)
			}
		})
	}
}

// TestCredentialsSecretName: a Secret reference is mounted as it is, an
// identity through its mirror.
func TestCredentialsSecretName(t *testing.T) {
	t.Parallel()
	if got := CredentialsSecretName(infrav1.IdentityReference{Name: "s", Kind: infrav1.IdentityKindSecret}); got != "s" {
		t.Errorf("Secret: %q, want s", got)
	}
	if got := CredentialsSecretName(infrav1.IdentityReference{Name: "aws"}); got != MirrorName("aws") {
		t.Errorf("identity: %q, want %q", got, MirrorName("aws"))
	}
}

// TestSecretKindIsNoUser proves an object whose identityRef is a Secret
// that happens to share an identity's name is not a user of that identity.
func TestSecretKindIsNoUser(t *testing.T) {
	t.Parallel()
	tc := labeledCluster(tenant, "tc", "c1", "other", "")
	tc.Spec.IdentityRef = infrav1.IdentityReference{Name: idName, Kind: infrav1.IdentityKindSecret}
	r := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(tc, labeledMachine(tenant, "m", "c1", "")).Build()
	users, err := Users(t.Context(), r, idName)
	if err != nil || len(users) != 0 {
		t.Errorf("Users = %v, %v; want none", users, err)
	}
}

// TestLocalSecret reads a Secret of the given namespace, and reports a
// missing one as ErrSecretNotFound.
func TestLocalSecret(t *testing.T) {
	t.Parallel()
	s := &corev1.Secret{}
	s.Namespace, s.Name = tenant, "creds"
	r := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(s).Build()
	if _, err := LocalSecret(t.Context(), r, tenant, "creds"); err != nil {
		t.Errorf("present: %v", err)
	}
	if _, err := LocalSecret(t.Context(), r, "other", "creds"); err == nil {
		t.Error("another namespace's Secret was found")
	}
}
