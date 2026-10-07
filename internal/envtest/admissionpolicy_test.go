//go:build envtest

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

package envtest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestPlanApprovalPolicy proves the plan-approval admission policy of
// config/admission-policy, evaluated by the API server with no webhook
// installed (the manager down, its webhook configuration removed), stops a
// user approving a TerraformPlan in someone else's name, and lets one
// approve in their own. The test's copy of the binding matches only its
// own namespace, so the other tests of this API server are not bound.
func TestPlanApprovalPolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const scope = "captf-test/plan-approval-policy"
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "t-", Labels: map[string]string{scope: "true"}}}
	if err := bare.client.Create(ctx, nsObj); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	ns := nsObj.Name

	policy, binding := loadPlanApprovalPolicy(t)
	suffix := "-" + ns
	policy.Name += suffix
	binding.Name += suffix
	binding.Spec.PolicyName = policy.Name
	binding.Spec.MatchResources = &admissionregistrationv1.MatchResources{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{scope: "true"}},
	}
	for _, o := range []client.Object{policy, binding} {
		if err := bare.client.Create(ctx, o); err != nil {
			t.Fatalf("create %s: %v", o.GetName(), err)
		}
	}
	alice, bob := bare.userClient(t, "alice"), bare.userClient(t, "bob")

	// The policy takes effect once the API server has compiled it: a fresh
	// pending plan per try.
	approve := func(name, approvedBy string) error {
		if err := alice.Create(ctx, plan(ns, name, nil, "")); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		p := &infrav1.TerraformPlan{}
		if err := bob.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, p); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		p.Spec.Approved, p.Spec.ApprovedBy = ptr.To(true), approvedBy
		return bob.Update(ctx, p)
	}
	var err error
	for i, deadline := 0, time.Now().Add(eventually); time.Now().Before(deadline); i++ {
		if err = approve("forged-"+strconv.Itoa(i), "alice"); err != nil {
			break
		}
		time.Sleep(tick)
	}
	if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "spec.approvedBy to your own username") {
		t.Fatalf("approval naming someone else: %v, want the policy's denial", err)
	}
	if err := approve("own", "bob"); err != nil {
		t.Errorf("approval naming the requester: %v", err)
	}
}

// loadPlanApprovalPolicy returns the ValidatingAdmissionPolicy and its
// binding from config/admission-policy/plan-approval.yaml, failing t when
// the file does not hold exactly those two.
func loadPlanApprovalPolicy(t *testing.T) (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "admission-policy", "plan-approval.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	for _, into := range []any{policy, binding} {
		if err := dec.Decode(into); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	var extra map[string]any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("plan-approval.yaml holds more than the policy and its binding: %v", err)
	}
	if policy.Kind != "ValidatingAdmissionPolicy" || binding.Kind != "ValidatingAdmissionPolicyBinding" {
		t.Fatalf("kinds = %s, %s", policy.Kind, binding.Kind)
	}
	return policy, binding
}
