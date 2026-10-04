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

package webhooks

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// testImage is a syntactically valid image reference used wherever a test
// needs one.
const testImage = "ghcr.io/example/module:v1.0.0"

// wantInvalid asserts, failing test t otherwise, that err is a 422 Invalid
// status whose message mentions every one of fragments, or that err is nil
// when invalid is false.
func wantInvalid(t *testing.T, err error, invalid bool, fragments ...string) {
	t.Helper()
	if !invalid {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("error = %v, want an Invalid status", err)
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) || status.Status().Code != http.StatusUnprocessableEntity {
		t.Fatalf("error = %v, want HTTP 422", err)
	}
	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error %q does not mention %q", err.Error(), f)
		}
	}
}

// TestValidateSource proves the source webhook check accepts a tagged,
// digested or short-name image reference and rejects an empty, uppercase or
// malformed-digest one.
func TestValidateSource(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		src     infrav1.Source
		invalid bool
		frag    string
	}{
		{name: "tag", src: infrav1.Source{Image: testImage}},
		{name: "digest", src: infrav1.Source{Image: "ghcr.io/example/module@sha256:" + strings.Repeat("a", 64)}},
		{name: "docker hub short name", src: infrav1.Source{Image: "module:v1"}},
		{name: "empty image", src: infrav1.Source{}, invalid: true, frag: "spec.source.image: Required"},
		{name: "uppercase repository", src: infrav1.Source{Image: "ghcr.io/Example/module:v1"}, invalid: true, frag: "not a valid image reference"},
		{name: "bad digest", src: infrav1.Source{Image: "ghcr.io/example/module@sha256:abc"}, invalid: true, frag: "not a valid image reference"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := &infrav1.TerraformMachine{Spec: infrav1.TerraformMachineSpec{WorkspaceSpec: infrav1.WorkspaceSpec{Source: tt.src}}}
			_, err := (&TerraformMachine{}).ValidateCreate(context.Background(), m)
			wantInvalid(t, err, tt.invalid, tt.frag)
		})
	}
}

// TestTerraformCluster proves TerraformCluster.ValidateCreate and
// ValidateUpdate require spec.identityRef (not the defaults-only fallback),
// validate the source and every jobs policy, allow an image change on
// update, and that ValidateDelete allows every delete.
func TestTerraformCluster(t *testing.T) {
	t.Parallel()
	w := &TerraformCluster{}
	ctx := context.Background()
	withIdentity := infrav1.TerraformClusterSpec{WorkspaceSpec: infrav1.WorkspaceSpec{Source: infrav1.Source{Image: testImage}, IdentityRef: infrav1.IdentityReference{Name: "id"}}}
	// defaults.identityRef is for machines only; the cluster needs its own.
	onlyDefaultIdentity := infrav1.TerraformClusterSpec{
		WorkspaceSpec: infrav1.WorkspaceSpec{Source: infrav1.Source{Image: testImage}},
		Defaults:      &infrav1.TerraformClusterDefaults{IdentityRef: infrav1.IdentityReference{Name: "id"}},
	}
	noIdentity := infrav1.TerraformClusterSpec{WorkspaceSpec: infrav1.WorkspaceSpec{Source: infrav1.Source{Image: testImage}}, Defaults: &infrav1.TerraformClusterDefaults{}}
	badDefaultsJobs := withIdentity
	badDefaultsJobs.Defaults = &infrav1.TerraformClusterDefaults{Jobs: &infrav1.JobPolicy{SecurityContext: &corev1.SecurityContext{Privileged: new(true)}}}
	badJobs := withIdentity
	badJobs.Jobs = &infrav1.JobPolicy{LockTimeoutSeconds: new(int32(600)), ActiveDeadlineSeconds: 600}

	tests := []struct {
		name    string
		spec    infrav1.TerraformClusterSpec
		invalid bool
		frag    string
	}{
		{name: "identityRef", spec: withIdentity},
		{name: "defaults.identityRef only", spec: onlyDefaultIdentity, invalid: true, frag: "spec.identityRef: Required"},
		{name: "no identity", spec: noIdentity, invalid: true, frag: "identity is mandatory"},
		{name: "bad source", spec: infrav1.TerraformClusterSpec{WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: "id"}}}, invalid: true, frag: "spec.source.image"},
		{name: "privileged defaults.jobs", spec: badDefaultsJobs, invalid: true, frag: "spec.defaults.jobs.securityContext.privileged"},
		{name: "lock timeout not below deadline", spec: badJobs, invalid: true, frag: "spec.jobs.lockTimeoutSeconds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			obj := &infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Name: "c"}, Spec: tt.spec}
			_, err := w.ValidateCreate(ctx, obj)
			wantInvalid(t, err, tt.invalid, tt.frag)
			_, err = w.ValidateUpdate(ctx, &infrav1.TerraformCluster{Spec: withIdentity}, obj)
			wantInvalid(t, err, tt.invalid, tt.frag)
		})
	}

	// A new image is a new module version: allowed on update.
	newer := withIdentity.DeepCopy()
	newer.Source.Image = "ghcr.io/example/module:v2.0.0"
	_, err := w.ValidateUpdate(ctx, &infrav1.TerraformCluster{Spec: withIdentity}, &infrav1.TerraformCluster{Spec: *newer})
	wantInvalid(t, err, false)

	obj := &infrav1.TerraformCluster{Spec: withIdentity}
	if _, err := w.ValidateDelete(ctx, obj); err != nil {
		t.Errorf("ValidateDelete: %v", err)
	}
}

// TestTerraformClusterControlPlaneEndpoint proves ValidateUpdate lets
// controlPlaneEndpoint be set from unset or cleared to empty, but forbids
// changing its host or port, or clearing it, once it has a host.
func TestTerraformClusterControlPlaneEndpoint(t *testing.T) {
	t.Parallel()
	w := &TerraformCluster{}
	cluster := func(ep *clusterv1.APIEndpoint) *infrav1.TerraformCluster {
		return &infrav1.TerraformCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "c"},
			Spec: infrav1.TerraformClusterSpec{
				WorkspaceSpec:        infrav1.WorkspaceSpec{Source: infrav1.Source{Image: testImage}, IdentityRef: infrav1.IdentityReference{Name: "id"}},
				ControlPlaneEndpoint: ep,
			},
		}
	}
	set := &clusterv1.APIEndpoint{Host: "api.example.com", Port: 6443}
	tests := []struct {
		name     string
		old, cur *clusterv1.APIEndpoint
		invalid  bool
	}{
		{name: "unset stays unset"},
		{name: "empty to value (the controller writes it once)", cur: set},
		{name: "empty host to value", old: &clusterv1.APIEndpoint{}, cur: set},
		{name: "unchanged", old: set, cur: &clusterv1.APIEndpoint{Host: "api.example.com", Port: 6443}},
		{name: "host changed", old: set, cur: &clusterv1.APIEndpoint{Host: "other.example.com", Port: 6443}, invalid: true},
		{name: "port changed", old: set, cur: &clusterv1.APIEndpoint{Host: "api.example.com", Port: 443}, invalid: true},
		{name: "cleared", old: set, invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := w.ValidateUpdate(context.Background(), cluster(tt.old), cluster(tt.cur))
			wantInvalid(t, err, tt.invalid, "spec.controlPlaneEndpoint: Forbidden")
		})
	}
}

// TestValidateJobPolicy proves the jobs-policy webhook check accepts a
// hardened security context and a lock timeout below the deadline, rejects
// a privileged, escalating or capability-adding security context and a
// lock timeout at or above the deadline, and applies identically to a
// TerraformMachine, a TerraformMachineTemplate and a
// TerraformClusterTemplate's defaults.jobs.
func TestValidateJobPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		jobs    *infrav1.JobPolicy
		invalid bool
		frag    string
	}{
		{name: "nil"},
		{name: "hardened context", jobs: &infrav1.JobPolicy{SecurityContext: &corev1.SecurityContext{
			Privileged: new(false), AllowPrivilegeEscalation: new(false), RunAsNonRoot: new(true),
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		}}},
		{name: "privileged", jobs: &infrav1.JobPolicy{SecurityContext: &corev1.SecurityContext{Privileged: new(true)}},
			invalid: true, frag: "spec.jobs.securityContext.privileged: Forbidden"},
		{name: "privilege escalation", jobs: &infrav1.JobPolicy{SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: new(true)}},
			invalid: true, frag: "spec.jobs.securityContext.allowPrivilegeEscalation: Forbidden"},
		{name: "added capability", jobs: &infrav1.JobPolicy{SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}}}},
			invalid: true, frag: "spec.jobs.securityContext.capabilities.add: Forbidden"},
		{name: "lock timeout below deadline", jobs: &infrav1.JobPolicy{LockTimeoutSeconds: new(int32(300)), ActiveDeadlineSeconds: 301}},
		{name: "lock timeout equals deadline", jobs: &infrav1.JobPolicy{LockTimeoutSeconds: new(int32(300)), ActiveDeadlineSeconds: 300},
			invalid: true, frag: "spec.jobs.lockTimeoutSeconds"},
		{name: "lock timeout above deadline", jobs: &infrav1.JobPolicy{LockTimeoutSeconds: new(int32(3600)), ActiveDeadlineSeconds: 60},
			invalid: true, frag: "must be less than activeDeadlineSeconds (60)"},
		{name: "only lock timeout", jobs: &infrav1.JobPolicy{LockTimeoutSeconds: new(int32(600))}},
		{name: "only lock timeout at default deadline", jobs: &infrav1.JobPolicy{LockTimeoutSeconds: new(int32(3600))},
			invalid: true, frag: "less than the default activeDeadlineSeconds (3600)"},
		{name: "only deadline", jobs: &infrav1.JobPolicy{ActiveDeadlineSeconds: 301}},
		{name: "only deadline at default lock timeout", jobs: &infrav1.JobPolicy{ActiveDeadlineSeconds: 300},
			invalid: true, frag: "spec.jobs.activeDeadlineSeconds: Invalid value: 300: must be greater than the default lockTimeoutSeconds (300)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := &infrav1.TerraformMachine{Spec: infrav1.TerraformMachineSpec{WorkspaceSpec: infrav1.WorkspaceSpec{Source: infrav1.Source{Image: testImage}, Jobs: tt.jobs}}}
			_, err := (&TerraformMachine{}).ValidateCreate(context.Background(), m)
			wantInvalid(t, err, tt.invalid, tt.frag)
			tm := &infrav1.TerraformMachineTemplate{Spec: infrav1.TerraformMachineTemplateSpec{Template: infrav1.TerraformMachineTemplateResource{Spec: m.Spec}}}
			_, err = (&TerraformMachineTemplate{}).ValidateCreate(context.Background(), tm)
			wantInvalid(t, err, tt.invalid, strings.Replace(tt.frag, "spec.", "spec.template.spec.", 1))
			tc := &infrav1.TerraformClusterTemplate{Spec: infrav1.TerraformClusterTemplateSpec{Template: infrav1.TerraformClusterTemplateResource{
				Spec: infrav1.TerraformClusterSpec{
					WorkspaceSpec: infrav1.WorkspaceSpec{Source: m.Spec.Source, IdentityRef: infrav1.IdentityReference{Name: "id"}},
					Defaults:      &infrav1.TerraformClusterDefaults{Jobs: tt.jobs},
				},
			}}}
			_, err = (&TerraformClusterTemplate{}).ValidateCreate(context.Background(), tc)
			wantInvalid(t, err, tt.invalid, strings.Replace(tt.frag, "spec.jobs", "spec.template.spec.defaults.jobs", 1))
		})
	}
}

// machine returns a valid TerraformMachine named m in namespace ns, with
// providerID and a hardened, resourced jobs policy.
func machine(providerID string) *infrav1.TerraformMachine {
	return &infrav1.TerraformMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
		Spec: infrav1.TerraformMachineSpec{
			ProviderID: providerID,
			WorkspaceSpec: infrav1.WorkspaceSpec{
				Source: infrav1.Source{Image: testImage},
				Jobs: &infrav1.JobPolicy{Resources: &corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				}},
			},
		},
	}
}

// TestTerraformMachineUpdate proves ValidateUpdate lets providerID be set
// once, forbids changing or clearing it, forbids a source or identityRef
// change, allows jobs, drift, remediation and metadata changes (re-checking
// a new jobs policy), and treats an equal quantity in a different form as
// no change.
func TestTerraformMachineUpdate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(old, updated *infrav1.TerraformMachine)
		invalid bool
		frag    string
	}{
		{name: "no change"},
		{name: "same providerID", mutate: func(o, u *infrav1.TerraformMachine) {
			o.Spec.ProviderID, u.Spec.ProviderID = "aws:///i-1", "aws:///i-1"
		}},
		{name: "change providerID", mutate: func(o, u *infrav1.TerraformMachine) {
			o.Spec.ProviderID, u.Spec.ProviderID = "aws:///i-1", "aws:///i-2"
		},
			invalid: true, frag: "spec.providerID: Forbidden"},
		{name: "clear providerID", mutate: func(o, _ *infrav1.TerraformMachine) { o.Spec.ProviderID = "aws:///i-1" },
			invalid: true, frag: "spec.providerID: Forbidden"},
		{name: "change image", mutate: func(_, u *infrav1.TerraformMachine) { u.Spec.Source.Image = "ghcr.io/example/module:v2" },
			invalid: true, frag: "spec.source: Forbidden"},
		{name: "change pull policy", mutate: func(_, u *infrav1.TerraformMachine) { u.Spec.Source.ImagePullPolicy = corev1.PullAlways },
			invalid: true, frag: "spec.source: Forbidden"},
		{name: "change identity", mutate: func(_, u *infrav1.TerraformMachine) { u.Spec.IdentityRef.Name = "other" },
			invalid: true, frag: "spec.identityRef: Forbidden"},
		{name: "change jobs (operational policy)", mutate: func(_, u *infrav1.TerraformMachine) {
			u.Spec.Jobs.ActiveDeadlineSeconds = 7200
			u.Spec.Jobs.Env = []corev1.EnvVar{{Name: "X", Value: "y"}}
		}},
		{name: "change drift", mutate: func(_, u *infrav1.TerraformMachine) {
			u.Spec.Drift = &infrav1.MachineDriftPolicy{IntervalSeconds: new(int32(0))}
		}},
		{name: "change remediation", mutate: func(_, u *infrav1.TerraformMachine) {
			u.Spec.Remediation = &infrav1.MachineRemediation{AnnotateMachine: new(false)}
		}},
		{name: "new jobs policy is still validated", mutate: func(_, u *infrav1.TerraformMachine) {
			u.Spec.Jobs.SecurityContext = &corev1.SecurityContext{Privileged: new(true)}
		}, invalid: true, frag: "spec.jobs.securityContext.privileged"},
		{name: "metadata only (clusterctl move annotation, KCP labels)", mutate: func(_, u *infrav1.TerraformMachine) {
			u.Annotations = map[string]string{clusterctlv1.DeleteForMoveAnnotation: ""}
			u.Labels = map[string]string{"cluster.x-k8s.io/cluster-name": "c"}
		}},
		{name: "equal quantity, different form", mutate: func(_, u *infrav1.TerraformMachine) {
			u.Spec.Jobs.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("1024Mi")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			old, updated := machine(""), machine("")
			if tt.mutate != nil {
				tt.mutate(old, updated)
			}
			_, err := (&TerraformMachine{ManagerUser: testManagerUser}).ValidateUpdate(userContext("someone"), old, updated)
			wantInvalid(t, err, tt.invalid, tt.frag)
		})
	}
}

// testManagerUser is the manager ServiceAccount username the providerID
// tests configure.
const testManagerUser = "system:serviceaccount:captf-system:captf-manager"

// userContext returns a context carrying an admission request made by the
// user named username.
func userContext(username string) context.Context {
	return admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{Username: username}},
	})
}

// TestTerraformMachineProviderID proves that setting providerID on an
// existing TerraformMachine, from empty to non-empty, is allowed only for
// the manager's ServiceAccount: not for another user, not when the manager
// identity is unknown, and not (as an InternalError) without an admission
// request. Create keeps accepting a providerID, for clusterctl move.
func TestTerraformMachineProviderID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		w       *TerraformMachine
		ctx     context.Context
		invalid bool
	}{
		{name: "manager sets it", w: &TerraformMachine{ManagerUser: testManagerUser}, ctx: userContext(testManagerUser)},
		{name: "another user", w: &TerraformMachine{ManagerUser: testManagerUser}, ctx: userContext("system:serviceaccount:ns:other"), invalid: true},
		{name: "human user", w: &TerraformMachine{ManagerUser: testManagerUser}, ctx: userContext("alice"), invalid: true},
		{name: "manager identity unknown fails closed", w: &TerraformMachine{}, ctx: userContext(""), invalid: true},
		{name: "manager identity unknown, other user", w: &TerraformMachine{}, ctx: userContext("alice"), invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := tt.w.ValidateUpdate(tt.ctx, machine(""), machine("aws:///i-1"))
			wantInvalid(t, err, tt.invalid, "spec.providerID: Forbidden")
		})
	}

	t.Run("no admission request", func(t *testing.T) {
		t.Parallel()
		_, err := (&TerraformMachine{ManagerUser: testManagerUser}).ValidateUpdate(context.Background(), machine(""), machine("aws:///i-1"))
		var status apierrors.APIStatus
		if !errors.As(err, &status) || status.Status().Code != http.StatusInternalServerError {
			t.Fatalf("error = %v, want InternalError", err)
		}
	})
	t.Run("create accepts providerID", func(t *testing.T) {
		t.Parallel()
		_, err := (&TerraformMachine{}).ValidateCreate(context.Background(), machine("aws:///i-1"))
		wantInvalid(t, err, false)
	})
	t.Run("unchanged providerID needs no manager", func(t *testing.T) {
		t.Parallel()
		_, err := (&TerraformMachine{}).ValidateUpdate(userContext("alice"), machine("aws:///i-1"), machine("aws:///i-1"))
		wantInvalid(t, err, false)
	})
}

// pool returns a valid TerraformMachinePool named p in namespace ns, with
// providerID and a hardened, resourced jobs policy.
func pool(providerID string) *infrav1.TerraformMachinePool {
	return &infrav1.TerraformMachinePool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec: infrav1.TerraformMachinePoolSpec{
			ProviderID: providerID,
			WorkspaceSpec: infrav1.WorkspaceSpec{
				Source: infrav1.Source{Image: testImage},
				Jobs: &infrav1.JobPolicy{Resources: &corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				}},
			},
		},
	}
}

// TestTerraformMachinePool proves TerraformMachinePool.ValidateCreate
// validates the source, jobs policy and variables for the machinepool
// role, ValidateUpdate applies the same checks with no immutable field
// (providerID, providerIDList, source and drift may all change), and
// ValidateDelete allows every delete.
func TestTerraformMachinePool(t *testing.T) {
	t.Parallel()
	w := &TerraformMachinePool{}
	ctx := context.Background()

	badSource := pool("")
	badSource.Spec.Source.Image = ""
	badVariables := pool("")
	badVariables.Spec.Variables = runtime.RawExtension{Raw: []byte(`{"captf_x":"y"}`)}

	tests := []struct {
		name    string
		obj     *infrav1.TerraformMachinePool
		invalid bool
		frag    string
	}{
		{name: "valid", obj: pool("")},
		{name: "bad source", obj: badSource, invalid: true, frag: "spec.source.image: Required"},
		{name: "bad variables", obj: badVariables, invalid: true, frag: "spec.variables[captf_x]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := w.ValidateCreate(ctx, tt.obj)
			wantInvalid(t, err, tt.invalid, tt.frag)
			_, err = w.ValidateUpdate(ctx, pool(""), tt.obj)
			wantInvalid(t, err, tt.invalid, tt.frag)
		})
	}

	// Nothing is immutable: the controller writes providerID and
	// providerIDList, and source, jobs and drift are all mutable or
	// re-applied inputs of a pool.
	old := pool("aws-asg:///p-1")
	updated := pool("aws-asg:///p-2")
	updated.Spec.ProviderIDList = []string{"aws:///i-1", "aws:///i-2"}
	updated.Spec.Source.Image = "ghcr.io/example/module:v2.0.0"
	updated.Spec.Drift = &infrav1.MachinePoolDriftPolicy{IntervalSeconds: 1800, Action: infrav1.DriftActionRemediate}
	_, err := w.ValidateUpdate(ctx, old, updated)
	wantInvalid(t, err, false)

	if _, err := w.ValidateDelete(ctx, pool("")); err != nil {
		t.Errorf("ValidateDelete: %v", err)
	}
}

// TestTerraformMachinePoolTemplate proves
// TerraformMachinePoolTemplate.ValidateCreate validates the template
// metadata and source, ValidateUpdate forbids any spec.template.spec
// change except during a ClusterClass topology dry-run while allowing a
// metadata-only change, fails without an admission request, and
// ValidateDelete allows every delete.
func TestTerraformMachinePoolTemplate(t *testing.T) {
	t.Parallel()
	w := &TerraformMachinePoolTemplate{}
	tmpl := func(image string) *infrav1.TerraformMachinePoolTemplate {
		return &infrav1.TerraformMachinePoolTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "t"},
			Spec: infrav1.TerraformMachinePoolTemplateSpec{Template: infrav1.TerraformMachinePoolTemplateResource{
				Spec: infrav1.TerraformMachinePoolSpec{WorkspaceSpec: infrav1.WorkspaceSpec{Source: infrav1.Source{Image: image}}},
			}},
		}
	}
	topologyDryRun := func(o *infrav1.TerraformMachinePoolTemplate) *infrav1.TerraformMachinePoolTemplate {
		o.Annotations = map[string]string{clusterv1.TopologyDryRunAnnotation: ""}
		return o
	}
	metadataOnly := tmpl(testImage)
	metadataOnly.Spec.Template.ObjectMeta = &clusterv1.ObjectMeta{Labels: map[string]string{"a": "b"}}
	badMeta := tmpl(testImage)
	badMeta.Spec.Template.ObjectMeta = &clusterv1.ObjectMeta{Labels: map[string]string{"bad key!": "v"}}

	_, err := w.ValidateCreate(context.Background(), tmpl(testImage))
	wantInvalid(t, err, false)
	_, err = w.ValidateCreate(context.Background(), badMeta)
	wantInvalid(t, err, true, "spec.template.metadata.labels")
	_, err = w.ValidateCreate(context.Background(), tmpl(""))
	wantInvalid(t, err, true, "spec.template.spec.source.image")

	tests := []struct {
		name    string
		ctx     context.Context
		updated *infrav1.TerraformMachinePoolTemplate
		invalid bool
		frag    string
	}{
		{name: "unchanged", ctx: dryRunContext(false), updated: tmpl(testImage)},
		{name: "metadata-only change", ctx: dryRunContext(false), updated: metadataOnly},
		{name: "spec change", ctx: dryRunContext(false), updated: tmpl("ghcr.io/example/module:v2"), invalid: true, frag: "spec.template.spec: Forbidden"},
		{name: "plain dry-run is still checked", ctx: dryRunContext(true), updated: tmpl("ghcr.io/example/module:v2"), invalid: true, frag: "spec.template.spec: Forbidden"},
		{name: "topology dry-run skips immutability", ctx: dryRunContext(true), updated: topologyDryRun(tmpl("ghcr.io/example/module:v2"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := w.ValidateUpdate(tt.ctx, tmpl(testImage), tt.updated)
			wantInvalid(t, err, tt.invalid, tt.frag)
		})
	}

	_, err = w.ValidateUpdate(context.Background(), tmpl(testImage), tmpl(testImage))
	if !apierrors.IsBadRequest(err) {
		t.Errorf("update without an admission request: error = %v, want BadRequest", err)
	}

	obj := tmpl(testImage)
	if _, err := w.ValidateDelete(context.Background(), obj); err != nil {
		t.Errorf("ValidateDelete: %v", err)
	}
}

// testScheme builds a runtime.Scheme carrying the CAPI core types, failing
// test t on a registration error. It returns the built scheme.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clusterv1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return s
}

// machineFor returns a CAPI Machine named name whose infrastructureRef
// names the TerraformMachine ref of group, kind and name given.
func machineFor(name string, ref clusterv1.ContractVersionedObjectReference) *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       clusterv1.MachineSpec{InfrastructureRef: ref},
	}
}

// TestTerraformMachineDelete proves ValidateDelete forbids deleting a
// TerraformMachine while a Machine that is not being deleted references it,
// whatever its ownerReferences and cluster-name label say (they can be
// edited by anyone who may update it), allows it once no such Machine
// exists, honors delete-for-move only while the Cluster is paused, and
// surfaces a lookup failure as an InternalError rather than a rejection.
func TestTerraformMachineDelete(t *testing.T) {
	t.Parallel()
	now := metav1.Now()
	backref := clusterv1.ContractVersionedObjectReference{APIGroup: infrav1.GroupVersion.Group, Kind: "TerraformMachine", Name: "m"}
	live := machineFor("live", backref)
	deleting := machineFor("deleting", backref)
	deleting.DeletionTimestamp, deleting.Finalizers = &now, []string{"machine.cluster.x-k8s.io"}
	wrongName := machineFor("other-name", clusterv1.ContractVersionedObjectReference{APIGroup: infrav1.GroupVersion.Group, Kind: "TerraformMachine", Name: "x"})
	wrongKind := machineFor("other-kind", clusterv1.ContractVersionedObjectReference{APIGroup: infrav1.GroupVersion.Group, Kind: "TerraformMachinePool", Name: "m"})
	wrongGroup := machineFor("other-group", clusterv1.ContractVersionedObjectReference{APIGroup: "example.com", Kind: "TerraformMachine", Name: "m"})
	elsewhere := machineFor("elsewhere", backref)
	elsewhere.Namespace = "other-ns"
	pausedCluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "paused", Namespace: "ns"},
		Spec: clusterv1.ClusterSpec{Paused: new(true)}}
	runningCluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "running", Namespace: "ns"}}

	// bare is the TerraformMachine after a client stripped its ownerRef and
	// cluster-name label; it is still referenced by "live".
	bare := machine("")
	forMove := func(cluster string) *infrav1.TerraformMachine {
		m := machine("")
		m.Annotations = map[string]string{clusterctlv1.DeleteForMoveAnnotation: ""}
		if cluster != "" {
			m.Labels = map[string]string{clusterv1.ClusterNameLabel: cluster}
		}
		return m
	}
	owned := machine("")
	owned.OwnerReferences = []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: "live", UID: "u"}}
	relabeled := machine("")
	relabeled.Labels = map[string]string{clusterv1.ClusterNameLabel: "paused"}

	forbidden := http.StatusUnprocessableEntity
	tests := []struct {
		name     string
		obj      *infrav1.TerraformMachine
		machines []*clusterv1.Machine
		listErr  error
		getErr   error
		status   int32
	}{
		{name: "no Machine references it", obj: bare},
		{name: "referenced by a live Machine", obj: bare, machines: []*clusterv1.Machine{live}, status: int32(forbidden)},
		{name: "owner reference present, Machine live", obj: owned, machines: []*clusterv1.Machine{live}, status: int32(forbidden)},
		{name: "owner reference and label stripped, Machine still live", obj: bare, machines: []*clusterv1.Machine{live}, status: int32(forbidden)},
		{name: "label points at another Cluster, Machine still live", obj: relabeled, machines: []*clusterv1.Machine{live}, status: int32(forbidden)},
		{name: "owner reference to a missing Machine, none references it", obj: owned},
		{name: "referencing Machine is deleting", obj: bare, machines: []*clusterv1.Machine{deleting}},
		{name: "deleting and live Machines: live blocks", obj: bare, machines: []*clusterv1.Machine{deleting, live}, status: int32(forbidden)},
		{name: "Machine names another TerraformMachine", obj: bare, machines: []*clusterv1.Machine{wrongName}},
		{name: "Machine names another kind", obj: bare, machines: []*clusterv1.Machine{wrongKind}},
		{name: "Machine names another API group", obj: bare, machines: []*clusterv1.Machine{wrongGroup}},
		{name: "Machine in another namespace", obj: bare, machines: []*clusterv1.Machine{elsewhere}},
		{name: "delete-for-move with the Cluster paused", obj: forMove("paused"), machines: []*clusterv1.Machine{live}},
		// The annotation alone is not a move: it would skip drain and hooks.
		{name: "delete-for-move with the Cluster running", obj: forMove("running"), machines: []*clusterv1.Machine{live}, status: int32(forbidden)},
		{name: "delete-for-move without a cluster-name label", obj: forMove(""), machines: []*clusterv1.Machine{live}, status: int32(forbidden)},
		{name: "delete-for-move with the Cluster missing", obj: forMove("gone"), machines: []*clusterv1.Machine{live}, status: int32(forbidden)},
		{name: "delete-for-move, Cluster lookup fails", obj: forMove("paused"), getErr: errors.New("boom"), status: http.StatusInternalServerError},
		{name: "Machine list fails", obj: bare, listErr: errors.New("boom"), status: http.StatusInternalServerError},
	}
	scheme := testScheme(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// The fake client mutates its seed objects; give each subtest copies.
			b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pausedCluster.DeepCopy(), runningCluster.DeepCopy())
			for _, m := range tt.machines {
				b = b.WithObjects(m.DeepCopy())
			}
			b = b.WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if tt.getErr != nil {
						return tt.getErr
					}
					return c.Get(ctx, key, obj, opts...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if tt.listErr != nil {
						return tt.listErr
					}
					return c.List(ctx, list, opts...)
				},
			})
			_, err := (&TerraformMachine{Reader: b.Build()}).ValidateDelete(context.Background(), tt.obj)
			if tt.status == 0 {
				wantInvalid(t, err, false)
				return
			}
			var status apierrors.APIStatus
			if !errors.As(err, &status) || status.Status().Code != tt.status {
				t.Fatalf("error = %v, want HTTP %d", err, tt.status)
			}
			if tt.status == int32(forbidden) && !strings.Contains(err.Error(), "delete the Machine live instead") {
				t.Errorf("error = %v, want it to name the Machine to delete", err)
			}
		})
	}
}

// dryRunContext returns a context carrying an admission request whose
// DryRun field is dryRun, with no other request fields set.
func dryRunContext(dryRun bool) context.Context {
	return admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{DryRun: &dryRun},
	})
}

// TestTerraformMachineTemplate proves TerraformMachineTemplate.ValidateCreate
// validates the template metadata and source, ValidateUpdate forbids any
// spec.template.spec change except during a ClusterClass topology dry-run,
// fails without an admission request, and ValidateDelete allows every
// delete.
func TestTerraformMachineTemplate(t *testing.T) {
	t.Parallel()
	w := &TerraformMachineTemplate{}
	tmpl := func(image string) *infrav1.TerraformMachineTemplate {
		return &infrav1.TerraformMachineTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "t"},
			Spec: infrav1.TerraformMachineTemplateSpec{Template: infrav1.TerraformMachineTemplateResource{
				Spec: infrav1.TerraformMachineSpec{WorkspaceSpec: infrav1.WorkspaceSpec{Source: infrav1.Source{Image: image}}},
			}},
		}
	}
	topologyDryRun := func(o *infrav1.TerraformMachineTemplate) *infrav1.TerraformMachineTemplate {
		o.Annotations = map[string]string{clusterv1.TopologyDryRunAnnotation: ""}
		return o
	}
	badMeta := tmpl(testImage)
	badMeta.Spec.Template.ObjectMeta = &clusterv1.ObjectMeta{Labels: map[string]string{"bad key!": "v"}}

	_, err := w.ValidateCreate(context.Background(), tmpl(testImage))
	wantInvalid(t, err, false)
	_, err = w.ValidateCreate(context.Background(), badMeta)
	wantInvalid(t, err, true, "spec.template.metadata.labels")
	_, err = w.ValidateCreate(context.Background(), tmpl(""))
	wantInvalid(t, err, true, "spec.template.spec.source.image")

	tests := []struct {
		name    string
		ctx     context.Context
		updated *infrav1.TerraformMachineTemplate
		invalid bool
		frag    string
	}{
		{name: "unchanged", ctx: dryRunContext(false), updated: tmpl(testImage)},
		{name: "spec change", ctx: dryRunContext(false), updated: tmpl("ghcr.io/example/module:v2"), invalid: true, frag: "spec.template.spec: Forbidden"},
		{name: "plain dry-run is still checked", ctx: dryRunContext(true), updated: tmpl("ghcr.io/example/module:v2"), invalid: true, frag: "spec.template.spec: Forbidden"},
		{name: "topology dry-run skips immutability", ctx: dryRunContext(true), updated: topologyDryRun(tmpl("ghcr.io/example/module:v2"))},
		{name: "metadata still validated", ctx: dryRunContext(false), updated: badMeta, invalid: true, frag: "spec.template.metadata.labels"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := w.ValidateUpdate(tt.ctx, tmpl(testImage), tt.updated)
			wantInvalid(t, err, tt.invalid, tt.frag)
		})
	}

	_, err = w.ValidateUpdate(context.Background(), tmpl(testImage), tmpl(testImage))
	if !apierrors.IsBadRequest(err) {
		t.Errorf("update without an admission request: error = %v, want BadRequest", err)
	}
	// Templates stay fully immutable, operational policy included.
	withJobs := tmpl(testImage)
	withJobs.Spec.Template.Spec.Jobs = &infrav1.JobPolicy{ActiveDeadlineSeconds: 60}
	_, err = w.ValidateUpdate(dryRunContext(false), tmpl(testImage), withJobs)
	wantInvalid(t, err, true, "spec.template.spec: Forbidden")

	obj := tmpl(testImage)
	if _, err := w.ValidateDelete(context.Background(), obj); err != nil {
		t.Errorf("ValidateDelete: %v", err)
	}

	// A template never carries a providerID, on create or update.
	withID := tmpl(testImage)
	withID.Spec.Template.Spec.ProviderID = "aws:///i-1"
	_, err = w.ValidateCreate(context.Background(), withID)
	wantInvalid(t, err, true, "spec.template.spec.providerID: Forbidden")
	_, err = w.ValidateUpdate(dryRunContext(true), withID, withID)
	wantInvalid(t, err, true, "spec.template.spec.providerID: Forbidden")
}

// TestTerraformClusterTemplate proves TerraformClusterTemplate.ValidateCreate
// validates the source and warns, without rejecting, when the template sets
// no identityRef (even with only defaults.identityRef set), ValidateUpdate
// forbids a spec.template.spec change except during a ClusterClass topology
// dry-run, fails without an admission request, and ValidateDelete allows
// every delete.
func TestTerraformClusterTemplate(t *testing.T) {
	t.Parallel()
	w := &TerraformClusterTemplate{}
	tmpl := func(image, identity string) *infrav1.TerraformClusterTemplate {
		return &infrav1.TerraformClusterTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "t"},
			Spec: infrav1.TerraformClusterTemplateSpec{Template: infrav1.TerraformClusterTemplateResource{
				Spec: infrav1.TerraformClusterSpec{WorkspaceSpec: infrav1.WorkspaceSpec{Source: infrav1.Source{Image: image}, IdentityRef: infrav1.IdentityReference{Name: identity}}},
			}},
		}
	}

	warnings, err := w.ValidateCreate(context.Background(), tmpl(testImage, "id"))
	wantInvalid(t, err, false)
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	// No identity: a warning, not a rejection.
	warnings, err = w.ValidateCreate(context.Background(), tmpl(testImage, ""))
	wantInvalid(t, err, false)
	if len(warnings) != 1 || warnings[0] != missingIdentityWarning {
		t.Errorf("warnings = %v, want the missing-identity warning", warnings)
	}
	_, err = w.ValidateCreate(context.Background(), tmpl("", "id"))
	wantInvalid(t, err, true, "spec.template.spec.source.image")

	_, err = w.ValidateUpdate(dryRunContext(false), tmpl(testImage, "id"), tmpl(testImage, "id"))
	wantInvalid(t, err, false)
	_, err = w.ValidateUpdate(dryRunContext(false), tmpl(testImage, "id"), tmpl(testImage, "other"))
	wantInvalid(t, err, true, "spec.template.spec: Forbidden")
	topo := tmpl(testImage, "other")
	topo.Annotations = map[string]string{clusterv1.TopologyDryRunAnnotation: ""}
	warnings, err = w.ValidateUpdate(dryRunContext(true), tmpl(testImage, "id"), topo)
	wantInvalid(t, err, false)
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	_, err = w.ValidateUpdate(context.Background(), tmpl(testImage, "id"), tmpl(testImage, "id"))
	if !apierrors.IsBadRequest(err) {
		t.Errorf("update without an admission request: error = %v, want BadRequest", err)
	}

	// A template may leave identityRef to a ClusterClass patch even when
	// defaults.identityRef is set: warned, not rejected.
	onlyDefaults := tmpl(testImage, "")
	onlyDefaults.Spec.Template.Spec.Defaults = &infrav1.TerraformClusterDefaults{IdentityRef: infrav1.IdentityReference{Name: "id"}}
	warnings, err = w.ValidateCreate(context.Background(), onlyDefaults)
	wantInvalid(t, err, false)
	if len(warnings) != 1 {
		t.Errorf("warnings = %v, want the missing-identity warning", warnings)
	}

	obj := tmpl(testImage, "id")
	if _, err := w.ValidateDelete(context.Background(), obj); err != nil {
		t.Errorf("ValidateDelete: %v", err)
	}
}

// TestTerraformClusterIdentity proves ValidateCreate and ValidateUpdate
// require a Secret reference, reject the ambiguous empty allowedNamespaces
// object, allow a list and a selector together, and validate a list
// namespace name and a selector's syntax.
func TestTerraformClusterIdentity(t *testing.T) {
	t.Parallel()
	w := &TerraformClusterIdentity{Client: sarClient(t, func(*authorizationv1.SubjectAccessReview) bool { return true }, nil)}
	ctx := requestContext("admin")
	ref := infrav1.SecretReference{Name: "creds", Namespace: "captf-system"}
	tests := []struct {
		name    string
		spec    infrav1.TerraformClusterIdentitySpec
		invalid bool
		frag    string
	}{
		{name: "no namespaces allowed", spec: infrav1.TerraformClusterIdentitySpec{SecretRef: ref}},
		{name: "all namespaces", spec: infrav1.TerraformClusterIdentitySpec{SecretRef: ref, AllowedNamespaces: &infrav1.AllowedNamespaces{Selector: &metav1.LabelSelector{}}}},
		{name: "empty object rejected", spec: infrav1.TerraformClusterIdentitySpec{SecretRef: ref, AllowedNamespaces: &infrav1.AllowedNamespaces{}},
			invalid: true, frag: "write selector: {} to allow every namespace"},
		{name: "list and selector", spec: infrav1.TerraformClusterIdentitySpec{SecretRef: ref, AllowedNamespaces: &infrav1.AllowedNamespaces{
			List:     []string{"team-a", "b"},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tenant": "a"}},
		}}},
		{name: "no secret ref", spec: infrav1.TerraformClusterIdentitySpec{}, invalid: true, frag: "spec.secretRef.namespace: Required"},
		{name: "bad namespace", spec: infrav1.TerraformClusterIdentitySpec{SecretRef: ref, AllowedNamespaces: &infrav1.AllowedNamespaces{
			List: []string{"ok", "Not_OK"},
		}}, invalid: true, frag: "spec.allowedNamespaces.list[1]"},
		{name: "bad selector", spec: infrav1.TerraformClusterIdentitySpec{SecretRef: ref, AllowedNamespaces: &infrav1.AllowedNamespaces{
			Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "k", Operator: "Bogus"}}},
		}}, invalid: true, frag: "spec.allowedNamespaces.selector"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			obj := &infrav1.TerraformClusterIdentity{ObjectMeta: metav1.ObjectMeta{Name: "i"}, Spec: tt.spec}
			_, err := w.ValidateCreate(ctx, obj)
			wantInvalid(t, err, tt.invalid, tt.frag)
			_, err = w.ValidateUpdate(ctx, obj, obj)
			wantInvalid(t, err, tt.invalid, tt.frag)
		})
	}
}

// requestContext carries an admission request from user, as the webhook
// server builds it. It returns the resulting context.
func requestContext(user string) context.Context {
	return admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
			Username: user, UID: user + "-uid", Groups: []string{"g1"},
			Extra: map[string]authenticationv1.ExtraValue{"scope": {"s"}},
		}},
	})
}

// sarClient answers SubjectAccessReview creates with allow(sar), recording
// each review in *seen, or fails them with createErr; test t fails on a
// scheme registration error. It returns the built fake client.
func sarClient(t *testing.T, allow func(*authorizationv1.SubjectAccessReview) bool, createErr error, seen ...*[]authorizationv1.SubjectAccessReview) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(identityScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			sar, ok := o.(*authorizationv1.SubjectAccessReview)
			if !ok {
				return c.Create(ctx, o, opts...)
			}
			if createErr != nil {
				return createErr
			}
			for _, s := range seen {
				*s = append(*s, *sar.DeepCopy())
			}
			sar.Status.Allowed = allow(sar)
			return nil
		},
	}).Build()
}

// identityScheme builds a runtime.Scheme carrying the CAPI core, CAPTF and
// authorization types, failing test t on a registration error. It returns
// the built scheme.
func identityScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := testScheme(t)
	for _, add := range []func(*runtime.Scheme) error{infrav1.AddToScheme, authorizationv1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return s
}

// TestTerraformClusterIdentitySecretAccess: creating an identity, or
// pointing one at another Secret, needs `get` on that Secret for the
// requesting user, checked with a SubjectAccessReview.
func TestTerraformClusterIdentitySecretAccess(t *testing.T) {
	t.Parallel()
	ref := infrav1.SecretReference{Name: "creds", Namespace: "captf-system"}
	obj := &infrav1.TerraformClusterIdentity{ObjectMeta: metav1.ObjectMeta{Name: "i"}, Spec: infrav1.TerraformClusterIdentitySpec{SecretRef: ref}}
	moved := obj.DeepCopy()
	moved.Spec.SecretRef.Name = "other"
	relabeled := obj.DeepCopy()
	relabeled.Labels = map[string]string{"a": "b"}
	onlyAdmin := func(sar *authorizationv1.SubjectAccessReview) bool { return sar.Spec.User == "admin" }

	t.Run("review content", func(t *testing.T) {
		t.Parallel()
		var seen []authorizationv1.SubjectAccessReview
		w := &TerraformClusterIdentity{Client: sarClient(t, onlyAdmin, nil, &seen)}
		if _, err := w.ValidateCreate(requestContext("admin"), obj); err != nil {
			t.Fatalf("ValidateCreate: %v", err)
		}
		if len(seen) != 1 {
			t.Fatalf("reviews = %d, want 1", len(seen))
		}
		spec := seen[0].Spec
		want := authorizationv1.ResourceAttributes{Namespace: "captf-system", Verb: "get", Resource: "secrets", Name: "creds"}
		if spec.ResourceAttributes == nil || *spec.ResourceAttributes != want {
			t.Errorf("resource attributes = %+v, want %+v", spec.ResourceAttributes, want)
		}
		if spec.User != "admin" || spec.UID != "admin-uid" || len(spec.Groups) != 1 || spec.Groups[0] != "g1" ||
			len(spec.Extra["scope"]) != 1 || spec.Extra["scope"][0] != "s" {
			t.Errorf("review subject = %+v", spec)
		}
	})

	tests := []struct {
		name    string
		user    string
		op      func(w *TerraformClusterIdentity, ctx context.Context) error
		invalid bool
		status  int32
	}{
		{name: "create, may read", user: "admin", op: func(w *TerraformClusterIdentity, ctx context.Context) error {
			_, err := w.ValidateCreate(ctx, obj)
			return err
		}},
		{name: "create, may not read", user: "mallory", invalid: true, op: func(w *TerraformClusterIdentity, ctx context.Context) error {
			_, err := w.ValidateCreate(ctx, obj)
			return err
		}},
		{name: "update to another Secret, may not read", user: "mallory", invalid: true, op: func(w *TerraformClusterIdentity, ctx context.Context) error {
			_, err := w.ValidateUpdate(ctx, obj, moved)
			return err
		}},
		{name: "update keeping the Secret needs no read", user: "mallory", op: func(w *TerraformClusterIdentity, ctx context.Context) error {
			_, err := w.ValidateUpdate(ctx, obj, relabeled)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := &TerraformClusterIdentity{Client: sarClient(t, onlyAdmin, nil)}
			err := tt.op(w, requestContext(tt.user))
			wantInvalid(t, err, tt.invalid, `spec.secretRef: Forbidden: user "mallory" may not get Secret captf-system/`)
		})
	}

	t.Run("review fails", func(t *testing.T) {
		t.Parallel()
		w := &TerraformClusterIdentity{Client: sarClient(t, onlyAdmin, errors.New("boom"))}
		_, err := w.ValidateCreate(requestContext("admin"), obj)
		if !apierrors.IsInternalError(err) {
			t.Errorf("error = %v, want InternalError", err)
		}
	})
	t.Run("no admission request", func(t *testing.T) {
		t.Parallel()
		w := &TerraformClusterIdentity{Client: sarClient(t, onlyAdmin, nil)}
		_, err := w.ValidateCreate(context.Background(), obj)
		if !apierrors.IsInternalError(err) {
			t.Errorf("error = %v, want InternalError", err)
		}
	})
}

// TestTerraformClusterIdentityDelete: an identity in use, directly, through
// a machine's cluster fallback, or by a mirror still listed in its status,
// cannot be deleted; the message names what uses it.
func TestTerraformClusterIdentityDelete(t *testing.T) {
	t.Parallel()
	const name = "aws"
	labels := map[string]string{clusterv1.ClusterNameLabel: "c1"}
	tc := func(own, def string) *infrav1.TerraformCluster {
		c := &infrav1.TerraformCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "tc", Labels: labels},
			Spec: infrav1.TerraformClusterSpec{WorkspaceSpec: infrav1.WorkspaceSpec{IdentityRef: infrav1.IdentityReference{Name: own}}}}
		if def != "" {
			c.Spec.Defaults = &infrav1.TerraformClusterDefaults{IdentityRef: infrav1.IdentityReference{Name: def}}
		}
		return c
	}
	tm := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "tm", Labels: labels}}
	tmp := &infrav1.TerraformMachinePool{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "tmp", Labels: labels}}
	id := &infrav1.TerraformClusterIdentity{ObjectMeta: metav1.ObjectMeta{Name: name}}
	mirrored := id.DeepCopy()
	mirrored.Status.Namespaces = []string{"team-b"}

	tests := []struct {
		name    string
		objs    []client.Object
		id      *infrav1.TerraformClusterIdentity
		listErr error
		frag    string
		status  int32
	}{
		{name: "unused", objs: []client.Object{tc("other", "")}, id: id},
		{name: "cluster identityRef", objs: []client.Object{tc(name, "")}, id: id,
			frag: "in use by TerraformCluster team-a/tc", status: http.StatusUnprocessableEntity},
		{name: "machine through the cluster defaults", objs: []client.Object{tc("other", name), tm.DeepCopy()}, id: id,
			frag: "in use by TerraformMachine team-a/tm", status: http.StatusUnprocessableEntity},
		{name: "pool through the cluster defaults", objs: []client.Object{tc("other", name), tmp.DeepCopy()}, id: id,
			frag: "in use by TerraformMachinePool team-a/tmp", status: http.StatusUnprocessableEntity},
		{name: "still mirrored", id: mirrored,
			frag: "still mirrored into namespace(s) team-b", status: http.StatusUnprocessableEntity},
		{name: "list fails", id: id, listErr: errors.New("boom"), status: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := fake.NewClientBuilder().WithScheme(identityScheme(t)).WithObjects(tt.objs...)
			if tt.listErr != nil {
				b = b.WithInterceptorFuncs(interceptor.Funcs{
					List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
						return tt.listErr
					},
				})
			}
			_, err := (&TerraformClusterIdentity{Reader: b.Build()}).ValidateDelete(context.Background(), tt.id)
			if tt.status == 0 {
				wantInvalid(t, err, false)
				return
			}
			if tt.frag != "" {
				wantInvalid(t, err, true, tt.frag)
			}
			var status apierrors.APIStatus
			if !errors.As(err, &status) || status.Status().Code != tt.status {
				t.Fatalf("error = %v, want HTTP %d", err, tt.status)
			}
		})
	}
}
