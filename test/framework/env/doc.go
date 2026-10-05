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

// Package env orchestrates the CAPTF test environment: it composes engine,
// kindcluster, images, providers, wait and diag into five operations, each
// logged step by step with timings to one io.Writer.
//
//   - Up builds and smoke-checks the manager image from the tree, pulls the
//     pinned noop images, creates the kind cluster (or reuses it), side-loads
//     the images into every node, installs cert-manager, cluster-api and
//     CAPTF through a clusterctl local repository, waits for readiness and
//     writes state.json and env.sh into the work directory. Any failure once
//     the cluster exists collects diagnostics first.
//   - Down deletes the named cluster (or every captf-test-* cluster), removes
//     its work directory except artifacts/, and removes the dedicated kind
//     network once no test cluster is left.
//   - Status reports whether the cluster exists, its nodes, the non-ready
//     provider pods and a summary of state.json.
//   - Collect writes a diagnostics bundle on demand.
//   - Reload rebuilds the manager image, side-loads it and points the
//     captf-controller-manager Deployment at it: the inner loop.
//
// Config comes from the environment (ConfigFromEnv); the Make targets set
// it. The work directory of cluster <name> is <repo>/bin/testenv/<name>/.
//
// Every side effect sits behind one of three small interfaces, so the
// sequence and its decisions are unit-tested with fakes: Host (processes:
// git, make, the container engine, clusterctl), Cluster (kind) and Kube
// (the cluster's API). New wires the real ones unless WithDeps replaces
// them.
//
// Safety: every cluster name passes kindcluster.ValidateName before it is
// touched, every child process gets an explicit KUBECONFIG (the
// orchestrator never reads ~/.kube/config), and Down removes only paths
// under <repo>/bin/testenv/ named after a valid cluster.
package env
