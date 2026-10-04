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

package enginetest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
)

// Call is one recorded command.
type Call struct {
	// Name is the program.
	Name string
	// Args are its arguments.
	Args []string
}

// Line returns the call's command line: Name and Args joined by single
// spaces, the key On matches against.
func (c Call) Line() string {
	return strings.Join(append([]string{c.Name}, c.Args...), " ")
}

// Response is what a fake command returns.
type Response struct {
	// Stdout is the command's stdout.
	Stdout []byte
	// Err is the command's error, typically from Exit; nil means success.
	Err error
}

// Runner is a fake engine.Runner. It is safe for concurrent use; build it
// with New and script it before the code under test runs.
type Runner struct {
	// mu guards every field below.
	mu sync.Mutex
	// calls is every Run call, in order.
	calls []Call
	// responses maps a command line to its scripted response.
	responses map[string]Response
	// respond, when set, answers every command On did not script.
	respond func(Call) Response
}

// Compile-time check that Runner is an engine.Runner.
var _ engine.Runner = (*Runner)(nil)

// New returns an empty Runner, which fails every command until scripted.
func New() *Runner {
	return &Runner{responses: map[string]Response{}}
}

// On scripts the command whose Line is line to return stdout and err, and
// returns r for chaining. A later On for the same line replaces it.
func (r *Runner) On(line, stdout string, err error) *Runner {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.responses[line] = Response{Stdout: []byte(stdout), Err: err}
	return r
}

// Respond sets fn to answer every command On did not script, and returns
// r for chaining.
func (r *Runner) Respond(fn func(c Call) Response) *Runner {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.respond = fn
	return r
}

// Run records the call of name with args and returns its scripted
// response: ctx's error if ctx is done, the On response for its line, else
// the Respond answer, else an "unexpected command" error.
func (r *Runner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	c := Call{Name: name, Args: slices.Clone(args)}
	r.mu.Lock()
	r.calls = append(r.calls, c)
	resp, ok := r.responses[c.Line()]
	respond := r.respond
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ok {
		if respond == nil {
			return nil, fmt.Errorf("enginetest: unexpected command %q", c.Line())
		}
		resp = respond(c)
	}
	return slices.Clone(resp.Stdout), resp.Err
}

// Calls returns a copy of every recorded call, in order.
func (r *Runner) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Call, len(r.calls))
	for i, c := range r.calls {
		out[i] = Call{Name: c.Name, Args: slices.Clone(c.Args)}
	}
	return out
}

// Lines returns the Line of every recorded call, in order.
func (r *Runner) Lines() []string {
	calls := r.Calls()
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Line()
	}
	return out
}

// Exit returns the *engine.RunError of a command that exited with code and
// wrote stderr. Its Name and Args are empty; only ExitCode, Stderr and Err
// matter to the code under test.
func Exit(code int, stderr string) error {
	return &engine.RunError{
		ExitCode: code,
		Stderr:   stderr,
		Err:      fmt.Errorf("exit status %d", code),
	}
}
