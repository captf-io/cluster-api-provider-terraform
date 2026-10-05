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

// Package objects builds the Cluster API and CAPTF objects the end-to-end
// suites create, and names their resources.
//
// The builders return *unstructured.Unstructured (and typed Secrets), so the
// test module never imports the product's or Cluster API's Go types: field
// names are spelled out here and checked by unit tests, and, for the CAPTF
// kinds, against the CRD schemas in config/crd/bases. The CAPI objects use
// cluster.x-k8s.io/v1beta2, whose object references name an apiGroup rather
// than an apiVersion. The GVR variables are what the dynamic client needs to
// create, read and delete each kind. Builders take explicit names and a
// small options struct; a zero option means "leave the field out".
package objects
