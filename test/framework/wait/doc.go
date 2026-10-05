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

// Package wait polls a management cluster until it is ready, and reports
// while it waits.
//
// Every wait checks at once, then every Options.Interval, and prints the
// current status plus a summary of the non-ready pods every
// Options.ReportEvery, so a stuck bring-up is visible in seconds instead of
// at the timeout. A wait ends on success, on the timeout (the error names
// the last status seen) or when the context is done.
//
// The waits are DeploymentsAvailable (provider Deployments rolled out),
// CRDsEstablished and WebhookServing (the CAPTF admission webhook, not
// merely the API server, rejects an invalid TerraformCluster). Clients is
// built from an explicit kubeconfig path only: default loading rules are
// never used, so the operator's own kubeconfig is never read.
//
// The package needs only client-go: CAPI and CAPTF objects are
// unstructured, so the test module never imports the product's types.
// Every error is prefixed "wait:" and wraps its cause.
package wait
