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

package render

import (
	"fmt"
	"strconv"
)

// CLIConfigPath is where the runner writes the CLI configuration and what
// TF_CLI_CONFIG_FILE points at when the image ships a provider mirror.
const CLIConfigPath = "/captf/work/cli.tfrc"

// CLIConfig returns the provider_installation block that makes init use only
// the provider mirror at providersDir. The three-segment */*/* matches every
// provider on every registry host; the two-segment */* would match only the
// default host (verified with Terraform 1.16.4 and OpenTofu 1.12.6).
func CLIConfig(providersDir string) []byte {
	return fmt.Appendf(nil, `provider_installation {
  filesystem_mirror {
    path    = %s
    include = ["*/*/*"]
  }
  direct {
    exclude = ["*/*/*"]
  }
}
`, strconv.Quote(providersDir))
}
