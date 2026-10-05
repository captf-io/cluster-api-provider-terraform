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

// Package diag collects the artifacts needed to debug a test environment:
// pod logs, events, the CAPI and CAPTF objects, the nodes and, through a
// hook, the kind node logs.
//
// Collect is best-effort. A failure for one item is appended to
// errors.txt in the artifact directory and collection continues, because a
// broken cluster is exactly when the artifacts matter. It returns an error
// only when nothing at all could be written.
//
// Secrets are never collected: the Secret resource is skipped even when a
// caller lists it, and no object of kind Secret is written. Pod logs are
// copied as the cluster serves them.
//
// The layout under the artifact directory is:
//
//	pods/<namespace>/<pod>/<container>.log        (+ .previous.log after a restart)
//	events/<namespace>.yaml
//	objects/<resource>/<namespace>/<name>.yaml    (cluster-scoped: namespace "_cluster")
//	nodes.yaml
//	node-logs/                                    (written by Options.NodeLogs)
//	errors.txt                                    (only when something failed)
//
// Like wait, the package uses client-go only and keeps CAPI and CAPTF
// objects unstructured. Every error is prefixed "diag:" and wraps its
// cause.
package diag
