/*
Copyright 2026 The cluster-api-provider-terraform Authors.

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

// Package contract is the v1alpha1 module contract as Go types: one inputs
// and one outputs struct per role, the input and required-output name sets,
// the Terraform declaration of every input (InputSpecs: type expression,
// nullable, sensitive), and the contract's JSON Schemas (embedded
// verbatim). The renderer declares the generated root from InputSpecs and
// tfcapi-lint checks modules against it, so the two cannot disagree.
//
// The same inputs structs are rendered into terraform.tfvars.json, hashed
// into captf.io/inputs-hash and stored in the durable inputs Secret, so
// "rendered inputs are hashed inputs" holds by construction. JSON tags
// equal the contract's variable and output names. No input field is
// omitempty: the schemas require every input key, nullable ones as null.
//
// The package imports only the standard library, because tfcapi-lint uses
// it too; conversion from CAPI types lives with the callers.
package contract
