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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestVariablesSourceToTemplates proves a labeled ConfigMap or Secret maps
// to the machine templates of its namespace that name it, and only those.
func TestVariablesSourceToTemplates(t *testing.T) {
	t.Parallel()
	tpl := func(ns, name string, from ...infrav1.VariablesSource) *infrav1.TerraformMachineTemplate {
		t := &infrav1.TerraformMachineTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
		t.Spec.Template.Spec.VariablesFrom = from
		return t
	}
	cmRef := func(n string) infrav1.VariablesSource {
		return infrav1.VariablesSource{ConfigMapRef: infrav1.VariablesSourceReference{Name: n}}
	}
	secretRef := func(n string) infrav1.VariablesSource {
		return infrav1.VariablesSource{SecretRef: infrav1.VariablesSourceReference{Name: n}}
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(tpl(testNS, "uses-cm", cmRef("vars")), tpl(testNS, "uses-secret", secretRef("vars")),
			tpl(testNS, "none"), tpl("other-ns", "uses-cm", cmRef("vars"))).
		WithIndex(&infrav1.TerraformMachineTemplate{}, VariablesSourceIndex, TemplateVariablesSourceIndexer).Build()
	fn := VariablesSourceToTemplates(c)
	if got := names(fn(t.Context(), configMapMeta(metav1.ObjectMeta{Namespace: testNS, Name: "vars", Labels: varsLabel}))); !slices.Equal(got, []string{testNS + "/uses-cm"}) {
		t.Errorf("ConfigMap -> templates = %v", got)
	}
	if got := names(fn(t.Context(), secretMeta(metav1.ObjectMeta{Namespace: testNS, Name: "vars", Labels: varsLabel}))); !slices.Equal(got, []string{testNS + "/uses-secret"}) {
		t.Errorf("Secret -> templates = %v", got)
	}
	if TemplateVariablesSourceIndexer(&infrav1.TerraformMachine{}) != nil {
		t.Error("the indexer accepted the wrong kind")
	}
}
