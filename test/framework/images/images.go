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

package images

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"sigs.k8s.io/kind/pkg/cluster/nodes"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
)

// managerRepo is the manager image repository. The localhost/ prefix keeps
// podman from treating it as a registry name, and no registry is ever
// contacted: the image is built locally and side-loaded.
const managerRepo = "localhost/captf/manager"

// dirtyHexLen is how many hex digits of the change hash TreeID appends.
const dirtyHexLen = 12

// config holds the settings Options change.
type config struct {
	// log receives progress lines; nil discards them.
	log io.Writer
}

// Option changes how an operation in this package runs.
type Option func(*config)

// WithLog returns an Option that writes one progress line per step to w.
// Without it, operations are silent.
func WithLog(w io.Writer) Option {
	return func(c *config) { c.log = w }
}

// newConfig returns the config the options opts describe.
func newConfig(opts []Option) *config {
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	return c
}

// logf writes one line formatted from format and args to the log, if there
// is one.
func (c *config) logf(format string, args ...any) {
	if c.log != nil {
		fmt.Fprintf(c.log, "images: "+format+"\n", args...)
	}
}

// TreeID returns, under ctx, the identity of the working tree r runs git in: the
// 12-character HEAD sha, followed by "-dirty-" and the first 12 hex digits
// of a sha256 over `git diff HEAD` and the untracked-file list when the
// tree has uncommitted changes. The same tree always yields the same ID,
// so an unchanged tree reuses its image tag. The content of untracked
// files is not hashed, only their names. It returns an error when git
// fails or prints no sha.
func TreeID(ctx context.Context, r engine.Runner) (string, error) {
	out, err := r.Run(ctx, "git", "rev-parse", "--short=12", "HEAD")
	if err != nil {
		return "", fmt.Errorf("images: tree id: %w", err)
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", errors.New("images: tree id: git printed no HEAD sha")
	}
	diff, err := r.Run(ctx, "git", "diff", "HEAD")
	if err != nil {
		return "", fmt.Errorf("images: tree id: %w", err)
	}
	untracked, err := r.Run(ctx, "git", "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return "", fmt.Errorf("images: tree id: %w", err)
	}
	if len(bytes.TrimSpace(diff)) == 0 && len(bytes.TrimSpace(untracked)) == 0 {
		return sha, nil
	}
	h := sha256.New()
	h.Write(diff)
	h.Write([]byte{0})
	h.Write(untracked)
	return sha + "-dirty-" + hex.EncodeToString(h.Sum(nil))[:dirtyHexLen], nil
}

// ManagerRef returns the manager image reference for treeID,
// "localhost/captf/manager:<treeID>". It is never tagged latest, so the
// default pull policy (IfNotPresent) uses the side-loaded image.
func ManagerRef(treeID string) string {
	return managerRepo + ":" + treeID
}

// BuildManager builds the manager image ref from the tree r runs in (the
// repository root), with `make docker-build CONTAINER_TOOL=<engine>
// IMG=<ref>` where the engine is e's name, and returns an error if make
// fails or ctx is done. opts may add WithLog. It
// refuses a ref without an explicit tag and a ref tagged latest. The
// Makefile's target builds the Dockerfile, which holds both /manager and
// /runner.
func BuildManager(ctx context.Context, r engine.Runner, e *engine.Engine, ref string, opts ...Option) error {
	c := newConfig(opts)
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i:], "/") || ref[i+1:] == "" || ref[i+1:] == "latest" {
		return fmt.Errorf("images: build manager: ref %q must carry an explicit non-latest tag", ref)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("images: build manager: %w", err)
	}
	c.logf("building %s with %s", ref, e.Name)
	if _, err := r.Run(ctx, "make", "docker-build", "CONTAINER_TOOL="+string(e.Name), "IMG="+ref); err != nil {
		return fmt.Errorf("images: build manager: %w", err)
	}
	return nil
}

// SmokeManager runs the local image ref through engine e under ctx, twice
// in throwaway containers, `/manager --help` then `/runner --help` (opts
// may add WithLog), and returns an error naming
// the binary that failed. Both print usage and exit 0 without touching a
// cluster, so a failure means the image is stale, truncated or built for
// another platform. It never pulls.
func SmokeManager(ctx context.Context, e *engine.Engine, ref string, opts ...Option) error {
	c := newConfig(opts)
	for _, bin := range []string{"/manager", "/runner"} {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("images: smoke %s: %w", bin, err)
		}
		c.logf("smoke-checking %s %s --help", ref, bin)
		if _, err := e.RunRemove(ctx, ref, bin, "--help"); err != nil {
			return fmt.Errorf("images: smoke %s: %w", bin, err)
		}
	}
	return nil
}

// SideLoad copies each of refs from the engine's local storage into every
// node of nodeList. For each ref it saves a docker-archive to a file in
// workDir (created if missing), streams that file into
// nodeutils.LoadImageArchive on each node in turn, and removes the archive
// whether or not loading succeeded. The archive is never read into
// memory. Images are saved with engine e; opts may add WithLog. It returns
// an error at the first failure or when ctx is done, and nil when every
// ref is on every node.
func SideLoad(ctx context.Context, e *engine.Engine, nodeList []nodes.Node, refs []string, workDir string, opts ...Option) error {
	c := newConfig(opts)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("images: side-load: %w", err)
	}
	for i, ref := range refs {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("images: side-load: %w", err)
		}
		archive := filepath.Join(workDir, "image-"+strconv.Itoa(i)+".tar")
		c.logf("saving %s", ref)
		err := e.Save(ctx, ref, archive)
		if err == nil {
			err = loadEverywhere(ctx, c, nodeList, ref, archive)
		}
		if rmErr := os.Remove(archive); rmErr != nil && !os.IsNotExist(rmErr) && err == nil {
			err = fmt.Errorf("images: side-load: %w", rmErr)
		}
		if err != nil {
			if strings.HasPrefix(err.Error(), "images: ") {
				return err
			}
			return fmt.Errorf("images: side-load: %w", err)
		}
	}
	return nil
}

// loadEverywhere streams the archive file into every node of nodeList
// under ctx, opening it afresh for each, logging to c, and returns an
// error naming ref and the node that failed.
func loadEverywhere(ctx context.Context, c *config, nodeList []nodes.Node, ref, archive string) error {
	for _, n := range nodeList {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("images: side-load: %w", err)
		}
		if err := loadOne(c, n, ref, archive); err != nil {
			return err
		}
	}
	return nil
}

// loadOne streams the archive file into node n, where ref names the image
// for logs and errors, logging to c, and closes the file. It returns an
// error if the file cannot be opened or the node rejects the archive.
func loadOne(c *config, n nodes.Node, ref, archive string) error {
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("images: side-load: %w", err)
	}
	defer f.Close()
	c.logf("loading %s into %s", ref, n)
	if err := nodeutils.LoadImageArchive(n, f); err != nil {
		return fmt.Errorf("images: side-load: %s into %s: %w", ref, n, err)
	}
	return nil
}

// NodePull pulls ref into node n's containerd with `crictl pull <ref>` run
// on the node under ctx, so the node holds the image with the repo digests
// the registry published (a side-load drops them). It is idempotent: an
// image the node already holds is not fetched again. opts may add WithLog.
// It returns an error, with crictl's stderr, when the command fails or ctx
// is done.
func NodePull(ctx context.Context, n nodes.Node, ref string, opts ...Option) error {
	c := newConfig(opts)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("images: node pull %s on %s: %w", ref, n, err)
	}
	c.logf("pulling %s on %s", ref, n)
	var stderr bytes.Buffer
	cmd := n.CommandContext(ctx, "crictl", "pull", ref).SetStderr(&stderr)
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("images: node pull %s on %s: %w: %s", ref, n, err, msg)
		}
		return fmt.Errorf("images: node pull %s on %s: %w", ref, n, err)
	}
	return nil
}

// NodeImageInfo is what one node's containerd holds for an image.
type NodeImageInfo struct {
	// Ref is the reference that was inspected.
	Ref string
	// ID is the image ID ("sha256:<hex>").
	ID string
	// RepoTags are the tags the node knows the image by.
	RepoTags []string
	// RepoDigests are the repository@digest references the node knows;
	// empty means the side-load dropped the digests.
	RepoDigests []string
}

// NodeImage returns what node n's containerd holds for ref, from
// `crictl inspecti -o json <ref>` run on the node under ctx. It returns an
// error when the command fails (including an absent image) or prints
// something other than crictl's JSON.
func NodeImage(ctx context.Context, n nodes.Node, ref string) (NodeImageInfo, error) {
	var stdout, stderr bytes.Buffer
	cmd := n.CommandContext(ctx, "crictl", "inspecti", "-o", "json", ref).
		SetStdout(&stdout).SetStderr(&stderr)
	if err := cmd.Run(); err != nil {
		return NodeImageInfo{}, fmt.Errorf("images: node image: %s on %s: %w: %s", ref, n, err, strings.TrimSpace(stderr.String()))
	}
	var resp struct {
		Status struct {
			ID          string   `json:"id"`
			RepoTags    []string `json:"repoTags"`
			RepoDigests []string `json:"repoDigests"`
		} `json:"status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return NodeImageInfo{}, fmt.Errorf("images: node image: parse crictl output for %s: %w", ref, err)
	}
	if resp.Status.ID == "" {
		return NodeImageInfo{}, fmt.Errorf("images: node image: crictl reported no image ID for %s", ref)
	}
	return NodeImageInfo{
		Ref:         ref,
		ID:          resp.Status.ID,
		RepoTags:    resp.Status.RepoTags,
		RepoDigests: resp.Status.RepoDigests,
	}, nil
}
