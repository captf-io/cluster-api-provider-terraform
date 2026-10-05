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

package image

import (
	"archive/tar"
	"slices"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// hardlink returns an entry named name that hard-links to the tar entry to.
func hardlink(name, to string) entry {
	return entry{name: name, typ: tar.TypeLink, link: to, mode: 0o644}
}

// backendTF is a module file that declares a backend.
const backendTF = "terraform {\n  backend \"local\" {}\n}\n"

// TestModuleLinks: a symlink or hard link inside the module is linted as the
// file it names, and one that ends elsewhere is an image/module-link error.
func TestModuleLinks(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		extra   []entry
		want    string // finding ID that must appear
		wantNot string // finding ID that must not
	}{
		{"symlink inside the module", []entry{file("captf/module/real/backend.tf", backendTF), symlink("captf/module/backend.tf", "real/backend.tf")}, "module/backend", IDModuleLink},
		{"absolute symlink inside the module", []entry{file("captf/module/real.tf", backendTF), symlink("captf/module/backend.tf", "/captf/module/real.tf")}, "module/backend", IDModuleLink},
		{"hard link", []entry{file("captf/module/real.tf", backendTF), hardlink("captf/module/backend.tf", "captf/module/real.tf")}, "module/backend", IDModuleLink},
		{"symlinked parent directory", []entry{file("captf/module/real/backend.tf", backendTF), symlink("captf/module/alias", "real"), symlink("captf/module/b.tf", "alias/backend.tf")}, "module/backend", IDModuleLink},
		{"symlink outside the module", []entry{file("etc/b.tf", backendTF), symlink("captf/module/backend.tf", "/etc/b.tf")}, IDModuleLink, "module/backend"},
		{"dangling symlink", []entry{symlink("captf/module/backend.tf", "nowhere.tf")}, IDModuleLink, ""},
		{"symlink cycle", []entry{symlink("captf/module/a.tf", "b.tf"), symlink("captf/module/b.tf", "a.tf")}, IDModuleLink, ""},
	} {
		got := ids(check(t, build(t, "amd64", nil, append(goodEntries(t, "cluster"), tt.extra...)), contract.RoleCluster))
		if !slices.Contains(got, tt.want) || (tt.wantNot != "" && slices.Contains(got, tt.wantNot)) {
			t.Errorf("%s: findings %v, want %s and not %q", tt.name, got, tt.want, tt.wantNot)
		}
	}
}

// TestModuleLinkOnly: a module whose only .tf file is a symlink is present.
func TestModuleLinkOnly(t *testing.T) {
	t.Parallel()
	es := []entry{
		dir("captf/"), dir("captf/module/"), exe("captf/runtime"),
		file("captf/module/real.txt", backendTF), symlink("captf/module/main.tf", "real.txt"),
	}
	got := ids(check(t, build(t, "amd64", nil, es), contract.RoleCluster))
	if slices.Contains(got, IDModulePresent) || !slices.Contains(got, "module/backend") {
		t.Errorf("findings %v: want the linked file linted", got)
	}
}

// TestProviderLinks: linked provider zips and unpacked directories are
// judged by their targets.
func TestProviderLinks(t *testing.T) {
	t.Parallel()
	aws := "terraform {\n  required_providers {\n    aws = {\n      source = \"hashicorp/aws\"\n    }\n  }\n}\n"
	base := append(goodEntries(t, "cluster"), file("captf/module/providers.tf", aws))
	const reg = "captf/providers/registry.terraform.io/hashicorp/aws/"
	zip := "terraform-provider-aws_5.1.0_linux_amd64.zip"
	for _, tt := range []struct {
		name   string
		mirror []entry
		want   []string
	}{
		{"symlinked zip to a real file", []entry{file("blobs/z", "z"), symlink(reg+zip, "/blobs/z")}, nil},
		{"dangling zip symlink", []entry{symlink(reg+zip, "/blobs/gone")}, []string{IDProvidersComplete, IDProvidersComplete}},
		{"zip symlink to a directory", []entry{dir("blobs/d"), symlink(reg+zip, "/blobs/d")}, []string{IDProvidersComplete, IDProvidersComplete}},
		{"hard-linked zip", []entry{file("blobs/z", "z"), hardlink(reg+zip, "blobs/z")}, nil},
		{"symlinked unpacked directory", []entry{dir("blobs/u"), symlink(reg+"5.1.0/linux_amd64", "/blobs/u")}, nil},
		{"dangling unpacked directory", []entry{symlink(reg+"5.1.0/linux_amd64", "/blobs/gone")}, []string{IDProvidersComplete, IDProvidersComplete}},
	} {
		got := ids(check(t, build(t, "amd64", nil, append(slices.Clone(base), tt.mirror...)), contract.RoleCluster))
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s: findings %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestUserUnresolved: a named user draws a warning; root and a numeric uid
// do not.
func TestUserUnresolved(t *testing.T) {
	t.Parallel()
	for user, want := range map[string]bool{"nobody": true, "app:app": true, "65532": false, "65532:65532": false, "root": false, "": false} {
		got := ids(check(t, build(t, "amd64", func(cf *v1.ConfigFile) { cf.Config.User = user }, goodEntries(t, "cluster")), contract.RoleCluster))
		if slices.Contains(got, IDUserUnresolved) != want {
			t.Errorf("user %q: findings %v, want %s present=%v", user, got, IDUserUnresolved, want)
		}
	}
}
