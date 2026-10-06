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

package lint

import (
	"testing"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/varschema"
)

// TestVariablesSchema maps a module's user variables, skipping captf_ names
// and contract inputs, to the compact schema.
func TestVariablesSchema(t *testing.T) {
	t.Parallel()
	m := module(t, contract.RoleMachine, func(_, v map[string]string) {
		v["instance_type"] = "  type = string\n"
		v["disk_gib"] = "  type = number\n  default = 20\n"
		v["tags"] = "  type = map(string)\n  default = {}\n"
		v["zones"] = "  type = set(string)\n  default = []\n"
		v["pair"] = "  type = tuple([string, number])\n  default = [\"a\", 1]\n"
		v["anything"] = "  default = null\n"
		v["volume"] = "  type = object({\n    size = number\n    kind = optional(string, \"gp3\")\n  })\n  default = null\n"
	})
	s, err := VariablesSchema(m, contract.RoleMachine)
	if err != nil {
		t.Fatal(err)
	}
	got, err := varschema.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"object","properties":{"anything":{},"disk_gib":{"type":"number"},"instance_type":{"type":"string"},` +
		`"pair":{"type":"array","prefixItems":[{"type":"string"},{"type":"number"}]},` +
		`"tags":{"type":"object","additionalProperties":{"type":"string"}},` +
		`"volume":{"type":"object","properties":{"kind":{"type":"string"},"size":{"type":"number"}},"required":["size"]},` +
		`"zones":{"type":"array","items":{"type":"string"}}},"required":["instance_type"],"additionalProperties":false}`
	if got != want {
		t.Errorf("schema\n got %s\nwant %s", got, want)
	}
	if _, err := VariablesSchema(m, "worker"); err == nil {
		t.Error("an unknown role is an error")
	}
}
