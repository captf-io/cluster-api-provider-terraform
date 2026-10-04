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

package render

// The fixed paths of the image contract and of the Job's /captf/work
// emptyDir.
const (
	// ModuleSource is how the generated root calls the image's module: a
	// relative local path, because Terraform/OpenTofu treat only ./ and ../
	// paths as local. From RootDir it resolves to ModuleDir.
	ModuleSource = "../../module"
	// ModuleDir is the image's module.
	ModuleDir = "/captf/module"
	// RuntimePath is the image's terraform or tofu binary.
	RuntimePath = "/captf/runtime"
	// ProvidersDir is the image's optional provider mirror.
	ProvidersDir = "/captf/providers"
	// WorkDir is the writable emptyDir of every Job.
	WorkDir = "/captf/work"
	// RootDir holds the rendered main.tf.json and terraform.tfvars.json.
	RootDir = "/captf/work/root"
	// DataDir is TF_DATA_DIR.
	DataDir = "/captf/work/.terraform"
)

// File names of the rendered root.
const (
	MainTFFile = "main.tf.json"
	TFVarsFile = "terraform.tfvars.json"
)
