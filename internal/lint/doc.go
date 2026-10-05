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

// Package lint is tfcapi-lint's engine: it loads a module
// directory with terraform-config-inspect and checks its variables and
// outputs against the contract in internal/contract, the same source the
// controller renders the generated root from, so the tool and the
// controller never disagree on names or types.
//
// A second pass parses the module with hcl/v2 directly, for what
// terraform-config-inspect does not expose: terraform { backend | cloud }
// blocks, provider attributes, .tofu files and expressions. It reads
// OpenTofu's file set when the module ships .tofu files (x.tofu shadows
// x.tf), and module/tofu-shadow flags where that set's declarations differ
// from Terraform's, which the contract checks read. Its expression checks
// are best-effort: nothing is evaluated.
//
// A check yields Findings. Each finding has an ID (one of the check ID
// constants declared alongside the checks) and a severity, and a Report
// orders them deterministically by file, line and ID. The controller never
// validates modules; this is the only gate.
package lint
