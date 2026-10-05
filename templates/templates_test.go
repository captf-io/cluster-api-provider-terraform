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

package templates_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	bootstrapv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
	controlplanev1 "sigs.k8s.io/cluster-api/api/controlplane/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/webhooks"
)

// validateIdentity uses t to run the identity admission webhook on id as
// an administrator allowed to read the referenced Secret: the templates
// must render an identity the webhook accepts. The webhook's
// SubjectAccessReview is answered in-process. It returns the webhook's
// error, if any.
func validateIdentity(t *testing.T, id *infrav1.TerraformClusterIdentity) error {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, infrav1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if sar, ok := o.(*authorizationv1.SubjectAccessReview); ok {
				sar.Status.Allowed = true
				return nil
			}
			return c.Create(ctx, o, opts...)
		},
	}).Build()
	ctx := admission.NewContextWithRequest(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		UserInfo:  authenticationv1.UserInfo{Username: "admin"},
	}})
	_, err := (&webhooks.TerraformClusterIdentity{Client: c, Reader: c}).ValidateCreate(ctx, id)
	return err
}

// clusterVars sets every variable cluster-template.yaml needs and none of
// the ones it defaults.
var clusterVars = map[string]string{
	"CLUSTER_NAME":                "demo",
	"KUBERNETES_VERSION":          "v1.36.2",
	"CONTROL_PLANE_MACHINE_COUNT": "3",
	"WORKER_MACHINE_COUNT":        "2",
	"TERRAFORM_CLUSTER_IMAGE":     "ghcr.io/example/cluster:v1",
	"TERRAFORM_MACHINE_IMAGE":     "ghcr.io/example/machine:v1",
	"TERRAFORM_IDENTITY_NAME":     "example",
}

// varRef matches clusterctl's ${VAR} and ${VAR:=default} variable
// references, capturing the name and, when present, the default.
var varRef = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)(?::=([^}]*))?\}`)

// escaped is clusterctl's $$ escape for a literal $ (drone/envsubst), used by
// shell scripts embedded in templates.
const escaped = "\x00dollar\x00"

// render reads path and substitutes ${VAR} and ${VAR:=default} from vars
// as clusterctl does, keeps $$ as a literal $, and fails on a variable
// that is neither in vars nor defaulted. It returns the rendered bytes, or
// an error naming the missing variables.
func render(path string, vars map[string]string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var missing []string
	src := strings.ReplaceAll(string(raw), "$$", escaped)
	out := varRef.ReplaceAllStringFunc(src, func(ref string) string {
		m := varRef.FindStringSubmatch(ref)
		if v, ok := vars[m[1]]; ok {
			return v
		}
		if strings.Contains(ref, ":=") {
			return m[2]
		}
		missing = append(missing, m[1])
		return ref
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("value for variables [%s] is not set", strings.Join(missing, ", "))
	}
	return []byte(strings.ReplaceAll(out, escaped, "$")), nil
}

// with returns a copy of clusterVars with overrides applied: a non-empty
// value replaces the variable, an empty one deletes it.
func with(overrides map[string]string) map[string]string {
	vars := map[string]string{}
	for k, v := range clusterVars {
		vars[k] = v
	}
	for k, v := range overrides {
		if v == "" {
			delete(vars, k)
		} else {
			vars[k] = v
		}
	}
	return vars
}

// decode uses t to split a rendered template and decode every document
// strictly (unknown fields fail) into its registered type, failing t on
// error. It returns the decoded objects.
func decode(t *testing.T, rendered []byte) []runtime.Object {
	t.Helper()
	objs, err := decodeAll(rendered)
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

// decodeAll splits rendered into YAML documents and decodes each
// strictly (unknown fields fail) into its registered type, skipping a
// document that is empty once comments are stripped. It returns the
// decoded objects, or an error when a document fails to read or decode.
func decodeAll(rendered []byte) ([]runtime.Object, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, clusterv1.AddToScheme, controlplanev1.AddToScheme,
		bootstrapv1.AddToScheme, infrav1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	deserializer := serializer.NewCodecFactory(scheme, serializer.EnableStrict).UniversalDeserializer()
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(rendered)))
	var objs []runtime.Object
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return objs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read document: %w", err)
		}
		if len(bytes.TrimSpace(stripComments(doc))) == 0 {
			continue
		}
		obj, _, err := deserializer.Decode(doc, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("strict decode: %w\n%s", err, doc)
		}
		objs = append(objs, obj)
	}
}

// TestStrictDecode: the decoder the template tests rely on rejects a
// misspelled field and an unregistered kind, so a passing decode means
// every field name exists in the API types.
func TestStrictDecode(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"misspelled field": "apiVersion: controlplane.cluster.x-k8s.io/v1beta2\nkind: KubeadmControlPlane\nmetadata:\n  name: x\nspec:\n  remediation:\n    maxRetries: 3\n",
		"unknown kind":     "apiVersion: cluster.x-k8s.io/v1beta2\nkind: MachineThing\nmetadata:\n  name: x\n",
	} {
		if _, err := decodeAll([]byte(doc)); err == nil {
			t.Errorf("%s: decoded without error", name)
		}
	}
}

// stripComments returns doc with every line whose trimmed content starts
// with "#" removed.
func stripComments(doc []byte) []byte {
	var out [][]byte
	for _, line := range bytes.Split(doc, []byte("\n")) {
		if !bytes.HasPrefix(bytes.TrimSpace(line), []byte("#")) {
			out = append(out, line)
		}
	}
	return bytes.Join(out, []byte("\n"))
}

// objectsOf returns the elements of objs whose dynamic type is T.
func objectsOf[T runtime.Object](objs []runtime.Object) []T {
	var out []T
	for _, o := range objs {
		if v, ok := o.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

// TestClusterTemplate: every object decodes strictly into its v1beta2 or
// v1alpha1 type with no namespace (clusterctl sets --target-namespace), the
// KubeadmControlPlane and MachineHealthCheck defaults hold, and the CAPTF
// objects pass the admission webhooks.
func TestClusterTemplate(t *testing.T) {
	t.Parallel()
	rendered, err := render("cluster-template.yaml", clusterVars)
	if err != nil {
		t.Fatal(err)
	}
	objs := decode(t, rendered)
	if len(objs) != 9 {
		t.Fatalf("%d objects, want 9", len(objs))
	}
	for _, o := range objs {
		if ns := o.(interface{ GetNamespace() string }).GetNamespace(); ns != "" {
			t.Errorf("%T has namespace %q; the template must leave it to --target-namespace", o, ns)
		}
	}

	cluster := objectsOf[*clusterv1.Cluster](objs)[0]
	port := cluster.Spec.ClusterNetwork.APIServerPort
	kcp := objectsOf[*controlplanev1.KubeadmControlPlane](objs)[0]
	if kcp.Spec.Remediation.MaxRetry == nil || *kcp.Spec.Remediation.MaxRetry != 3 ||
		kcp.Spec.Remediation.RetryPeriodSeconds == nil || *kcp.Spec.Remediation.RetryPeriodSeconds != 300 {
		t.Errorf("KCP remediation = %+v, want maxRetry 3, retryPeriodSeconds 300", kcp.Spec.Remediation)
	}
	if s := kcp.Spec.Rollout.Strategy.RollingUpdate.MaxSurge; s == nil || s.IntValue() != 0 {
		t.Errorf("KCP maxSurge = %v, want 0", s)
	}
	if kcp.Spec.Replicas == nil || *kcp.Spec.Replicas != 3 || kcp.Spec.Version != "v1.36.2" {
		t.Errorf("KCP replicas %v version %q", kcp.Spec.Replicas, kcp.Spec.Version)
	}
	cfg := kcp.Spec.KubeadmConfigSpec
	if cfg.InitConfiguration.LocalAPIEndpoint.BindPort != port || cfg.JoinConfiguration.ControlPlane == nil ||
		cfg.JoinConfiguration.ControlPlane.LocalAPIEndpoint.BindPort != port {
		t.Errorf("bindPort differs from clusterNetwork.apiServerPort %d", port)
	}

	tc := objectsOf[*infrav1.TerraformCluster](objs)[0]
	if tc.Spec.Source.Image != clusterVars["TERRAFORM_CLUSTER_IMAGE"] || tc.Spec.IdentityRef.Name != "example" ||
		tc.Spec.Defaults == nil || tc.Spec.Defaults.IdentityRef.Name != "example" {
		t.Errorf("TerraformCluster spec = %+v", tc.Spec)
	}
	if _, err := (&webhooks.TerraformCluster{}).ValidateCreate(context.Background(), tc); err != nil {
		t.Errorf("TerraformCluster webhook: %v", err)
	}
	tmts := objectsOf[*infrav1.TerraformMachineTemplate](objs)
	if len(tmts) != 2 {
		t.Fatalf("%d TerraformMachineTemplates, want 2", len(tmts))
	}
	for _, tmt := range tmts {
		if tmt.Spec.Template.Spec.Source.Image != clusterVars["TERRAFORM_MACHINE_IMAGE"] {
			t.Errorf("%s image %q", tmt.Name, tmt.Spec.Template.Spec.Source.Image)
		}
		w := &webhooks.TerraformMachineTemplate{}
		if _, err := w.ValidateCreate(context.Background(), tmt); err != nil {
			t.Errorf("%s webhook: %v", tmt.Name, err)
		}
	}

	md := objectsOf[*clusterv1.MachineDeployment](objs)[0]
	if md.Spec.Replicas == nil || *md.Spec.Replicas != 2 {
		t.Errorf("MachineDeployment replicas = %v, want 2", md.Spec.Replicas)
	}
	checkMHCs(t, objectsOf[*clusterv1.MachineHealthCheck](objs), 1800, 1200, 3600, 1800)

	// Every reference resolves to an object in the template with that
	// group, kind and name: a typo here decodes and renders but fails at
	// reconcile.
	for _, ref := range []struct {
		from string
		ref  clusterv1.ContractVersionedObjectReference
	}{
		{"Cluster controlPlaneRef", cluster.Spec.ControlPlaneRef},
		{"Cluster infrastructureRef", cluster.Spec.InfrastructureRef},
		{"KCP machineTemplate infrastructureRef", kcp.Spec.MachineTemplate.Spec.InfrastructureRef},
		{"MachineDeployment infrastructureRef", md.Spec.Template.Spec.InfrastructureRef},
		{"MachineDeployment bootstrap configRef", md.Spec.Template.Spec.Bootstrap.ConfigRef},
	} {
		if !resolves(objs, ref.ref) {
			t.Errorf("%s %+v resolves to no object in the template", ref.from, ref.ref)
		}
	}
	if kcp.Spec.MachineTemplate.Spec.InfrastructureRef.Name == md.Spec.Template.Spec.InfrastructureRef.Name {
		t.Error("control plane and workers share a TerraformMachineTemplate")
	}
	for _, mhc := range objectsOf[*clusterv1.MachineHealthCheck](objs) {
		if name, ok := mhc.Spec.Selector.MatchLabels[clusterv1.MachineDeploymentNameLabel]; ok && name != md.Name {
			t.Errorf("worker MHC selects deployment %q, want %q", name, md.Name)
		}
		if mhc.Spec.ClusterName != cluster.Name {
			t.Errorf("MHC %s clusterName %q, want %q", mhc.Name, mhc.Spec.ClusterName, cluster.Name)
		}
	}
	if md.Spec.ClusterName != cluster.Name || md.Spec.Template.Spec.ClusterName != cluster.Name {
		t.Errorf("MachineDeployment clusterName %q / %q, want %q", md.Spec.ClusterName, md.Spec.Template.Spec.ClusterName, cluster.Name)
	}
}

// resolves reports whether objs holds an object with ref's group, kind and
// name.
func resolves(objs []runtime.Object, ref clusterv1.ContractVersionedObjectReference) bool {
	return slices.ContainsFunc(objs, func(o runtime.Object) bool {
		gvk := o.GetObjectKind().GroupVersionKind()
		name := o.(interface{ GetName() string }).GetName()
		return gvk.Group == ref.APIGroup && gvk.Kind == ref.Kind && name == ref.Name
	})
}

// checkMHCs uses t to check that mhcs holds exactly a worker and a
// control-plane MachineHealthCheck, each checking InfrastructureReady=False
// with the given unhealthy timeout (worker or cp) and node-startup timeout
// (startup or cpStartup).
func checkMHCs(t *testing.T, mhcs []*clusterv1.MachineHealthCheck, worker, startup, cp, cpStartup int32) {
	t.Helper()
	if len(mhcs) != 2 {
		t.Fatalf("%d MachineHealthChecks, want 2", len(mhcs))
	}
	for _, mhc := range mhcs {
		conds := mhc.Spec.Checks.UnhealthyMachineConditions
		if len(conds) != 1 || conds[0].Type != clusterv1.InfrastructureReadyCondition ||
			conds[0].Status != "False" || conds[0].TimeoutSeconds == nil {
			t.Fatalf("%s conditions = %+v, want InfrastructureReady=False with a timeout", mhc.Name, conds)
		}
		_, controlPlane := mhc.Spec.Selector.MatchLabels[clusterv1.MachineControlPlaneLabel]
		switch {
		case controlPlane:
			if *conds[0].TimeoutSeconds != cp || mhc.Spec.Checks.NodeStartupTimeoutSeconds == nil ||
				*mhc.Spec.Checks.NodeStartupTimeoutSeconds != cpStartup {
				t.Errorf("control-plane MHC timeout %d, node startup %v, want %d, %d",
					*conds[0].TimeoutSeconds, mhc.Spec.Checks.NodeStartupTimeoutSeconds, cp, cpStartup)
			}
		default:
			if *conds[0].TimeoutSeconds != worker || mhc.Spec.Checks.NodeStartupTimeoutSeconds == nil ||
				*mhc.Spec.Checks.NodeStartupTimeoutSeconds != startup {
				t.Errorf("worker MHC timeout %d, node startup %v, want %d, %d",
					*conds[0].TimeoutSeconds, mhc.Spec.Checks.NodeStartupTimeoutSeconds, worker, startup)
			}
		}
	}
}

// TestClusterTemplateTimeoutOverrides: the MHC timeouts are variables with
// defaults, so a user value replaces the default.
func TestClusterTemplateTimeoutOverrides(t *testing.T) {
	t.Parallel()
	rendered, err := render("cluster-template.yaml", with(map[string]string{
		"TERRAFORM_UNHEALTHY_TIMEOUT":       "2400",
		"TERRAFORM_NODE_STARTUP_TIMEOUT":    "900",
		"TERRAFORM_CP_UNHEALTHY_TIMEOUT":    "7200",
		"TERRAFORM_CP_NODE_STARTUP_TIMEOUT": "2700",
	}))
	if err != nil {
		t.Fatal(err)
	}
	checkMHCs(t, objectsOf[*clusterv1.MachineHealthCheck](decode(t, rendered)), 2400, 900, 7200, 2700)
}

// TestClusterTemplateRequiredVariables: images, identity and sizes have no
// defaults, so rendering without one fails and names it.
func TestClusterTemplateRequiredVariables(t *testing.T) {
	t.Parallel()
	for name := range clusterVars {
		_, err := render("cluster-template.yaml", with(map[string]string{name: ""}))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: err = %v, want it named", name, err)
		}
	}
}

// TestIdentity: the admin-applied identity admits only ${NAMESPACE}, and its
// Secret lives in the provider namespace.
func TestIdentity(t *testing.T) {
	t.Parallel()
	rendered, err := render("identity.yaml", map[string]string{"TERRAFORM_IDENTITY_NAME": "example", "NAMESPACE": "team-a"})
	if err != nil {
		t.Fatal(err)
	}
	objs := decode(t, rendered)
	ids := objectsOf[*infrav1.TerraformClusterIdentity](objs)
	secrets := objectsOf[*corev1.Secret](objs)
	if len(ids) != 1 || len(secrets) != 1 {
		t.Fatalf("got %d identities and %d Secrets, want 1 each", len(ids), len(secrets))
	}
	id := ids[0]
	if id.Spec.AllowedNamespaces == nil || !slices.Equal(id.Spec.AllowedNamespaces.List, []string{"team-a"}) ||
		id.Spec.SecretRef.Name != "example" || id.Spec.SecretRef.Namespace != "captf-system" {
		t.Errorf("identity spec = %+v", id.Spec)
	}
	if secrets[0].Namespace != "captf-system" || secrets[0].Name != "example" {
		t.Errorf("Secret %s/%s, want captf-system/example", secrets[0].Namespace, secrets[0].Name)
	}
	if err := validateIdentity(t, id); err != nil {
		t.Errorf("identity webhook: %v", err)
	}
	if _, err := render("identity.yaml", map[string]string{"TERRAFORM_IDENTITY_NAME": "example"}); err == nil ||
		!strings.Contains(err.Error(), "NAMESPACE") {
		t.Errorf("without NAMESPACE: err = %v", err)
	}
}
