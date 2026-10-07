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

package jobs

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// TestListFiltersController shows List returns only the Jobs the owner
// controls: a Job of an earlier object of the same name (another UID, its
// garbage collection pending) and one with no controller carry the same
// labels but are left out.
func TestListFiltersController(t *testing.T) {
	t.Parallel()
	owner := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Name: "prod-md-0-abcde", Namespace: "team-a", UID: "uid-new"}}
	earlier := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Name: owner.Name, Namespace: owner.Namespace, UID: "uid-old"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := NewRunner(c, c)
	ctx := t.Context()

	mine, _ := Build(spec(OpApply), "runner:img")
	if err := r.Create(ctx, owner, mine); err != nil {
		t.Fatalf("Create: %v", err)
	}
	s := spec(OpApply)
	s.Attempt = 2
	old, _ := Build(s, "runner:img")
	if err := r.Create(ctx, earlier, old); err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.Attempt = 3
	orphan, _ := Build(s, "runner:img")
	if err := c.Create(ctx, orphan); err != nil {
		t.Fatalf("Create: %v", err)
	}

	list, err := r.List(ctx, owner, state.KindTerraformMachine)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != mine.Name {
		t.Errorf("List = %v, want only %s", names(list), mine.Name)
	}
	if list, _ := r.List(ctx, earlier, state.KindTerraformMachine); len(list) != 1 || list[0].Name != old.Name {
		t.Errorf("List of the earlier object = %v, want only %s", names(list), old.Name)
	}
}

// names returns the names of list's Jobs.
func names(list []batchv1.Job) []string {
	out := make([]string, 0, len(list))
	for i := range list {
		out = append(out, list[i].Name)
	}
	return out
}
