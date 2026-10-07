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
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TemplateVariablesSourceIndexer returns o, a TerraformMachineTemplate's,
// spec.template.spec.variablesFrom sources, in the VariablesSourceIndex
// format. Only machine templates are indexed: they check their variables
// against the image's schema (VariablesValid); the other templates run
// nothing and check nothing.
func TemplateVariablesSourceIndexer(o client.Object) []string {
	t, ok := o.(*infrav1.TerraformMachineTemplate)
	if !ok {
		return nil
	}
	return VariablesSourceKeys(t.Spec.Template.Spec.VariablesFrom)
}

// VariablesSourceToTemplates maps a labeled ConfigMap or Secret to the
// TerraformMachineTemplates in its namespace that name it in variablesFrom,
// so fixing a referenced source re-checks VariablesValid. It lists
// templates through the reader c and returns the MapFunc to register as a
// watch handler.
func VariablesSourceToTemplates(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		key := variablesSourceKey(o)
		if key == "" {
			return nil
		}
		return list(ctx, c, &infrav1.TerraformMachineTemplateList{}, client.InNamespace(o.GetNamespace()),
			client.MatchingFields{VariablesSourceIndex: key})
	}
}
