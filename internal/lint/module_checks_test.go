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
	"strings"
	"testing"
)

// TestTofuShadowDeclarations: a .tofu file that adds or changes a resource,
// data source, local, module call, required_providers entry,
// required_version, or a nested block (variable validation, provider
// assume_role) is reported, naming the declaration but not its values; a
// reformatted copy is not.
func TestTofuShadowDeclarations(t *testing.T) {
	t.Parallel()
	const base = "terraform {\n  required_version = \">= 1.5\"\n  required_providers {\n    aws = { source = \"hashicorp/aws\" }\n  }\n}\n" +
		"variable \"v\" {\n  validation {\n    condition = var.v != \"\"\n    error_message = \"m\"\n  }\n}\n" +
		"provider \"aws\" {\n  assume_role {\n    role_arn = \"a\"\n  }\n}\n" +
		"locals {\n  a = 1\n}\n" +
		"data \"aws_ami\" \"x\" {\n  owners = [\"1\"]\n}\n" +
		"resource \"null_resource\" \"r\" {\n  triggers = {}\n}\n" +
		"module \"child\" {\n  source = \"example.com/a/b/c\"\n}\n"
	for _, tt := range []struct {
		name  string
		tofu  string
		wants []string // substrings, one per expected finding
	}{
		{"identical, reformatted", strings.ReplaceAll(base, "  ", "    "), nil},
		{"extra resource", base + "resource \"null_resource\" \"extra\" {}\n", []string{"resource.null_resource.extra"}},
		{"extra data", base + "data \"aws_ami\" \"y\" {}\n", []string{"data.aws_ami.y"}},
		{"extra local", base + "locals {\n  b = 2\n}\n", []string{"local.b"}},
		{"changed local", strings.Replace(base, "a = 1", "a = 2", 1), []string{"local.a"}},
		{"extra module", base + "module \"other\" {\n  source = \"example.com/g/h/i\"\n}\n", []string{"module.other"}},
		{"changed module", strings.Replace(base, "example.com/a/b/c", "example.com/d/e/f", 1), []string{"module.child"}},
		{"extra provider alias", base + "provider \"aws\" {\n  alias = \"b\"\n}\n", []string{"provider.aws."}},
		{"nested provider block", strings.Replace(base, "role_arn = \"a\"", "role_arn = \"b\"", 1), []string{"provider.aws"}},
		{"nested validation block", strings.Replace(base, "var.v != \"\"", "true", 1), []string{"variable.v"}},
		{"required_version", strings.Replace(base, ">= 1.5", ">= 2", 1), []string{"required_version"}},
		{"required_providers entry", strings.Replace(base, "hashicorp/aws", "other/aws", 1), []string{"required_providers.aws"}},
		{"changed resource", strings.Replace(base, "triggers = {}", "triggers = {a = 1}", 1), []string{"resource.null_resource.r"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := poolModule(t, map[string]string{"main.tf": base, "main.tofu": tt.tofu})
			got := moduleTofuShadow(m)
			if len(got) != len(tt.wants) {
				t.Fatalf("findings = %+v, want %d", got, len(tt.wants))
			}
			for i, w := range tt.wants {
				if !strings.Contains(got[i].Message, w) {
					t.Errorf("message %q lacks %q", got[i].Message, w)
				}
				if strings.Contains(got[i].Message, "triggers") || strings.Contains(got[i].Message, "other/aws") {
					t.Errorf("message leaks a value: %q", got[i].Message)
				}
			}
		})
	}
}

// TestProviderConfigLiterals: module/provider-config flags a credential-named
// string literal at the top of a provider block, in a nested block, in an
// object value and in JSON, and ignores references, non-credential names,
// empty strings and non-string values.
func TestProviderConfigLiterals(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		file  string
		body  string
		wants int
	}{
		{"top-level literal", "p.tf", "provider \"aws\" {\n  secret_key = \"s\"\n}\n", 1},
		{"nested block literal", "p.tf", "provider \"aws\" {\n  assume_role {\n    token = \"t\"\n  }\n}\n", 1},
		{"object literal", "p.tf", "provider \"x\" {\n  auth = {\n    client_secret = \"s\"\n  }\n}\n", 1},
		{"new name api_key", "p.tf", "provider \"x\" {\n  API_KEY = \"k\"\n}\n", 1},
		{"private_key", "p.tf", "provider \"x\" {\n  private_key = \"-----BEGIN\"\n}\n", 1},
		{"reference", "p.tf", "provider \"x\" {\n  assume_role {\n    token = var.t\n  }\n}\n", 0},
		{"interpolated", "p.tf", "provider \"x\" {\n  password = \"p-${var.t}\"\n}\n", 0},
		{"empty", "p.tf", "provider \"x\" {\n  password = \"\"\n}\n", 0},
		{"number", "p.tf", "provider \"x\" {\n  token = 5\n}\n", 0},
		{"non-credential", "p.tf", "provider \"x\" {\n  region = \"eu\"\n  uri = \"qemu:///system\"\n}\n", 0},
		{"JSON top-level", "p.tf.json", `{"provider": {"x": {"password": "p"}}}`, 1},
		{"JSON nested object", "p.tf.json", `{"provider": {"x": {"assume_role": {"client_secret": "s", "role": "r"}}}}`, 1},
		{"JSON nested list", "p.tf.json", `{"provider": {"x": {"assume_role": [{"token": "s"}]}}}`, 1},
		{"JSON reference", "p.tf.json", `{"provider": {"x": {"assume_role": {"token": "${var.t}"}}}}`, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := poolModule(t, map[string]string{tt.file: tt.body})
			got := moduleProviderConfig(m)
			if len(got) != tt.wants {
				t.Errorf("findings = %+v, want %d", got, tt.wants)
			}
		})
	}
}
