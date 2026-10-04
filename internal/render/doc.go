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

// Package render generates the root module every Job runs against:
// main.tf.json with the kubernetes backend stub, one variable per contract
// input and per user variable (spec.variables, variablesFrom), the module
// "role" call on the image's module and a sensitive re-export of every
// required output; and terraform.tfvars.json with the input values. Both
// are built with encoding/json, never templates.
//
// The tfvars are the marshaled contract inputs struct plus the user
// variables, the same values the inputs hash covers, so rendered inputs are
// hashed inputs.
package render
