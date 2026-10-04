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
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/hashicorp/terraform-config-inspect/tfconfig"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/imageinspect/labels"
	"github.com/captf-io/cluster-api-provider-terraform/internal/lint"
)

// Image check IDs.
const (
	IDModulePresent     = "image/module-present"
	IDRuntimePresent    = "image/runtime-present"
	IDRuntimeVersion    = "image/runtime-version"
	IDProvidersLayout   = "image/providers-layout"
	IDProvidersComplete = "image/providers-complete"
	IDProvidersAbsent   = "image/providers-absent"
	IDReservedPaths     = "image/reserved-paths"
	IDLabelRole         = "image/label-role"
	IDLabelContract     = "image/label-contract"
	IDLabelCapacity     = "image/label-capacity"
	IDUserRoot          = "image/user-root"
	IDModuleReadable    = "image/module-readable"
	IDModuleLink        = "image/module-link"
	IDUserUnresolved    = "image/user-unresolved"
	IDEntrypoint        = "image/entrypoint"
	IDPlatform          = "image/platform"
)

// Labels the image checks read.
const (
	labelRole     = "io.captf.role"
	labelContract = "io.captf.contract"
	labelRuntime  = "io.captf.runtime"
)

// reservedPaths are mounted over at run time, so image content there is
// shadowed: /captf/work and /var/run/captf/credentials (the Job's own
// mounts), plus /captf/bin and /captf/config. It returns that fixed list.
func reservedPaths() []string {
	return []string{"/captf/work", "/var/run/captf/credentials", "/captf/bin", "/captf/config"}
}

// imageChecks checks one platform's image. module is the extracted module
// (nil when it is missing or empty).
type imageChecks struct {
	t        *tree
	cfg      *v1.ConfigFile
	role     contract.Role
	contract string
	target   string // the provider target, e.g. linux_amd64
	module   *lint.Module
}

// run runs every check on c's image and returns their combined findings.
func (c imageChecks) run() []lint.Finding {
	var out []lint.Finding
	for _, check := range []func() []lint.Finding{
		c.modulePresent, c.runtimePresent, c.runtimeVersion, c.providers, c.reserved,
		c.labelRole, c.labelContract, c.labelCapacity, c.userRoot, c.userUnresolved, c.moduleReadable,
		c.moduleLinks, c.entrypoint,
	} {
		out = append(out, check()...)
	}
	return out
}

// finding builds a lint.Finding with the given id, severity sev, file and
// message msg, and returns it.
func finding(id string, sev lint.Severity, file, msg string) lint.Finding {
	return lint.Finding{ID: id, Severity: sev, File: file, Message: msg}
}

// modulePresent checks that /captf/module holds at least one module file
// at its top, and returns the IDModulePresent finding when it does not.
func (c imageChecks) modulePresent() []lint.Finding {
	for _, h := range c.t.under(moduleDir, tar.TypeReg) {
		if name := path.Clean("/" + h.Name); path.Dir(name) == moduleDir && lint.IsModuleFile(name) {
			return nil
		}
	}
	return []lint.Finding{finding(IDModulePresent, lint.SeverityError, moduleDir,
		moduleDir+" has no .tf, .tf.json, .tofu or .tofu.json file")}
}

// user is config.User as the checks read it: root, a numeric uid[:gid], or
// a name the linter cannot resolve (it reads no /etc/passwd).
type user struct {
	root     bool
	numeric  bool
	uid, gid int
}

// parseUser parses s, an image config User value, into a user: root when s
// is empty, "root" or "0"; numeric when s parses as uid[:gid]; the zero
// user (an unresolvable name) otherwise. It returns the parsed user.
func parseUser(s string) user {
	name, group, _ := strings.Cut(s, ":")
	if name == "" || name == "root" || name == "0" {
		return user{root: true}
	}
	uid, err := strconv.Atoi(name)
	if err != nil {
		return user{}
	}
	u := user{numeric: true, uid: uid, gid: -1}
	if g, err := strconv.Atoi(group); err == nil {
		u.gid = g
	}
	return u
}

// can reports whether u has any of the permission bits perm (0o4 read,
// 0o1 execute) on h. Root and unresolvable names get any bit.
func (u user) can(h *tar.Header, perm int64) bool {
	mode := h.Mode
	switch {
	case u.root || !u.numeric:
		return mode&(perm|perm<<3|perm<<6) != 0
	case u.uid == h.Uid:
		return mode&(perm<<6) != 0
	case u.gid == h.Gid:
		return mode&(perm<<3) != 0
	}
	return mode&perm != 0
}

// runtimePresent checks that /captf/runtime is a regular file, or a
// symlink to one inside the image, executable by config.User, and returns
// the IDRuntimePresent finding when it is not.
func (c imageChecks) runtimePresent() []lint.Finding {
	h := c.t.resolve(runtimePath)
	switch {
	case h == nil:
		return []lint.Finding{finding(IDRuntimePresent, lint.SeverityError, runtimePath,
			runtimePath+" is missing, or a symlink whose target is not in the image")}
	case h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeLink:
		return []lint.Finding{finding(IDRuntimePresent, lint.SeverityError, runtimePath,
			runtimePath+" is not a regular file")}
	case !parseUser(c.cfg.Config.User).can(h, 0o1):
		return []lint.Finding{finding(IDRuntimePresent, lint.SeverityError, runtimePath,
			fmt.Sprintf("%s (mode %04o) is not executable by user %q", runtimePath, h.Mode&0o7777, c.cfg.Config.User))}
	}
	return nil
}

// runtimeVersion is a best-effort check: the runtime is named by the
// symlink target or the file name; a mismatch with io.captf.runtime
// returns the IDRuntimeVersion warning finding. Consistent or unknown
// returns no finding. The ELF version string is not read: that would mean
// materializing the binary.
func (c imageChecks) runtimeVersion() []lint.Finding {
	want := c.cfg.Config.Labels[labelRuntime]
	if want == "" {
		return nil
	}
	name := runtimePath
	if h := c.t.headers[runtimePath]; h != nil && h.Typeflag == tar.TypeSymlink {
		name = h.Linkname
	}
	var seen string
	switch {
	case strings.Contains(path.Base(name), "tofu"):
		seen = "tofu"
	case strings.Contains(path.Base(name), "terraform"):
		seen = "terraform"
	}
	if seen == "" || seen == want {
		return nil
	}
	return []lint.Finding{finding(IDRuntimeVersion, lint.SeverityWarning, runtimePath,
		fmt.Sprintf("%s appears to be %s (%s), but %s says %s", runtimePath, seen, name, labelRuntime, want))}
}

// packedZip and target match the provider mirror's two layout shapes:
// packedZip matches a packed provider zip name (capturing its
// os_arch target), and target matches an unpacked layout's bare TARGET
// path segment.
var (
	packedZip = regexp.MustCompile(`^terraform-provider-[^_]+_[^_]+_([a-z0-9]+_[a-z0-9]+)\.zip$`)
	target    = regexp.MustCompile(`^[a-z0-9]+_[a-z0-9]+$`)
)

// providers covers the mirror: providers-absent (info) without one,
// providers-layout for every entry, providers-complete for every provider
// the module requires. Version constraints are not evaluated: only
// presence for the platform (see Corrections). It returns the combined
// findings.
func (c imageChecks) providers() []lint.Finding {
	entries := c.t.under(providersDir, tar.TypeReg, tar.TypeSymlink, tar.TypeLink)
	if _, ok := c.t.headers[providersDir]; !ok && len(entries) == 0 {
		return []lint.Finding{finding(IDProvidersAbsent, lint.SeverityInfo, providersDir,
			"no provider mirror: init needs registry egress when the Job runs")}
	}
	var out []lint.Finding
	have := map[string]bool{} // HOST/NS/TYPE → a package for this target
	var broken []string       // links that do not end at a zip or directory
	for _, h := range entries {
		name := path.Clean("/" + h.Name)
		parts := strings.Split(strings.TrimPrefix(name, providersDir+"/"), "/")
		ok := false
		isLink := h.Typeflag != tar.TypeReg
		final := c.t.resolve(name)
		switch {
		case len(parts) == 4:
			// Packed: HOST/NS/TYPE/{index.json,VERSION.json,terraform-provider-…zip}.
			if m := packedZip.FindStringSubmatch(parts[3]); m != nil {
				ok = true
				switch {
				case isLink && (final == nil || final.Typeflag != tar.TypeReg):
					broken = append(broken, name)
				case m[1] == c.target:
					have[strings.Join(parts[:3], "/")] = true
				}
			} else if strings.HasSuffix(parts[3], ".json") {
				ok = true
			}
		case len(parts) == 5 && isLink && target.MatchString(parts[4]):
			// A symlinked unpacked TARGET directory.
			ok = true
			switch {
			case final == nil || final.Typeflag != tar.TypeDir:
				broken = append(broken, name)
			case parts[4] == c.target:
				have[strings.Join(parts[:3], "/")] = true
			}
		case len(parts) >= 6:
			// Unpacked: HOST/NS/TYPE/VERSION/TARGET/….
			ok = target.MatchString(parts[4])
			if ok && parts[4] == c.target {
				have[strings.Join(parts[:3], "/")] = true
			}
		}
		if !ok {
			out = append(out, finding(IDProvidersLayout, lint.SeverityError, name,
				name+" is neither a packed (HOST/NS/TYPE/*.zip, *.json) nor an unpacked (HOST/NS/TYPE/VERSION/TARGET/) mirror entry"))
		}
	}
	slices.Sort(broken)
	for _, name := range broken {
		out = append(out, finding(IDProvidersComplete, lint.SeverityError, name,
			name+" is a link whose target is missing from the image or is not a provider zip or directory: init cannot use it"))
	}
	if c.module == nil {
		return out
	}
	for _, rp := range c.module.RequiredProviders() {
		local, req := rp.Local, rp.Req
		if builtin(local, req) {
			continue // terraform_data and friends: built in, never mirrored
		}
		sources := providerSources(local, req, c.cfg.Config.Labels[labelRuntime])
		if !slices.ContainsFunc(sources, func(s string) bool { return have[s] }) {
			out = append(out, finding(IDProvidersComplete, lint.SeverityError, providersDir,
				fmt.Sprintf("the mirror has no %s package of %s (%s): init fails, since the Job excludes direct installation",
					c.target, local, strings.Join(sources, " or "))))
		}
	}
	return out
}

// builtin reports whether the required_providers entry named local, with
// requirement req, is the runtime's built-in provider (terraform_data,
// terraform_remote_state): terraform-config-inspect lists it as an implied
// requirement, but it ships with the runtime.
func builtin(local string, req *tfconfig.ProviderRequirement) bool {
	if req != nil && strings.Contains(req.Source, "/builtin/") {
		return true
	}
	return local == "terraform" && (req == nil || req.Source == "")
}

// providerSources normalizes the required_providers entry named local,
// with requirement req, to the mirror directories that would serve it:
// hashicorp/aws is registry.terraform.io/hashicorp/aws for Terraform and
// registry.opentofu.org/hashicorp/aws for OpenTofu (both when the runtime
// label is absent); a bare local name is hashicorp/<name>. It returns the
// one or two candidate source directories.
func providerSources(local string, req *tfconfig.ProviderRequirement, runtime string) []string {
	src := "hashicorp/" + local
	if req != nil && req.Source != "" {
		src = strings.ToLower(req.Source)
	}
	if strings.Count(src, "/") == 2 {
		return []string{src}
	}
	switch runtime {
	case "terraform":
		return []string{"registry.terraform.io/" + src}
	case "tofu":
		return []string{"registry.opentofu.org/" + src}
	}
	return []string{"registry.terraform.io/" + src, "registry.opentofu.org/" + src}
}

// reserved checks for files under a path mounted at run time, and returns
// an IDReservedPaths finding for each reserved directory that has any.
func (c imageChecks) reserved() []lint.Finding {
	var out []lint.Finding
	for _, dir := range reservedPaths() {
		files := c.t.under(dir, tar.TypeReg, tar.TypeSymlink, tar.TypeLink)
		if len(files) == 0 {
			continue
		}
		out = append(out, finding(IDReservedPaths, lint.SeverityError, dir,
			fmt.Sprintf("%d file(s) under %s, which is mounted when the Job runs: the image must not ship anything there", len(files), dir)))
	}
	return out
}

// labelRole checks io.captf.role against c.role and returns an info
// finding when it is unset or an error finding when it names a different
// role.
func (c imageChecks) labelRole() []lint.Finding {
	got, ok := c.cfg.Config.Labels[labelRole]
	switch {
	case !ok:
		return []lint.Finding{finding(IDLabelRole, lint.SeverityInfo, "", labelRole+" is not set")}
	case got != string(c.role):
		return []lint.Finding{finding(IDLabelRole, lint.SeverityError, "",
			fmt.Sprintf("%s is %q, but the image is linted as %q", labelRole, got, c.role))}
	}
	return nil
}

// labelContract checks io.captf.contract against c.contract and returns an
// info finding when it is unset or a warning finding when it names a
// different contract version.
func (c imageChecks) labelContract() []lint.Finding {
	got, ok := c.cfg.Config.Labels[labelContract]
	switch {
	case !ok:
		return []lint.Finding{finding(IDLabelContract, lint.SeverityInfo, "", labelContract+" is not set")}
	case got != c.contract:
		return []lint.Finding{finding(IDLabelContract, lint.SeverityWarning, "",
			fmt.Sprintf("%s is %q, but the image is linted against %q", labelContract, got, c.contract))}
	}
	return nil
}

// labelCapacity checks that the capacity labels parse as the
// TerraformMachineTemplate reconciler parses them. They matter to the
// machine role only. It returns an info finding when a label is irrelevant
// to the role or absent, and an error finding for each label present but
// invalid.
func (c imageChecks) labelCapacity() []lint.Finding {
	capLabel, hasCap := c.cfg.Config.Labels[labels.CapacityLabel]
	niLabel, hasNI := c.cfg.Config.Labels[labels.NodeInfoLabel]
	switch {
	case c.role != contract.RoleMachine && (hasCap || hasNI):
		return []lint.Finding{finding(IDLabelCapacity, lint.SeverityInfo, "",
			"capacity labels are read for the machine role only; ignored for "+string(c.role))}
	case c.role != contract.RoleMachine:
		return nil
	case !hasCap && !hasNI:
		return []lint.Finding{finding(IDLabelCapacity, lint.SeverityInfo, "",
			"neither "+labels.CapacityLabel+" nor "+labels.NodeInfoLabel+" is set: no scale-from-zero capacity")}
	}
	var out []lint.Finding
	if hasCap {
		if _, err := labels.ParseCapacity(capLabel); err != nil {
			out = append(out, finding(IDLabelCapacity, lint.SeverityError, "", err.Error()))
		}
	}
	if hasNI {
		if _, err := labels.ParseNodeInfo(niLabel); err != nil {
			out = append(out, finding(IDLabelCapacity, lint.SeverityError, "", err.Error()))
		}
	}
	return out
}

// userRoot checks c.cfg.Config.User and returns an IDUserRoot warning
// finding when the image runs as root.
func (c imageChecks) userRoot() []lint.Finding {
	if !parseUser(c.cfg.Config.User).root {
		return nil
	}
	return []lint.Finding{finding(IDUserRoot, lint.SeverityWarning, "",
		fmt.Sprintf("the image runs as root (User %q): the Job cannot run under the restricted Pod Security Standard", c.cfg.Config.User))}
}

// userUnresolved returns an IDUserUnresolved warning finding when the image
// User is a name other than root: the linter reads no /etc/passwd, so the
// runtime and module permission checks are approximated for it.
func (c imageChecks) userUnresolved() []lint.Finding {
	u := parseUser(c.cfg.Config.User)
	if u.root || u.numeric {
		return nil
	}
	return []lint.Finding{finding(IDUserUnresolved, lint.SeverityWarning, "",
		fmt.Sprintf("the image User %q is a name the linter cannot resolve to a uid: permission checks on the runtime and module are approximated, and the module-readable check is skipped; use a numeric uid", c.cfg.Config.User))}
}

// moduleLinks returns an IDModuleLink error finding for each symlink or hard
// link under /captf/module that does not end at a module file: Terraform
// loads it at run time, but the lint cannot see what it holds.
func (c imageChecks) moduleLinks() []lint.Finding {
	out := make([]lint.Finding, 0, len(c.t.badLinks))
	for _, l := range c.t.badLinks {
		out = append(out, finding(IDModuleLink, lint.SeverityError, l.name,
			fmt.Sprintf("%s links to %s, which is not a regular file inside %s: the module lint cannot read it, and the target may be missing at run time", l.name, l.target, moduleDir)))
	}
	return out
}

// moduleReadable checks that, with a numeric non-root user, every file
// under the module and the mirror is readable and every directory
// traversable, and returns one IDModuleReadable warning finding listing up
// to five offending paths when some are not.
func (c imageChecks) moduleReadable() []lint.Finding {
	u := parseUser(c.cfg.Config.User)
	if u.root || !u.numeric {
		return nil
	}
	var bad []string
	for _, dir := range []string{moduleDir, providersDir} {
		entries := c.t.under(dir, tar.TypeReg, tar.TypeDir)
		if h, ok := c.t.headers[dir]; ok {
			entries = append(entries, h)
		}
		for _, h := range entries {
			perm := int64(0o4)
			if h.Typeflag == tar.TypeDir {
				perm = 0o1
			}
			if !u.can(h, perm) {
				bad = append(bad, path.Clean("/"+h.Name))
			}
		}
	}
	if len(bad) == 0 {
		return nil
	}
	slices.Sort(bad)
	shown := bad[:min(len(bad), 5)]
	return []lint.Finding{finding(IDModuleReadable, lint.SeverityWarning, shown[0],
		fmt.Sprintf("%d path(s) not readable or traversable by user %d: %s", len(bad), u.uid, strings.Join(shown, ", ")))}
}

// entrypoint checks c.cfg.Config.Entrypoint and Cmd and returns an
// IDEntrypoint info finding when either is set, since the runner replaces
// them when the Job runs.
func (c imageChecks) entrypoint() []lint.Finding {
	if len(c.cfg.Config.Entrypoint) == 0 && len(c.cfg.Config.Cmd) == 0 {
		return nil
	}
	return []lint.Finding{finding(IDEntrypoint, lint.SeverityInfo, "",
		"ENTRYPOINT/CMD are set; the runner replaces them when the Job runs")}
}
