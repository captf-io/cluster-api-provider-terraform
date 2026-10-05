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

// This file defines the result document the runner writes to the
// termination message and the controller parses; internal/jobs uses these
// types, so there is one schema.

package runner

// ResultVersion is the version of the result document.
const ResultVersion = 1

// MaxResultBytes is the termination message cap: the kubelet truncates
// longer messages.
const MaxResultBytes = 4096

// Result is the compact JSON document written to /dev/termination-log.
type Result struct {
	Version int         `json:"version"`
	Op      string      `json:"op"`
	Image   ResultImage `json:"image"`
	Runtime Runtime     `json:"runtime"`
	Steps   []Step      `json:"steps"`
	Drift   *Drift      `json:"drift"`
	Error   *Error      `json:"error"`
	// Changes is what the apply or destroy step reported having changed,
	// from the runtime's final summary line; absent when the step printed
	// none (a failure, or another op). Encode drops it first.
	Changes *Changes `json:"changes,omitempty"`
	// Plan is the plan summary of a plan Job, or of an approved apply
	// whose plan changed (ErrorKindPlanChanged): what the controller
	// writes to status.plan. Encode drops its resources before its hash
	// and counts, which it never drops.
	Plan *Plan `json:"plan,omitempty"`
}

// Changes counts the resources an apply or destroy changed.
type Changes struct {
	Add     int `json:"add"`
	Change  int `json:"change"`
	Destroy int `json:"destroy"`
	Import  int `json:"import,omitempty"`
}

// ResultImage echoes the image reference the runner was given; the resolved
// digest comes from the pod status, not the runner.
type ResultImage struct {
	Ref             string `json:"ref"`
	ProvidersMirror bool   `json:"providersMirror"`
}

// Runtime is the binary invocation and its version.
type Runtime struct {
	Command []string `json:"command"`
	Version string   `json:"version"`
}

// Step is one runtime command.
type Step struct {
	Name    string  `json:"name"`
	Exit    int     `json:"exit"`
	Seconds float64 `json:"seconds"`
}

// Drift summarizes a drift plan.
type Drift struct {
	Detected  bool     `json:"detected"`
	Add       int      `json:"add"`
	Change    int      `json:"change"`
	Destroy   int      `json:"destroy"`
	Resources []string `json:"resources"`
}

// Error kinds, the values of the API's RunErrorKind
// (status.lastRun.error.kind). An interrupted run counts toward no retry
// backoff (internal/controllers/shared countFailures). A blocked run is a
// guarded apply that stopped before a plan deleting or replacing resources
// without an approval for its inputs hash; it changed nothing and is not
// retried until the inputs or the approval change. A plan-changed run is an
// apply approved for one plan (--expect-plan) whose plan is now another;
// it changed nothing, carries the new plan and waits for its approval.
const (
	ErrorKindImageLayout = "image-layout"
	ErrorKindStep        = "step"
	ErrorKindInterrupted = "interrupted"
	ErrorKindBlocked     = "blocked"
	ErrorKindPlanChanged = "plan-changed"
)

// Error describes the failure: an image that does not follow the image
// contract, or a failing step. Tail is despite its name a bounded, curated
// failure summary (MaxSummary bytes), not raw stderr: for a failing
// `validate` it is built from that step's JSON diagnostics, for every other
// step from the "Error: " lines of its stderr, falling back to a message
// naming the step and its exit code. The full stderr still reaches the pod
// log. The field keeps its original name because internal/controllers
// reads Error.Tail directly.
type Error struct {
	Kind string  `json:"kind"`
	Step *string `json:"step"`
	Tail string  `json:"tail"`
}
