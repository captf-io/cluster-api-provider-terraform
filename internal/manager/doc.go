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

// Package manager holds the manager's cache scoping, uncached-object list
// and typed Kubernetes scheme: the parts of the manager's wiring that have no
// dependency on the command line, so cmd/manager/app/options can import them
// without cmd/manager/app/options's flag parsing leaking into internal/.
// The manager's flags and options live in cmd/manager/app/options, since
// they are used only by that command; ManagedSecretLabel and
// JobOwnerKindLabel stay here because CacheOptions uses them directly.
//
// NewScheme registers every API group the manager reads or writes.
// CacheOptions and VariablesCacheOptions build the controller-runtime cache
// options for the manager's two caches (CAPD's DefaultNamespaces): the main
// cache scopes Secrets and Jobs to the labels CAPTF itself sets, and the
// second, label-scoped cache backs the spec.variablesFrom watches over
// operator-labeled ConfigMaps and Secrets, stripping their data with
// StripData before it is ever held in memory. UncachedObjects lists the
// types the default client reads straight from the API server instead of
// starting a cluster-wide informer for them, so code that must see objects
// outside the cache scope takes mgr.GetAPIReader() explicitly.
//
// Logging conventions: klog through logsv1, keys are klog.KObj(obj) under
// UpperCamelCase kind names (Cluster, Machine, TerraformCluster,
// TerraformMachine, Job, Secret, Identity); level 0 for
// errors and irreversible actions, 2 for the default flow, 4 for debug, 5 for
// trace. Bootstrap data, credentials and tfvars content are never logged.
package manager
