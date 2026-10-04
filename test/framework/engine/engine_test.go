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

package engine_test

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/kind/pkg/cluster"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine/enginetest"
)

// engines lists both supported engines for table tests.
var engines = []engine.Name{engine.Podman, engine.Docker}

// newEngine returns an Engine named n over a fresh fake Runner, and
// returns that Runner for scripting and assertions.
func newEngine(n engine.Name) (*engine.Engine, *enginetest.Runner) {
	r := enginetest.New()
	return &engine.Engine{Name: n, Runner: r}, r
}

// checkLines fails t unless r recorded exactly want.
func checkLines(t *testing.T, r *enginetest.Runner, want ...string) {
	t.Helper()
	if got := r.Lines(); !slices.Equal(got, want) {
		t.Errorf("commands = %q, want %q", got, want)
	}
}

// checkErr fails t unless err is non-nil, starts with prefix and wraps
// cause (when cause is non-nil).
func checkErr(t *testing.T, err error, prefix string, cause error) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want one starting %q", prefix)
	}
	if !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("error = %q, want prefix %q", err, prefix)
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Errorf("error %q does not wrap %q", err, cause)
	}
}

// TestParseName checks the accepted spellings and the rejects.
func TestParseName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want engine.Name
	}{
		{"podman", engine.Podman},
		{"docker", engine.Docker},
		{" docker\n", engine.Docker},
	} {
		got, err := engine.ParseName(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseName(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"", "Podman", "nerdctl", "podman docker"} {
		if _, err := engine.ParseName(in); err == nil {
			t.Errorf("ParseName(%q) succeeded, want an error", in)
		}
	}
}

// TestDetect covers the detection order, the override and its failures.
func TestDetect(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	tests := []struct {
		name     string
		override string
		script   map[string]error // command line -> error; unscripted lines fail
		want     engine.Name
		wantErr  string
		wantCmds []string
	}{
		{
			name:     "podman preferred",
			script:   map[string]error{"podman info": nil, "docker info": nil},
			want:     engine.Podman,
			wantCmds: []string{"podman info"},
		},
		{
			name:     "docker when podman fails",
			script:   map[string]error{"podman info": boom, "docker info": nil},
			want:     engine.Docker,
			wantCmds: []string{"podman info", "docker info"},
		},
		{
			name:     "neither",
			script:   map[string]error{"podman info": boom, "docker info": enginetest.Exit(1, "cannot connect")},
			wantErr:  "engine: detect: neither podman nor docker is usable",
			wantCmds: []string{"podman info", "docker info"},
		},
		{
			name:     "override docker skips podman",
			override: "docker",
			script:   map[string]error{"podman info": nil, "docker info": nil},
			want:     engine.Docker,
			wantCmds: []string{"docker info"},
		},
		{
			name:     "override podman",
			override: "podman",
			script:   map[string]error{"podman info": nil},
			want:     engine.Podman,
			wantCmds: []string{"podman info"},
		},
		{
			name:     "override not usable",
			override: "podman",
			script:   map[string]error{"podman info": boom, "docker info": nil},
			wantErr:  "engine: detect: podman was requested but is not usable",
			wantCmds: []string{"podman info"},
		},
		{
			name:     "invalid override probes nothing",
			override: "containerd",
			script:   map[string]error{"podman info": nil},
			wantErr:  "engine: detect: invalid override",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := enginetest.New()
			for line, err := range tc.script {
				r.On(line, "", err)
			}
			e, err := engine.Detect(t.Context(), tc.override, r)
			if tc.wantErr != "" {
				checkErr(t, err, tc.wantErr, nil)
				if e != nil {
					t.Errorf("engine = %+v, want nil on error", e)
				}
			} else {
				if err != nil {
					t.Fatalf("Detect: %v", err)
				}
				if e.Name != tc.want || e.Runner != r {
					t.Errorf("Detect = {%q %v}, want {%q, the given runner}", e.Name, e.Runner, tc.want)
				}
			}
			checkLines(t, r, tc.wantCmds...)
		})
	}
}

// TestDetectErrorsWrapBothProbes checks the no-engine error wraps each
// probe's error.
func TestDetectErrorsWrapBothProbes(t *testing.T) {
	t.Parallel()
	podmanErr, dockerErr := errors.New("podman down"), errors.New("docker down")
	r := enginetest.New().On("podman info", "", podmanErr).On("docker info", "", dockerErr)
	_, err := engine.Detect(t.Context(), "", r)
	if !errors.Is(err, podmanErr) || !errors.Is(err, dockerErr) {
		t.Errorf("error %q does not wrap both probe errors", err)
	}
}

// TestDetectCancelled checks a cancelled context stops detection after the
// first probe and surfaces the context error.
func TestDetectCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := enginetest.New().On("podman info", "", nil).On("docker info", "", nil)
	_, err := engine.Detect(ctx, "", r)
	checkErr(t, err, "engine: detect:", context.Canceled)
	checkLines(t, r, "podman info")
}

// TestCommands checks every typed method's argv for both engines.
func TestCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		call func(context.Context, *engine.Engine) error
		want map[engine.Name]string
	}{
		{
			name: "pull",
			call: func(ctx context.Context, e *engine.Engine) error { return e.Pull(ctx, "ghcr.io/x@sha256:abc") },
			want: map[engine.Name]string{
				engine.Podman: "podman pull --quiet ghcr.io/x@sha256:abc",
				engine.Docker: "docker pull --quiet ghcr.io/x@sha256:abc",
			},
		},
		{
			name: "tag",
			call: func(ctx context.Context, e *engine.Engine) error { return e.Tag(ctx, "src@sha256:abc", "dst:tag") },
			want: map[engine.Name]string{
				engine.Podman: "podman tag src@sha256:abc dst:tag",
				engine.Docker: "docker tag src@sha256:abc dst:tag",
			},
		},
		{
			name: "save",
			call: func(ctx context.Context, e *engine.Engine) error { return e.Save(ctx, "img:tag", "/tmp/a.tar") },
			want: map[engine.Name]string{
				engine.Podman: "podman save --format docker-archive --output /tmp/a.tar img:tag",
				engine.Docker: "docker save --output /tmp/a.tar img:tag",
			},
		},
		{
			name: "run remove with entrypoint",
			call: func(ctx context.Context, e *engine.Engine) error {
				_, err := e.RunRemove(ctx, "img:tag", "/runner", "--help")
				return err
			},
			want: map[engine.Name]string{
				engine.Podman: "podman run --rm --pull=never --entrypoint /runner img:tag --help",
				engine.Docker: "docker run --rm --pull=never --entrypoint /runner img:tag --help",
			},
		},
		{
			name: "run remove default entrypoint",
			call: func(ctx context.Context, e *engine.Engine) error {
				_, err := e.RunRemove(ctx, "img:tag", "", "--help")
				return err
			},
			want: map[engine.Name]string{
				engine.Podman: "podman run --rm --pull=never img:tag --help",
				engine.Docker: "docker run --rm --pull=never img:tag --help",
			},
		},
		{
			name: "raw run",
			call: func(ctx context.Context, e *engine.Engine) error {
				_, err := e.Run(ctx, "network", "ls")
				return err
			},
			want: map[engine.Name]string{
				engine.Podman: "podman network ls",
				engine.Docker: "docker network ls",
			},
		},
	}
	for _, tc := range tests {
		for _, n := range engines {
			t.Run(tc.name+"/"+string(n), func(t *testing.T) {
				t.Parallel()
				e, r := newEngine(n)
				r.On(tc.want[n], "", nil)
				if err := tc.call(t.Context(), e); err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
				checkLines(t, r, tc.want[n])
			})
		}
	}
}

// TestCommandErrors checks each method wraps the runner's error under its
// "engine: <op>:" prefix.
func TestCommandErrors(t *testing.T) {
	t.Parallel()
	cause := enginetest.Exit(125, "Error: something broke")
	tests := []struct {
		prefix string
		call   func(context.Context, *engine.Engine) error
	}{
		{"engine: pull: ", func(ctx context.Context, e *engine.Engine) error { return e.Pull(ctx, "x") }},
		{"engine: tag: ", func(ctx context.Context, e *engine.Engine) error { return e.Tag(ctx, "x", "y") }},
		{"engine: save: ", func(ctx context.Context, e *engine.Engine) error { return e.Save(ctx, "x", "/p") }},
		{"engine: run --rm: ", func(ctx context.Context, e *engine.Engine) error {
			_, err := e.RunRemove(ctx, "x", "")
			return err
		}},
		{"engine: run: ", func(ctx context.Context, e *engine.Engine) error {
			_, err := e.Run(ctx, "ps")
			return err
		}},
		{"engine: exists: ", func(ctx context.Context, e *engine.Engine) error {
			_, err := e.Exists(ctx, "x")
			return err
		}},
		{"engine: image id: ", func(ctx context.Context, e *engine.Engine) error {
			_, err := e.ImageID(ctx, "x")
			return err
		}},
	}
	for _, tc := range tests {
		for _, n := range engines {
			t.Run(tc.prefix+string(n), func(t *testing.T) {
				t.Parallel()
				e, r := newEngine(n)
				r.Respond(func(enginetest.Call) enginetest.Response { return enginetest.Response{Err: cause} })
				err := tc.call(t.Context(), e)
				checkErr(t, err, tc.prefix, cause)
				var re *engine.RunError
				if !errors.As(err, &re) || re.ExitCode != 125 {
					t.Errorf("error %q does not carry the RunError with exit code 125", err)
				}
			})
		}
	}
}

// TestInvalidEngine checks every method refuses an Engine with an unknown
// Name or no Runner, without running anything.
func TestInvalidEngine(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := enginetest.New()
	bad := []*engine.Engine{{Name: "nerdctl", Runner: r}, {Name: engine.Podman}}
	for _, e := range bad {
		if err := e.Pull(ctx, "x"); err == nil {
			t.Errorf("%+v: Pull succeeded", e)
		}
		if err := e.Tag(ctx, "x", "y"); err == nil {
			t.Errorf("%+v: Tag succeeded", e)
		}
		if err := e.Save(ctx, "x", "/p"); err == nil {
			t.Errorf("%+v: Save succeeded", e)
		}
		if _, err := e.Exists(ctx, "x"); err == nil {
			t.Errorf("%+v: Exists succeeded", e)
		}
		if _, err := e.ImageID(ctx, "x"); err == nil {
			t.Errorf("%+v: ImageID succeeded", e)
		}
		if _, err := e.RunRemove(ctx, "x", ""); err == nil {
			t.Errorf("%+v: RunRemove succeeded", e)
		}
		if _, err := e.Run(ctx, "ps"); err == nil {
			t.Errorf("%+v: Run succeeded", e)
		}
	}
	checkLines(t, r)
	unknown := &engine.Engine{Name: "nerdctl", Runner: r}
	if _, err := unknown.KindProvider(); err == nil {
		t.Error("KindProvider succeeded for an unknown engine")
	}
	if got := unknown.KindNetworkEnv(); got != "" {
		t.Errorf("KindNetworkEnv = %q for an unknown engine, want empty", got)
	}
}

// TestExists covers present, absent and failure for both engines.
func TestExists(t *testing.T) {
	t.Parallel()
	podmanLine := "podman image exists img"
	dockerLine := "docker image inspect --format {{.Id}} img"
	tests := []struct {
		name    string
		engine  engine.Name
		line    string
		err     error
		want    bool
		wantErr bool
	}{
		{"podman present", engine.Podman, podmanLine, nil, true, false},
		{"podman absent", engine.Podman, podmanLine, enginetest.Exit(1, ""), false, false},
		{"podman failure", engine.Podman, podmanLine, enginetest.Exit(125, "Error: storage"), false, true},
		{"docker present", engine.Docker, dockerLine, nil, true, false},
		{"docker absent", engine.Docker, dockerLine, enginetest.Exit(1, "Error response from daemon: No such image: img"), false, false},
		{"docker failure", engine.Docker, dockerLine, enginetest.Exit(1, "Cannot connect to the Docker daemon"), false, true},
		{"non-exit error", engine.Podman, podmanLine, errors.New("boom"), false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, r := newEngine(tc.engine)
			r.On(tc.line, "", tc.err)
			got, err := e.Exists(t.Context(), "img")
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("Exists = %v, %v; want %v, error %v", got, err, tc.want, tc.wantErr)
			}
			if err != nil {
				checkErr(t, err, "engine: exists: ", tc.err)
			}
			checkLines(t, r, tc.line)
		})
	}
}

// TestImageID checks the ID is normalized to sha256:<hex> for both
// engines' output, and that empty output is an error.
func TestImageID(t *testing.T) {
	t.Parallel()
	const hex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tests := []struct {
		engine  engine.Name
		stdout  string
		want    string
		wantErr bool
	}{
		{engine.Podman, hex + "\n", "sha256:" + hex, false},
		{engine.Docker, "sha256:" + hex + "\n", "sha256:" + hex, false},
		{engine.Podman, "\n", "", true},
		{engine.Docker, "a b\n", "", true},
	}
	for _, tc := range tests {
		t.Run(string(tc.engine)+"/"+strings.TrimSpace(tc.stdout), func(t *testing.T) {
			t.Parallel()
			e, r := newEngine(tc.engine)
			line := string(tc.engine) + " image inspect --format {{.Id}} img"
			r.On(line, tc.stdout, nil)
			got, err := e.ImageID(t.Context(), "img")
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("ImageID = %q, %v; want %q, error %v", got, err, tc.want, tc.wantErr)
			}
			checkLines(t, r, line)
		})
	}
}

// TestRunRemoveStdout checks RunRemove returns the container's stdout.
func TestRunRemoveStdout(t *testing.T) {
	t.Parallel()
	e, r := newEngine(engine.Podman)
	r.On("podman run --rm --pull=never img --help", "usage: manager\n", nil)
	out, err := e.RunRemove(t.Context(), "img", "", "--help")
	if err != nil || string(out) != "usage: manager\n" {
		t.Errorf("RunRemove = %q, %v; want the container's stdout", out, err)
	}
}

// TestKind checks the kind provider option and network variable for each
// engine.
func TestKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     engine.Name
		wantFunc string
		wantEnv  string
	}{
		{engine.Podman, "ProviderWithPodman", "KIND_EXPERIMENTAL_PODMAN_NETWORK"},
		{engine.Docker, "ProviderWithDocker", "KIND_EXPERIMENTAL_DOCKER_NETWORK"},
	}
	for _, tc := range tests {
		e, _ := newEngine(tc.name)
		opt, err := e.KindProvider()
		if err != nil {
			t.Fatalf("%s: KindProvider: %v", tc.name, err)
		}
		// Each option is a closure built by its constructor; inlined or
		// not, the closure's symbol names the constructor
		// ("...ProviderWithPodman.func1").
		if got := optionFunc(opt); !strings.Contains(got, "."+tc.wantFunc+".func") {
			t.Errorf("%s: KindProvider returned %s, want cluster.%s()", tc.name, got, tc.wantFunc)
		}
		if got := e.KindNetworkEnv(); got != tc.wantEnv {
			t.Errorf("%s: KindNetworkEnv = %q, want %q", tc.name, got, tc.wantEnv)
		}
	}
}

// optionFunc returns the symbol name of the closure inside the kind
// provider option opt.
func optionFunc(opt cluster.ProviderOption) string {
	return runtime.FuncForPC(reflect.ValueOf(opt).Pointer()).Name()
}
