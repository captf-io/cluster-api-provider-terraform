//go:build e2e

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

package noop

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/env"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/kubewait"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/objects"
)

// identityWait bounds the identity reaching Ready.
const identityWait = 2 * time.Minute

// setup is stage 1: it records the manager pod, creates the run's
// namespace, the identity's credentials Secret and the cluster-scoped
// TerraformClusterIdentity that admits only that namespace (waiting for
// Ready=True, SecretFound), and the bootstrap Secret every machine reads.
// Then it starts tracking the namespace's Job pods. The Secret comes
// before the identity: the identity controller does not watch Secrets.
// It runs under ctx and fails t on any problem.
func (s *suite) setup(ctx context.Context, t *testing.T) {
	m, err := s.currentManager(ctx)
	if err != nil {
		t.Fatalf("%v; inspect: %s", err, s.kubectl("-n "+env.ManagerNamespace+" get pods -o wide"))
	}
	s.manager = m
	t.Logf("manager pod %s (uid %s, %d restarts so far)", m.name, m.uid, m.restarts)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.ns, Labels: map[string]string{"captf.io/e2e": "noop"}}}
	if _, err := s.c.Kube.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace %s: %v", s.ns, err)
	}
	s.tracker = s.trackPods()

	if _, err := s.c.Kube.CoreV1().Secrets(env.ManagerNamespace).Create(ctx, objects.IdentitySecret(env.ManagerNamespace, s.identity), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the identity Secret %s/%s: %v", env.ManagerNamespace, s.identity, err)
	}
	s.create(ctx, t, objects.TerraformClusterIdentityGVR, objects.TerraformClusterIdentity(s.identity, env.ManagerNamespace, s.identity, []string{s.ns}))
	if _, err := kubewait.ConditionStatus(ctx, s.c.Dynamic, objects.TerraformClusterIdentityGVR, "", s.identity,
		"Ready", "True", "SecretFound", waitOpts(t, identityWait)); err != nil {
		t.Fatalf("expected TerraformClusterIdentity %s Ready=True (SecretFound): %v; inspect: %s", s.identity, err, s.kubectl("get terraformclusteridentity "+s.identity+" -o yaml"))
	}

	if _, err := s.c.Kube.CoreV1().Secrets(s.ns).Create(ctx, objects.BootstrapSecret(s.ns, bootstrapName, bootstrapValue), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the bootstrap Secret %s/%s: %v", s.ns, bootstrapName, err)
	}
	t.Logf("namespace %s, identity %s (Ready, admits only %s) and bootstrap Secret %s ready", s.ns, s.identity, s.ns, bootstrapName)
}
