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

// Package greenlight is the record that says the e2e test cluster passed
// the foundation suite, and the check that later, harder e2e tests make
// before they rely on it.
//
// The foundation suite writes a Record (the cluster, the tree and manager
// image it ran against, the pins, each stage with its duration, the
// stability window and a timestamp) to <work dir>/greenlight.json with
// Write, which is atomic. A later test calls Require with a Check holding
// what is true now: the current pins, the current manager reference, a
// function that says whether the cluster still exists, the maximum age
// (24 hours, or CAPTF_E2E_GREENLIGHT_MAX_AGE) and a clock. Validate, the
// pure core, rejects a record of an unknown version, a missing cluster,
// pins or a manager reference that differ (every difference is listed), a
// record older than the maximum age and any stage that did not pass.
// Require turns a rejection into t.Fatalf, never a skip, and tells the
// reader to run `make e2e-foundation`.
//
// Every error is prefixed "greenlight:" and wraps its cause.
package greenlight
