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
	"context"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// plan returns a TerraformPlan named name in ns that targets a cluster,
// with the approval given by approved (unset when nil) and approvedBy.
func plan(ns, name string, approved *bool, approvedBy string) *infrav1.TerraformPlan {
	return &infrav1.TerraformPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: infrav1.TerraformPlanSpec{
			TargetRef:  infrav1.PlanTargetRef{Kind: "TerraformCluster", Name: "demo"},
			PlanHash:   "p2:abc",
			InputsHash: "h2:def",
			Reason:     infrav1.PlanReasonDestructive,
			Summary:    infrav1.PlanSummary{Replace: ptr.To(int32(1))},
			Approved:   approved,
			ApprovedBy: approvedBy,
		},
	}
}

// machine returns a TerraformMachine named name in ns.
func machine(ns, name string) *infrav1.TerraformMachine {
	return &infrav1.TerraformMachine{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: infrav1.TerraformMachineSpec{WorkspaceSpec: infrav1.WorkspaceSpec{
			Source:      infrav1.Source{Image: "ghcr.io/captf-io/noop-machine:v1"},
			IdentityRef: infrav1.IdentityReference{Name: "id"},
		}},
	}
}

// wantDenied fails t unless err is an API status error whose message
// contains want, which is how a webhook denial reaches the caller.
func wantDenied(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted, want a denial containing %q", want)
	}
	if !apierrors.IsInvalid(err) && !apierrors.IsForbidden(err) {
		t.Fatalf("error = %v (reason %s), want Invalid or Forbidden", err, apierrors.ReasonForError(err))
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want one containing %q", err, want)
	}
}

// TestPlanApprovalNamesTheRequester proves the TerraformPlan webhook binds
// approvedBy to the authenticated caller, on create and on update, and
// that RBAC (which lets the caller write) is not what stops the others.
func TestPlanApprovalNamesTheRequester(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, hooked.client)
	alice, bob := hooked.userClient(t, "alice"), hooked.userClient(t, "bob")

	t.Run("create approved naming someone else", func(t *testing.T) {
		t.Parallel()
		wantDenied(t, alice.Create(ctx, plan(ns, "forged", ptr.To(true), "bob")),
			"approvedBy must be the username of the user that creates an approved plan")
	})
	t.Run("create approved naming the requester", func(t *testing.T) {
		t.Parallel()
		if err := alice.Create(ctx, plan(ns, "own", ptr.To(true), "alice")); err != nil {
			t.Fatalf("create: %v", err)
		}
	})
	t.Run("an admin who is not the manager is bound too", func(t *testing.T) {
		t.Parallel()
		wantDenied(t, hooked.client.Create(ctx, plan(ns, "admin", ptr.To(true), "alice")),
			"approvedBy must be the username of the user that creates an approved plan")
	})
	t.Run("approve naming someone else", func(t *testing.T) {
		t.Parallel()
		if err := alice.Create(ctx, plan(ns, "pending-1", nil, "")); err != nil {
			t.Fatalf("create: %v", err)
		}
		p := &infrav1.TerraformPlan{}
		if err := bob.Get(ctx, client.ObjectKey{Namespace: ns, Name: "pending-1"}, p); err != nil {
			t.Fatalf("get: %v", err)
		}
		p.Spec.Approved, p.Spec.ApprovedBy = ptr.To(true), "alice"
		wantDenied(t, bob.Update(ctx, p), "approvedBy must be the username of the user that approves")
	})
	t.Run("approve naming the requester", func(t *testing.T) {
		t.Parallel()
		if err := alice.Create(ctx, plan(ns, "pending-2", nil, "")); err != nil {
			t.Fatalf("create: %v", err)
		}
		p := &infrav1.TerraformPlan{}
		if err := bob.Get(ctx, client.ObjectKey{Namespace: ns, Name: "pending-2"}, p); err != nil {
			t.Fatalf("get: %v", err)
		}
		p.Spec.Approved, p.Spec.ApprovedBy = ptr.To(true), "bob"
		if err := bob.Update(ctx, p); err != nil {
			t.Fatalf("update: %v", err)
		}
	})
	t.Run("only the manager sets the phase label", func(t *testing.T) {
		t.Parallel()
		p := plan(ns, "phased", nil, "")
		p.Labels = map[string]string{infrav1.PlanPhaseLabel: string(infrav1.PlanPhaseApplied)}
		if err := alice.Create(ctx, p); err != nil {
			t.Fatalf("a terminal phase on create is accepted for a mover: %v", err)
		}
		q := plan(ns, "phased-2", nil, "")
		if err := alice.Create(ctx, q); err != nil {
			t.Fatalf("create: %v", err)
		}
		q.Labels = map[string]string{infrav1.PlanPhaseLabel: string(infrav1.PlanPhaseApplied)}
		wantDenied(t, alice.Update(ctx, q), "only the provider's controller may change the plan phase")
		manager := hooked.userClient(t, managerUser)
		cur := &infrav1.TerraformPlan{}
		if err := manager.Get(ctx, client.ObjectKeyFromObject(q), cur); err != nil {
			t.Fatalf("get: %v", err)
		}
		cur.Labels = map[string]string{infrav1.PlanPhaseLabel: string(infrav1.PlanPhaseApplied)}
		if err := manager.Update(ctx, cur); err != nil {
			t.Fatalf("the manager changes the phase: %v", err)
		}
	})
}

// TestMachineWebhookAddsToCEL proves the layering on a TerraformMachine:
// the CRD denies an immutable edit whatever the caller, and the webhook
// adds what CEL cannot know, who sets providerID, which the bare API
// server lets anyone set once.
func TestMachineWebhookAddsToCEL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, hooked.client)
	bareNS := newNamespace(t, bare.client)
	alice := hooked.userClient(t, "alice")

	t.Run("an immutable edit is denied", func(t *testing.T) {
		t.Parallel()
		m := machine(ns, "edit")
		if err := alice.Create(ctx, m); err != nil {
			t.Fatalf("create: %v", err)
		}
		m.Spec.Source.Image = "ghcr.io/captf-io/noop-machine:v2"
		wantInvalid(t, alice.Update(ctx, m), "spec.source is immutable")
	})
	t.Run("CEL still answers first with the webhook installed", func(t *testing.T) {
		t.Parallel()
		m := machine(ns, "cel-first")
		m.Spec.ProviderID = "aws:///i-1"
		if err := hooked.userClient(t, managerUser).Create(ctx, m); err != nil {
			t.Fatalf("create: %v", err)
		}
		m.Spec.ProviderID = "aws:///i-2"
		wantInvalid(t, hooked.userClient(t, managerUser).Update(ctx, m), "providerID can only be set once")
	})
	t.Run("a requester that is not the manager cannot set providerID", func(t *testing.T) {
		t.Parallel()
		m := machine(ns, "pid")
		if err := alice.Create(ctx, m); err != nil {
			t.Fatalf("create: %v", err)
		}
		m.Spec.ProviderID = "aws:///i-1"
		wantInvalid(t, alice.Update(ctx, m), "only the provider's controller may set providerID")
	})
	t.Run("the manager sets providerID once", func(t *testing.T) {
		t.Parallel()
		m := machine(ns, "pid-manager")
		if err := alice.Create(ctx, m); err != nil {
			t.Fatalf("create: %v", err)
		}
		m.Spec.ProviderID = "aws:///i-1"
		if err := hooked.userClient(t, managerUser).Update(ctx, m); err != nil {
			t.Fatalf("update: %v", err)
		}
	})
	t.Run("the same edit passes the CRD alone", func(t *testing.T) {
		t.Parallel()
		m := machine(bareNS, "pid")
		if err := bare.client.Create(ctx, m); err != nil {
			t.Fatalf("create: %v", err)
		}
		m.Spec.ProviderID = "aws:///i-1"
		if err := bare.client.Update(ctx, m); err != nil {
			t.Fatalf("update: %v", err)
		}
	})
}

// TestStatusIsNotBlockedBySpecWebhooks proves a write to the status
// subresource skips the spec webhooks: they match terraformmachines and
// terraformplans, not their /status, so a controller's status patch never
// waits on a webhook that may be down.
func TestStatusIsNotBlockedBySpecWebhooks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, hooked.client)
	alice := hooked.userClient(t, "alice")

	m := machine(ns, "status")
	if err := alice.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	m.Status.Initialization.Provisioned = ptr.To(true)
	if err := alice.Status().Update(ctx, m); err != nil {
		t.Fatalf("status update: %v", err)
	}
	got := &infrav1.TerraformMachine{}
	if err := alice.Get(ctx, client.ObjectKeyFromObject(m), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if p := got.Status.Initialization.Provisioned; p == nil || !*p {
		t.Errorf("status.initialization.provisioned = %v, want true", p)
	}

	cfg := validatingConfiguration(t, hooked)
	for _, w := range cfg.Webhooks {
		for _, r := range w.Rules {
			for _, res := range r.Resources {
				if strings.Contains(res, "/") || res == "*" {
					t.Errorf("webhook %s matches resource %q; no webhook may match a subresource", w.Name, res)
				}
			}
		}
	}
}

// validatingConfiguration returns the validating webhook configuration
// that envtest installed into te's API server. Test t fails when there is
// not exactly one.
func validatingConfiguration(t *testing.T, te *testEnv) *admissionv1.ValidatingWebhookConfiguration {
	t.Helper()
	list := &admissionv1.ValidatingWebhookConfigurationList{}
	if err := te.client.List(context.Background(), list); err != nil {
		t.Fatalf("list webhook configurations: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("%d validating webhook configurations, want 1", len(list.Items))
	}
	return &list.Items[0]
}

// TestWebhookFailurePolicies proves the installed configuration: every
// DELETE entry fails open (Ignore), so a down manager never blocks a
// deletion, and every CREATE/UPDATE entry fails closed (Fail).
func TestWebhookFailurePolicies(t *testing.T) {
	t.Parallel()
	cfg := validatingConfiguration(t, hooked)
	deletes, writes := 0, 0
	for _, w := range cfg.Webhooks {
		ops := map[admissionv1.OperationType]bool{}
		for _, r := range w.Rules {
			for _, op := range r.Operations {
				ops[op] = true
			}
		}
		policy := ptr.Deref(w.FailurePolicy, admissionv1.Fail)
		switch {
		case ops[admissionv1.Delete] && (ops[admissionv1.Create] || ops[admissionv1.Update]):
			t.Errorf("webhook %s mixes DELETE with CREATE or UPDATE", w.Name)
		case ops[admissionv1.Delete]:
			deletes++
			if policy != admissionv1.Ignore {
				t.Errorf("DELETE webhook %s has failurePolicy %s, want Ignore", w.Name, policy)
			}
		default:
			writes++
			if policy != admissionv1.Fail {
				t.Errorf("write webhook %s has failurePolicy %s, want Fail", w.Name, policy)
			}
		}
	}
	if deletes != 2 || writes != 8 {
		t.Errorf("%d DELETE and %d CREATE/UPDATE webhooks, want 2 and 8", deletes, writes)
	}
}

// TestMachineDeleteIsGuardedByTheMachine proves the delete webhook refuses
// to delete a TerraformMachine a Machine still references, and allows it
// once the Machine is gone.
func TestMachineDeleteIsGuardedByTheMachine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, hooked.client)
	m := machine(ns, "guarded")
	if err := hooked.client.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	owner := capiMachine(ns, "owner", m.Name)
	if err := hooked.client.Create(ctx, owner); err != nil {
		t.Fatalf("create the Machine: %v", err)
	}
	wantDenied(t, hooked.client.Delete(ctx, m), "delete the Machine owner instead")
	if err := hooked.client.Delete(ctx, owner); err != nil {
		t.Fatalf("delete the Machine: %v", err)
	}
	if err := hooked.client.Delete(ctx, m); err != nil {
		t.Fatalf("delete once the Machine is gone: %v", err)
	}
}

// capiMachine returns a Cluster API Machine named name in ns whose
// infrastructureRef names the TerraformMachine infra.
func capiMachine(ns, name, infra string) *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: clusterv1.MachineSpec{
			ClusterName: "demo",
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: infrav1.GroupVersion.Group, Kind: "TerraformMachine", Name: infra,
			},
		},
	}
}

// TestWebhookServerDown proves the failure policies end to end: once the
// webhook server stops, a TerraformMachine DELETE that the webhook denied
// is let through (fail-open) while a CREATE is refused (fail-closed).
func TestWebhookServerDown(t *testing.T) {
	// Not parallel: the webhook server of failopen is stopped.
	ctx := context.Background()
	ns := newNamespace(t, failopen.client)
	m := machine(ns, "guarded")
	if err := failopen.client.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := failopen.client.Create(ctx, capiMachine(ns, "owner", m.Name)); err != nil {
		t.Fatalf("create the Machine: %v", err)
	}
	wantDenied(t, failopen.client.Delete(ctx, m), "delete the Machine owner instead")

	failopen.stopWebhooks()

	deadline := time.Now().Add(eventually)
	var err error
	for {
		err = failopen.client.Create(ctx, machine(ns, "new"))
		if err != nil && !apierrors.IsNotFound(err) || time.Now().After(deadline) {
			break
		}
		time.Sleep(tick)
	}
	if err == nil {
		t.Fatal("create succeeded with the webhook server down, want it refused (fail-closed)")
	}
	if !strings.Contains(err.Error(), "failed calling webhook") {
		t.Errorf("create error = %v, want one that failed calling the webhook", err)
	}
	if !apierrors.IsInternalError(err) {
		t.Errorf("create error reason = %s, want InternalError", apierrors.ReasonForError(err))
	}

	if err := failopen.client.Delete(ctx, m); err != nil {
		t.Fatalf("delete with the webhook server down: %v, want success (fail-open)", err)
	}
}
