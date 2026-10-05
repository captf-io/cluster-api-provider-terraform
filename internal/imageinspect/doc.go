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

// Package imageinspect reads a source image's config (not its layers) from
// the registry, with the pull secrets of a namespace, and parses the
// io.captf.capacity and io.captf.node-info labels that feed
// TerraformMachineTemplate status for Cluster Autoscaler scale-from-zero.
//
// Only registry credentials from the named pull Secrets are used: never
// the manager's own Docker config or node credentials. Credentials stay in
// memory and are never logged.
package imageinspect
