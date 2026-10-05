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

// Package labels parses a source image's capacity labels: io.captf.capacity,
// a JSON object of resource name to quantity string such as
// {"cpu":"4","memory":"16Gi"}, and
// io.captf.node-info, a JSON object of {"architecture","operatingSystem"}
// naming the node the image boots. ParseCapacity and ParseNodeInfo decode
// and validate one label each, both rejecting unknown fields, trailing
// data and an empty or all-zero-value object, and both wrapping every
// failure in ErrInvalidLabel so a caller can distinguish "the label is
// present but malformed" from "the label is absent".
//
// The TerraformMachineTemplate reconciler reads these same labels through
// internal/imageinspect, and tfcapi-lint image checks a built image
// against the image contract using this package directly, so an image the
// linter accepts at build time is guaranteed to be one the controller can
// read the same way at reconcile time. This package imports no
// controller-runtime and pulls in only the Kubernetes API types it needs
// for the label values (corev1.ResourceList, infrav1.NodeInfo), so the
// lint binary that depends on it stays free of controller-runtime's much
// larger dependency graph.
package labels
