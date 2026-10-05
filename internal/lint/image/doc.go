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

// Package image is `tfcapi-lint image`: it reads a built source image
// without running it,
// checks the image contract (the module and runtime paths, the provider
// mirror, the mounted paths, the OCI labels and the user), and runs the
// module checks on the extracted /captf/module.
//
// The image's flattened filesystem is streamed once. Every decision is made
// from the tar headers; only the module's regular files are written to
// disk (lint.LoadModule reads a directory), and the provider mirror's JSON
// indexes are read into memory. Nothing is written outside the temporary
// directory, links are never created, and the extraction is capped.
package image
