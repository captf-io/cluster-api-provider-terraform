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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestResolveOwner proves ResolveOwner's branches: a gone owner, a
// mismatched owner, a stray ownerRef, no ownerRef, a valid owner and a
// lookup failure. A ConfigMap stands in for the owner type.
func TestResolveOwner(t *testing.T) {
	t.Parallel()
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner"}}
	withRef := metav1.ObjectMeta{Name: "obj", OwnerReferences: []metav1.OwnerReference{{Name: "x"}}}
	boom := errors.New("boom")
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "owner")
	lookup := func(o *corev1.ConfigMap, ok bool, err error, mismatch string) OwnerLookup[*corev1.ConfigMap] {
		return OwnerLookup[*corev1.ConfigMap]{
			Noun:           "Thing",
			Get:            func(context.Context) (*corev1.ConfigMap, bool, error) { return o, ok, err },
			Mismatch:       func(*corev1.ConfigMap) string { return mismatch },
			WaitingReason:  "Waiting",
			WaitingMessage: "waiting message",
			Assign: func(info *OwnerInfo, o *corev1.ConfigMap) {
				info.InfraCluster = &infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Name: o.Name}}
			},
		}
	}
	for _, tt := range []struct {
		name       string
		meta       metav1.ObjectMeta
		lookup     OwnerLookup[*corev1.ConfigMap]
		wantErr    string
		hasRef     bool
		gone       bool
		gateReason string
		gateMsg    string
		assigned   bool
	}{
		{name: "owner gone", meta: withRef, lookup: lookup(nil, false, notFound, ""), hasRef: true, gone: true},
		{name: "mismatch", meta: withRef, lookup: lookup(owner, true, nil, "forged"), hasRef: true, gateReason: infrav1.OwnerMismatchReason, gateMsg: "forged"},
		{name: "stray ownerRef", meta: withRef, lookup: lookup(nil, false, nil, ""), gateReason: "Waiting", gateMsg: "waiting message"},
		{name: "no ownerRef", meta: metav1.ObjectMeta{Name: "obj"}, lookup: lookup(nil, false, nil, "")},
		{name: "valid owner", meta: withRef, lookup: lookup(owner, true, nil, ""), hasRef: true, assigned: true},
		{name: "lookup failure", meta: withRef, lookup: lookup(nil, false, boom, ""), wantErr: "get owner Thing: boom"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveOwner(t.Context(), fake.NewClientBuilder().Build(), tt.meta, tt.lookup)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.HasOwnerRef != tt.hasRef || got.OwnerGone != tt.gone || (got.InfraCluster != nil) != tt.assigned {
				t.Errorf("owner = %+v, want hasRef=%v gone=%v assigned=%v", got, tt.hasRef, tt.gone, tt.assigned)
			}
			switch {
			case tt.gateReason == "" && got.Gate != nil:
				t.Errorf("gate = %+v, want none", got.Gate)
			case tt.gateReason != "" && (got.Gate == nil || got.Gate.Reason != tt.gateReason || got.Gate.Message != tt.gateMsg || got.Gate.Status != metav1.ConditionFalse):
				t.Errorf("gate = %+v, want %s %q", got.Gate, tt.gateReason, tt.gateMsg)
			}
		})
	}
}
