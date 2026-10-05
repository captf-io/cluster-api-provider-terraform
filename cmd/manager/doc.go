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

// Package main is the CAPTF controller manager binary: it wires up and
// starts the admission webhooks, the TerraformCluster, TerraformMachine,
// TerraformMachineTemplate and TerraformClusterIdentity reconcilers, the
// orphan RBAC sweep, and the secured metrics and health/ready endpoints,
// then blocks in the controller-runtime manager's Start until the process
// receives a shutdown signal. main itself only builds the cobra command
// from cmd/manager/app and hands it to k8s.io/component-base/cli.Run, the
// way kube-controller-manager's cmd/kube-controller-manager/controller-manager.go
// does; command construction, flag parsing, option validation and the
// `manager version`/`manager --version` shortcuts live in cmd/manager/app
// and cmd/manager/app/options, and the reconcilers themselves live under
// internal/controllers, the way CAPD's manager does.
//
// The blank imports below load every client-go authentication plugin, for
// running the manager out-of-cluster against a variety of kubeconfig auth
// methods, and register the JSON logging format component-base offers
// alongside the default text one.
package main
