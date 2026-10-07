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

package shared

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// TestPreambleConvertsForegroundDeletion proves a deleting object carrying
// the foregroundDeletion finalizer loses it, and only it, with one
// ForegroundDeletionConverted event; the next pass changes nothing.
func TestPreambleConvertsForegroundDeletion(t *testing.T) {
	t.Parallel()
	e := newEnv(t, world(machine(notPaused, deleting, func(m *infrav1.TerraformMachine) {
		m.Finalizers = []string{testFinal, metav1.FinalizerDeleteDependents}
	}))...)
	for range 2 {
		if _, err := Preamble(t.Context(), e.d, e.kindFor(t, readyOwner())); err != nil {
			t.Fatal(err)
		}
	}
	if m := e.get(t); m == nil || !slices.Equal(m.Finalizers, []string{testFinal}) {
		t.Errorf("finalizers = %+v, want only ours", m)
	}
	if n := e.rec.count(EventForegroundDeletionConverted); n != 1 {
		t.Errorf("ForegroundDeletionConverted events = %d, want 1", n)
	}
}

// secretsOf returns the Secrets in e that sel matches, failing t on error.
func secretsOf(t *testing.T, e *env, sel client.MatchingLabelsSelector) []corev1.Secret {
	t.Helper()
	var list corev1.SecretList
	if err := e.c.List(t.Context(), &list, client.InNamespace(testNS), sel); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// TestCascadeKeepsProtectedSecrets proves what a foreground cascading
// deletion leaves: the garbage collector deletes every dependent of the
// object, and only the protected ones (the base state Secret, the backups
// and both inputs records) stay readable, marked for deletion, while the
// unprotected -part-N chunk goes. The backup is still found for a restore.
func TestCascadeKeepsProtectedSecrets(t *testing.T) {
	t.Parallel()
	e := retainEnv(t, deleting)
	m := e.get(t)
	if err := state.Adopt(t.Context(), e.c, m, suffixOf(t, state.KindTerraformMachine, testName), "h1:x"); err != nil {
		t.Fatal(err)
	}
	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	// What the garbage collector does to every dependent.
	for _, s := range e.objectSecretMetas(t) {
		if err := e.c.Delete(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: s.meta.Name}}); err != nil {
			t.Fatal(err)
		}
	}
	chunks := secretsOf(t, e, client.MatchingLabelsSelector{Selector: state.Selector(suffix)})
	if len(chunks) != 1 || chunks[0].Name != state.SecretName(suffix) || chunks[0].DeletionTimestamp == nil || len(chunks[0].Data[state.DataKey]) == 0 {
		t.Errorf("state Secrets after the cascade = %d, want the base one, marked, with its data", len(chunks))
	}
	backups, err := state.ListBackups(t.Context(), e.c, testNS, suffix)
	if err != nil || len(backups) != 1 || !backups[0].Complete {
		t.Errorf("backups after the cascade = %+v, %v; want the complete one", backups, err)
	}
	d, err := inputs.Read(t.Context(), e.c, testNS, "m", testName)
	if err != nil || d == nil || d.Attempt == nil || d.Applied == nil {
		t.Errorf("inputs records after the cascade = %+v, %v; want both", d, err)
	}
}

// TestDestroyCleanupDeletesProtectedSecrets proves a successful destroy
// removes every protected Secret for good, state, backups and inputs
// records alike, so no Secret is left marked for deletion and a new object
// of the same name finds no backup of the destroyed one.
func TestDestroyCleanupDeletesProtectedSecrets(t *testing.T) {
	t.Parallel()
	e := retainEnv(t, deleting, func(m *infrav1.TerraformMachine) { m.Spec.DeletionPolicy = infrav1.DeletionPolicyDestroy })
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	js := e.jobsOf(t)
	if len(js) != 1 || jobs.OpOf(&js[0]) != jobs.OpDestroy {
		t.Fatalf("Jobs %v, want one destroy", js)
	}
	e.finishJob(t, js[0].Name, jobs.Succeeded)
	if _, err := Reconcile(t.Context(), e.d, healthyKind(t, e, testName)); err != nil {
		t.Fatal(err)
	}
	suffix := suffixOf(t, state.KindTerraformMachine, testName)
	for _, sel := range []client.MatchingLabelsSelector{{Selector: state.Selector(suffix)}, {Selector: state.BackupSelector(suffix)}} {
		if left := secretsOf(t, e, sel); len(left) != 0 {
			t.Errorf("%d Secrets left after the destroy: %s", len(left), left[0].Name)
		}
	}
	for _, name := range []string{inputs.Name("m", testName), inputs.AppliedName("m", testName)} {
		if err := e.c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, &corev1.Secret{}); client.IgnoreNotFound(err) != nil || err == nil {
			t.Errorf("inputs record %s: %v, want it gone", name, err)
		}
	}
}
