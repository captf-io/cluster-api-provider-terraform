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
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestTerraformClusterIdentityWidening: changing allowedNamespaces needs
// `get` on the Secret, since a wider list mirrors the Secret into more
// namespaces; an unrelated edit or an identical allowedNamespaces does not
// create a SubjectAccessReview.
func TestTerraformClusterIdentityWidening(t *testing.T) {
	t.Parallel()
	ref := infrav1.SecretReference{Name: "creds", Namespace: "captf-system"}
	old := &infrav1.TerraformClusterIdentity{ObjectMeta: metav1.ObjectMeta{Name: "i"}, Spec: infrav1.TerraformClusterIdentitySpec{
		SecretRef:         ref,
		AllowedNamespaces: &infrav1.AllowedNamespaces{List: []string{"a"}},
	}}
	widened := old.DeepCopy()
	widened.Spec.AllowedNamespaces = &infrav1.AllowedNamespaces{Selector: &metav1.LabelSelector{}}
	same := old.DeepCopy()
	same.Spec.AllowedNamespaces = &infrav1.AllowedNamespaces{List: []string{"a"}}
	same.Labels = map[string]string{"x": "y"}
	onlyAdmin := func(sar *authorizationv1.SubjectAccessReview) bool { return sar.Spec.User == "admin" }

	t.Run("widening without get is denied", func(t *testing.T) {
		t.Parallel()
		w := &TerraformClusterIdentity{Client: sarClient(t, onlyAdmin, nil)}
		_, err := w.ValidateUpdate(requestContext("mallory"), old, widened)
		wantInvalid(t, err, true, `may not get Secret captf-system/creds`)
	})
	t.Run("widening with get is allowed", func(t *testing.T) {
		t.Parallel()
		var seen []authorizationv1.SubjectAccessReview
		w := &TerraformClusterIdentity{Client: sarClient(t, onlyAdmin, nil, &seen)}
		_, err := w.ValidateUpdate(requestContext("admin"), old, widened)
		wantInvalid(t, err, false, "")
		if len(seen) != 1 {
			t.Errorf("reviews = %d, want 1", len(seen))
		}
	})
	t.Run("unrelated edit makes no review", func(t *testing.T) {
		t.Parallel()
		var seen []authorizationv1.SubjectAccessReview
		w := &TerraformClusterIdentity{Client: sarClient(t, onlyAdmin, nil, &seen)}
		_, err := w.ValidateUpdate(requestContext("mallory"), old, same)
		wantInvalid(t, err, false, "")
		if len(seen) != 0 {
			t.Errorf("reviews = %d, want 0", len(seen))
		}
	})
}
