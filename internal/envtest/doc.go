//go:build envtest

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

// Package envtest is the envtest tier: the CRDs' CEL rules, the admission
// webhooks and the engine's write paths, run against a real kube-apiserver
// and etcd started by controller-runtime's envtest. There is no kubelet and
// there are no controllers, so a test drives each reconcile by hand. The
// suite needs KUBEBUILDER_ASSETS (`make test-envtest` sets it) and the
// envtest build tag, so `go test ./...` never compiles it.
//
// Three API servers run for the whole package, started together by
// TestMain: bare, with the CRDs only (CEL alone), hooked, with the
// validating webhooks installed and served by the real handlers of
// internal/webhooks, and failopen, a second webhook environment whose
// webhook server a test stops.
package envtest
