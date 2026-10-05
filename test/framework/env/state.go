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

package env

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/providers"
)

// stateVersion is State.Version; bump it when the layout changes
// incompatibly, so reuse refuses an old file instead of misreading it.
const stateVersion = 1

// The files Up writes into the work directory.
const (
	// stateFile is the environment's state, for reuse, Status and Reload.
	stateFile = "state.json"
	// envFile is the shell snippet that exports KUBECONFIG and the
	// template variables.
	envFile = "env.sh"
)

// Pins is every version the environment is built from. Up reuses a
// cluster only when its recorded Pins equal CurrentPins.
type Pins struct {
	// KindVersion is framework.KindVersion.
	KindVersion string `json:"kindVersion"`
	// KindNodeImage is framework.KindNodeImage.
	KindNodeImage string `json:"kindNodeImage"`
	// CAPIVersion is framework.CAPIVersion.
	CAPIVersion string `json:"capiVersion"`
	// CertManagerVersion is framework.CertManagerVersion.
	CertManagerVersion string `json:"certManagerVersion"`
	// CAPTFVersion is providers.CAPTFVersion.
	CAPTFVersion string `json:"captfVersion"`
	// NoopImages are the pinned noop references (repository@digest).
	NoopImages []string `json:"noopImages"`
}

// CurrentPins returns the Pins of this build of the framework.
func CurrentPins() Pins {
	p := Pins{
		KindVersion:        framework.KindVersion,
		KindNodeImage:      framework.KindNodeImage,
		CAPIVersion:        framework.CAPIVersion,
		CertManagerVersion: framework.CertManagerVersion,
		CAPTFVersion:       providers.CAPTFVersion,
	}
	for _, img := range framework.NoopImages() {
		p.NoopImages = append(p.NoopImages, img.Pinned())
	}
	return p
}

// Equal reports whether p and o pin the same versions and images.
func (p Pins) Equal(o Pins) bool {
	return p.KindVersion == o.KindVersion && p.KindNodeImage == o.KindNodeImage &&
		p.CAPIVersion == o.CAPIVersion && p.CertManagerVersion == o.CertManagerVersion &&
		p.CAPTFVersion == o.CAPTFVersion && slices.Equal(p.NoopImages, o.NoopImages)
}

// NoopRef is one noop image as the nodes pull it.
type NoopRef struct {
	// Ref is the image's readable tag, which only the host knows.
	Ref string `json:"ref"`
	// Pinned is the digest reference the nodes pulled it by.
	Pinned string `json:"pinned"`
}

// NodeImage is what one node's containerd reports for one image
// (crictl inspecti).
type NodeImage struct {
	// Node is the node (container) name.
	Node string `json:"node"`
	// Ref is the reference that was inspected.
	Ref string `json:"ref"`
	// ID is the image ID.
	ID string `json:"id"`
	// RepoTags are the tags the node knows the image by.
	RepoTags []string `json:"repoTags,omitempty"`
	// RepoDigests are the repository@digest references the node knows;
	// empty means a side-load dropped them (a node pull keeps them).
	RepoDigests []string `json:"repoDigests,omitempty"`
}

// Timing is how long one step took.
type Timing struct {
	// Step is the step name.
	Step string `json:"step"`
	// Seconds is its wall time, rounded to milliseconds.
	Seconds float64 `json:"seconds"`
}

// State is <work>/state.json: what Up built, for reuse, Status and Reload.
type State struct {
	// Version is the file layout version.
	Version int `json:"version"`
	// Cluster is the kind cluster name.
	Cluster string `json:"cluster"`
	// Engine is the container engine ("podman" or "docker").
	Engine string `json:"engine"`
	// Workers is the number of worker nodes.
	Workers int `json:"workers"`
	// Kubeconfig is the absolute kubeconfig path.
	Kubeconfig string `json:"kubeconfig"`
	// Pins are the versions the environment was built from.
	Pins Pins `json:"pins"`
	// TreeID is the working-tree identity the manager image was built
	// from.
	TreeID string `json:"treeID"`
	// ManagerRef is the manager image reference.
	ManagerRef string `json:"managerRef"`
	// NoopImages are the noop images pulled in the nodes.
	NoopImages []NoopRef `json:"noopImages"`
	// NodeImages are the node-side records of the manager and noop images.
	NodeImages []NodeImage `json:"nodeImages"`
	// Timings are the step timings of the last Up or Reload.
	Timings []Timing `json:"timings"`
	// Operation is the operation that last wrote the file ("up",
	// "up (reused)" or "reload").
	Operation string `json:"operation"`
	// UpdatedAt is when the file was last written.
	UpdatedAt time.Time `json:"updatedAt"`
}

// ReadState reads and decodes the state.json in dir. It returns an error
// when the file is missing, unreadable, malformed or of another layout
// version.
func ReadState(dir string) (*State, error) {
	path := filepath.Join(dir, stateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("env: read state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("env: read state %s: %w", path, err)
	}
	if s.Version != stateVersion {
		return nil, fmt.Errorf("env: read state %s: layout version %d, want %d", path, s.Version, stateVersion)
	}
	return &s, nil
}

// WriteState writes s as indented JSON to dir/state.json, atomically
// (a temporary file renamed over it), with s.Version set. It returns an
// error if encoding or writing fails.
func WriteState(dir string, s *State) error {
	s.Version = stateVersion
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("env: write state: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, stateFile), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("env: write state: %w", err)
	}
	return nil
}

// writeFileAtomic writes data with mode perm to a temporary file next to
// path and renames it over path. It returns an error if any step fails,
// removing the temporary file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, perm)
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}

// shellQuote returns s single-quoted for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Script returns the env.sh content for s: KUBECONFIG, and the template
// variables TERRAFORM_CLUSTER_IMAGE and TERRAFORM_MACHINE_IMAGE set to the
// Terraform noop images the nodes pulled, as "<repository>:<tag>@<digest>":
// the digest is what a node holds and what CAPTF pins, and the tag keeps
// the reference readable.
func Script(s *State) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by test/framework/env for kind cluster %s; source it.\n", s.Cluster)
	fmt.Fprintf(&b, "export KUBECONFIG=%s\n", shellQuote(s.Kubeconfig))
	for _, v := range []struct {
		name string
		role framework.NoopRole
	}{
		{"TERRAFORM_CLUSTER_IMAGE", framework.RoleCluster},
		{"TERRAFORM_MACHINE_IMAGE", framework.RoleMachine},
	} {
		img, ok := framework.NoopImageFor(v.role, framework.RuntimeTerraform)
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "export %s=%s\n", v.name, shellQuote(img.Ref+"@"+img.Digest))
	}
	return b.String()
}

// writeEnvScript writes Script(s) to dir/env.sh. It returns an error if
// writing fails.
func writeEnvScript(dir string, s *State) error {
	if err := writeFileAtomic(filepath.Join(dir, envFile), []byte(Script(s)), 0o644); err != nil {
		return fmt.Errorf("env: write env.sh: %w", err)
	}
	return nil
}

// DigestFinding summarizes, from records, whether the noop images hold
// their pinned repo digests in the nodes' containerd: it returns "kept"
// when every noop record lists its pinned digest, "dropped" when none
// does, "partial" otherwise, and "unknown" without noop records. noops
// are the noop images; a record matches one by its readable Ref or by its
// Pinned reference.
func DigestFinding(records []NodeImage, noops []NoopRef) string {
	pinned := map[string]string{}
	for _, n := range noops {
		pinned[n.Ref] = n.Pinned
		pinned[n.Pinned] = n.Pinned
	}
	kept, total := 0, 0
	for _, r := range records {
		want, ok := pinned[r.Ref]
		if !ok {
			continue
		}
		total++
		if slices.Contains(r.RepoDigests, want) {
			kept++
		}
	}
	switch {
	case total == 0:
		return "unknown"
	case kept == total:
		return "kept"
	case kept == 0:
		return "dropped"
	default:
		return fmt.Sprintf("partial (%d/%d)", kept, total)
	}
}
