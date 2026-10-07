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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crenvtest "sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/webhooks"
)

const (
	// managerUser is the username the webhooks treat as the provider's own
	// ServiceAccount; no test authenticates as it.
	managerUser = "system:serviceaccount:captf-system:captf-controller-manager"
	// usersGroup is the group of the test users; the ClusterRole bound to it
	// grants every verb on the provider's kinds, so a denial is the
	// webhooks' or the CRDs', never RBAC's.
	usersGroup = "captf-test-users"
	// eventually is the longest any polling assertion waits for the API
	// server or the webhook server.
	eventually = 30 * time.Second
	// tick is the polling interval of those assertions.
	tick = 100 * time.Millisecond
)

var (
	// scheme knows the core, Cluster API and CAPTF types.
	scheme = newScheme()
	// bare is the API server with the CRDs and no webhooks: what the CRD
	// schemas and CEL rules enforce on their own.
	bare *testEnv
	// hooked is the API server with the CRDs and the validating webhooks
	// served by the real handlers.
	hooked *testEnv
	// failopen is a second hooked API server, for the test that stops its
	// webhook server.
	failopen *testEnv
)

// newScheme returns the scheme of every type the suite reads or writes. It
// panics on a registration error, which is a programming error.
func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, clusterv1.AddToScheme, infrav1.AddToScheme} {
		if err := add(s); err != nil {
			panic(err)
		}
	}
	return s
}

// testEnv is one running API server: its configuration, an admin client
// and, when webhooks are installed, the stop switch of the webhook server.
type testEnv struct {
	// env is the envtest environment, stopped by stop.
	env *crenvtest.Environment
	// cfg authenticates as system:masters.
	cfg *rest.Config
	// client is the admin client, direct to the API server.
	client client.Client
	// stopWebhooks stops the webhook server and returns once it has
	// stopped; nil for an environment without webhooks.
	stopWebhooks func()
	// usersMu guards users: envtest's AddUser is not safe for concurrent use.
	usersMu sync.Mutex
	// users holds the client of each user already provisioned, by name.
	users map[string]client.Client
}

// startEnv starts an API server with the CRDs of config/crd/bases and the
// Cluster API stand-ins of testdata. With withWebhooks it also installs the
// validating webhooks of config/webhook and serves them from a controller-
// runtime manager running the real handlers, and returns once the API
// server reaches them. It returns the running environment or the first
// error.
func startEnv(withWebhooks bool) (*testEnv, error) {
	e := &crenvtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("testdata", "crds"),
		},
		ErrorIfCRDPathMissing: true,
		Scheme:                scheme,
	}
	if withWebhooks {
		e.WebhookInstallOptions = crenvtest.WebhookInstallOptions{
			Paths: []string{filepath.Join("..", "..", "config", "webhook", "manifests.yaml")},
		}
	}
	cfg, err := e.Start()
	if err != nil {
		return nil, fmt.Errorf("start envtest: %w", err)
	}
	te := &testEnv{env: e, cfg: cfg}
	if te.client, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		_ = e.Stop()
		return nil, fmt.Errorf("admin client: %w", err)
	}
	if err := te.grantUsers(); err != nil {
		_ = e.Stop()
		return nil, err
	}
	if withWebhooks {
		if err := te.serveWebhooks(); err != nil {
			_ = e.Stop()
			return nil, err
		}
	}
	return te, nil
}

// grantUsers creates the ClusterRole and binding that let usersGroup do
// everything with the provider's kinds and read Namespaces. It returns the
// first create error.
func (te *testEnv) grantUsers() error {
	ctx := context.Background()
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: usersGroup},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{infrav1.GroupVersion.Group}, Resources: []string{"*"}, Verbs: []string{"*"}},
			{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get", "list"}},
		},
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: usersGroup},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: usersGroup},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "Group", Name: usersGroup}},
	}
	for _, o := range []client.Object{role, binding} {
		if err := te.client.Create(ctx, o); err != nil {
			return fmt.Errorf("grant test users: %w", err)
		}
	}
	return nil
}

// serveWebhooks starts a manager whose webhook server listens where
// envtest pointed the installed webhook configurations, with every
// handler of internal/webhooks registered, and waits until the API server
// reaches it. It sets stopWebhooks. It returns an error when the manager
// cannot be built or the webhooks do not become reachable.
func (te *testEnv) serveWebhooks() error {
	opts := &te.env.WebhookInstallOptions
	mgr, err := ctrl.NewManager(te.cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    opts.LocalServingHost,
			Port:    opts.LocalServingPort,
			CertDir: opts.LocalServingCertDir,
		}),
	})
	if err != nil {
		return fmt.Errorf("webhook manager: %w", err)
	}
	if err := webhooks.SetupWebhooks(mgr, managerUser, nil); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	var once sync.Once
	te.stopWebhooks = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	if err := crenvtest.WaitForWebhooks(te.cfg, nil, opts.ValidatingWebhooks, *opts); err != nil {
		te.stopWebhooks()
		return fmt.Errorf("wait for webhooks: %w", err)
	}
	return nil
}

// stop stops the webhook server, if any, then the API server. It returns
// the API server's stop error.
func (te *testEnv) stop() error {
	if te.stopWebhooks != nil {
		te.stopWebhooks()
	}
	return te.env.Stop()
}

// userClient returns a client that authenticates as the user name, a
// member of usersGroup, provisioning the user on first use. Test t fails
// when the user cannot be provisioned.
func (te *testEnv) userClient(t *testing.T, name string) client.Client {
	t.Helper()
	te.usersMu.Lock()
	defer te.usersMu.Unlock()
	if c, ok := te.users[name]; ok {
		return c
	}
	u, err := te.env.AddUser(crenvtest.User{Name: name, Groups: []string{usersGroup}}, nil)
	if err != nil {
		t.Fatalf("add user %s: %v", name, err)
	}
	c, err := client.New(u.Config(), client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client for %s: %v", name, err)
	}
	if te.users == nil {
		te.users = map[string]client.Client{}
	}
	te.users[name] = c
	return c
}

// TestMain starts the three API servers concurrently, runs the tests m
// holds and stops them. It exits 1 when an API server does not start (the usual
// cause is a missing KUBEBUILDER_ASSETS).
func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "envtest: KUBEBUILDER_ASSETS is not set; run `make test-envtest`")
		os.Exit(1)
	}
	var wg sync.WaitGroup
	var errs [3]error
	for i, s := range []struct {
		dst      **testEnv
		webhooks bool
	}{{&bare, false}, {&hooked, true}, {&failopen, true}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			*s.dst, errs[i] = startEnv(s.webhooks)
		}()
	}
	wg.Wait()
	code := 1
	if err := errors.Join(errs[:]...); err != nil {
		fmt.Fprintln(os.Stderr, "envtest:", err)
	} else {
		code = m.Run()
	}
	for _, te := range []*testEnv{bare, hooked, failopen} {
		if te == nil {
			continue
		}
		if err := te.stop(); err != nil {
			fmt.Fprintln(os.Stderr, "envtest: stop:", err)
		}
	}
	os.Exit(code)
}

// newNamespace creates a namespace with a unique name in c's API server
// and returns the name. Test t fails when the create fails.
func newNamespace(t *testing.T, c client.Client) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "t-"}}
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return ns.Name
}

// unstructuredOf returns a v1alpha1 object of kind kind named name in
// namespace ns (cluster-scoped when empty) whose spec is spec.
func unstructuredOf(kind, ns, name string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(infrav1.GroupVersion.WithKind(kind))
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

// wantInvalid fails t unless err is an Invalid (HTTP 422) status error
// whose message contains want, the rule's message.
func wantInvalid(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted, want Invalid containing %q", want)
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("error = %v (reason %s), want Invalid", err, apierrors.ReasonForError(err))
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want one containing %q", err, want)
	}
}

// clientKey returns the namespaced name of o.
func clientKey(o interface {
	GetNamespace() string
	GetName() string
}) types.NamespacedName {
	return types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}
}
