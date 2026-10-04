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

// Package plankey owns the per-object plan fingerprint key: a Secret,
// captf-plankey-<kindshort>-<name>, owned by the Terraform* object and
// holding KeySize random bytes. Plan and apply Jobs mount it at MountPath,
// and the runner keys the plan fingerprint (HMAC-SHA256) with it, so the
// fingerprint published in status binds every planned value, sensitive
// ones included, without letting a reader of status guess a value from it.
//
// Ensure creates the Secret once and never rotates it, Delete removes it,
// and Load reads the key back inside the Job. Never log the key.
package plankey
