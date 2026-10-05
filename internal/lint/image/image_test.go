/*
Copyright 2026 The cluster-api-provider-terraform Authors.

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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/lint"
)

// entry is one tar entry of a synthetic layer.
type entry struct {
	name string
	mode int64
	typ  byte
	body string
	link string
	uid  int
}

// dir returns a directory entry named name.
func dir(name string) entry { return entry{name: name, mode: 0o755, typ: tar.TypeDir} }

// file returns a regular-file entry named name with contents body.
func file(name, body string) entry {
	return entry{name: name, mode: 0o644, typ: tar.TypeReg, body: body}
}

// symlink returns an entry named name that links to target to.
func symlink(name, to string) entry {
	return entry{name: name, typ: tar.TypeSymlink, link: to, mode: 0o777}
}

// exe returns an executable regular-file entry named name.
func exe(name string) entry { return entry{name: name, mode: 0o755, typ: tar.TypeReg, body: "#!"} }

// mode returns a copy of e with its mode changed to m.
func mode(e entry, m int64) entry { e.mode = m; return e }

// owned returns a copy of e with its uid and gid changed to uid.
func owned(e entry, uid int) entry { e.uid = uid; return e }

// whiteout returns the entry that removes name from dir when a later layer
// is applied over it.
func whiteout(dir, name string) entry { return file(dir+"/.wh."+name, "") }

// layer uses t to build a tar layer from entries and returns it.
func layer(t *testing.T, entries []entry) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.typ, Linkname: e.link, Size: int64(len(e.body)), Uid: e.uid, Gid: e.uid}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// build uses t to make a linux/arch image from layers of entries; edit, if
// non-nil, changes its config after the good defaults are set. It returns
// the built image.
func build(t *testing.T, arch string, edit func(*v1.ConfigFile), layers ...[]entry) v1.Image {
	t.Helper()
	img := empty.Image
	for _, entries := range layers {
		var err error
		if img, err = mutate.AppendLayers(img, layer(t, entries)); err != nil {
			t.Fatal(err)
		}
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", arch
	cf.Config.User = "65532"
	cf.Config.Labels = map[string]string{labelRole: "cluster", labelContract: contract.Version}
	if edit != nil {
		edit(cf)
	}
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// goodEntries uses t to build the zero-findings image entries for role:
// tfcapi-lint's own good role module, an executable runtime, an empty
// mirror. It returns those entries.
func goodEntries(t *testing.T, role string) []entry {
	t.Helper()
	src := filepath.Join("..", "..", "..", "cmd", "tfcapi-lint", "testdata", "good", role)
	es := []entry{dir("captf/"), dir("captf/module/"), dir("captf/providers/"), exe("captf/runtime")}
	names, err := filepath.Glob(filepath.Join(src, "*.tf"))
	if err != nil || len(names) == 0 {
		t.Fatalf("no .tf files in %s: %v", src, err)
	}
	for _, path := range names {
		name := filepath.Base(path)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		es = append(es, file("captf/module/"+name, string(b)))
	}
	return es
}

// check uses t to run checkImage on img for role and returns the findings,
// failing t if checkImage itself errors.
func check(t *testing.T, img v1.Image, role contract.Role) []lint.Finding {
	t.Helper()
	fs, _, _, err := checkImage(img, Options{Role: role, Contract: contract.Version})
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

// ids returns fs's finding IDs, sorted.
func ids(fs []lint.Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.ID)
	}
	slices.Sort(out)
	return out
}

// TestGoodImage proves the good cluster image built from goodEntries has
// no findings.
func TestGoodImage(t *testing.T) {
	t.Parallel()
	if got := check(t, build(t, "amd64", nil, goodEntries(t, "cluster")), contract.RoleCluster); len(got) != 0 {
		t.Errorf("findings on the good image: %+v", got)
	}
}

// TestImageChecks: one synthetic image per check, asserting exact IDs.
func TestImageChecks(t *testing.T) {
	t.Parallel()
	withLabels := func(kv ...string) func(*v1.ConfigFile) {
		return func(cf *v1.ConfigFile) {
			for i := 0; i < len(kv); i += 2 {
				if kv[i+1] == "" {
					delete(cf.Config.Labels, kv[i])
				} else {
					cf.Config.Labels[kv[i]] = kv[i+1]
				}
			}
		}
	}
	replace := func(es []entry, name string, e entry) []entry {
		out := slices.Clone(es)
		for i := range out {
			if out[i].name == name {
				out[i] = e
			}
		}
		return out
	}
	good := goodEntries(t, "cluster")
	for _, tt := range []struct {
		name   string
		role   contract.Role
		edit   func(*v1.ConfigFile)
		layers [][]entry
		want   []string
	}{
		{"runtime 0644", contract.RoleCluster, nil, [][]entry{replace(good, "captf/runtime", mode(exe("captf/runtime"), 0o644))}, []string{IDRuntimePresent}},
		{"runtime owner-only exec, other user", contract.RoleCluster, nil, [][]entry{replace(good, "captf/runtime", mode(exe("captf/runtime"), 0o744))}, []string{IDRuntimePresent}},
		{"runtime owner-only exec, owning user", contract.RoleCluster, nil,
			[][]entry{replace(good, "captf/runtime", owned(mode(exe("captf/runtime"), 0o700), 65532))}, nil},
		{"runtime missing", contract.RoleCluster, nil, [][]entry{replace(good, "captf/runtime", dir("captf/bin-not-runtime/"))}, []string{IDRuntimePresent}},
		{"runtime a directory", contract.RoleCluster, nil, [][]entry{replace(good, "captf/runtime", dir("captf/runtime/"))}, []string{IDRuntimePresent}},
		{"runtime symlink", contract.RoleCluster, withLabels(labelRuntime, "terraform"),
			[][]entry{append(replace(good, "captf/runtime", symlink("captf/runtime", "/bin/terraform")), exe("bin/terraform"))}, nil},
		{"runtime relative symlink", contract.RoleCluster, nil,
			[][]entry{append(replace(good, "captf/runtime", symlink("captf/runtime", "../bin/tofu")), exe("bin/tofu"))}, nil},
		{"runtime dangling symlink", contract.RoleCluster, nil, [][]entry{replace(good, "captf/runtime", symlink("captf/runtime", "/bin/terraform"))}, []string{IDRuntimePresent}},
		{"runtime label tofu", contract.RoleCluster, withLabels(labelRuntime, "tofu"),
			[][]entry{append(replace(good, "captf/runtime", symlink("captf/runtime", "../bin/tofu")), exe("bin/tofu"))}, nil},
		{"runtime label mismatch", contract.RoleCluster, withLabels(labelRuntime, "tofu"),
			[][]entry{append(replace(good, "captf/runtime", symlink("captf/runtime", "/bin/terraform")), exe("bin/terraform"))}, []string{IDRuntimeVersion}},
		{"file under /captf/work", contract.RoleCluster, nil, [][]entry{append(slices.Clone(good), file("captf/work/x", "x"))}, []string{IDReservedPaths}},
		{"file under /captf/config", contract.RoleCluster, nil, [][]entry{append(slices.Clone(good), file("captf/config/main.tf.json", "{}"))}, []string{IDReservedPaths}},
		{"credentials dir with a file", contract.RoleCluster, nil,
			[][]entry{append(slices.Clone(good), file("var/run/captf/credentials/token", "x"))}, []string{IDReservedPaths}},
		{"empty reserved dir is fine", contract.RoleCluster, nil, [][]entry{append(slices.Clone(good), dir("captf/work/"))}, nil},
		{"whiteout removes a reserved file", contract.RoleCluster, nil,
			[][]entry{append(slices.Clone(good), file("captf/work/x", "x")), {whiteout("captf/work", "x")}}, nil},
		{"module missing", contract.RoleCluster, nil,
			[][]entry{{dir("captf/"), dir("captf/module/"), dir("captf/providers/"), exe("captf/runtime"), file("captf/module/README.md", "x")}}, []string{IDModulePresent}},
		{"module files only nested", contract.RoleCluster, nil,
			[][]entry{{dir("captf/"), dir("captf/module/"), dir("captf/providers/"), exe("captf/runtime"), file("captf/module/sub/main.tf", "")}}, []string{IDModulePresent}},
		{"no mirror", contract.RoleCluster, nil,
			[][]entry{slices.DeleteFunc(slices.Clone(good), func(e entry) bool { return e.name == "captf/providers/" })}, []string{IDProvidersAbsent}},
		{"bad mirror entry", contract.RoleCluster, nil, [][]entry{append(slices.Clone(good), file("captf/providers/aws.zip", "x"))}, []string{IDProvidersLayout}},
		{"role label mismatch", contract.RoleCluster, withLabels(labelRole, "machine"), [][]entry{good}, []string{IDLabelRole}},
		{"role label absent", contract.RoleCluster, withLabels(labelRole, ""), [][]entry{good}, []string{IDLabelRole}},
		{"contract label mismatch", contract.RoleCluster, withLabels(labelContract, "v1beta1"), [][]entry{good}, []string{IDLabelContract}},
		{"contract label absent", contract.RoleCluster, withLabels(labelContract, ""), [][]entry{good}, []string{IDLabelContract}},
		{"capacity on a cluster image", contract.RoleCluster, withLabels("io.captf.capacity", `{"cpu":"4"}`), [][]entry{good}, []string{IDLabelCapacity}},
		{"root user", contract.RoleCluster, func(cf *v1.ConfigFile) { cf.Config.User = "" }, [][]entry{good}, []string{IDUserRoot}},
		{"root by name", contract.RoleCluster, func(cf *v1.ConfigFile) { cf.Config.User = "root:root" }, [][]entry{good}, []string{IDUserRoot}},
		{"named user is not resolved", contract.RoleCluster, func(cf *v1.ConfigFile) { cf.Config.User = "captf" }, [][]entry{good}, []string{IDUserUnresolved}},
		{"entrypoint", contract.RoleCluster, func(cf *v1.ConfigFile) { cf.Config.Entrypoint = []string{"/bin/terraform"} }, [][]entry{good}, []string{IDEntrypoint}},
		{"unreadable module file", contract.RoleCluster, nil,
			[][]entry{append(slices.Clone(good), mode(file("captf/module/secret.txt", "x"), 0o600))}, []string{IDModuleReadable}},
		{"untraversable module dir", contract.RoleCluster, nil,
			[][]entry{replace(good, "captf/module/", mode(dir("captf/module/"), 0o700))}, []string{IDModuleReadable}},
		{"module findings are prefixed", contract.RoleCluster, nil,
			[][]entry{append(slices.Clone(good), file("captf/module/backend.tf", "terraform {\n  backend \"s3\" {}\n}\n"))}, []string{"module/backend"}},
	} {
		got := check(t, build(t, "amd64", tt.edit, tt.layers...), tt.role)
		if !slices.Equal(ids(got), tt.want) {
			t.Errorf("%s: findings %+v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestModuleFindingPaths: module findings carry /captf/module paths.
func TestModuleFindingPaths(t *testing.T) {
	t.Parallel()
	es := append(goodEntries(t, "cluster"), file("captf/module/backend.tf", "terraform {\n  backend \"s3\" {}\n}\n"))
	got := check(t, build(t, "amd64", nil, es), contract.RoleCluster)
	if len(got) != 1 || got[0].File != "/captf/module/backend.tf" || got[0].Line != 2 {
		t.Errorf("findings = %+v", got)
	}
}

// TestLabelCapacity proves the capacity checks report an error severity
// for an invalid capacity or node-info label, no finding for a valid one,
// and an info severity when both are absent.
func TestLabelCapacity(t *testing.T) {
	t.Parallel()
	machine := goodEntries(t, "machine")
	label := func(kv ...string) func(*v1.ConfigFile) {
		return func(cf *v1.ConfigFile) {
			cf.Config.Labels[labelRole] = "machine"
			for i := 0; i < len(kv); i += 2 {
				cf.Config.Labels[kv[i]] = kv[i+1]
			}
		}
	}
	for _, tt := range []struct {
		name string
		edit func(*v1.ConfigFile)
		want []lint.Severity
	}{
		{"cpu four", label("io.captf.capacity", `{"cpu":"four"}`), []lint.Severity{lint.SeverityError}},
		{"invalid resource name", label("io.captf.capacity", `{"Not A Name":"1"}`), []lint.Severity{lint.SeverityError}},
		{"valid", label("io.captf.capacity", `{"cpu":"4","memory":"8Gi"}`, "io.captf.node-info", `{"architecture":"arm64"}`), nil},
		{"bad node info", label("io.captf.node-info", `{"architecture":"mips"}`), []lint.Severity{lint.SeverityError}},
		{"both absent", label(), []lint.Severity{lint.SeverityInfo}},
	} {
		var got []lint.Severity
		for _, f := range check(t, build(t, "amd64", tt.edit, machine), contract.RoleMachine) {
			if f.ID != IDLabelCapacity {
				t.Errorf("%s: unexpected %+v", tt.name, f)
			}
			got = append(got, f.Severity)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s: severities %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestProvidersComplete proves providers-complete fires once per required
// provider missing from the mirror, is satisfied by a packed or unpacked
// layout matching the runtime's registry, and still fires for the wrong
// registry or a mirror entry for another platform only.
func TestProvidersComplete(t *testing.T) {
	t.Parallel()
	// aws is declared; tls is implied by a resource, a bare local name.
	aws := "terraform {\n  required_providers {\n    aws = {\n      source = \"hashicorp/aws\"\n      version = \"~> 5.0\"\n    }\n  }\n}\nresource \"tls_private_key\" \"k\" {}\n"
	base := append(goodEntries(t, "cluster"), file("captf/module/providers.tf", aws))
	for _, tt := range []struct {
		name    string
		runtime string
		mirror  []entry
		want    int
	}{
		{"nothing mirrored", "", nil, 2},
		{"packed for this platform", "terraform", []entry{
			file("captf/providers/registry.terraform.io/hashicorp/aws/index.json", "{}"),
			file("captf/providers/registry.terraform.io/hashicorp/aws/terraform-provider-aws_5.1.0_linux_amd64.zip", "z"),
			file("captf/providers/registry.terraform.io/hashicorp/tls/terraform-provider-tls_4.0.0_linux_amd64.zip", "z"),
		}, 0},
		{"unpacked for OpenTofu", "tofu", []entry{
			exe("captf/providers/registry.opentofu.org/hashicorp/aws/5.1.0/linux_amd64/terraform-provider-aws"),
			exe("captf/providers/registry.opentofu.org/hashicorp/tls/4.0.0/linux_amd64/terraform-provider-tls"),
		}, 0},
		{"wrong registry for the runtime", "tofu", []entry{
			file("captf/providers/registry.terraform.io/hashicorp/aws/terraform-provider-aws_5.1.0_linux_amd64.zip", "z"),
			file("captf/providers/registry.terraform.io/hashicorp/tls/terraform-provider-tls_4.0.0_linux_amd64.zip", "z"),
		}, 2},
		{"only another platform", "", []entry{
			file("captf/providers/registry.terraform.io/hashicorp/aws/terraform-provider-aws_5.1.0_linux_arm64.zip", "z"),
			file("captf/providers/registry.terraform.io/hashicorp/tls/terraform-provider-tls_4.0.0_linux_amd64.zip", "z"),
		}, 1},
	} {
		img := build(t, "amd64", func(cf *v1.ConfigFile) {
			if tt.runtime != "" {
				cf.Config.Labels[labelRuntime] = tt.runtime
			}
		}, append(slices.Clone(base), tt.mirror...))
		n := 0
		for _, f := range check(t, img, contract.RoleCluster) {
			if f.ID != IDProvidersComplete {
				t.Errorf("%s: unexpected %+v", tt.name, f)
			}
			n++
		}
		if n != tt.want {
			t.Errorf("%s: %d providers-complete findings, want %d", tt.name, n, tt.want)
		}
	}
}

// TestProvidersCompleteNested: a provider only a nested local module
// requires is still visible to image/providers-complete, even though the
// root module's own required_providers never names it.
func TestProvidersCompleteNested(t *testing.T) {
	t.Parallel()
	nested := "module \"child\" {\n  source = \"./child\"\n}\n"
	childTLS := "terraform {\n  required_providers {\n    tls = {\n      source = \"hashicorp/tls\"\n    }\n  }\n}\n"
	es := append(goodEntries(t, "cluster"),
		file("captf/module/nested.tf", nested),
		file("captf/module/child/main.tf", childTLS))
	got := check(t, build(t, "amd64", nil, es), contract.RoleCluster)
	var findings []string
	for _, f := range got {
		findings = append(findings, f.ID)
	}
	if !slices.Contains(findings, IDProvidersComplete) {
		t.Errorf("no providers-complete finding for a nested module's requirement: %+v", got)
	}
}

// TestExtractLimits proves checkImage returns ErrTooLarge over MaxBytes, a
// module path that tries to climb out of the extraction root stays inside
// it, and a module that fails to parse returns lint.ErrParse rather than
// findings.
func TestExtractLimits(t *testing.T) {
	t.Parallel()
	img := build(t, "amd64", nil, append(goodEntries(t, "cluster"), file("captf/module/big.tf", strings.Repeat("#", 4096))))
	if _, _, _, err := checkImage(img, Options{Role: contract.RoleCluster, Contract: contract.Version, MaxBytes: 1024}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v", err)
	}
	// A name that tries to climb out never lands outside the extraction
	// root. Older go-containerregistry hands it to extract, which clamps it
	// under the root; v0.22+ drops such entries before extract sees them.
	// Either is safe; writing outside the root is not.
	img = build(t, "amd64", nil, append(goodEntries(t, "cluster"), file("../../captf/module/escape.tf", "")))
	parent := t.TempDir()
	root := filepath.Join(parent, "a", "b")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	tr, err := extract(img, root, DefaultMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(parent, "captf", "module", "escape.tf")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("escape.tf landed outside the extraction root (stat err %v)", err)
	}
	if tr.headers["/captf/module/escape.tf"] != nil {
		if _, err := os.Stat(filepath.Join(root, "captf", "module", "escape.tf")); err != nil {
			t.Errorf("escape.tf recorded but not written under the root: %v", err)
		}
	}
	// A module that does not parse is an error (exit 2), not findings.
	img = build(t, "amd64", nil, append(goodEntries(t, "cluster"), file("captf/module/broken.tf", "variable {")))
	if _, _, _, err := checkImage(img, Options{Role: contract.RoleCluster, Contract: contract.Version}); !errors.Is(err, lint.ErrParse) {
		t.Errorf("broken module: %v", err)
	}
}

// TestExtractEntryBudget: every tar entry charges a fixed per-entry cost
// against the budget, even a zero-size directory or an entry outside the
// paths extract materializes, so a hostile image cannot exhaust memory or
// time with millions of cheap headers or by hiding large files where the
// size check never looked.
func TestExtractEntryBudget(t *testing.T) {
	t.Parallel()
	var dirs []entry
	for i := range 20 {
		dirs = append(dirs, dir(fmt.Sprintf("d%d/", i)))
	}
	// Skipped bytes count against scanFactor·maxBytes, not maxBytes.
	big := file("usr/share/big", strings.Repeat("x", 8192))
	for _, tt := range []struct {
		name     string
		entries  []entry
		maxBytes int64
		wantErr  bool
	}{
		{"many empty entries exhaust the per-entry budget", dirs, 19 * 512, true},
		{"many empty entries fit", dirs, 20 * 512, false},
		{"a large file outside the checked paths exhausts the scan cap", []entry{dirs[0], dirs[1], big}, 8192/scanFactor - 1, true},
		{"a skipped file larger than the kept budget is fine", []entry{dirs[0], dirs[1], big}, 4096, false},
	} {
		img := build(t, "amd64", nil, tt.entries)
		root := t.TempDir()
		tr, err := extract(img, root, tt.maxBytes)
		if tt.wantErr {
			if !errors.Is(err, ErrTooLarge) {
				t.Errorf("%s: err = %v, want %v", tt.name, err, ErrTooLarge)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if tr.headers["/usr/share/big"] == nil && slices.ContainsFunc(tt.entries, func(e entry) bool { return e.name == "usr/share/big" }) {
			t.Errorf("%s: /usr/share/big not recorded", tt.name)
		}
	}
}

// push uses t to serve an in-memory registry and pushes img (or, when img
// is nil, idx) to it as repo:v1. It returns the pushed reference.
func push(t *testing.T, repo string, img v1.Image, idx v1.ImageIndex) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	ref := strings.TrimPrefix(srv.URL, "http://") + "/" + repo + ":v1"
	r, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if img != nil {
		err = remote.Write(r, img)
	} else {
		err = remote.WriteIndex(r, idx)
	}
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// TestLintRegistry proves Lint pulls a single-platform image from a real
// registry reference, reports image/platform when --platform names a
// platform the image is not for, and returns ErrReference for an invalid
// reference, ErrPull for a missing image, and a parse error for a
// malformed --platform value.
func TestLintRegistry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ref := push(t, "noop-cluster", build(t, "amd64", nil, goodEntries(t, "cluster")), nil)
	res, err := Lint(ctx, ref, Options{Role: contract.RoleCluster, Contract: contract.Version, Platform: DefaultPlatform, Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Report.Findings) != 0 || !strings.HasPrefix(res.Info.Digest, "sha256:") || !slices.Equal(res.Info.Platforms, []string{"linux/amd64"}) || res.Report.FileSet != "terraform" {
		t.Errorf("result = %+v", res)
	}
	// An explicit --platform the image is not for.
	res, err = Lint(ctx, ref, Options{Role: contract.RoleCluster, Contract: contract.Version, Platform: "linux/arm64", PlatformSet: true, Insecure: true})
	if err != nil || !slices.Equal(ids(res.Report.Findings), []string{IDPlatform}) {
		t.Errorf("wrong platform: %+v, %v", res.Report.Findings, err)
	}
	if _, err := Lint(ctx, "Not A Ref", Options{Platform: DefaultPlatform}); !errors.Is(err, ErrReference) {
		t.Errorf("bad reference: %v", err)
	}
	if _, err := Lint(ctx, strings.Replace(ref, "noop-cluster", "missing", 1), Options{Role: contract.RoleCluster, Platform: DefaultPlatform, Insecure: true}); !errors.Is(err, ErrPull) {
		t.Errorf("missing image: %v", err)
	}
	if _, err := Lint(ctx, ref, Options{Role: contract.RoleCluster, Platform: "linux/amd64/v8/extra/junk", Insecure: true}); err == nil {
		t.Error("a bad platform was accepted")
	}
}

// TestLintLayout: oci:<dir> reads an OCI image layout, as `podman save
// --format oci-dir` writes it: a manifest without a platform (taken from
// the image config), possibly under a nested index.
func TestLintLayout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	good := build(t, "amd64", nil, goodEntries(t, "cluster"))
	flat := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: good, Descriptor: v1.Descriptor{MediaType: types.OCIManifestSchema1}})
	nested := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: flat, Descriptor: v1.Descriptor{MediaType: types.OCIImageIndex}})
	o := Options{Role: contract.RoleCluster, Contract: contract.Version, Platform: DefaultPlatform}
	for name, idx := range map[string]v1.ImageIndex{"flat": flat, "nested": nested} {
		dir := t.TempDir()
		if _, err := layout.Write(dir, idx); err != nil {
			t.Fatal(err)
		}
		res, err := Lint(ctx, LayoutPrefix+dir, o)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(res.Report.Findings) != 0 || !slices.Equal(res.Info.Platforms, []string{"linux/amd64"}) || !strings.HasPrefix(res.Info.Digest, "sha256:") {
			t.Errorf("%s: %+v", name, res)
		}
	}
	if _, err := Lint(ctx, LayoutPrefix+t.TempDir(), o); !errors.Is(err, ErrPull) {
		t.Errorf("empty directory: %v", err)
	}
}

// TestLintIndex: under --all-platforms a platform missing /captf/runtime is
// an error attributed to it; --platform picks one; an index without the
// platform is image/platform.
func TestLintIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	good := build(t, "amd64", nil, goodEntries(t, "cluster"))
	noRuntime := build(t, "arm64", func(cf *v1.ConfigFile) { cf.Config.Labels["io.captf.capacity"] = `{"cpu":"2"}` },
		slices.DeleteFunc(goodEntries(t, "cluster"), func(e entry) bool { return e.name == "captf/runtime" }))
	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: good, Descriptor: v1.Descriptor{MediaType: types.OCIManifestSchema1, Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: noRuntime, Descriptor: v1.Descriptor{MediaType: types.OCIManifestSchema1, Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
	)
	ref := push(t, "multi", nil, idx)
	o := Options{Role: contract.RoleCluster, Contract: contract.Version, Platform: DefaultPlatform, Insecure: true, AllPlatforms: true}
	res, err := Lint(ctx, ref, o)
	if err != nil {
		t.Fatal(err)
	}
	var runtime []lint.Finding
	for _, f := range res.Report.Findings {
		if f.ID == IDRuntimePresent {
			runtime = append(runtime, f)
		}
	}
	if len(runtime) != 1 || !strings.HasPrefix(runtime[0].Message, "[linux/arm64] ") || len(res.Info.Platforms) != 2 {
		t.Errorf("findings = %+v, platforms %v", res.Report.Findings, res.Info.Platforms)
	}
	// The capacity label differs across the two platforms.
	if !slices.Contains(ids(res.Report.Findings), IDLabelCapacity) {
		t.Errorf("no capacity difference warning: %+v", res.Report.Findings)
	}
	o.AllPlatforms = false
	if res, err = Lint(ctx, ref, o); err != nil || len(res.Report.Findings) != 0 || !slices.Equal(res.Info.Platforms, []string{"linux/amd64"}) {
		t.Errorf("--platform linux/amd64: %+v, %v", res, err)
	}
	o.Platform = "linux/s390x"
	if res, err = Lint(ctx, ref, o); err != nil || !slices.Equal(ids(res.Report.Findings), []string{IDPlatform}) {
		t.Errorf("--platform linux/s390x: %+v, %v", res.Report.Findings, err)
	}
}
