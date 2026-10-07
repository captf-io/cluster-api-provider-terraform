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
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestSetupWebhooks registers every webhook with a manager that is never
// started, and checks that controller-runtime serves exactly the paths the
// kubebuilder markers put into config/webhook/manifests.yaml. A mismatch
// would reject every request once failurePolicy=Fail applies.
func TestSetupWebhooks(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	if err := infrav1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	mgr, server := newTestManager(t, scheme)
	if err := SetupWebhooks(mgr, ""); err != nil {
		t.Fatalf("SetupWebhooks: %v", err)
	}
	mux := server.WebhookMux()

	var markers []string
	for _, f := range []string{
		"terraformcluster.go", "terraformclustertemplate.go",
		"terraformmachine.go", "terraformmachinetemplate.go",
		"terraformmachinepool.go", "terraformmachinepooltemplate.go",
		"terraformclusteridentity.go", "terraformplan.go",
	} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range regexp.MustCompile(`\+kubebuilder:webhook:.*?path=([^,]+),`).FindAllSubmatch(src, -1) {
			markers = append(markers, string(m[1]))
		}
		if strings.Contains(string(src), "mutating=true") {
			t.Errorf("%s declares a mutating webhook; nothing is defaulted at admission", f)
		}
	}
	// One validating webhook per kind, and no defaulting webhook.
	if len(markers) != 8 {
		t.Fatalf("found %d webhook markers, want 8: %v", len(markers), markers)
	}
	for _, path := range markers {
		if _, pattern := mux.Handler(httptest.NewRequest(http.MethodPost, path, http.NoBody)); pattern != path {
			t.Errorf("marker path %s is not served (matched %q)", path, pattern)
		}
		mutate := strings.Replace(path, "/validate-", "/mutate-", 1)
		if _, pattern := mux.Handler(httptest.NewRequest(http.MethodPost, mutate, http.NoBody)); pattern == mutate {
			t.Errorf("defaulting path %s is served", mutate)
		}
	}
}

// TestSetupWebhooksNeedsCAPIScheme: without cluster.x-k8s.io types every
// owned TerraformMachine delete would be a 500, so setup must fail instead.
func TestSetupWebhooksNeedsCAPIScheme(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := infrav1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	mgr, _ := newTestManager(t, scheme)
	if err := SetupWebhooks(mgr, ""); err == nil || !strings.Contains(err.Error(), "lacks cluster.x-k8s.io Machine") {
		t.Fatalf("SetupWebhooks error = %v, want the missing-Machine error", err)
	}
}

// newTestManager builds a manager that is never started, with scheme and a
// webhook server bound to an ephemeral port; test t fails on a build error.
// It returns the manager and its webhook server.
func newTestManager(t *testing.T, scheme *runtime.Scheme) (ctrl.Manager, webhook.Server) {
	t.Helper()
	server := webhook.NewServer(webhook.Options{})
	mgr, err := ctrl.NewManager(&rest.Config{Host: "https://127.0.0.1:1"}, ctrl.Options{
		Scheme:        scheme,
		Metrics:       metricsserver.Options{BindAddress: "0"},
		WebhookServer: server,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return mgr, server
}
