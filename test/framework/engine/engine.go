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

package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/kind/pkg/cluster"
)

// Name is a container engine's CLI name.
type Name string

// The supported engines.
const (
	// Podman is the podman CLI; detection prefers it.
	Podman Name = "podman"
	// Docker is the docker CLI against a real Docker daemon.
	Docker Name = "docker"
)

// ParseName returns the Name s spells ("podman" or "docker", surrounding
// space ignored), or an error for anything else, the empty string
// included.
func ParseName(s string) (Name, error) {
	switch n := Name(strings.TrimSpace(s)); n {
	case Podman, Docker:
		return n, nil
	default:
		return "", fmt.Errorf("engine: parse: unknown engine %q, want %q or %q", s, Podman, Docker)
	}
}

// Engine runs one container engine's commands through a Runner. Build it
// with Detect, or directly (for example over enginetest.Runner in a test);
// Name must be Podman or Docker, and methods return an error otherwise.
type Engine struct {
	// Name is the engine, which is also the program every command runs.
	Name Name
	// Runner runs the commands.
	Runner Runner
}

// Detect returns the engine to use, running its probe commands through r
// under ctx.
// A non-empty override (the CAPTF_TESTENV_ENGINE value) must be "podman"
// or "docker" and wins, provided `<override> info` succeeds. Otherwise it
// picks podman if `podman info` succeeds, else docker if `docker info`
// succeeds. It returns an error naming every failed probe when no engine
// is usable, and for an invalid override without probing anything.
func Detect(ctx context.Context, override string, r Runner) (*Engine, error) {
	if override != "" {
		n, err := ParseName(override)
		if err != nil {
			return nil, fmt.Errorf("engine: detect: invalid override: %w", err)
		}
		if _, err := r.Run(ctx, string(n), "info"); err != nil {
			return nil, fmt.Errorf("engine: detect: %s was requested but is not usable: %w", n, err)
		}
		return &Engine{Name: n, Runner: r}, nil
	}
	var errs []error
	for _, n := range []Name{Podman, Docker} {
		_, err := r.Run(ctx, string(n), "info")
		if err == nil {
			return &Engine{Name: n, Runner: r}, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("engine: detect: neither podman nor docker is usable: %w", errors.Join(errs...))
}

// check returns an error for op unless e has a supported Name and a
// Runner.
func (e *Engine) check(op string) error {
	if e.Name != Podman && e.Name != Docker {
		return fmt.Errorf("engine: %s: unknown engine %q", op, e.Name)
	}
	if e.Runner == nil {
		return fmt.Errorf("engine: %s: no Runner", op)
	}
	return nil
}

// Run runs `<engine> args...` under ctx and returns its stdout. It is the
// escape hatch for commands the typed methods don't cover; their argv
// differences between engines are the caller's concern. It returns an
// error prefixed "engine: run:" that wraps the *RunError.
func (e *Engine) Run(ctx context.Context, args ...string) ([]byte, error) {
	return e.run(ctx, "run", args...)
}

// run runs `<engine> args...` under ctx and returns its stdout, with any
// error prefixed "engine: <op>:".
func (e *Engine) run(ctx context.Context, op string, args ...string) ([]byte, error) {
	if err := e.check(op); err != nil {
		return nil, err
	}
	out, err := e.Runner.Run(ctx, string(e.Name), args...)
	if err != nil {
		return out, fmt.Errorf("engine: %s: %w", op, err)
	}
	return out, nil
}

// Pull pulls ref under ctx, quietly, and returns an error if the pull
// fails. Pull a digest reference (repository@sha256:...), then Tag it.
func (e *Engine) Pull(ctx context.Context, ref string) error {
	_, err := e.run(ctx, "pull", "pull", "--quiet", ref)
	return err
}

// Tag adds the local tag dst to the local image src under ctx, and
// returns an error if src does not exist or tagging fails.
func (e *Engine) Tag(ctx context.Context, src, dst string) error {
	_, err := e.run(ctx, "tag", "tag", src, dst)
	return err
}

// Exists reports whether the image ref is present in local storage under
// ctx. It returns false with a nil error when the image is absent, and an
// error only when the engine itself fails.
func (e *Engine) Exists(ctx context.Context, ref string) (bool, error) {
	if err := e.check("exists"); err != nil {
		return false, err
	}
	var args []string
	if e.Name == Podman {
		// Exit 0: present; exit 1: absent; anything else: failure.
		args = []string{"image", "exists", ref}
	} else {
		args = []string{"image", "inspect", "--format", "{{.Id}}", ref}
	}
	_, err := e.Runner.Run(ctx, string(e.Name), args...)
	if err == nil {
		return true, nil
	}
	if re, ok := errors.AsType[*RunError](err); ok {
		if e.Name == Podman && re.ExitCode == 1 {
			return false, nil
		}
		if e.Name == Docker && strings.Contains(strings.ToLower(re.Stderr), "no such image") {
			return false, nil
		}
	}
	return false, fmt.Errorf("engine: exists: %w", err)
}

// ImageID returns the local image ID of ref under ctx, normalized to
// "sha256:<hex>" (podman prints bare hex, docker the prefixed form). It
// returns an error when the image is absent or the engine fails.
func (e *Engine) ImageID(ctx context.Context, ref string) (string, error) {
	out, err := e.run(ctx, "image id", "image", "inspect", "--format", "{{.Id}}", ref)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id == "" || strings.ContainsAny(id, " \n") {
		return "", fmt.Errorf("engine: image id: unexpected output %q for %s", id, ref)
	}
	if !strings.HasPrefix(id, "sha256:") {
		id = "sha256:" + id
	}
	return id, nil
}

// Save writes the local image ref to the file path under ctx as a
// docker-archive tarball, the format kind's LoadImageArchive reads, and
// returns an error if saving fails. Save a tagged reference: the archive
// records ref as the image's name, and a digest-only reference leaves the
// loaded image without one.
func (e *Engine) Save(ctx context.Context, ref, path string) error {
	if err := e.check("save"); err != nil {
		return err
	}
	args := []string{"save", "--output", path, ref}
	if e.Name == Podman {
		// podman's default is docker-archive too; say so, in case a
		// containers.conf changes it.
		args = []string{"save", "--format", "docker-archive", "--output", path, ref}
	}
	_, err := e.run(ctx, "save", args...)
	return err
}

// RunRemove runs the local image in a throwaway container under ctx
// (`run --rm --pull=never`) and returns its stdout. A non-empty entrypoint
// replaces the image's entrypoint, and args follow the image. It never
// pulls, so it always runs the local image (for example to smoke-check a
// freshly built one), and it returns an error when the container exits
// non-zero or cannot start.
func (e *Engine) RunRemove(ctx context.Context, image, entrypoint string, args ...string) ([]byte, error) {
	argv := []string{"run", "--rm", "--pull=never"}
	if entrypoint != "" {
		argv = append(argv, "--entrypoint", entrypoint)
	}
	argv = append(argv, image)
	argv = append(argv, args...)
	return e.run(ctx, "run --rm", argv...)
}

// KindProvider returns the kind library option that selects this engine's
// node provider (cluster.ProviderWithPodman or cluster.ProviderWithDocker).
// Pass it to cluster.NewProvider: the library never reads
// KIND_EXPERIMENTAL_PROVIDER. It returns an error for an unknown Name.
func (e *Engine) KindProvider() (cluster.ProviderOption, error) {
	switch e.Name {
	case Podman:
		return cluster.ProviderWithPodman(), nil
	case Docker:
		return cluster.ProviderWithDocker(), nil
	default:
		return nil, fmt.Errorf("engine: kind provider: unknown engine %q", e.Name)
	}
}

// KindNetworkEnv returns the environment variable kind's provider for this
// engine reads to pick its node network (KIND_EXPERIMENTAL_PODMAN_NETWORK
// or KIND_EXPERIMENTAL_DOCKER_NETWORK), or "" for an unknown Name. kind
// reads it from the process environment when it creates a cluster, so it
// must be set in-process (os.Setenv) before Create.
func (e *Engine) KindNetworkEnv() string {
	switch e.Name {
	case Podman:
		return "KIND_EXPERIMENTAL_PODMAN_NETWORK"
	case Docker:
		return "KIND_EXPERIMENTAL_DOCKER_NETWORK"
	default:
		return ""
	}
}
