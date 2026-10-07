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

// Package runner is the Job-side runner shared by cmd/runner and its tests.
// The runner binary is injected into arbitrary source images, so it is
// built static (CGO_ENABLED=0).
//
// Diagnostic logging (step start/finish, plan summaries, guard decisions,
// event-emission failures) goes through the klog.Logger in ctx
// (klog.FromContext), never straight to Options.Stderr. In the binary
// that is the klog logger cmd/runner/app configures from the
// component-base logging flags (--logging-format, -v, --vmodule); tests
// attach their own logger to ctx. Options.Stdout and Options.Stderr carry
// only the runtime's own step output.
//
// Verified runtime behavior (Terraform 1.16.4, OpenTofu 1.12.6):
//
//   - `version -json` reports the runtime version under the key
//     "terraform_version" in both runtimes.
//   - force-unlock needs an initialized backend ("Backend initialization
//     required" otherwise), so Steps runs init, then force-unlock, then the
//     operation. init with -lock-timeout succeeds while the lock is held.
//   - A module missing a contract output fails at validate
//     ("Unsupported attribute").
//   - A guarded apply (--guard-deletes) runs `plan -detailed-exitcode
//     -out=<workdir>/apply.tfplan`, `show -json` of that file and `apply
//     -input=false -no-color -lock-timeout=<t> <planfile>`: a saved plan
//     needs no -auto-approve and takes no -var-file. `show -json` reports a
//     replace as ["delete","create"] (or ["create","delete"] under
//     create_before_destroy) and a removal as ["delete"] (checked with
//     OpenTofu 1.11.5).
//   - An apply or destroy that cannot write the state to the backend at
//     its end writes it to errored.tfstate in the working directory
//     instead and exits non-zero; the runner then pushes that file with
//     `state push` (pushErroredState), so the resources the step created
//     are recorded rather than lost with the pod.
package runner
