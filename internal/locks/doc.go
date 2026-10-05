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

// Package locks detects stale state locks before a Job is created.
//
// Both backends lock through the Lease lock-tfstate-default-<suffix>:
// spec.holderIdentity carries the lock ID and the app.terraform.io/lock-info
// annotation the JSON lock info. Its Who field is fmt.Sprintf("%s@%s",
// user.Current().Username, os.Hostname()), verified in NewLockInfo of
// internal/states/statemgr/locker.go at Terraform v1.16.4 and OpenTofu
// v1.12.6. The user part is empty when user.Current fails, as in a
// distroless image without /etc/passwd. In a Job pod the hostname is the pod
// name: Job names are capped at 57 characters (internal/jobs) so the pod
// name stays within the kubelet's 63-character hostname limit.
//
// A lock is stale only when its holder is known and that pod no longer
// exists. A lock whose holder cannot be determined is never forced.
package locks
