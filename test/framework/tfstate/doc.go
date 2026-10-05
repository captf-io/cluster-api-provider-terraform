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

// Package tfstate reads the Terraform state CAPTF's module Jobs write to the
// kubernetes backend, so an end-to-end test can compare a module's real
// outputs with what CAPTF reports in status and passes on to later Jobs.
//
// The state of one Terraform* object lives in the Secret
// tfstate-default-<suffix>, plus tfstate-default-<suffix>-part-N Secrets when
// Terraform chunks a large state. Each holds a slice of one gzip stream under
// the data key "tfstate". SuffixFor reimplements the product's naming
// algorithm (it cannot import it: the test module never depends on the
// product), pinned by a test vector computed with the product's own
// function. Read reassembles the chunks, gunzips them and decodes the
// version 4 state file. Every error is prefixed "tfstate:" and wraps its
// cause; a missing Secret wraps ErrNoState.
package tfstate
